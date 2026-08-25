# Contributing

Thanks for wanting to change this. Keep the surface small: one binary, one
SQLite file, configuration in the UI rather than a pile of env vars.

## Before you start

- Read [Architecture](https://docs.openpreflight.xyz/understanding/architecture/)
  and the [decision records](https://docs.openpreflight.xyz/adr/005-check-suite-gating/).
  Both live in [openpreflight/docs](https://github.com/openpreflight/docs); a
  change here that alters documented behaviour needs a pull request there too.
- Security-sensitive reports go to [SECURITY.md](SECURITY.md), not a public
  issue.

## Dev loop

```bash
go test ./...
go vet ./...

CI_SECRET_KEY="$(openssl rand -base64 48)" DATA_DIR=./data WORKSPACE_DIR=./workspace \
  LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

Tests need no network and no credentials. Coolify and GitHub are faked; clone
and pipeline tests talk to a local `git-http-backend`.

If you change `internal/web/templates` or `internal/web/styles`, rebuild CSS
before committing (the Dockerfile does this at image-build time):

```bash
cd internal/web && npm ci && npm run css
```

## What we will take

- Bug fixes with a test that would have failed before the change.
- Gaps in v1 that stay inside the current process: configurator + local worker.
- Docs that match the code.

## What we will not take yet

Anything listed under **Not in v1** in the README: GitHub Actions YAML,
`actions/runner`, creating GitHub Apps for you, matrices, caches, or artifacts.

## Pull requests

- One concern per PR.
- Do not commit `.env`, `*.pem`, `data/`, or `workspace/`.
- Do not add `pkg/` unless something here is meant to be imported by other
  modules. Client code for Coolify and GitHub stays under `internal/`.
- Fill in `.github/PULL_REQUEST_TEMPLATE.md`.
