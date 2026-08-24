# ADR 001: SQLite in-process, secrets encrypted at rest

- Status: accepted
- Date: 2026-08-24

## Context

The process is a single replica: one configurator, one worker, a few jobs at a
time. It must remember GitHub Apps (PEM + webhook secret), Coolify tokens,
bindings, and job history across Coolify redeploys. The Docker image should
stay `CGO_ENABLED=0`.

## Decision

Use SQLite via `modernc.org/sqlite` (pure Go). File path is
`$DATA_DIR/ci.db`. WAL + `busy_timeout` + a single open connection. Schema
changes are append-only migrations in `internal/store`.

Secret columns (`pem_enc`, `webhook_secret_enc`, `api_token_enc`) are
AES-256-GCM sealed by `internal/secret` under a key derived from
`CI_SECRET_KEY` (SHA-256 → 32 bytes). GET responses return a redacted marker.
There is no key rotation in v1.

## Consequences

- A persistent volume on `/data` is mandatory. Lose it and you re-enter Apps
  and tokens; lose `CI_SECRET_KEY` and the existing ciphertext is unreadable.
- No Postgres/Redis to operate. No horizontal scale: `max_concurrent_jobs`
  is in-process.
- A stolen database without the key is not a full secret leak. A stolen key
  plus the database is.
- CGO stays off; the runtime image does not need a libc sqlite shim.
