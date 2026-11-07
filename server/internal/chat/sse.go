package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/kaki-studios/newton/server/internal/llm"
	"github.com/labstack/echo/v4"
)

// The wire protocol. These names are the contract with src/ai/protocol.ts and with the recorded
// frames in testdata/sse: changing one is a protocol change, not a refactor.
const (
	EventDelta = "delta"
	EventDone  = "done"
	EventError = "error"
)

// DeltaEvent is one fragment of the model's text. Deltas are not guaranteed to align with
// anything: the client concatenates them and extracts the fenced block at the end.
type DeltaEvent struct {
	Text string `json:"text"`
}

// DoneEvent ends a successful stream. The client needs the conversation id to continue the
// conversation and the usage so it can show what the turn cost.
type DoneEvent struct {
	ConversationID string `json:"conversationId"`
	GenerationID   string `json:"generationId"`
	Usage          Usage  `json:"usage"`
}

// Usage is the token count, in the provider's units.
type Usage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
}

// ErrorEvent ends a failed stream. It carries a stable `code` for the client to switch on and a
// `message` already written for a human to read.
type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes. They are stable identifiers: the client's degradation matrix switches on them, so
// the message is free to change and the code is not.
const (
	// CodeQuotaExceeded: the daily or global budget is spent.
	CodeQuotaExceeded = "quota_exceeded"
	// CodeTooManyTurns: this conversation used its repair budget.
	CodeTooManyTurns = "conversation_turns"
	// CodeInFlight: a reply is already being generated.
	CodeInFlight = "in_flight"
	// CodeProviderUnavailable: the provider is misconfigured or unreachable.
	CodeProviderUnavailable = "provider_unavailable"
	// CodeProviderOverloaded: retry later.
	CodeProviderOverloaded = "provider_overloaded"
	// CodeCanceled: the client went away. It is only ever seen by a client that did not leave,
	// so in practice it is a diagnostic rather than a UI state.
	CodeCanceled = "canceled"
	// CodeInternal: anything else. The message is deliberately vague; the log has the detail.
	CodeInternal = "internal"
)

// sseWriter serializes events and flushes each one, so the client sees tokens as they are
// produced instead of after the whole completion.
//
// Writes come from two goroutines — the provider callback emitting deltas, and the keepalive
// ticker emitting comments — so the mutex is load-bearing rather than decorative. net/http does
// serialize writes to one response, but relying on that would tie correctness to a behaviour the
// ResponseWriter contract never promises, and it leaves a real bug: a keepalive can fire after the
// terminal event, writing bytes into a finished stream. `closed` prevents that.
type sseWriter struct {
	mu     sync.Mutex
	w      *echo.Response
	closed bool
	// wroteAny records whether anything was written, so a caller can tell "the client is gone"
	// from "we never got that far".
	wroteAny bool
}

// event writes one SSE frame and flushes it. A terminal event (done or error) closes the writer,
// so a keepalive that fires afterwards writes nothing.
func (s *sseWriter) event(name string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("chat: encoding the %s event: %w", name, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return fmt.Errorf("chat: writing the %s event: %w", name, err)
	}
	s.w.Flush()
	s.wroteAny = true
	if name == EventDone || name == EventError {
		s.closed = true
	}
	return nil
}

// comment writes an SSE comment, used as the keepalive. Comments are ignored by every compliant
// EventSource implementation and by the client's parser, so they cost nothing but a byte of
// bandwidth and keep an idle proxy from closing the stream.
func (s *sseWriter) comment(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return fmt.Errorf("chat: writing a keepalive comment: %w", err)
	}
	s.w.Flush()
	return nil
}

// close marks the stream finished without writing anything. It is what makes the deferred
// keepalive shutdown safe.
func (s *sseWriter) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}

// errorCode maps a provider failure onto the stable wire code.
func errorCode(err error) string {
	switch {
	case errors.Is(err, llm.ErrUnavailable):
		return CodeProviderUnavailable
	case errors.Is(err, llm.ErrOverloaded):
		return CodeProviderOverloaded
	default:
		return CodeInternal
	}
}

// clientMessage is what a person reads. It never contains a provider's raw error text: that goes
// to the log, where it is useful, and not to a screen where it is noise.
func clientMessage(err error) string {
	switch {
	case errors.Is(err, llm.ErrUnavailable):
		return "The AI is not available right now. The simulator and the JSON editor still work."
	case errors.Is(err, llm.ErrOverloaded):
		return "The AI is busy. Try again in a minute."
	default:
		return "The AI response failed. Try again; the simulator is unaffected."
	}
}
