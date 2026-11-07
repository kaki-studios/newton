# AGENTS.md

Newton: a physics visualizer. Natural-language descriptions of physical systems become
time-evolving visualizations showing forces and properties.

Today: a 2D canvas demo (a floor, a ball on a distance joint) with a scene editor, plus a Go
backend that serves those files and exposes an **authenticated, quota-metered** `/api/chat` relay
to a language model. The model's output is validated **in the browser**, never on the server.

Hard constraint, inherited from the original design: **the simulator must keep working with no
backend at all.** `npm run dev` (static files + `python3 -m http.server`) is the primary
development loop. The AI is additive; when it is absent, unavailable, or disabled, the app
degrades to exactly the editor it has always been.

## Commands

```bash
nix develop                 # node 24, typescript, go 1.26, prettier (CC=clang for -race)
npm install
npm run build               # tsc -> dist/
npm test                    # tsc && node --test 'dist/**/*.test.js'   (93 tests)
npm run dev                 # tsc && python3 -m http.server 8080   — no backend, AI unavailable
npm run dev:full            # tsc && public/ + go run ./cmd/api -provider fake   — full stack
npm run schema              # regenerate server/internal/prompt/scene-schema.json from zod

npm run test:go             # CGO_ENABLED=0 go test ./internal/...
npm run test:go:race        # CC=clang CGO_ENABLED=1 go test -race ./...   (needs cgo)
npm run vet                 # CGO_ENABLED=0 go vet ./...
npm run test:all            # both suites
```

`CGO_ENABLED=0` is not decoration: this NixOS box has no `gcc`, and Go defaults to cgo for
`go run`/`go test`. The release build is static anyway (`modernc.org/sqlite`, not cgo sqlite).
`-race` does need cgo, hence `CC=clang`.

`go -C server ...` runs the binary with `server/` as its working directory, so relative paths in
scripts are written from there (`-static ../public -db ../data/newton.db`).

## Layout

```
index.html              importmap; account bar + AI status line; tabs; editor
src/main.ts             bootstrap, 1/120 step loop, panel wiring, account wiring
src/account.ts          /api/config + /api/me -> AiState; the degradation matrix, no DOM
src/chat.ts             conversation state + ChatBackend seam; idle it answers "dummy"
src/tabs.ts             roving-tabindex tablist, aria-selected is the styling truth
src/scene.json          the document the editor starts from (`npm run dev` serves it)
src/scene/*             zod syntax gate -> semantics -> planck world. THE ONLY VALIDATOR
src/render/*            planck World -> Frame -> CanvasRenderer

server/
  cmd/api/main.go       flags, wiring, graceful shutdown, -healthcheck subcommand
  internal/config/      flags + env + secrets-as-files; Validate names every problem at once
  internal/app/         routes, middleware, /api/health, /api/config, /api/me; e2e test
  internal/auth/        goth OAuth, hashed opaque sessions, RequireUser, SameOrigin CSRF
  internal/chat/        POST /api/chat: limits, reserve/settle, SSE, prompt assembly
  internal/llm/         Provider interface; fake; OpenAI-compatible streaming adapter
  internal/prompt/      embedded system prompt + zod-generated JSON Schema artifact
  internal/quota/       Reserve/Settle policy: daily, per-conversation, global cost
  internal/store/       SQLite: pragmas, embedded goose migrations, all SQL
  internal/web/         static serving, security headers, error handler, CSRF
  internal/id/          opaque ids (swappable for deterministic tests)
  migrations/           00001_init.sql (+ //go:embed)
testdata/sse/           recorded SSE frames: the one Go<->TS contract
```

## Conventions

- **Layering is the point.** `frame.ts` flattens everything through planck's `DistanceProxy`, so
  backends never see planck. `canvas2d.ts` never imports planck. Keep that true: a new backend
  means one new file implementing `Renderer`, nothing else.
- **The server never validates scene documents.** It builds the prompt, streams text, counts
  tokens. The browser runs the output through the same `buildSceneJson` it runs on the textarea and
  reports `SceneError.issues` back on the next turn. One copy of the schema means springs and
  revolute joints do not need a second implementation in Go that silently drifts.
- **Every spend control is a spend control.** Chat requires a session; one pending generation per
  user (a DB partial unique index, not a mutex); a daily cap; a per-conversation cap (the repair
  loop bound); a global cost ceiling; `max_tokens`; a kill switch. Reserve → call → settle, and a
  cancelled generation still counts (else abort-and-retry is a free plan).
- Mutable sim state stays in planck; `Frame` is a fresh plain value per step.
- Body identity travels via `body.setUserData(name)` → `RenderBody.id`. Tests assert on it.
- Units: metres in world space, `Camera.scale` is pixels per metre. **+y is up.**
- The side panel's views stay dumb: `chat.ts`/`account.ts` know nothing about the DOM; `tabs.ts`
  takes elements instead of querying the document. All three test without a browser.
- TS is strict, `NodeNext` — relative imports need `.js`.
- Go: no SQL outside `internal/store`; no `flag` outside `internal/config`; no `if err != nil {
  return err }` without a `fmt.Errorf` that says what was being attempted, in these words: what
  failed, the value, and the fix.
- Never log a prompt, a completion, or a secret. `Config.Redacted()` is the only config that
  reaches a log line.

## Scene document (`src/scene/`)

A scene is JSON: `{ gravity, bodies[], joints[] }`, SI units, +y up. It travels `parseScene`
(zod, syntax) → `validateSemantics` (physics) → `buildWorld` (planck). Any failure throws
`SceneError` with every issue and its path (`bodies[1].shape.radius`), so a generator can fix them
in one pass.

```jsonc
{
  "gravity": { "x": 0, "y": -9.81 },          // m/s², default -9.81 on y
  "bodies": [
    { "id": "platform", "type": "static",     // ids are unique, non-blank, -> RenderBody.id
      "position": { "x": 0, "y": -10 }, "angle": 0, "density": 1,
      "shape": { "kind": "edge", "a": { "x": -50, "y": 0 }, "b": { "x": 50, "y": 0 } } }
  ],
  "joints": [
    { "kind": "distance", "bodyA": "platform", "bodyB": "ball",
      "localAnchorA": { "x": 0, "y": 0 }, "localAnchorB": { "x": 0, "y": 0 },
      "length": 2, "collideConnected": false }
  ]
}
```

Rules that hold today:

- Shapes: `circle{radius}`, `box{halfWidth,halfHeight}`, `edge{a,b}` — one per body, local to the
  body. Compound bodies become a `shapes` array when there is a need.
- Joint anchors are **local** to their body. `length` omitted means "measure the anchors at build
  time"; `0` is a pin. Only `distance` exists; new kinds go in the `kind` union in `schema.ts`.
- Validation at the boundary, never a silent fallback: unknown keys, unknown `kind`, missing
  positions, duplicate ids, dangling refs, self-joints, dynamic edges (zero area → planck's silent
  1 kg default), and zero-length edges are all rejected by path.
- Every schema addition gets a test in `src/scene/*.test.ts`, **and** `npm run schema` must be
  re-run: `server/internal/prompt/scene-schema.json` is generated from zod and a Go test fails when
  the committed artifact differs.
- `main.ts` fetches `/dist/scene.json` into the editor. Apply parses with `buildSceneJson` and swaps
  the running world; a bad document keeps the old scene on screen and prints its `SceneError` paths.

## The chat contract

`POST /api/chat` (session required) takes
`{conversationId?, message, scene?, report?: {ok, issues[]}}` and streams SSE:
`event: delta` (`{text}`)*, then `event: done` (`{conversationId, generationId, usage}`) or
`event: error` (`{code, message}`). Status codes are decided before the first byte (401/409/415/429/
503); after it, failures are frames.

Limits: 4 KiB message, 64 KiB scene, 8 KiB report, 96 KiB body, 20 issues rendered, 2000 completion
tokens. `testdata/sse/*.txt` are recorded frames: a Go test asserts the server reproduces them
byte-for-byte and the client parser consumes the same files. Changing the wire format means changing
both, then the fixture.

## Backend facts worth knowing before you change them

- **SQLite DSN pragmas are load-bearing**: WAL (Litestream needs it), `foreign_keys(1)` (cascades),
  `busy_timeout`, and `_txlock=immediate` (a read-then-write reserve cannot fail on lock upgrade).
  `SetMaxOpenConns(1)` serializes access, which removes `SQLITE_BUSY` handling entirely. Tests pin
  the pragma values.
- **`one_pending`** is a partial unique index: `create unique index one_pending on generations
  (user_id) where status = 'pending'`. That is the real concurrency control; 8 concurrent reserves
  produce exactly one winner (tested).
- A pending row whose request died blocks its user until `Reserve`'s reaper marks it failed
  (older than 3 minutes, in the same transaction).
- Migrations are embedded and run at startup before the listener. **Expand/contract only**: Swarm
  rolls back to the previous binary on a failed healthcheck, and that binary must read the schema.
- Sessions are opaque tokens; only `sha256(token)` is stored. Expiry is enforced on read *and* by an
  hourly sweeper.
- Cookies are `__Host-` prefixed in production (no Domain, `Secure`, `Path=/`) and lose the prefix in
  `-dev`, where `http://localhost` cannot satisfy the prefix rules.
- CSRF is structural: JSON-only binding (415 without `Content-Type: application/json`, so a
  cross-site form cannot play) + `SameSite=Lax` + a `Sec-Fetch-Site`/`Origin` check.
- `-admin ban|unban|sessions <email>` is the operator escape hatch: distroless has no `sqlite3`, so
  the binary itself runs the UPDATE. `ban` sets `disabled_at` **and** revokes sessions in one
  transaction, or un-banning would resurrect them.
- `-healthcheck` is the container healthcheck: distroless has no shell, so the binary GETs its own
  `/api/health`, which runs `select 1` and reads the goose version.
- The rate limiter is per IP at 300 burst / 20 per second. A cold page load is ~40 requests; the
  original 20 burst answered real page loads with 429, which a browser test caught.

## Deployment

One image: `node:24-alpine` (tsc) → `golang:1.26-alpine` (static binary) → distroless nonroot.
`-static /app/public`. One Swarm service, `replicas: 1`, with a `newton_data` volume; Litestream
sidecar replicates it to object storage and `exec`s the app so a fresh volume restores first.
Secrets are Swarm secrets read as **files** (`*_FILE` env vars), never env values.

Skipped until needed: forces beyond distance joints, compound shapes, springs, published scenes,
anonymous trials, debug traces.
