-- The whole schema for accounts + metered generations.
--
-- Timestamps are ISO-8601 UTC text with millisecond precision. SQLite has no native date
-- type, and text compares lexicographically in the same order it sorts chronologically, so
-- `created_at >= ?` works with a bound string. The day boundary is computed in Go and passed
-- in, which is what lets quota tests use an injected clock instead of sleeping.

-- +goose Up
create table users (
  id            text primary key,
  email         text unique,
  display_name  text,
  created_at    text not null default (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  disabled_at   text
);

-- One row per (provider, provider id). The same human may arrive through GitHub and Google;
-- linking happens by verified email at sign-in time (internal/store/users.go).
create table identities (
  provider          text not null,
  provider_user_id  text not null,
  user_id           text not null references users(id) on delete cascade,
  primary key (provider, provider_user_id)
);
create index identities_user on identities (user_id);

-- Only sha256(cookie value) is stored: a database dump contains no usable session.
create table sessions (
  token_hash  blob primary key,
  user_id     text not null references users(id) on delete cascade,
  expiry      text not null,
  created_at  text not null default (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
create index sessions_user on sessions (user_id);
create index sessions_expiry on sessions (expiry);

-- One row per model call. Written as 'pending' *before* the upstream request so two
-- concurrent requests cannot jointly overshoot the global cap; settled after it returns.
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
create index generations_conversation on generations (user_id, conversation_id, created_at);

-- One generation in flight per user, enforced by the database rather than by a mutex in
-- one process: a second pending row for the same user is impossible. Settling frees it.
create unique index one_pending on generations (user_id) where status = 'pending';

-- +goose Down
drop table generations;
drop table sessions;
drop table identities;
drop table users;
