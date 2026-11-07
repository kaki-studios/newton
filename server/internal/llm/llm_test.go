package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFakeIsDeterministic(t *testing.T) {
	collect := func() string {
		var b strings.Builder
		usage, err := NewFake().Chat(context.Background(), Request{System: "sys"}, func(d string) error {
			b.WriteString(d)
			return nil
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		if usage.InputTokens == 0 || usage.OutputTokens == 0 {
			t.Fatalf("usage = %+v, want non-zero token counts", usage)
		}
		return b.String()
	}
	if a, b := collect(), collect(); a != b {
		t.Fatal("the fake provider is not deterministic: the SSE fixtures cannot be stable")
	}
}

func TestFakeStreamsInSeveralDeltas(t *testing.T) {
	var deltas []string
	if _, err := NewFake().Chat(context.Background(), Request{}, func(d string) error {
		deltas = append(deltas, d)
		return nil
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(deltas) < 5 {
		t.Fatalf("%d deltas, want the reply chunked", len(deltas))
	}
	if got := strings.Join(deltas, ""); got != DefaultReply {
		t.Fatal("the concatenated deltas do not equal the reply")
	}
}

func TestFakeStopsWhenTheConsumerFails(t *testing.T) {
	stop := errors.New("client went away")
	deltas := 0
	_, err := NewFake().Chat(context.Background(), Request{}, func(string) error {
		deltas++
		if deltas == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the consumer's error", err)
	}
	if deltas != 2 {
		t.Fatalf("emitted %d deltas after the consumer failed, want 2", deltas)
	}
}

func TestFakeHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewFake().Chat(ctx, Request{}, func(string) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestFakeReportsUsageOnFailure(t *testing.T) {
	// A cancelled or failed generation still costs money, so usage must come back with it.
	usage, err := (&Fake{Chunk: 4, Fail: ErrOverloaded, FailAfter: 2, Usage: Usage{7, 9}}).
		Chat(context.Background(), Request{}, func(string) error { return nil })
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("err = %v, want ErrOverloaded", err)
	}
	if usage != (Usage{7, 9}) {
		t.Fatalf("usage = %+v, want the partial usage to survive the error", usage)
	}
}

// fakeServer returns an httptest server that answers one streaming completion with frames.
func fakeServer(t *testing.T, status int, frames []string, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("Authorization = %q, want a bearer token", r.Header.Get("Authorization"))
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if body != "" {
			_, _ = io.WriteString(w, body)
			return
		}
		flusher, _ := w.(http.Flusher)
		for _, frame := range frames {
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

func dataFrame(content string) string {
	return `data: {"choices":[{"delta":{"content":` + jsonString(content) + `}}]}` + "\n\n"
}

func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestOpenAIStreamsDeltas(t *testing.T) {
	frames := []string{
		dataFrame("```json\n"),
		": keepalive\n\n",
		dataFrame(`{"bodies":[]}`),
		dataFrame("\n```\n"),
		`data: {"choices":[],"usage":{"prompt_tokens":2412,"completion_tokens":1033}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	srv := fakeServer(t, http.StatusOK, frames, "")
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})
	var got strings.Builder
	usage, err := p.Chat(context.Background(), Request{System: "s", Messages: []Message{{Role: RoleUser, Text: "hi"}}},
		func(d string) error {
			got.WriteString(d)
			return nil
		})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if want := "```json\n{\"bodies\":[]}\n```\n"; got.String() != want {
		t.Fatalf("content = %q, want %q", got.String(), want)
	}
	if usage != (Usage{2412, 1033}) {
		t.Fatalf("usage = %+v, want the reported token counts", usage)
	}
}

func TestOpenAIRequestShape(t *testing.T) {
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := decodeJSON(r.Body, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		captured = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k", Model: "configured-model"})
	if _, err := p.Chat(context.Background(), Request{
		System:    "the system prompt",
		Messages:  []Message{{Role: RoleUser, Text: "a block on a ramp"}},
		MaxTokens: 2000,
	}, func(string) error { return nil }); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if captured["model"] != "configured-model" {
		t.Errorf("model = %v, want the configured one", captured["model"])
	}
	if captured["stream"] != true {
		t.Errorf("stream = %v, want true", captured["stream"])
	}
	if captured["max_tokens"] != float64(2000) {
		t.Errorf("max_tokens = %v, want 2000 (the completion cap)", captured["max_tokens"])
	}
	messages, ok := captured["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("messages = %v, want system + user", captured["messages"])
	}
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "the system prompt" {
		t.Errorf("first message = %v, want the system prompt first (it is the cacheable prefix)", first)
	}
	if opts, _ := captured["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Errorf("stream_options = %v, want include_usage", captured["stream_options"])
	}
}

func TestOpenAIMapsUpstreamErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{http.StatusUnauthorized, ErrUnavailable},
		{http.StatusForbidden, ErrUnavailable},
		{http.StatusTooManyRequests, ErrOverloaded},
		{http.StatusBadGateway, ErrOverloaded},
	} {
		srv := fakeServer(t, tc.status, nil, `{"error":{"message":"nope"}}`)
		p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
		_, err := p.Chat(context.Background(), Request{}, func(string) error { return nil })
		srv.Close()
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d → %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestOpenAIRejectsMalformedFrames(t *testing.T) {
	srv := fakeServer(t, http.StatusOK, []string{"data: not json\n\n"}, "")
	defer srv.Close()

	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	_, err := p.Chat(context.Background(), Request{}, func(string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("err = %v, want a complaint about the frame", err)
	}
}

func TestOpenAICancellationStopsTheStream(t *testing.T) {
	// A server that holds the connection open: only the context can end this.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, dataFrame("first"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	p := NewOpenAI(Config{BaseURL: srv.URL, APIKey: "k", Model: "m", Timeout: 10 * time.Second})
	_, err := p.Chat(ctx, Request{}, func(string) error {
		cancel() // the client closed the tab after the first delta
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOpenAIMissingModelIsUnavailable(t *testing.T) {
	p := NewOpenAI(Config{APIKey: "k"})
	_, err := p.Chat(context.Background(), Request{}, func(string) error { return nil })
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable naming MODEL", err)
	}
	if !strings.Contains(err.Error(), "MODEL") {
		t.Errorf("err = %q, want it to name the setting to fix", err)
	}
}

func TestNewRejectsUnknownKind(t *testing.T) {
	if _, err := New(Config{Kind: "chatgpt-4"}); err == nil {
		t.Fatal("New accepted an unknown provider kind")
	} else if !strings.Contains(err.Error(), "fake and openai") {
		t.Errorf("err = %q, want it to list the supported kinds", err)
	}
}

func TestNewWithoutKeyIsUnavailableNotFatal(t *testing.T) {
	_, err := New(Config{Kind: "openai"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable (the server keeps serving the simulator)", err)
	}
}

func decodeJSON(r io.Reader, into any) error { return json.NewDecoder(r).Decode(into) }
