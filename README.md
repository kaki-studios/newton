# Newton

Newton is a physics visualizer: describe a physical system in words and watch it evolve, with the
forces and properties drawn on it.

The simulator is a static web app — one HTML file, an importmap, and plain `tsc` output. The
backend is a small Go binary that serves those files and exposes an authenticated, quota-metered
relay to a language model. **The simulator works with no backend at all**; the AI is additive.

## Running it

```bash
nix develop        # node 24, go 1.26
npm install
npm run dev        # http://localhost:8080 — the editor, no AI
```

`npm run dev` is the primary loop: `tsc` plus `python3 -m http.server`. There is no API, so the
chat panel says "AI unavailable in static mode" and the editor works normally.

To run everything, including the chat endpoint streaming from a deterministic fake model (no key,
no spend):

```bash
npm run dev:full   # http://localhost:8080 — editor + AI with the fake provider
```

## The AI endpoint

`POST /api/chat` requires a signed-in session and streams server-sent events:

```
event: delta
data: {"text":"{\n  \"gravity\""}

event: done
data: {"conversationId":"0f3c…","generationId":"…","usage":{"input":2412,"output":1033}}
```

The server **never validates the scene document**. It builds the prompt, streams the text, and
counts tokens. The browser runs the reply through the same validator it runs on the editor's
textarea — one copy of the schema — and reports the failures back on the next turn so the model can
repair in one pass. A document that does not validate leaves the running scene untouched.

Sign-in is OAuth only (GitHub, optionally Google): no passwords to hash, reset, or leak. Sessions are
opaque tokens in `__Host-` cookies; only their sha256 is stored.

## Configuration

Flags and environment are documented by `server/newton -h`. Anything secret is a **file path** in
the environment, read once at startup:

| Variable | Meaning |
|---|---|
| `BASE_URL` | absolute origin for OAuth callbacks, e.g. `https://newton.example.com` |
| `CHAT_ENABLED` | kill switch, default `false` |
| `PROVIDER`, `MODEL` | `fake` (default) or `openai`, plus the model id |
| `DAILY_GENERATIONS`, `MAX_CONVERSATION_TURNS`, `DAILY_COST_MICROS` | spend controls |
| `GITHUB_CLIENT_ID_FILE`, `GITHUB_CLIENT_SECRET_FILE`, `SESSION_KEY_FILE`, `PROVIDER_API_KEY_FILE` | Swarm secrets |

The server refuses to start with a setting that cannot work and names the fix; it does not start
half-configured and fail at a login click.

## Tests

```bash
npm test              # 93 TypeScript tests
npm run test:go       # Go: store, quota, auth, chat, prompt, config, web, app
npm run test:go:race  # the same, with the race detector (needs cgo: see flake.nix)
npm run test:all      # both
```

`testdata/sse/*.txt` are recorded streams that both suites read: a Go test asserts the server
reproduces them byte-for-byte, and the client parser consumes the same bytes. That directory is the
only anti-drift mechanism the two languages need.

## Running the server

```bash
npm run dev:full        # everything, fake model, http://localhost:8080
npm run server          # the API only, with whatever env you have set
```

The binary also carries the operator tools, because the final image is distroless and has no
`sqlite3` or `curl`:

```bash
newton -healthcheck                              # what the container healthcheck runs
newton -admin sessions -db <file> ada@example.com
newton -admin ban      -db <file> ada@example.com   # sets disabled_at and revokes sessions
newton -admin unban    -db <file> ada@example.com
```

## Database backup

Litestream replicates the SQLite file to object storage. Run the restore drill before you need it:

```bash
litestream restore -config /etc/litestream.yml -o /tmp/restored.db s3://<bucket>/newton
sqlite3 /tmp/restored.db 'pragma integrity_check; select count(*) from users;'
```

## Layout

See `AGENTS.md` for the architecture and the invariants that make it work.
