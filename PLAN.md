# PLAN — LLM scene authoring, accounts, and a backend

Status: proposed, not implemented. Nothing in this document is in the repo yet.
Last verified against the working tree at `e10b54e` (`ai dummy`).

---

## 1. Goal

Let a user type *"a 2 kg block slides down a 30° ramp"* and get a running scene, with an
account system in front of the model so that strangers cannot spend the owner's API budget.

Hard constraint, inherited from the existing design: **the simulator must keep working
with no backend at all.** `npm run dev` (static files + `python3 -m http.server`) is the
primary development loop, and the deployed app is currently static. The AI is additive;
when it is absent, unavailable, or disabled, the app degrades to exactly what it is today.

---

## 2. What is actually in the repo today

Facts, measured, not assumed:

| Fact | Value |
|---|---|
| Source lines | ~1820 across `src/**/*.ts` + `index.html` |
| Tests | `npm test` → **73 passing, 0 failing** |
| Panel | `index.html` already has a tabbed panel: `<div role="tablist">` with `chat-panel` and `scene-json` `role="tabpanel"` sections |
| Tabs | `src/tabs.ts` — roving tabindex, Home/End, `aria-selected` drives styling, tested |
| Chat | `src/chat.ts` — `Chat` (conversation + `isPending` + `subscribe`), `ChatBackend` seam, `dummyChat` returns `"dummy"` |
| Wiring | `src/main.ts` renders the log incrementally (`rendered` counter), never `innerHTML`, disables the composer while pending |
| Validation | `buildSceneJson()` → `parseScene` (zod) → `validateSemantics` → `buildWorld`, all failures throw `SceneError` with **every** issue and its path |
| Validation locality | the only copy of the schema is TypeScript, in the browser |
| Assets | `index.html` uses an importmap for `planck` and `zod`; no bundler; `tsc` → `dist/` |
| Deploy | nginx static image, single-replica Swarm service, Traefik + certresolver, GHCR via GitHub Actions |
| Toolchain | Node 24 (flake), Go 1.26.10 installed locally, nixpkgs has `go_1_26` |

`AGENTS.md` and `pitch/pitch.tex` both still claim 57 tests and "one static platform, one
ball"; the panels and `dummyChat` landed after they were written. **Update both as part of
milestone 0** — a stale architecture doc is worse than none.

---

## 3. Architecture in one picture

```
┌──────────────────────────────────────────── browser ─────────────────────────────────────────┐
│  index.html  (one origin, no build step, no framework)                                      │
│    ├─ canvas  ← src/render/canvas2d.ts ← src/render/frame.ts ← planck World                  │
│    ├─ Chat tab      src/main.ts (view) ⇄ src/chat.ts (state) ⇒ src/ai/client.ts (SSE)        │
│    └─ Scene JSON tab  textarea ⇒ buildSceneJson() ⇒ world        ← the only validator        │
└───────────────────────────────────┬──────────────────────────────────────────────────────────┘
                                    │  fetch POST /api/chat, text/event-stream
                                    │  (session cookie: __Host-newton)
┌───────────────────────────────────▼──────────────────────────────────────────────────────────┐
│  Go + Echo binary (single container)                                                        │
│    static files ── /api/auth/* ── /api/me ── /api/chat ── /api/health                       │
│                                                      │                                       │
│    quota.Reserve (DB) ──── stream ──── quota.Settle  │  llm.Provider (interface)            │
│                                                      └──► fake | real provider (upstream)    │
│    store: SQLite (WAL), embedded goose migrations                                           │
│    sidecar: litestream replicating the same volume to S3/B2                                 │
└──────────────────────────────────────────────────────────────────────────────────────────────┘
```

**The load-bearing decision: the server never validates scene documents.**

The backend is an authenticated, quota-metered relay. It builds the prompt, streams text,
counts tokens. The browser runs the model's output through the *same* `buildSceneJson` it
already runs on the textarea, and sends the resulting `SceneError.issues` back on the next
turn so the model can repair in one pass.

Why, concretely:

- One copy of the schema. Springs, revolute joints and compound shapes are on the roadmap
  (`pitch.tex` phase 1); every one of them would otherwise need a second implementation in
  Go that silently drifts from the one the user sees.
- The model repairs against the real messages — including the physics ones
  (`"a dynamic body needs area to get mass from density"`), which a reimplementation would
  not reproduce.
- Cost risk is bounded by turn caps and budgets, not by a second parser. A second validator
  does not stop a repair loop; a turn counter does.

Rejected: validating in Go with a hand-written or generated JSON Schema. It buys nothing at
this size and costs a second source of truth. If server-side syntax checking ever becomes
necessary (cheap 400s before spending tokens), the right move is to generate the schema from
zod and commit it as an artifact — see §8.4.

Second decision: **Go + Echo, not Next.js.** Next's only real advantage here was importing
`src/scene`, which the design deliberately does not want. What remains is a second runtime,
a second build pipeline and a second base image to serve one streaming JSON endpoint, while
the static app keeps its own build. Echo gives SSE with `c.Response().Flush()` (verified
against v4.16.3), ships `BodyLimit`, `Secure`, `Recover`, `RequestID`, `RateLimiter` and
`CSRF` in `middleware/` (verified: `middleware/rate_limiter.go` exists at that tag), and
compiles to one static binary.

Third decision: **SQLite, not Postgres.** One file on a Docker volume, `replicas: 1`, write
volume measured in touched rows per minute. Litestream handles backup. Switch trigger:
multiple writers, read replicas, or wanting `psql`/logical replication.

---

## 4. Repository layout after this work

```
newton/
├── index.html                 + sign-in entry, AI status line (small additions)
├── src/
│   ├── main.ts                + apply()/report() exported to chat glue; streaming render
│   ├── chat.ts                + streaming seam, error surface, repair hook
│   ├── tabs.ts                unchanged
│   ├── ai/
│   │   ├── protocol.ts        request/response types, shared with Go by fixture tests
│   │   ├── sse.ts             pure SSE chunk parser            (new, tested)
│   │   ├── jsonblock.ts       extract the fenced document     (new, tested)
│   │   ├── client.ts          fetch + stream + abort          (new, tested w/ stubbed fetch)
│   │   └── glue.ts            verdict → textarea/apply/repair (new, tested)
│   ├── scene/*                unchanged — the only validator
│   └── render/*               unchanged
├── server/                    new Go module
│   ├── go.mod                 go 1.26
│   ├── cmd/api/main.go        flags, wiring, graceful shutdown, -healthcheck subcommand
│   ├── internal/auth/         goth wiring, sessions, middleware, handlers
│   ├── internal/chat/         handler, SSE writer, repair rendering
│   ├── internal/llm/          Provider interface, fake, real adapter
│   ├── internal/prompt/       embedded prompt templates + schema artifact
│   ├── internal/store/        *sql.DB, DSN, queries, embedded migrations
│   ├── internal/quota/        reserve/settle, clock injection, limits
│   ├── internal/web/          static file serving, security headers, error mapping
│   └── migrations/            00001_init.sql … (goose, //go:embed)
├── testdata/sse/              recorded frames: the Go↔TS contract fixtures
├── Dockerfile                 node build → go build (CGO_ENABLED=0) → distroless
├── docker-stack.yml           app + litestream, named volume, secrets by file
├── flake.nix                  + go_1_26
└── package.json               + scripts: public, dev:full, test:contract
```

`Dockerfile.nginx` is deleted in the same commit that lands the Go server (§12).

---

## 5. Backend: process, configuration, static serving

### 5.1 Flags and environment

| Setting | Source | Notes |
|---|---|---|
| `-addr` | flag, default `:8080` | |
| `-static` | flag, default `public` | directory of files to serve; `-static .` in dev |
| `-db` | flag, default `data/newton.db` | |
| `-dev` | flag/bool | relaxes cookie flags, verbose logs, allows `http://localhost` redirect URIs |
| `BASE_URL` | env | absolute, used for OAuth callbacks; fail fast if missing in prod |
| `CHAT_ENABLED` | env/flag, default `false` | kill switch; `false` ⇒ `/api/chat` 503 |
| `MODEL`, `PROMPT_VERSION` | env | recorded on every generation row |
| `DAILY_GENERATIONS` | env, default `10` | per user |
| `MAX_CONVERSATION_TURNS` | env, default `6` | |
| `DAILY_COST_MICROS` | env | global ceiling |
| provider secrets, `SESSION_*` | **Swarm secrets, read as files** | see §12.3 |

Secrets are files under `/run/secrets/*` (Swarm mounts them there), read once at startup. Env
vars leak to anyone who can `docker inspect` a container; secrets do not.

Fail fast and loudly on missing config at boot: a server that starts with no OAuth secret and
fails at the login click is worse than one that refuses to start. Every startup error names
the setting and the fix.

### 5.2 Static serving

`http.FileServer` (or Echo's `Static`) over `-static`, with two specifics:

- `.mjs` must be `text/javascript`. Verified: Go's builtin mime table returns
  `text/javascript; charset=utf-8` for `.mjs` on this Go version, so no override is needed —
  but a test asserts it, because a Go upgrade changing that silently breaks the importmap
  the way nginx's `mime.types` did (hence the existing `Dockerfile.nginx` comment).
- Unknown paths return 404, never `index.html`. This app has no client-side routes; a typo in
  a `/dist/...` URL must fail visibly.

**One origin** is the point: cookies with `__Host-` work, no CORS exists, and there is no proxy
between the browser and the SSE stream to buffer it. This is why nginx goes away rather than
staying in front of the API.

### 5.3 Development

```bash
npm run dev        # today's loop: tsc + python3 http.server, no backend, chat disabled
npm run dev:full   # tsc && go run ./cmd/api -static . -dev -chat
```

`npm run dev` must not throw when `/api/me` 404s — that is a supported mode, not an error path.

### 5.4 Health

`GET /api/health` → 200 `{ok, version, migrated}`; it runs `SELECT 1` and reads the goose
version. Distroless has no shell, so the container healthcheck must be the binary itself:
`HEALTHCHECK CMD ["/app/newton", "-healthcheck"]`, and `-healthcheck` does one HTTP GET
against `127.0.0.1:$PORT/api/health` and exits non-zero on failure. Swarm's update/rollback
config depends on this being real.

---

## 6. Data layer

### 6.1 DSN and connection policy

```
file:data/newton.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)&_txlock=immediate
db.SetMaxOpenConns(1)
```

Verified with `modernc.org/sqlite v1.61.1`: the DSN parses, `journal_mode` comes back `wal`,
`foreign_keys` is `1`, and `_txlock=immediate` is a recognized value (it produces `BEGIN
IMMEDIATE`, so a read-then-write reserve cannot fail on lock upgrade).

`SetMaxOpenConns(1)` serializes access. At this workload (a handful of writes per minute) it
costs nothing and removes `SQLITE_BUSY` handling entirely. WAL still lets the sidecar and
external readers work.

### 6.2 Schema

`migrations/00001_init.sql`, verified to apply via goose with `SetDialect("sqlite3")` and
`SetBaseFS`:

```sql
-- +goose Up
create table users (
  id            text primary key,
  email         text unique,
  display_name  text,
  created_at    text not null default (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  disabled_at   text
);

create table identities (
  provider          text not null,
  provider_user_id  text not null,
  user_id           text not null references users(id) on delete cascade,
  primary key (provider, provider_user_id)
);

create table sessions (
  token_hash  blob primary key,          -- sha256 of the cookie value, never the value
  user_id     text not null references users(id) on delete cascade,
  expiry      text not null,
  created_at  text not null default (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
create index sessions_user on sessions (user_id);

create table generations (
  id               text primary key,
  user_id          text not null references users(id),
  conversation_id  text not null,
  status           text not null check (status in ('pending','ok','failed','canceled')),
  model            text not null,
  prompt_version   text not null,
  input_tokens     integer,
  output_tokens    integer,
  cost_micros      integer,
  scene_valid      integer,              -- 0/1/NULL, reported by the browser
  created_at       text not null default (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
create index generations_user_created on generations (user_id, created_at desc);

-- One generation in flight per user, enforced by the database.
create unique index one_pending on generations (user_id) where status = 'pending';

-- +goose Down
drop table generations;
drop table sessions;
drop table identities;
drop table users;
```

Verified behavior, all in a throwaway proof of concept outside this repo:

1. migrations apply under goose;
2. the partial index exists and rejects a second `pending` row for the same user
   (`UNIQUE constraint failed: generations.user_id`);
3. settling a row (`status='ok'`) frees the slot;
4. 8 concurrent inserts produce **exactly one** winner.

Timestamps are `text` (ISO-8601 UTC), so the day boundary is computed in Go and passed as a
bound parameter. That is what makes quota tests use an injected clock instead of sleeping.

### 6.3 The stuck-pending failure mode

A `pending` row whose request died (process restart, OOM, cancelled context) blocks that user
forever. `Reserve` therefore starts with a reaper inside the same transaction:

```sql
update generations set status='failed'
 where user_id = ? and status = 'pending' and created_at < ?   -- now - 3 minutes
```

then attempts the insert and maps a unique violation to `ErrInFlight` (409). Both paths are
tested.

### 6.4 Scenes (optional, milestone 4)

```sql
create table scenes (
  id text primary key, owner_id text not null references users(id) on delete cascade,
  slug text not null unique, title text not null,
  doc text not null,                       -- the JSON document, verbatim
  visibility text not null check (visibility in ('private','unlisted','public')),
  created_at text not null, updated_at text not null
);
```

Deliberately `text`, not a parsed structure: the server keeps its promise not to understand
scene documents. Ownership and quota are the only things it reasons about.

### 6.5 Migrations policy

Embedded (`//go:embed migrations/*.sql`), run at startup before the listener opens. Single
replica makes that safe; an advisory lock is unnecessary and would be cargo cult. Rule:
**expand/contract only** — a migration must be readable by the previous binary, because
Swarm's `update_config` will roll back to it on a failed healthcheck.

---

## 7. Accounts

### 7.1 OAuth only

`markbates/goth` v1.82.0 with the GitHub provider (verified constructor:
`github.New(clientID, secret, callbackURL, "user:email")`, `Name() == "github"`), plus Google
if desired. `gothic.BeginAuthHandler` / `gothic.CompleteUserAuth` do the state dance; the goth
session store is a short-lived signed cookie carrying only `state` and the return path.

No passwords means no: password hashing parameters to get wrong, reset flow, email
verification loop, credential stuffing, breach notifications. Verified email comes free from
the provider, which is the actual anti-farming control.

First successful callback: upsert `identities(provider, provider_user_id)`; if it is new and a
`users` row with the same verified email exists, attach to it; otherwise insert a user. This is
the standard "link by verified email" rule and must be checked in that order to avoid both
duplicate accounts and silent account takeover by an unverified email.

### 7.2 Sessions

Opaque token, not a JWT. Revocation is a `DELETE`, redeploy does not log anyone out, and the
session table already exists for the quota query.

```
Set-Cookie: __Host-newton=<32 random bytes, base64url>; Path=/; Secure; HttpOnly; SameSite=Lax; Max-Age=<30d>
```

- `__Host-` forbids `Domain` and requires `Secure` + `Path=/`, so a sibling subdomain cannot set
  it. `-dev` drops the prefix and `Secure` for localhost.
- Store `sha256(token)` in `sessions`; a database dump then contains no usable session.
- Rotate on login (new row, delete the pre-login one), sliding expiry on use, logout deletes.
- Timing-safe compare is not needed for a hash lookup; constant-time is needed for the OAuth
  `state` comparison (`gothic` does this — verified `validateState` exists).

### 7.3 CSRF

JSON-only endpoints plus `SameSite=Lax` plus a check of `Sec-Fetch-Site` / `Origin` on
state-changing requests. No token: a cross-site HTML form cannot produce
`Content-Type: application/json`, and the endpoint binds typed JSON (verified: Echo's binder
returns **415** when the header is missing — that is the behavior relying on, so it gets a
test).

### 7.4 Routes

| Method | Path | Notes |
|---|---|---|
| GET | `/api/auth/{provider}` | 302 to provider with signed state |
| GET | `/api/auth/{provider}/callback` | verify state → upsert → set cookie → 302 `/` |
| POST | `/api/auth/logout` | delete session row, clear cookie |
| GET | `/api/me` | `{id, displayName, quota:{used, limit, resetsAt}}`; 401 when anonymous |
| POST | `/api/chat` | SSE; 401 anonymous, 429 quota, 409 in-flight, 503 disabled |
| GET | `/api/health` | liveness + migration version |
| GET / POST | `/api/scenes…` | milestone 4 |

The simulator never requires a session. Only `/api/chat` (and scene writes) do.

---

## 8. The chat endpoint

### 8.1 Request

```jsonc
POST /api/chat
Content-Type: application/json

{
  "conversationId": "0f3c…",        // omit to start one; echoed back on every event
  "message": "a 2 kg block slides down a 30° ramp",
  "scene": "<current textarea text, optional>",   // ≤ 64 KiB
  "report": {                        // optional; the previous attempt's verdict
    "ok": false,
    "issues": [ { "path": "bodies[1].shape.radius",
                  "message": "too small: must be greater than 0" } ]
  }
}
```

`report` is advisory: used to render a repair instruction and to record `scene_valid`, never
for quota or access decisions. The client is untrusted by construction.

Limits, enforced by `BodyLimit` + explicit checks: 4 KiB message, 64 KiB scene, 8 KiB report.

### 8.2 Response: SSE

```
event: delta
data: {"text":"{\n  \"gravity\""}

event: delta
data: {"text":"…"}

event: done
data: {"conversationId":"0f3c…","generationId":"…","usage":{"input":2412,"output":1033}}

event: error
data: {"code":"quota_exceeded","message":"Daily limit reached (10). Resets 00:00 UTC."}
```

- Verified: Echo's `c.Response().Flush()` works and buffers correctly under `httptest`
  (`rec.Flushed == true`).
- The provider request is bound to `r.Context()`; a closed tab cancels the handler and
  aborts the upstream call, so a user who closes the tab stops costing money.
- A keepalive comment (`: ping`) every 15 s prevents idle proxies from closing the stream.
- Because the status line is already sent, application errors after the first byte must be
  `event: error`, not HTTP codes. The client treats both paths identically.
- `Cache-Control: no-store`, `X-Accel-Buffering: no`, `Connection: keep-alive` as defence in
  depth for whatever ends up in front of it later.

### 8.3 Cancellation and partial output

On client abort: cancel the upstream request, settle the generation row as `canceled` with the
tokens actually consumed, and release the pending slot. A cancelled generation still counts
against quota — otherwise abort-and-retry is a free unlimited plan. The reasoning is recorded
in the code comment next to `settle`.

### 8.4 Prompt assembly

Three parts, assembled server-side so the wording lives in one testable place:

1. **System** (`prompt/scene-<version>.txt`, embedded):
   - the JSON Schema generated from zod — verified to work:
     `z.toJSONSchema(sceneSpecSchema, { io: 'input' })` →
     **2513 bytes**, Draft 2020-12, `additionalProperties: false` on every object,
     `oneOf` for the shape and joint unions, `minItems: 1` on `bodies`.
   - `io: 'input'` matters and was verified: with `io: 'output'`, defaulted fields
     (`angle`, `density`, `gravity`, `joints`, `linearVelocity`) land in `required`, which
     would tell the model to emit fields it does not need. Input mode requires only `bodies`
     at the root and `id/type/position/shape` on a body.
   - the four rules models actually get wrong here: `+y` is up; SI units; joint anchors are
     **local to their body**; ground is a static **`edge`**, never a thin box.
   - one complete worked example — the pendulum from `src/scene.json`.
   - output contract: exactly one fenced ` ```json ` block containing the **whole** document,
     never a diff, never prose after the block.
2. **User**: `message`, plus the current scene when present (`"make the ramp steeper"` needs
   it). Scene text is fenced and labelled as data.
3. **Repair turn** (only when `report.ok === false`): the issues rendered as
   `bodies[1].shape.radius: too small …` under a one-line instruction, appended *by the
   server* so the wording is consistent and testable, not composed by the client.

Generation parameters: `max_tokens` ≈ 2000 (bounds both cost and a runaway `bodies` array),
low temperature, and prompt caching on the system block where the provider supports it.

**Prompt injection** is structurally handled: model output is only ever `JSON.parse`d and
schema-validated, never executed, and the view uses `textContent` (already noted in
`main.ts`: *"textContent, never innerHTML: the reply is untrusted"*).

Schema drift: a committed artifact `server/internal/prompt/scene-schema.json` plus
`npm run schema` to regenerate and a test that fails when regeneration differs. The prompt
therefore cannot describe a schema the browser does not enforce.

### 8.5 Provider interface

```go
type Provider interface {
    // Chat streams deltas to emit. Returning an error cancels the generation.
    Chat(ctx context.Context, req Request, emit func(delta string) error) (Usage, error)
}

type Usage struct { InputTokens, OutputTokens int }
```

Implementations: `fake` (deterministic, scriptable, used by every test and by
`npm run dev:full` with `-provider fake`), and `real` (one hosted API; the model id is
configuration, not a hardcoded constant). No provider is named in this document on purpose —
prices and model ids change faster than plans.

---

## 9. Quota and abuse control

The threat is the owner's provider bill. Every control is a spend control.

| Layer | Mechanism | Default |
|---|---|---|
| Authentication | chat requires a session | — |
| In-flight | `one_pending` partial unique index | 1 per user |
| Per user/day | `count(*)` over `generations` since UTC midnight | 10 |
| Per conversation | turn counter, checked server-side | 6 |
| Request size | `BodyLimit` + explicit checks | 4/64/8 KiB |
| Completion size | `max_tokens` | 2000 |
| Per IP/day | `middleware.RateLimiter` (built into Echo; memory store) | 20 |
| Global | `sum(cost_micros)` since midnight, checked during reserve | configured |
| Kill switch | `CHAT_ENABLED=false` | off by default |

Reserve → call → settle, all with the *reserved* row inserted before the upstream request so
two concurrent requests cannot jointly overshoot the global cap. `cost_micros` comes from a
price table in code (`input/output per Mtok`), which is reconciled against invoices monthly
rather than trusted.

Logging: `request_id`, user id, model, prompt version, token counts, `scene_valid`. **Not**
prompts or completions. This is a classroom tool aimed at minors; a `debug_traces` table
behind a flag with 7-day expiry is the escape hatch for prompt iteration.

User-level kill: `users.disabled_at`. One `UPDATE` bans a farm.

---

## 10. Frontend integration

No framework — confirmed by measurement, not taste: every new surface is append-only text,
and the highest-frequency update (streamed tokens) wants a single text-node mutation rather
than a re-render. A framework would also force a bundler into a repo whose `index.html` is
an importmap and whose `dist/` is plain `tsc` output.

### 10.1 Changes to existing files

| File | Change |
|---|---|
| `src/chat.ts` | `ChatBackend` gains a delta sink; `Chat` exposes streaming state. Existing tests keep passing (the current `async () => 'dummy'` signature adapts) |
| `src/main.ts` | `apply(text)` and `report(target, err)` exported (or injected) so the AI glue can drive them; the incremental log renderer updates the **last** row while streaming instead of only appending |
| `index.html` | sign-in link, AI status line, `data-state` on the composer; markup for the verdict row |
| `src/ai/*` | new: protocol types, SSE parser, fenced-block extractor, client, glue |
| `src/scene/*`, `src/render/*`, `src/tabs.ts`, `src/loop.ts` | **untouched** |

### 10.2 The streaming seam

```ts
export interface DeltaSink { delta(text: string): void }
export type ChatBackend = (messages: readonly ChatMessage[], sink: DeltaSink) => Promise<string>;
```

`Chat.send` pushes an empty assistant message immediately, calls the backend with a sink that
appends and notifies, then finalises the text. Consequences:

- the bubble exists before the first token, so there is no layout jump;
- `isPending` keeps its current meaning;
- a failed stream still leaves the user turn (existing tested behavior);
- `dummyChat` remains a valid backend and stays the default when the API is absent.

### 10.3 Verdict and repair

`src/ai/glue.ts`, after `done`:

1. `extractJsonBlock(assembledText)` → the last complete fenced block, or the whole text if
   it is bare JSON. Unterminated fence ⇒ error, never a partial parse.
2. `buildSceneJson(block)` — the existing validator, unchanged.
3. **valid**: write into the textarea, show `✓ valid — applied`, call `apply()`.
   Not auto-apply: the user sees what changed, and `apply()` already keeps the old scene
   running on failure and resets the clock.
4. **invalid**: leave the textarea and the running scene alone, render every
   `SceneError.issues` path next to the message, show **Ask to fix**, which sends the issues
   as `report` with an empty user message on the next turn.

The old scene keeps playing throughout. That is existing behavior and it is the right demo
behavior.

### 10.4 Degradation matrix

| Condition | UI |
|---|---|
| `/api/me` 401 | composer disabled, "Sign in with GitHub to use AI" |
| `/api/me` network error / 404 (static dev) | same, labelled "AI unavailable in static mode" |
| `/api/chat` 503 | "AI is paused", composer disabled |
| `/api/chat` 429 | message with reset time, composer re-enabled |
| stream drops mid-way | text kept, "connection lost — retry", cancel already settled server-side |

---

## 11. Testing

Baseline to preserve: **73 passing**, and `src/scene/*` unmodified.

### Go (`go test ./...`, no network, no real provider)

- `quota`: injected clock; day boundary in UTC; over-limit; `pending` not double-counted;
  8 concurrent reserves → exactly 1 winner; stale `pending` reaped; settle after cancel still
  consumes quota.
- `auth`: absent/expired/revoked session; cookie attributes; logout deletes; `-dev` differences;
  `state` mismatch rejected; email-linking rule (same email + provider ⇒ same user).
- `chat`: fake provider streaming fixed deltas ⇒ exact SSE framing; abort cancels the provider
  context; repair request contains each issue path; `max_tokens` set; 401/409/429/503 paths;
  binder returns 415 without `Content-Type`.
- `store`: migrations on a fresh `t.TempDir()` database; DSN pragmas actually applied
  (`journal_mode=wal`, `foreign_keys=1`); partial index present.
- `web`: `.mjs` → `text/javascript`; unknown path → 404 not `index.html`; security headers.
- `httptest` end to end: login → `/api/me` → chat with fake → quota exhausted → 429.

### TypeScript (`npm test`)

- `jsonblock`: fenced, bare, trailing prose, unterminated fence, two blocks (take the last).
- `sse`: event split across chunk boundaries, multi-line `data:`, keepalive comments,
  `error` after `delta`.
- `client`: stubbed `fetch` returning a `ReadableStream`; abort propagates; non-200 mapping.
- `glue`: valid ⇒ textarea updated and `apply` called; invalid ⇒ textarea untouched and every
  path surfaced; valid-but-apply-fails keeps the old scene.
- `chat.ts`: streaming appends to one message; existing 8 tests unchanged.
- `tabs.ts`, `scene/*`, `render/*`, `loop.ts`: unchanged and still green.

### Contract fixtures

`testdata/sse/*.txt` are recorded server frames. A Go test asserts the server reproduces them
byte-for-byte; a TS test asserts the client parses them. That is the only anti-drift mechanism
the two languages need, and it costs one directory.

---

## 12. Deployment

### 12.1 Image

```
node:24-alpine   → npm ci && npm run build            → /out/dist
golang:1.26-alpine → CGO_ENABLED=0 go build -trimpath  → /out/newton
distroless/static-debian12:nonroot                    → /app/newton + /app/public
```

Verified constraints:

- **Go 1.26 is required, not 1.24.** `github.com/labstack/echo/v4@v4.16.3`,
  `modernc.org/sqlite@v1.61.1` and `github.com/pressly/goose/v3@v3.29.0` all declare
  `go 1.26.0`; `goth@v1.82.0` needs 1.23. `golang:1.26-alpine` exists on Docker Hub
  (checked) and nixpkgs unstable provides `go_1_26`.
- **`CGO_ENABLED=0` is mandatory.** Without it the build tries to compile `runtime/cgo` and
  fails when no C compiler is present (observed: `gcc: executable file not found`). With it,
  the whole dependency set — including `modernc.org/sqlite` — builds static. A 16 MB binary
  was produced by the proof of concept.
- `public/` is a plain directory in the image (`index.html`, `dist/`, `planck.mjs`, `zod/`),
  served via `-static /app/public`. No `go:embed` for assets: it would make `go build` depend
  on a build ordering that `npm run dev` does not have.
- Payload is ~8.6 MB of static assets, dominated by `zod` (8.1 MB) and `planck.mjs` (396 KB).
  That is today's cost, unchanged by this plan, and worth trimming later (`zod/mini`, or
  pre-bundling the importmap targets) but explicitly out of scope.

### 12.2 Stack file

One service, two containers sharing `newton_data`:

```yaml
services:
  newton:
    image: ghcr.io/kaki-studios/newton:${IMAGE_TAG:-main}
    volumes: [newton_data:/data]
    environment:
      - BASE_URL=https://newton.kaarlo.dev
      - CHAT_ENABLED=${CHAT_ENABLED:-false}
    secrets: [github_client_secret, session_key, provider_api_key]
    healthcheck:                     # the binary checks itself; distroless has no shell
      test: ["CMD", "/app/newton", "-healthcheck"]
    deploy:
      replicas: 1
      placement: { constraints: [node.role == manager] }   # the volume must not move
  litestream:
    image: litestream/litestream:0.5
    command: ["replicate", "-config", "/etc/litestream.yml"]
    volumes: [newton_data:/data]
    secrets: [litestream_credentials]

volumes:
  newton_data:
```

`litestream.yml` (verified against v0.5.18, which supports `exec`, `restore-if-db-not-exists`
and a single `replica:` key):

```yaml
dbs:
  - path: /data/newton.db
    replica:
      url: s3://<bucket>/newton
exec: /app/newton -static /app/public -db /data/newton.db
```

With `exec`, Litestream restores if the database is missing, waits for WAL recovery, starts the
app, and shuts it down cleanly when it exits — so a fresh node or an emptied volume comes up
rather than starting empty. `_litestream_lock` appears in the source database; that is expected
and must not be dropped.

**Restore drill, before it is needed:**
```
litestream restore -config /etc/litestream.yml -o /tmp/restored.db s3://<bucket>/newton
sqlite3 /tmp/restored.db 'pragma integrity_check; select count(*) from users;'
```
Write the command into the README and run it once against the real bucket.

### 12.3 Secrets

Swarm secrets mount as files at `/run/secrets/<name>`. Read with `os.ReadFile` at startup; do
not copy them into env. `.env` files stay out of the repo (`.gitignore` already covers nothing
relevant — add `.env`, `data/`, `server/newton`).

### 12.4 CI

Insert before the image build in `.github/workflows/deploy.yml`:

```yaml
- run: go vet ./...
  working-directory: server
- run: go test ./... -race
  working-directory: server
```

`-race` is cheap here and catches the concurrency in quota reserve. Keep the existing tag →
push → deploy → poll-for-replicas loop untouched.

### 12.5 Nix

Add `go_1_26` to the devShell. `CGO_ENABLED=0` keeps the shell from needing a C toolchain.

---

## 13. Milestones

Each one is independently shippable and leaves `npm test` + `go test ./...` green.

**M0 — docs and baseline (small).**
Fix the stale counts and descriptions in `AGENTS.md` and `pitch.tex` (73 tests, tabs exist,
`dummyChat` exists). Add `.env`, `data/` to `.gitignore`. No behavior change.

**M1 — chat UI against a fake stream (frontend only).**
`src/ai/{protocol,sse,jsonblock,client,glue}.ts`, streaming `ChatBackend`, verdict UI, repair
button, degradation matrix. A `-provider fake` is not needed yet: a `FakeStream` backend in
`src/ai/` emits a canned document character by character. Proves the *whole* loop — stream →
extract → validate → textarea → apply → repair — with zero backend. `npm test` covers all of it.

**M2 — Go shell.**
Module, config, Echo, static serving with the `.mjs` test, `/api/health`, `/api/me` → 401,
migrations + store + pragma tests, Dockerfile rewrite (delete `Dockerfile.nginx`), stack file
with volume + litestream, CI steps, nix shell. Deployed: same app, no AI.

**M3 — accounts.**
goth, sessions, `/api/auth/*`, `/api/me` with quota, sign-in link in the UI. Independently
useful; still no model spend.

**M4 — `/api/chat` with the fake provider.**
Real SSE, real quota, real reserve/settle, `-provider fake`. The full path is live and costs
nothing. Flip one env var to go live, `CHAT_ENABLED=true`.

**M5 — scenes and gallery** (optional; the pitch's phase 3): autosave for signed-in users,
`?scene=slug`, gallery. The `scenes` table ships in this milestone's migration, not
before it: nothing in M1–M4 needs it.

---

## 14. Risks and mitigations

| Risk | Mitigation |
|---|---|
| Schema drift between prompt and validator | schema is *generated* from zod + a regeneration-diff test |
| Repair loop burning budget | 6-turn cap per conversation, 10/day, `max_tokens`, global ceiling |
| Stuck `pending` row locks a user out | reaper on every reserve (§6.3), tested |
| Model emits plausible-but-wrong physics | validator + the old scene keeps running; this is existing behavior and the pitch's core claim |
| Account wall kills the hackathon demo | `CHAT_ENABLED` kill switch; anonymous trial can be added behind the same flag (§15) |
| SQLite lock contention | `SetMaxOpenConns(1)` + WAL + `busy_timeout`; revisit only with evidence |
| Volume lands on the wrong node | placement constraint + named volume; restore drill is part of the deploy checklist |
| Secrets leak via `docker inspect` | Swarm secrets as files, never env |
| Prompt/completion retention of minors' text | not logged by default; flagged debug table with expiry |

---

## 15. Open decisions (deliberately not decided here)

1. **Anonymous trial.** OAuth-only means a judge must sign up before the first prompt. A
   cookie- and IP-capped anonymous allowance (e.g. 3/day) is a flag on the chat handler, not an
   architectural change — recommended default for a demo, `off` for production.
2. **Which provider/model.** Configuration, not code. Mid-tier is enough: the task is to emit a
   document against a strict schema.
3. **Provider-side moderation.** Recommended on; it is a classroom tool.
4. **`bodies` array cap.** Bounded today by `max_tokens`; an explicit semantic limit (e.g.
   ≤ 50 bodies) would be a new rule in `semantics.ts` and is a separate decision with its own
   tests.

---

## 16. Appendix — facts verified while writing this plan

Everything below was executed or fetched, not assumed:

- `npm test` → 73 tests, 73 pass.
- `z.toJSONSchema(sceneSpecSchema, {io:'input'})` → 2513 bytes; root `required: ["bodies"]`;
  body `required: ["id","type","position","shape"]`; `additionalProperties:false`
  throughout; `oneOf` for shapes and joints.
  With `io:'output'`, defaulted fields become required — hence input mode.
- `github.com/labstack/echo/v4@v4.16.3` declares `go 1.26.0`; `modernc.org/sqlite@v1.61.1`
  and `goose@v3.29.0` likewise; `goth@v1.82.0` declares `go 1.23.0`.
- Local toolchain is Go 1.26.10; `golang:1.26-alpine` exists on Docker Hub.
- `CGO_ENABLED=0` is required: without it the build fails on missing `gcc`.
- `modernc.org/sqlite` DSN with `_pragma=journal_mode(WAL)`, `busy_timeout(5000)`,
  `foreign_keys(1)`, `synchronous(NORMAL)`, `_txlock=immediate` parses and applies.
- goose `SetBaseFS` + `SetDialect("sqlite3")` + `Up(db, "migrations")` applies the schema.
- The `one_pending` partial unique index rejects a second pending row; settling frees it;
  8 concurrent inserts yield exactly one winner.
- `c.Response().Flush()` streams and `httptest` reports `Flushed == true`.
- Echo's binder returns **415** without `Content-Type: application/json`.
- `goth/providers/github.New(clientID, secret, callbackURL, "user:email")` exists and
  `Name() == "github"`; `gothic` exposes `BeginAuthHandler` and `validateState`.
- Echo v4 ships `middleware/{body_limit,rate_limiter,secure,recover,request_id,csrf}.go`.
- Go's builtin mime table returns `text/javascript; charset=utf-8` for `.mjs`.
- `litestream/litestream:0.5` exists; v0.5 config supports `exec` and
  `restore -if-db-not-exists`; a `_litestream_lock` table appears in the source DB.
- Static payload: `zod` 8.1 MB, `planck.mjs` 396 KB, `dist/` 144 KB.
- `dist/scene.json` differs from `src/scene.json` only by `tsc`'s JSON formatting — not a bug.
