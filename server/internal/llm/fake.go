package llm

import (
	"context"
	"strings"
	"time"
)

// Fake is the deterministic provider. It is used by every Go test, by `npm run dev:full`, and
// as the fallback when no provider credentials are configured.
//
// It answers with a small, always-valid scene document, streamed in chunks, so the whole path —
// SSE framing, browser extraction, validation, apply — can be exercised without spending money.
// Its output is a function of the request, never of the clock or of random state, which is what
// lets the SSE fixtures in testdata/sse be compared byte-for-byte.
type Fake struct {
	// Chunk is how many characters each delta carries. Small chunks are what makes a streaming
	// bug (a frame split across flushes) visible in a test.
	Chunk int
	// Delay is inserted between deltas. Zero in tests; a small value makes `npm run dev:full`
	// look like a real stream.
	Delay time.Duration
	// Reply overrides the canned document. Empty means DefaultReply.
	Reply string
	// Fail, when set, is returned after FailAfter deltas: the test hook for a mid-stream
	// provider error.
	Fail      error
	FailAfter int
	// Usage is reported regardless of how the stream ended, because a cancelled call still
	// burned input tokens.
	Usage Usage
}

// DefaultReply is the pendulum from src/scene.json. Using the real starting document means the
// fake's output is also a useful smoke test of the schema.
const DefaultReply = "```json\n" + `{
  "gravity": { "x": 0, "y": -9.81 },
  "bodies": [
    {
      "id": "floor",
      "type": "static",
      "position": { "x": 0, "y": -10 },
      "shape": { "kind": "edge", "a": { "x": -50, "y": 0 }, "b": { "x": 50, "y": 0 } }
    },
    {
      "id": "ball",
      "type": "dynamic",
      "position": { "x": 5, "y": 10 },
      "shape": { "kind": "circle", "radius": 0.4 }
    }
  ],
  "joints": [
    {
      "kind": "distance",
      "bodyA": "floor",
      "bodyB": "ball",
      "localAnchorA": { "x": 0, "y": 20 },
      "localAnchorB": { "x": 0, "y": 0 },
      "length": 6
    }
  ]
}` + "\n```\n"

// NewFake returns a fake provider with chunked streaming and no delay.
func NewFake() *Fake {
	return &Fake{Chunk: 16, Usage: Usage{InputTokens: 2412, OutputTokens: 1033}}
}

func (f *Fake) Chat(ctx context.Context, req Request, emit func(string) error) (Usage, error) {
	reply := f.Reply
	if reply == "" {
		reply = DefaultReply
	}
	chunk := f.Chunk
	if chunk <= 0 {
		chunk = len(reply) // one delta: still exercises the framing
	}
	usage := f.Usage
	if usage == (Usage{}) {
		// A deterministic estimate keeps /api/me and the cost ledger populated in dev without
		// pretending to be the provider's own count.
		usage = Usage{InputTokens: len(req.System)/4 + 1, OutputTokens: len(reply) / 4}
	}

	sent := 0
	// checkFail is evaluated before each delta and once more after the last one, so
	// FailAfter: 0 is "fail immediately" and FailAfter: 1 is "fail after exactly one delta",
	// which is what "the provider died mid-stream" means in the tests.
	checkFail := func() error {
		if f.Fail != nil && sent >= f.FailAfter {
			return f.Fail
		}
		return nil
	}
	for start := 0; start < len(reply); start += chunk {
		if err := ctx.Err(); err != nil {
			return usage, err
		}
		if err := checkFail(); err != nil {
			return usage, err
		}
		end := min(start+chunk, len(reply))
		if err := emit(reply[start:end]); err != nil {
			// The consumer failed (client disconnected mid-write). Return what was burned.
			return usage, err
		}
		sent++
		if f.Delay > 0 {
			select {
			case <-ctx.Done():
				return usage, ctx.Err()
			case <-time.After(f.Delay):
			}
		}
	}
	return usage, checkFail()
}

// String is used in logs and in /api/me's status, so an operator can see which provider is live.
func (f *Fake) String() string { return "fake" }

// ReplyText is the reply with the fence stripped, for tests that need the document alone.
func (f *Fake) ReplyText() string {
	reply := f.Reply
	if reply == "" {
		reply = DefaultReply
	}
	return strings.TrimSuffix(strings.TrimPrefix(reply, "```json\n"), "\n```\n")
}
