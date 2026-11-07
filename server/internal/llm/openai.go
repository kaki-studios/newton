package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI is an adapter for any provider that speaks the OpenAI chat-completions streaming
// protocol (OpenAI itself, Azure OpenAI's compatible mode, OpenRouter, Groq, vLLM, Ollama and
// friends). One adapter therefore covers "which provider" as configuration, which is the
// point: model ids and prices change faster than plans.
//
// It streams `data:` lines of server-sent events whose payloads are chat.completion.chunk
// objects. The `[DONE]` sentinel ends the stream.
type OpenAI struct {
	baseURL   string
	apiKey    string
	model     string
	client    *http.Client
	maxTokens int
}

// NewOpenAI builds the adapter. cfg.BaseURL empty means the public API.
func NewOpenAI(cfg Config) *OpenAI {
	baseURL := strings.TrimSuffix(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2000
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		// A whole 2000-token completion at a normal rate takes well under a minute; two
		// minutes leaves room for a slow provider without pinning a connection forever.
		timeout = 2 * time.Minute
	}
	return &OpenAI{
		baseURL:   baseURL,
		apiKey:    cfg.APIKey,
		model:     cfg.Model,
		client:    &http.Client{Timeout: timeout},
		maxTokens: maxTokens,
	}
}

// String reports the adapter and model for logs and /api/me.
func (o *OpenAI) String() string { return "openai:" + o.model }

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature,omitempty"`
	Stream      bool          `json:"stream"`
	// stream_options asks for a final usage chunk. Not every compatible provider honours it,
	// which is why usage is optional below.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat implements Provider.
//
// Error mapping matters more than the happy path: a 401 is an operator misconfiguration and
// must not look like a user error, and a 429 means "come back later" rather than "this is
// broken".
func (o *OpenAI) Chat(ctx context.Context, req Request, emit func(string) error) (Usage, error) {
	model := req.Model
	if model == "" {
		model = o.model
	}
	if model == "" {
		return Usage{}, fmt.Errorf("%w: no model configured (set MODEL, e.g. a mid-tier chat model)",
			ErrUnavailable)
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = o.maxTokens
	}

	messages := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		// The system block goes first and is the only stable prefix, so a provider that
		// caches prefixes gets a hit on it.
		messages = append(messages, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		messages = append(messages, chatMessage{Role: string(m.Role), Content: m.Text})
	}

	body, err := json.Marshal(chatRequest{
		Model:         model,
		Messages:      messages,
		MaxTokens:     maxTokens,
		Temperature:   req.Temperature,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return Usage{}, fmt.Errorf("llm: encoding the request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Usage{}, fmt.Errorf("llm: building the request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(httpReq)
	if err != nil {
		// A cancelled context is the client closing the tab, not a provider failure.
		if ctx.Err() != nil {
			return Usage{}, ctx.Err()
		}
		return Usage{}, fmt.Errorf("llm: calling %s: %w", o.baseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read a bounded amount of the body: it usually names the problem (bad model id,
		// expired key, missing quota) and is worth putting in the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Usage{}, statusError(resp.StatusCode, detail)
	}

	return o.stream(ctx, resp.Body, emit)
}

// stream parses SSE frames until `[DONE]`, EOF, an error, or a canceled context.
func (o *OpenAI) stream(ctx context.Context, r io.Reader, emit func(string) error) (Usage, error) {
	var usage Usage
	scanner := bufio.NewScanner(r)
	// A single chunk can carry a large delta; the default 64 KiB line limit is generous
	// already, but a long scene in one chunk must not be truncated silently.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return usage, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // keepalive or separator
		}
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // event:/id:/retry: are not used by this API
		}
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			return usage, nil
		}

		var chunk chatChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// A malformed frame is a provider bug. Fail loudly rather than silently
			// dropping tokens: a half-parsed scene is worse than an error the user can see.
			return usage, fmt.Errorf("llm: the provider sent a frame that is not JSON: %w", err)
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content == "" {
				continue
			}
			if err := emit(choice.Delta.Content); err != nil {
				return usage, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return usage, ctx.Err()
		}
		return usage, fmt.Errorf("llm: reading the stream: %w", err)
	}
	// EOF without [DONE]: some providers just close. The content received so far is usable,
	// so this is not an error; a truncated document will be caught by the validator.
	return usage, nil
}

// statusError maps an upstream status onto this package's sentinels where that changes what the
// user sees.
func statusError(code int, detail []byte) error {
	snippet := strings.TrimSpace(string(detail))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("%w: the provider rejected the API key (%d): %s",
			ErrUnavailable, code, snippet)
	case code == http.StatusTooManyRequests:
		return fmt.Errorf("%w: rate limited by the provider (%d): %s",
			ErrOverloaded, code, snippet)
	case code >= 500:
		return fmt.Errorf("%w: the provider returned %d: %s", ErrOverloaded, code, snippet)
	default:
		return fmt.Errorf("llm: the provider returned %d: %s", code, snippet)
	}
}

// Ensure the adapter is a Provider at compile time.
var _ Provider = (*OpenAI)(nil)
