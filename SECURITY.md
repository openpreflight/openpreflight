# Security policy

This service stores GitHub App private keys, webhook secrets, and Coolify API
tokens. Treat a running instance as a secrets store, not just a CI worker.

## Reporting a vulnerability

Do **not** open a public issue.

This repository is private. GitHub private vulnerability reporting and
Security Advisories are not available on it. Email **trivedivatsal005@gmail.com**
instead.

If the repository is later made public, use GitHub Security Advisories:

https://github.com/trivedi-vatsal/coolify-github-ci/security/advisories/new

Include enough to reproduce: the endpoint or UI path, what you sent, and what
you observed. Do not attach live PEMs, tokens, or a copy of `ci.db`.

You should hear back within a week. Please give a reasonable window to patch
before any public write-up.

## What this project already treats as sensitive

- `CI_SECRET_KEY` — AES-256-GCM key material. Losing it (and any
  `CI_SECRET_KEY_OLD` still needed for a rotate) makes stored PEMs and tokens
  unreadable. To rotate, boot once with both keys set, then unset the old one.
- GitHub App PEM and webhook secret (encrypted at rest, redacted on GET).
- Coolify API tokens (same).
- Session cookies and Bearer session tokens.
- Shareable job-log URLs, when a binding opts into them.

A stolen `ci.db` without `CI_SECRET_KEY` is not a full compromise of those
secret columns. A stolen key plus the database is.

## Scope notes

- Fork pull requests are skipped by default. Enabling them requires a reachable
  Docker engine and `default_runtime`; those jobs always run in Docker
  (`no-new-privileges`, `cap-drop ALL`, no docker.sock in the job container).
  That is isolation, not a sandbox VM.
- `/webhook/{slug}` is public and HMAC-verified. `/health` is public. A
  shareable `/runs/{id}` is public only when that job's binding opted in.
- Job environments are built from scratch: no `CI_SECRET_KEY`, no PEMs, no
  Coolify tokens, no installation token.
