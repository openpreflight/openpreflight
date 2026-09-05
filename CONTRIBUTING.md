# Contributing

Thanks for wanting to change this. Keep the surface small: one binary, one
SQLite file, configuration in the UI rather than a pile of env vars.

## Before you start

- Read [Architecture](https://docs.openpreflight.xyz/reference/architecture/)
  and the [decision records](https://docs.openpreflight.xyz/reference/decisions/005-check-suite-gating/).
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

If you change `internal/web/layouts`, `internal/web/pages`, or
`internal/web/assets/css`, regenerate templ and rebuild CSS before committing
(the Dockerfile does both at image-build time):

```bash
templ generate ./internal/web/...
cd internal/web && npm ci && npm run css
```

## What we will take

- Bug fixes with a test that would have failed before the change.
- Gaps that stay inside the current process: configurator + local worker.
- Docs that match the code.

## What we will not take yet

Anything listed under **Out of scope** in the README: GitHub Actions YAML,
`actions/runner`, matrices, caches, or artifacts.

## Pull requests

- One concern per PR.
- Do not commit `.env`, `*.pem`, `data/`, or `workspace/`.
- Do not add `pkg/` unless something here is meant to be imported by other
  modules. Client code for Coolify and GitHub stays under `internal/`.
- Fill in `.github/PULL_REQUEST_TEMPLATE.md`.

## Versioning

Semantic versioning, with the boundaries drawn where they actually bite for a
self-hosted server that owns your branch protection:

| Bump | Means | Examples |
|---|---|---|
| **Major** | A change that can break a working install | A removed or renamed endpoint or JSON field; a setting whose default changes behaviour; a migration that cannot be applied without a decision from the operator |
| **Minor** | Additive | New endpoints and pages; new settings that default to today's behaviour; migrations that only add columns |
| **Patch** | Fixes | Bug fixes, UI work, docs — no schema change, no contract change |

Three commitments that follow from that, and are worth stating because each has
already decided something:

- **Released tags are never renumbered.** `v2.0.0` stays `v2.0.0` even though
  the version before it was `v1.1.0` and the jump was mostly operator UI. The
  changelog is a record, not a narrative.
- **"Additive" is a constraint on design, not a description after the fact.**
  When `on_empty_pipeline` was added, its default was chosen to be the existing
  behaviour *so that* the release could honestly be a minor. If a change cannot
  be made additive, it waits for a major rather than being shipped as one.
- **The check name is effectively part of the contract.** GitHub matches a
  required status check by its name string, so changing an existing install's
  default check name would leave its branch protection rule permanently
  unsatisfiable. That is why `defaultSettings()` affects new installs only.

Every release section in `CHANGELOG.md` carries an **Upgrade** line saying
either *No action required* or exactly what to run and what to set. A release
without one is not ready to tag.
