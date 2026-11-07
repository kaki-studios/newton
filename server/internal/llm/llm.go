// Package llm is the model-provider seam. Nothing above it knows whether text came from a real
// API or from the deterministic fake: the chat handler only sees deltas and a token count.
//
// The interface is deliberately small and pull-free: the provider pushes deltas into a callback
// and returns usage when it is done. That is what lets the HTTP handler stream and flush
// without buffering the whole completion, and it keeps cancellation a matter of the context.
package llm

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Role is one turn's author, as the provider expects it.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of the conversation sent to the provider.
type Message struct {
	Role Role
	Text string
}

// Request is a complete provider call.
type Request struct {
	// System is the stable prefix (schema, rules, example). It is separate from Messages so a
	// provider can apply prompt caching to it, and so the fake can echo it deterministically.
	System string
	// Messages are the conversation turns, oldest first.
	Messages []Message
	// Model is the provider's model id, from configuration rather than code.
	Model string
	// MaxTokens bounds both cost and a runaway scene array.
	MaxTokens int
	// Temperature is deliberately low: the task is to emit a document against a strict
	// schema, not to be creative.
	Temperature float64
}

// Usage is what the call cost, in the provider's own token units.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Provider streams one completion.
//
// Implementations must:
//   - call emit for every delta, in order, and stop as soon as emit returns an error;
//   - return promptly with ctx.Err() when ctx is canceled, closing any upstream response;
//   - return the usage they actually observed, even when they also return an error. A
//     cancelled generation still counts against quota, so its partial cost has to survive.
type Provider interface {
	Chat(ctx context.Context, req Request, emit func(delta string) error) (Usage, error)
}

// ErrUnavailable means the provider is configured but cannot currently serve (no API key, or a
// kill switch). Handlers turn it into 503, not 500: it is an operator condition, not a bug.
var ErrUnavailable = errors.New("llm: provider unavailable")

// ErrOverloaded is the provider saying "try later" (429/503 upstream, or a rate limit we hit).
// Distinct from ErrUnavailable because the client message differs: paused vs busy.
var ErrOverloaded = errors.New("llm: provider overloaded")

// Emitter is the callback signature, named so tests can build one without a closure verbosity
// tax.
type Emitter func(delta string) error

// Config configures the real provider adapter. It is all strings because it comes from the
// environment.
type Config struct {
	// Kind selects the implementation: "fake" or "openai" (OpenAI-compatible chat completions).
	Kind string
	// BaseURL is the API root, without a trailing slash. Empty uses the implementation's
	// default.
	BaseURL string
	// APIKey is the bearer token. Empty makes New return ErrUnavailable for a real provider.
	APIKey string
	// Model is the provider's model id, recorded on every generation row.
	Model string
	// Timeout bounds one whole call. Zero uses a default.
	Timeout time.Duration
	// MaxTokens overrides the request's cap when set.
	MaxTokens int
}

// New builds the configured provider. A missing API key is not fatal at boot: the server keeps
// serving the simulator, and /api/chat answers 503 with an actionable message. That is the same
// degradation as CHAT_ENABLED=false, and it means a typo in one secret does not take the whole
// site down.
func New(cfg Config) (Provider, error) {
	switch cfg.Kind {
	case "fake", "":
		return NewFake(), nil
	case "openai":
		if cfg.APIKey == "" {
			return nil, fmt.Errorf("%w: %s needs an API key (the provider_api_key secret)",
				ErrUnavailable, cfg.Kind)
		}
		return NewOpenAI(cfg), nil
	default:
		return nil, fmt.Errorf("llm: unknown provider kind %q: supported are fake and openai", cfg.Kind)
	}
}
