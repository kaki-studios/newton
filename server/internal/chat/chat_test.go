package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/kaki-studios/newton/server/internal/llm"
	"github.com/kaki-studios/newton/server/internal/quota"
	"github.com/kaki-studios/newton/server/internal/store"
)

// --- fixtures ---------------------------------------------------------------------------------

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var noon = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

type harness struct {
	echo    *echo.Echo
	handler *Handler
	store   *store.Store
	quota   *quota.Manager
	clock   *clock
	userID  string
	limits  quota.Limits
	ids     *idSequence
}

// idSequence mints deterministic ids: the SSE fixtures need stable ids, and the quota table needs
// them to be unique.
type idSequence struct {
	mu sync.Mutex
	n  int
}

func newIDSequence() *idSequence { return &idSequence{} }

func (s *idSequence) NewID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("gen-fixture-%d", s.n)
}

type options struct {
	enabled  bool
	provider llm.Provider
	limits   quota.Limits
	noUser   bool
}

func newHarness(t *testing.T, opts options) *harness {
	t.Helper()
	clk := &clock{now: noon}
	ids := newIDSequence()
	st, err := store.Open(context.Background(), store.Options{
		Path: filepath.Join(t.TempDir(), "newton.db"),
		Now:  clk.Now,
	})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	user, err := st.UpsertIdentity(context.Background(), store.Identity{
		Provider: "github", ProviderUserID: "u1", Email: "chat@example.com",
	})
	if err != nil {
		t.Fatalf("UpsertIdentity: %v", err)
	}

	limits := opts.limits
	if limits.StaleAfter == 0 {
		limits.StaleAfter = quota.DefaultStaleAfter
	}
	qm := &quota.Manager{
		Store:  st,
		Limits: limits,
		// The SSE fixtures are compared byte-for-byte, so ids cannot be random. A counter
		// keeps them deterministic *and* unique, which is what a real id source guarantees.
		NewID: ids.NewID,
	}

	provider := opts.provider
	if provider == nil {
		provider = llm.NewFake()
	}

	var userID string
	if !opts.noUser {
		userID = user.ID
	}
	h, err := New(Config{
		Enabled:  opts.enabled,
		Provider: provider,
		Quota:    qm,
		User: func(echo.Context) (string, bool) {
			if userID == "" {
				return "", false
			}
			return userID, true
		},
		NewID:     func() string { return "conv-fixture" },
		KeepAlive: -1, // no 15s ticker in tests
		Model:     "fake-model",
		Logger:    testLogger(t),
	})
	if err != nil {
		t.Fatalf("chat.New: %v", err)
	}

	e := echo.New()
	e.HideBanner = true
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Response().Header().Set(echo.HeaderXRequestID, "req-fixture")
			return next(c)
		}
	})
	h.Register(e.Group("/api"))

	return &harness{echo: e, handler: h, store: st, quota: qm, clock: clk, userID: user.ID, limits: limits, ids: ids}
}

func (h *harness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, "application/json")
	rec := httptest.NewRecorder()
	h.echo.ServeHTTP(rec, req)
	return rec
}

func (h *harness) postJSON(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encoding the request: %v", err)
	}
	return h.post(t, string(encoded))
}

// --- the contract fixtures --------------------------------------------------------------------

// The recorded files in testdata/sse are the whole Go↔TypeScript contract. The server must
// reproduce them byte for byte; src/ai/sse.test.ts asserts the client parses the same bytes. If
// this fails, the wire format changed and the fixture (and the client) must change with it.
func TestSSEMatchesRecordedFixtures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fixture  string
		provider llm.Provider
		body     map[string]any
	}{
		{
			name:     "happy path",
			fixture:  "happy-path.txt",
			provider: &llm.Fake{Chunk: len(llm.DefaultReply), Usage: llm.Usage{InputTokens: 2412, OutputTokens: 1033}},
			body:     map[string]any{"message": "a ball hanging from a string above a floor"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, options{
				enabled:  true,
				provider: tc.provider,
				limits:   quota.Limits{DailyGenerations: 10, MaxTurns: 10},
			})
			rec := h.postJSON(t, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
			}
			want := readFixture(t, tc.fixture)
			if got := rec.Body.String(); got != want {
				t.Fatalf("stream does not match %s\n--- got ---\n%s\n--- want ---\n%s", tc.fixture, got, want)
			}
			if !rec.Flushed {
				t.Error("the response was never flushed: a proxy would buffer the whole stream")
			}
		})
	}
}

func TestSSEErrorFrameMatchesFixture(t *testing.T) {
	h := newHarness(t, options{
		enabled:  true,
		provider: &llm.Fake{Reply: "partial reply", Chunk: 512, Fail: llm.ErrOverloaded, FailAfter: 1, Usage: llm.Usage{InputTokens: 1, OutputTokens: 2}},
		limits:   quota.Limits{DailyGenerations: 10, MaxTurns: 10},
	})
	rec := h.postJSON(t, map[string]any{"message": "anything"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the failure happens after the headers", rec.Code)
	}
	want := readFixture(t, "provider-error.txt")
	if got := rec.Body.String(); got != want {
		t.Fatalf("stream does not match provider-error.txt\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestKeepaliveCommentBetweenDeltas(t *testing.T) {
	// A provider that pauses between deltas, so the keepalive ticker can fire.
	slow := &pausingProvider{first: "first", second: "second", pause: 60 * time.Millisecond}
	h := newHarness(t, options{
		enabled:  true,
		provider: slow,
		limits:   quota.Limits{DailyGenerations: 10, MaxTurns: 10},
	})
	h.handler.cfg.KeepAlive = 10 * time.Millisecond

	rec := h.postJSON(t, map[string]any{"message": "anything"})
	body := rec.Body.String()
	if !strings.Contains(body, ": ping\n\n") {
		t.Fatalf("no keepalive comment was written:\n%s", body)
	}
	// The recorded keepalive fixture has one ping between the two deltas.
	want := readFixture(t, "keepalive.txt")
	if strings.Count(body, "event: delta") != strings.Count(want, "event: delta") {
		t.Fatalf("deltas were dropped around the keepalive:\n%s", body)
	}
}

// pausingProvider emits two deltas with a pause in between, so the keepalive has a window to
// write in.
type pausingProvider struct {
	first, second string
	pause         time.Duration
}

func (p *pausingProvider) Chat(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Usage, error) {
	if err := emit(p.first); err != nil {
		return llm.Usage{}, err
	}
	select {
	case <-ctx.Done():
		return llm.Usage{}, ctx.Err()
	case <-time.After(p.pause):
	}
	if err := emit(p.second); err != nil {
		return llm.Usage{}, err
	}
	return llm.Usage{InputTokens: 1, OutputTokens: 2}, nil
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "testdata", "sse", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

// --- request handling -------------------------------------------------------------------------

func TestChatIs503WhenDisabled(t *testing.T) {
	h := newHarness(t, options{enabled: false, limits: quota.Limits{DailyGenerations: 10}})
	rec := h.postJSON(t, map[string]any{"message": "hi"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "paused") {
		t.Errorf("body = %s, want it to say the AI is paused", rec.Body.String())
	}
	// Nothing was reserved: a disabled endpoint must not touch the quota table.
	var n int
	if err := h.store.DB().QueryRow(`select count(*) from generations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d generation rows were written while disabled", n)
	}
}

// This is the whole CSRF defence for a state-changing endpoint: a cross-site HTML form can only
// send form encodings or text/plain, and none of them may reach the handler.
func TestChatRequiresJSONContentType(t *testing.T) {
	h := newHarness(t, options{enabled: true, limits: quota.Limits{DailyGenerations: 10}})

	for _, contentType := range []string{
		"", // a form POST with no header at all
		"application/x-www-form-urlencoded",
		"multipart/form-data; boundary=x",
		"text/plain",
		"text/plain;charset=UTF-8",
		"application/jsonx", // a near miss must not match
		"application/x-json",
	} {
		t.Run("content type "+contentType, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"hi"}`))
			if contentType != "" {
				req.Header.Set(echo.HeaderContentType, contentType)
			}
			rec := httptest.NewRecorder()
			h.echo.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("Content-Type %q = %d %s, want 415", contentType, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "application/json") {
				t.Errorf("body = %s, want it to name the required header", rec.Body.String())
			}
		})
	}

	// Parameters of a JSON content type are irrelevant and must not be rejected: several clients
	// append a charset, and the media type is what matters.
	for _, contentType := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON", // media types are case-insensitive
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"hi"}`))
		req.Header.Set(echo.HeaderContentType, contentType)
		rec := httptest.NewRecorder()
		h.echo.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Content-Type %q = %d %s, want 200", contentType, rec.Code, rec.Body.String())
		}
	}
}

func TestChatRequiresASession(t *testing.T) {
	h := newHarness(t, options{enabled: true, noUser: true, limits: quota.Limits{DailyGenerations: 10}})
	rec := h.postJSON(t, map[string]any{"message": "hi"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestChatRejectsBadBodies(t *testing.T) {
	h := newHarness(t, options{enabled: true, limits: quota.Limits{DailyGenerations: 100, MaxTurns: 100}})

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		// A truncated body is a client bug, not a content-type problem: echoing the parser's
		// own message names it precisely.
		{"not json", "{", "not a valid chat request"},
		// An unknown key is refused rather than ignored: a typo in `message` must not silently
		// become an empty message.
		{"unknown key", `{"mesage":"hi"}`, "unknown field"},
		{"empty message", `{"message":"   "}`, "message is empty"},
		{"oversized message", `{"message":"` + strings.Repeat("a", MaxMessageBytes+1) + `"}`, "message is"},
		{"oversized scene", `{"message":"x","scene":"` + strings.Repeat("a", MaxSceneBytes+1) + `"}`, "scene is"},
		{
			"oversized report",
			`{"message":"x","report":{"ok":false,"issues":[{"path":"` + strings.Repeat("p", MaxReportBytes) + `","message":"m"}]}}`,
			"report is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.post(t, tc.body)
			if rec.Code != http.StatusBadRequest && !(tc.name == "not json" && rec.Code == http.StatusUnsupportedMediaType) {
				t.Fatalf("status = %d %s, want 400", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body = %s, want it to mention %q", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestChatQuotaExhaustedIs429(t *testing.T) {
	h := newHarness(t, options{enabled: true, limits: quota.Limits{DailyGenerations: 1, MaxTurns: 100}})

	if rec := h.postJSON(t, map[string]any{"message": "one"}); rec.Code != http.StatusOK {
		t.Fatalf("first request = %d %s", rec.Code, rec.Body.String())
	}
	rec := h.postJSON(t, map[string]any{"message": "two"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Daily limit reached") {
		t.Errorf("body = %s, want the quota message", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("no Retry-After header on a 429")
	}
}

func TestChatInFlightIs409(t *testing.T) {
	// A provider that blocks until the test releases it, so two requests really overlap.
	release := make(chan struct{})
	blocking := providerFunc(func(ctx context.Context, emit func(string) error) (llm.Usage, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return llm.Usage{}, ctx.Err()
		}
		return llm.Usage{}, emit("done")
	})
	h := newHarness(t, options{enabled: true, provider: blocking, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { firstDone <- h.postJSON(t, map[string]any{"message": "first"}) }()
	waitForPending(t, h.store, 1)

	second := h.postJSON(t, map[string]any{"message": "second"})
	if second.Code != http.StatusConflict {
		t.Fatalf("concurrent request = %d %s, want 409", second.Code, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "already being generated") {
		t.Errorf("body = %s, want the in-flight message", second.Body.String())
	}

	close(release)
	if rec := <-firstDone; rec.Code != http.StatusOK {
		t.Fatalf("the first request failed too: %d %s", rec.Code, rec.Body.String())
	}
}

func TestChatConversationTurnLimitIs429(t *testing.T) {
	h := newHarness(t, options{enabled: true, limits: quota.Limits{DailyGenerations: 100, MaxTurns: 1}})

	if rec := h.postJSON(t, map[string]any{"message": "one", "conversationId": "c1"}); rec.Code != http.StatusOK {
		t.Fatalf("first = %d %s", rec.Code, rec.Body.String())
	}
	rec := h.postJSON(t, map[string]any{"message": "two", "conversationId": "c1"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second turn = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Start a new conversation") {
		t.Errorf("body = %s, want the escape hatch named", rec.Body.String())
	}
	// A new conversation is a fresh budget: that is the advice the message gives.
	if rec := h.postJSON(t, map[string]any{"message": "three", "conversationId": "c2"}); rec.Code != http.StatusOK {
		t.Fatalf("new conversation = %d %s", rec.Code, rec.Body.String())
	}
}

// --- prompt assembly --------------------------------------------------------------------------

type recordingProvider struct {
	mu       sync.Mutex
	requests []llm.Request
}

func (p *recordingProvider) Chat(_ context.Context, req llm.Request, emit func(string) error) (llm.Usage, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	return llm.Usage{InputTokens: 1, OutputTokens: 2}, emit("```json\n{}\n```")
}

func (p *recordingProvider) last() llm.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[len(p.requests)-1]
}

func TestRepairTurnContainsEveryIssuePath(t *testing.T) {
	rec := &recordingProvider{}
	h := newHarness(t, options{enabled: true, provider: rec, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})

	recorder := h.postJSON(t, map[string]any{
		"message": "make the ramp steeper",
		"scene":   `{"bodies":[]}`,
		"report": map[string]any{
			"ok": false,
			"issues": []map[string]string{
				{"path": "bodies[1].shape.radius", "message": "too small: must be greater than 0"},
				{"path": "joints[0].bodyB", "message": "no body with id \"crte\""},
			},
		},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d %s", recorder.Code, recorder.Body.String())
	}

	last := rec.last()
	text := last.Messages[len(last.Messages)-1].Text
	for _, want := range []string{
		"make the ramp steeper",
		"bodies[1].shape.radius: too small: must be greater than 0",
		"joints[0].bodyB: no body with id \"crte\"",
		"```json",
		`{"bodies":[]}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the user turn is missing %q:\n%s", want, text)
		}
	}
	if last.MaxTokens != MaxCompletionTokens {
		t.Errorf("max_tokens = %d, want %d", last.MaxTokens, MaxCompletionTokens)
	}
	if last.Temperature != Temperature {
		t.Errorf("temperature = %v, want %v", last.Temperature, Temperature)
	}
	if !strings.Contains(last.System, "JSON Schema") && !strings.Contains(last.System, "$schema") {
		t.Error("the system prompt does not carry the schema")
	}
}

func TestRepairTurnIsOmittedWhenTheReportIsOK(t *testing.T) {
	rec := &recordingProvider{}
	h := newHarness(t, options{enabled: true, provider: rec, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})
	h.postJSON(t, map[string]any{
		"message": "again",
		"report":  map[string]any{"ok": true, "issues": []any{}},
	})
	text := rec.last().Messages[0].Text
	if strings.Contains(text, "rejected by the validator") {
		t.Errorf("a valid report still produced a repair instruction:\n%s", text)
	}
}

func TestRepairIssuesAreTruncated(t *testing.T) {
	rec := &recordingProvider{}
	h := newHarness(t, options{enabled: true, provider: rec, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})
	issues := make([]map[string]string, MaxIssues+5)
	for i := range issues {
		issues[i] = map[string]string{"path": "bodies[0]", "message": "issue"}
	}
	h.postJSON(t, map[string]any{
		"message": "",
		"report":  map[string]any{"ok": false, "issues": issues},
	})
	text := rec.last().Messages[0].Text
	if got := strings.Count(text, "- bodies[0]: issue"); got != MaxIssues {
		t.Fatalf("%d issues rendered, want %d (the cap)", got, MaxIssues)
	}
}

// --- settle and cancellation ------------------------------------------------------------------

func TestGenerationIsSettledOKWithUsage(t *testing.T) {
	h := newHarness(t, options{
		enabled:  true,
		provider: &llm.Fake{Chunk: 32, Usage: llm.Usage{InputTokens: 11, OutputTokens: 22}},
		limits:   quota.Limits{DailyGenerations: 10, MaxTurns: 10},
	})
	if rec := h.postJSON(t, map[string]any{"message": "hi"}); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	gen := h.onlyGeneration(t)
	if gen.Status != store.StatusOK {
		t.Errorf("status = %s, want ok", gen.Status)
	}
	if gen.InputTokens != 11 || gen.OutputTokens != 22 {
		t.Errorf("usage = %d/%d, want 11/22", gen.InputTokens, gen.OutputTokens)
	}
	if gen.ConversationID != "conv-fixture" {
		t.Errorf("conversation = %q, want the minted id", gen.ConversationID)
	}
	if gen.PromptVersion == "" {
		t.Error("prompt version not recorded")
	}
}

func TestSceneValidIsRecordedFromTheReport(t *testing.T) {
	h := newHarness(t, options{enabled: true, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})

	h.postJSON(t, map[string]any{
		"message": "fix it",
		"report":  map[string]any{"ok": false, "issues": []map[string]string{{"path": "bodies", "message": "nope"}}},
	})
	gen := h.onlyGeneration(t)
	if gen.SceneValid == nil || *gen.SceneValid {
		t.Fatalf("scene_valid = %v, want false", gen.SceneValid)
	}
}

func TestFailedProviderStillSettles(t *testing.T) {
	h := newHarness(t, options{
		enabled:  true,
		provider: &llm.Fake{Fail: errors.New("boom"), FailAfter: 1, Chunk: 4},
		limits:   quota.Limits{DailyGenerations: 10, MaxTurns: 10},
	})
	rec := h.postJSON(t, map[string]any{"message": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: the error is a frame, not a status", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("body = %s, want an error frame", rec.Body.String())
	}
	gen := h.onlyGeneration(t)
	if gen.Status != store.StatusFailed {
		t.Fatalf("status = %s, want failed", gen.Status)
	}
	// The pending slot is free again, so the user is not locked out by a provider failure.
	if rec := h.postJSON(t, map[string]any{"message": "again"}); rec.Code != http.StatusOK {
		t.Fatalf("a retry after a provider failure = %d, want 200", rec.Code)
	}
}

func TestClientDisconnectCancelsTheProviderAndSettles(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	provider := providerFunc(func(ctx context.Context, emit func(string) error) (llm.Usage, error) {
		close(started)
		if err := emit("partial"); err != nil {
			return llm.Usage{InputTokens: 5}, err
		}
		<-ctx.Done() // the client closes the tab
		close(canceled)
		return llm.Usage{InputTokens: 5, OutputTokens: 3}, ctx.Err()
	})

	h := newHarness(t, options{enabled: true, provider: provider, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"message":"hi"}`)).WithContext(ctx)
	req.Header.Set(echo.HeaderContentType, "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.echo.ServeHTTP(httptest.NewRecorder(), req)
	}()

	<-started
	cancel() // the tab closes
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the provider context was never canceled: closing the tab would keep costing money")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after the client disconnected")
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gen := h.onlyGeneration(t)
		if gen.Status == store.StatusCanceled {
			// A cancelled generation still counts against quota: that is the point.
			if gen.InputTokens != 5 {
				t.Errorf("input tokens = %d, want the 5 that were actually consumed", gen.InputTokens)
			}
			usage, err := h.quota.Today(context.Background(), h.userID)
			if err != nil {
				t.Fatal(err)
			}
			if usage.Used != 1 {
				t.Errorf("usage = %d, want the cancelled generation to count", usage.Used)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the generation was never settled as canceled")
}

func TestEveryGenerationEndsInATerminalState(t *testing.T) {
	// A table of outcomes, each of which must leave the row settled. A pending row would lock
	// the user out until the reaper noticed.
	for _, tc := range []struct {
		name     string
		provider llm.Provider
		want     store.Status
	}{
		{"ok", llm.NewFake(), store.StatusOK},
		{"provider error", &llm.Fake{Fail: errors.New("x"), FailAfter: 1, Chunk: 4}, store.StatusFailed},
		{"overloaded", &llm.Fake{Fail: llm.ErrOverloaded, FailAfter: 1, Chunk: 4}, store.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, options{enabled: true, provider: tc.provider, limits: quota.Limits{DailyGenerations: 10, MaxTurns: 10}})
			h.postJSON(t, map[string]any{"message": "hi"})
			if got := h.onlyGeneration(t).Status; got != tc.want {
				t.Fatalf("status = %s, want %s", got, tc.want)
			}
		})
	}
}

// --- helpers ----------------------------------------------------------------------------------

func (h *harness) onlyGeneration(t *testing.T) store.Generation {
	t.Helper()
	var id string
	if err := h.store.DB().QueryRow(`select id from generations order by created_at desc limit 1`).Scan(&id); err != nil {
		t.Fatalf("no generation row: %v", err)
	}
	gen, err := h.store.GenerationByID(context.Background(), id)
	if err != nil {
		t.Fatalf("loading generation %s: %v", id, err)
	}
	return gen
}

func waitForPending(t *testing.T, st *store.Store, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := st.DB().QueryRow(`select count(*) from generations where status = 'pending'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending generations", want)
}

// providerFunc adapts a function to the Provider interface, for tests that need the context.
type providerFunc func(ctx context.Context, emit func(string) error) (llm.Usage, error)

func (f providerFunc) Chat(ctx context.Context, _ llm.Request, emit func(string) error) (llm.Usage, error) {
	return f(ctx, emit)
}

// testLogger discards everything, but keeps slog's structured fields available if a test wants
// to assert on them via the handler's Logger field.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
