# Development

Go 1.26 (see `go.mod`). No CGO. Tests need `git`; they do not need network,
GitHub, or Coolify.

## Run locally

```bash
go build ./...
go test ./...
go vet ./...

CI_SECRET_KEY="$(openssl rand -base64 48)" DATA_DIR=./data WORKSPACE_DIR=./workspace \
  LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

Open http://127.0.0.1:8080 and complete the setup wizard, or set
`CI_BOOTSTRAP_ADMIN_PASSWORD` and drive `POST /api/v1/login`.

`data/` and `workspace/` are gitignored. Do not point `DATA_DIR` at a
directory you care about: jobs write checkouts there.

## Tests

`go test ./...` fakes the Coolify and GitHub HTTP APIs and runs clone/pipeline
tests against a real `git-http-backend` over a fixture repository
(`internal/testsupport`).

A change that touches store, auth, webhook HMAC, or the runner should come
with a test that would have failed before the change.

## UI / CSS

Templates live in `internal/web/templates`. Styles are Tailwind, compiled into
`internal/web/static/app.css` and embedded at build time. There is no SPA and
almost no JavaScript.

After editing templates or `internal/web/styles/input.css`:

```bash
cd internal/web && npm ci && npm run css
```

The Dockerfile always rebuilds CSS in a Node stage so a forgotten `npm run css`
cannot ship stale styles. Local `go run` uses whatever is already in
`static/app.css`.

## Layout

Do not introduce `pkg/` or flatten `internal/` into generic `auth` /
`database` / `service` packages. Name packages after the work they do. See
[architecture.md](architecture.md) and [CONTRIBUTING.md](../CONTRIBUTING.md).
