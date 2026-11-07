# SSE contract fixtures

Recorded `POST /api/chat` response bodies. They are the only anti-drift mechanism between the Go
server and the TypeScript client, and they cost one directory.

- `happy-path.txt` — a fake provider streams a valid scene; `delta`* then `done`.
- `provider-error.txt` — the provider fails after two deltas; `delta`* then `error`.
- `keepalive.txt` — a keepalive comment between deltas.

Two tests read these:

- `server/internal/chat/contract_test.go` asserts the server reproduces `happy-path.txt` and
  `provider-error.txt` **byte for byte**.
- `src/ai/sse.test.ts` asserts the client parser consumes the same files into the same events.

Rule: change the wire format in both places at once, then update the fixture. If a fixture stops
matching, that is the mechanism working, not a nuisance.
