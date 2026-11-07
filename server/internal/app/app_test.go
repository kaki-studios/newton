package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/kaki-studios/newton/server/internal/auth"
	"github.com/kaki-studios/newton/server/internal/config"
	"github.com/kaki-studios/newton/server/internal/llm"
	"github.com/kaki-studios/newton/server/internal/store"
)

// These are the end-to-end tests from the plan: login → /api/me → chat with the fake provider →
// quota exhausted → 429, against the same wiring production uses. Only the OAuth round trip is
// replaced with a directly-created session, because a real one needs GitHub.

type e2e struct {
	app     *App
	echo    *echo.Echo
	store   *store.Store
	session *http.Cookie
	static  string
}

type e2eOptions struct {
	chatEnabled bool
	dailyLimit  int
	noAuth      bool
	provider    llm.Provider
	files       map[string]string
}

func newE2E(t *testing.T, opts e2eOptions) *e2e {
	t.Helper()

	static := t.TempDir()
	files := map[string]string{
		"index.html":           "<!doctype html><title>Newton</title>",
		"dist/main.js":         "export {};",
		"node_modules/zod.mjs": "export {};",
	}
	for name, content := range opts.files {
		files[name] = content
	}
	for name, content := range files {
		path := filepath.Join(static, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	st, err := store.Open(context.Background(), store.Options{Path: filepath.Join(t.TempDir(), "newton.db")})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Config{
		Addr:                 ":0",
		Static:               static,
		DB:                   filepath.Join(t.TempDir(), "unused.db"),
		Dev:                  true,
		BaseURL:              "http://localhost:8080",
		ChatEnabled:          opts.chatEnabled,
		Model:                "fake-model",
		DailyGenerations:     opts.dailyLimit,
		MaxConversationTurns: 6,
		ProviderKind:         "fake",
		SessionKey:           []byte("0123456789abcdef0123456789abcdef"),
		SessionTTL:           time.Hour,
		LogLevel:             "error",
		Version:              "test",
	}

	var a *auth.Auth
	if !opts.noAuth {
		a, err = auth.New(auth.Config{
			BaseURL:    cfg.BaseURL,
			Dev:        true,
			SessionTTL: cfg.SessionTTL,
			CookieKey:  cfg.SessionKey,
			Providers: []auth.Provider{{
				Name: "github", ClientID: "id", ClientSecret: "secret",
			}},
		}, st)
		if err != nil {
			t.Fatalf("auth.New: %v", err)
		}
	}

	provider := opts.provider
	if provider == nil {
		provider = &llm.Fake{Chunk: 64, Usage: llm.Usage{InputTokens: 10, OutputTokens: 20}}
	}

	application, err := New(context.Background(), Options{
		Config:   cfg,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})),
		Store:    st,
		Provider: provider,
		Auth:     a,
		NoAuth:   opts.noAuth,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	e := &e2e{app: application, echo: application.Echo, store: st, static: static}

	if !opts.noAuth {
		// Create the session directly: the OAuth round trip is auth's own test's job, and here it
		// would only add a provider mock.
		user, err := st.UpsertIdentity(context.Background(), store.Identity{
			Provider: "github", ProviderUserID: "u1", Email: "ada@example.com", DisplayName: "Ada",
		})
		if err != nil {
			t.Fatalf("UpsertIdentity: %v", err)
		}
		const token = "test-session-token"
		if _, err := st.CreateSession(context.Background(), user.ID, token, time.Hour); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		e.session = &http.Cookie{Name: auth.SessionCookieName(true), Value: token}
	}
	return e
}

func (e *e2e) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if e.session != nil {
		req.AddCookie(e.session)
	}
	rec := httptest.NewRecorder()
	e.echo.ServeHTTP(rec, req)
	return rec
}

func (e *e2e) post(t *testing.T, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(encoded)))
	req.Header.Set(echo.HeaderContentType, "application/json")
	if e.session != nil {
		req.AddCookie(e.session)
	}
	rec := httptest.NewRecorder()
	e.echo.ServeHTTP(rec, req)
	return rec
}

func TestHealthReportsSchemaAndDrift(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10})
	rec := e.get(t, "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("health body is not JSON: %v", err)
	}
	if body["ok"] != true {
		t.Errorf("ok = %v, want true", body["ok"])
	}
	if body["migrated"] != true {
		t.Errorf("migrated = %v, want true", body["migrated"])
	}
	if body["version"] != "test" {
		t.Errorf("version = %v, want test", body["version"])
	}
	if body["schema"] != float64(1) {
		t.Errorf("schema = %v, want 1", body["schema"])
	}
}

func TestHealthIs503WhenTheDatabaseIsGone(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10})
	// Closing the handle is the closest a test can get to the volume disappearing.
	if err := e.store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := e.get(t, "/api/health")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health = %d, want 503: a healthcheck must fail when the database is unreachable", rec.Code)
	}
}

func TestPublicConfigDoesNotNeedASession(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10, chatEnabled: true})
	e.session = nil
	rec := e.get(t, "/api/config")
	if rec.Code != http.StatusOK {
		t.Fatalf("config = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"chatEnabled":true`) {
		t.Errorf("body = %s, want chatEnabled", body)
	}
	if !strings.Contains(body, "github") {
		t.Errorf("body = %s, want the configured providers", body)
	}
}

func TestMeDefinesTheShapeTheComposerUses(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 3})
	rec := e.get(t, "/api/me")
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
		Quota       struct {
			Used     int    `json:"used"`
			Limit    int    `json:"limit"`
			ResetsAt string `json:"resetsAt"`
		} `json:"quota"`
		InFlight bool `json:"inFlight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("me is not JSON: %v", err)
	}
	if body.ID == "" || body.DisplayName != "Ada" {
		t.Errorf("me = %+v, want the signed-in user", body)
	}
	if body.Quota.Used != 0 || body.Quota.Limit != 3 {
		t.Errorf("quota = %+v, want 0/3", body.Quota)
	}
	if _, err := time.Parse(time.RFC3339, body.Quota.ResetsAt); err != nil {
		t.Errorf("resetsAt = %q, want RFC3339: %v", body.Quota.ResetsAt, err)
	}
	if body.InFlight {
		t.Error("inFlight = true with nothing running")
	}
}

// The two endpoints the account bar reads must not be served from a cache. /api/config carries the
// build's version, and /api/me carries one user's quota: the first going stale is a UI describing a
// previous deploy, the second going shared is a worse bug than a wrong number.
func TestAccountEndpointsAreNotCacheable(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 3})

	if got := e.get(t, "/api/config").Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("/api/config Cache-Control = %q, want \"no-cache\": the version changes per deploy", got)
	}
	if got := e.get(t, "/api/me").Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("/api/me Cache-Control = %q, want it to contain \"no-store\": the body is one "+
			"user's quota and nothing shared may retain it", got)
	}
}

func TestEndToEndChatThenQuotaExhaustion(t *testing.T) {
	e := newE2E(t, e2eOptions{chatEnabled: true, dailyLimit: 2})

	// 1. The first turn streams and lands a settled row.
	rec := e.post(t, "/api/chat", map[string]any{"message": "a ball on a string above a floor"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: delta") {
		t.Fatalf("no deltas were streamed: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: done") {
		t.Fatalf("no done event: %s", rec.Body.String())
	}

	// 2. /api/me now reports the spend.
	me := e.get(t, "/api/me")
	var body struct {
		Quota struct {
			Used  int `json:"used"`
			Limit int `json:"limit"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(me.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Quota.Used != 1 {
		t.Fatalf("used = %d, want 1", body.Quota.Used)
	}

	// 3. Use the second one and then hit the wall.
	if rec := e.post(t, "/api/chat", map[string]any{"message": "second"}); rec.Code != http.StatusOK {
		t.Fatalf("second chat = %d %s", rec.Code, rec.Body.String())
	}
	rec = e.post(t, "/api/chat", map[string]any{"message": "third"})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third chat = %d %s, want 429", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Daily limit reached") {
		t.Errorf("body = %s, want the actionable quota message", rec.Body.String())
	}

	// 4. The scene editor still works: the simulator never needs a session or a quota.
	if rec := e.get(t, "/"); rec.Code != http.StatusOK {
		t.Fatalf("static index = %d", rec.Code)
	}
}

func TestChatIs401WithoutASession(t *testing.T) {
	e := newE2E(t, e2eOptions{chatEnabled: true, dailyLimit: 10})
	e.session = nil
	rec := e.post(t, "/api/chat", map[string]any{"message": "hi"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestChatIs503WhenTheKillSwitchIsOff(t *testing.T) {
	e := newE2E(t, e2eOptions{chatEnabled: false, dailyLimit: 10})
	rec := e.post(t, "/api/chat", map[string]any{"message": "hi"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "paused") {
		t.Errorf("body = %s, want it to say paused", rec.Body.String())
	}
}

// Without BASE_URL there is no OAuth, so every auth route answers 503 with a reason instead of a
// 404 the client would have to interpret. That is what makes the degradation matrix one case.
func TestWithoutAuthEveryAuthRouteIs503(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10, noAuth: true})

	for _, path := range []string{"/api/me", "/api/auth/github"} {
		rec := e.get(t, path)
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503", path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "BASE_URL") {
			t.Errorf("GET %s body = %s, want it to name the setting", path, rec.Body.String())
		}
	}
	// Chat is not registered at all when there is no session source: anonymous paid generations
	// are the one thing that must be impossible.
	if rec := e.post(t, "/api/chat", map[string]any{"message": "hi"}); rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/chat without auth = %d, want 404", rec.Code)
	}
}

func TestUnknownAPIRouteIs404JSON(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10})
	rec := e.get(t, "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(ct, echo.MIMEApplicationJSON) {
		t.Errorf("Content-Type = %q, want JSON so a client can parse the error", ct)
	}
	if !strings.Contains(rec.Body.String(), "no such API endpoint") {
		t.Errorf("body = %s, want an actionable message", rec.Body.String())
	}
}

func TestStaticIsServedOnTheSameOrigin(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10})
	for _, tc := range []struct{ path, want string }{
		{"/", "Newton"},
		{"/index.html", "Newton"},
		{"/dist/main.js", "export"},
	} {
		rec := e.get(t, tc.path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("GET %s body = %q", tc.path, rec.Body.String())
		}
	}
	if rec := e.get(t, "/dist/mian.js"); rec.Code != http.StatusNotFound {
		t.Errorf("a typo in a dist path = %d, want 404", rec.Code)
	}
}

func TestProviderUnavailableDegradesChatOnly(t *testing.T) {
	// An unconfigured real provider must not take the process down: /api/health and the static app
	// keep working, and chat reports why it cannot.
	e := newE2E(t, e2eOptions{
		chatEnabled: true,
		dailyLimit:  10,
		provider:    &failingProvider{},
	})
	if rec := e.get(t, "/api/health"); rec.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", rec.Code)
	}
	if rec := e.get(t, "/"); rec.Code != http.StatusOK {
		t.Fatalf("static = %d, want 200", rec.Code)
	}
	rec := e.post(t, "/api/chat", map[string]any{"message": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("chat = %d: a provider failure is an SSE frame, not a status", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "event: error") {
		t.Errorf("body = %s, want an error frame", rec.Body.String())
	}
}

// failingProvider stands in for a real provider with no credentials: it answers every call with
// llm.ErrUnavailable.
type failingProvider struct{}

func (p *failingProvider) Chat(context.Context, llm.Request, func(string) error) (llm.Usage, error) {
	return llm.Usage{}, llm.ErrUnavailable
}

// Registration order is the routing policy: the /api/* JSON 404 and the static catch-all must come
// after every real route. Chat registered behind them would be shadowed, which is a bug that only
// shows up as "the endpoint 404s in production".
func TestChatRouteIsNotShadowedByTheCatchAlls(t *testing.T) {
	e := newE2E(t, e2eOptions{chatEnabled: true, dailyLimit: 10})

	// A handshake-shaped request: if chat is reachable, the handler answers 200 with SSE. If it
	// were shadowed, this would be the JSON 404 from /api/*.
	rec := e.post(t, "/api/chat", map[string]any{"message": "hi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/chat = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestHeadOnAStaticFile(t *testing.T) {
	e := newE2E(t, e2eOptions{dailyLimit: 10, files: map[string]string{"node_modules/planck.mjs": "export {};"}})
	req := httptest.NewRequest(http.MethodHead, "/node_modules/planck.mjs", nil)
	rec := httptest.NewRecorder()
	e.echo.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD .mjs = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get(echo.HeaderContentType); !strings.HasPrefix(ct, "text/javascript") {
		t.Fatalf("HEAD .mjs Content-Type = %q, want text/javascript", ct)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned %d body bytes, want none", rec.Body.Len())
	}
}

// A cold page load is dozens of requests: index.html, dist/*, and the importmap's zod tree. The
// per-IP limiter must not answer a real visitor with 429s -- an earlier burst of 20 did exactly
// that, which a browser test caught and this test pins.
func TestAColdPageLoadIsNotRateLimited(t *testing.T) {
	files := map[string]string{"index.html": "<title>Newton</title>", "dist/main.js": "export {};"}
	// The zod tree: a few hundred modules is the realistic worst case.
	for i := range 120 {
		files[fmt.Sprintf("node_modules/zod/v4/core/mod%d.js", i)] = "export {};"
	}
	e := newE2E(t, e2eOptions{dailyLimit: 10, files: files})

	for path := range files {
		rec := e.get(t, "/"+path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /%s = %d during a cold page load, want 200 (the limiter is too tight)", path, rec.Code)
		}
	}
}
