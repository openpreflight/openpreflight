# Security policy

This service stores GitHub App private keys, webhook secrets, and Coolify API
tokens. Treat a running instance as a secrets store, not just a CI worker.

## Reporting a vulnerability

Do **not** open a public issue.

Report privately via GitHub Security Advisories:

https://github.com/trivedi-vatsal/coolify-github-ci/security/advisories/new

Include enough to reproduce: the endpoint or UI path, what you sent, and what
you observed. Do not attach live PEMs, tokens, or a copy of `ci.db`.

You should hear back within a week. Please give a reasonable window to patch
before any public write-up.

## What this project already treats as sensitive

- `CI_SECRET_KEY` — AES-256-GCM key material. Losing it makes stored PEMs and
  tokens unreadable. There is no rotation in v1.
- GitHub App PEM and webhook secret (encrypted at rest, redacted on GET).
- Coolify API tokens (same).
- Session cookies and Bearer session tokens.
- Shareable job-log URLs, when a binding opts into them.

A stolen `ci.db` without `CI_SECRET_KEY` is not a full compromise of those
secret columns. A stolen key plus the database is.

## Scope notes

- Fork pull requests are skipped on purpose: a pipeline runs the repo's own
  commands on this host.
- `/webhook/{slug}` is public and HMAC-verified. `/health` is public. A
  shareable `/runs/{id}` is public only when that job's binding opted in.
- Job environments are built from scratch: no `CI_SECRET_KEY`, no PEMs, no
  Coolify tokens, no installation token.
