# ADR 002: Local admin, opaque sessions

- Status: accepted
- Date: 2026-08-24

## Context

The configurator can create GitHub Apps' PEMs, Coolify tokens, and an
allow-list of repos whose code will execute on this host. That surface must
not be open to anyone who can reach the URL. GitHub already authenticates
webhooks with HMAC; that is a different trust boundary.

## Decision

v1 has a single local user. Password is bcrypt (cost default, minimum 12
characters). First boot is either the setup wizard or
`CI_BOOTSTRAP_ADMIN_PASSWORD`.

Login issues an opaque 32-byte session token stored in SQLite (14-day TTL):

- Browser: `ci_session` HttpOnly cookie, `Secure` behind HTTPS,
  `SameSite=Lax`. Cookie writes require a CSRF token (`ci_csrf` cookie +
  form field / `X-CSRF-Token`).
- CLI: the same token as `Authorization: Bearer …` from `POST /api/v1/login`.
  Bearer callers have no ambient cookie, so CSRF is skipped.

There are no separate API tokens, no GitHub OAuth for the UI, and no roles.

Public without a session: `GET /health`, `POST /webhook/{slug}`, and
`GET /runs/{id}` / job logs only when that job's binding opted into
shareable logs.

## Consequences

- Operators are not coupled to GitHub identity. A stolen GitHub session
  cannot open the configurator.
- There is one admin. Sharing the password is the access model.
- Logout must delete both the cookie and any Bearer token the caller
  presented; JSON login does not set a cookie.
- Shareable log links are unguessable UUIDs but still secrets if the binding
  opted in.
