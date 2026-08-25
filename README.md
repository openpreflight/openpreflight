# openpreflight

A small CI provider for private repos: one Go binary that is both a
**configurator** (add Coolify team tokens, GitHub Apps, repo bindings in a web UI
or over JSON) and a **worker** (receive GitHub App webhooks, run install/test/build
on the exact commit, report one Check Run with full logs).

It is the smallest useful version of GitHub-native CI: a self-hosted Check Runs
runner for teams that want CI on their own server, without Actions and without
learning a pipeline DSL. The systems that already fill this slot are full
platforms, hosted control planes, Kubernetes-oriented, or heavyweight gating
systems; this one is a binary and a SQLite file on a box you already pay for.

Runs are gated on the commit, the way Zuul does it — trigger on the check suite,
build the immutable SHA, one live run per commit, result written back as a Check
Run. See [ADR 005](https://docs.openpreflight.xyz/adr/005-check-suite-gating/) for
what that borrows, what it rejects, and where the ceiling is.

```text
openpreflight
────────────────────
✓ install    8s
✓ test      21s
✓ build     13s

Passed in 42s

View full logs →
```

## Documentation

Published at **[docs.openpreflight.xyz](https://docs.openpreflight.xyz)** and
written in [openpreflight/docs](https://github.com/openpreflight/docs). A change
here that alters behaviour needs a pull request there alongside it.

- [Quickstart](https://docs.openpreflight.xyz/start/quickstart/) · [Configuration](https://docs.openpreflight.xyz/start/configuration/)
- [GitHub App](https://docs.openpreflight.xyz/setup/github-app/) · [Bindings](https://docs.openpreflight.xyz/setup/bindings/) · [Coolify](https://docs.openpreflight.xyz/setup/coolify/)
- [Pipelines](https://docs.openpreflight.xyz/using/pipelines/) · [Logs](https://docs.openpreflight.xyz/using/logs/) · [API](https://docs.openpreflight.xyz/using/api/)
- [Architecture](https://docs.openpreflight.xyz/understanding/architecture/) · [Security model](https://docs.openpreflight.xyz/understanding/security-model/) · [Deployment](https://docs.openpreflight.xyz/understanding/deployment/)
- [ADRs](https://docs.openpreflight.xyz/adr/005-check-suite-gating/) · [Development](https://docs.openpreflight.xyz/contributing/development/) · [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md) · [Changelog](CHANGELOG.md)

## Repository layout

This repository is the binary and nothing else. The prose and the two sites are
their own repositories under the [openpreflight](https://github.com/openpreflight)
org.

```text
cmd/server/          entrypoint
internal/            the whole implementation — no pkg/, nothing importable
  web/               templates, Tailwind styles, and the embedded static assets
examples/            a sample .ci.yml
.github/workflows/   ci.yml — Go vet, test, and a Docker build; gates merges
```

| Repo | What it is |
|---|---|
| **openpreflight** (here) | The Go binary — configurator and worker |
| [docs](https://github.com/openpreflight/docs) | Astro Starlight → [docs.openpreflight.xyz](https://docs.openpreflight.xyz) |
| [website](https://github.com/openpreflight/website) | Astro marketing → [openpreflight.xyz](https://openpreflight.xyz) |
| [.github](https://github.com/openpreflight/.github) | The org landing page |

`internal/web` carries the one `package.json` in this repo, and it exists only
to compile Tailwind for the Go UI. The Dockerfile installs it in an isolated
stage, so it must stay standalone rather than becoming part of a workspace.

## Requirements

- A GitHub App you own (permissions and events in the
  [docs](https://docs.openpreflight.xyz/setup/github-app/))
- A public HTTPS URL GitHub can reach
- `git` in the worker image (clone happens here). Node is needed in this
  image only when a job has no `runtime:` and runs as a process
- A reachable Docker engine (`CI_DOCKER_HOST` or a mounted `docker.sock`) if
  you use `runtime:` or opt into fork PRs
- Optionally, a Coolify API token — inventory, the repo picker, and
  install-worker

## Run it

```bash
export CI_SECRET_KEY="$(openssl rand -base64 48)"   # required, keep it forever
export CI_PUBLIC_BASE_URL="https://ci.example.com"  # optional seed
docker compose up --build
```

Then open the UI, complete the wizard, register your GitHub App, and enable
bindings. Full walkthrough:
[Quickstart](https://docs.openpreflight.xyz/start/quickstart/).

## Not in v1

GitHub Actions YAML, `actions/runner`, creating GitHub Apps for you, matrices,
caches, and artifacts. Jobs on another machine use `CI_DOCKER_HOST` /
`DOCKER_HOST` (a Docker engine), not Coolify's API as a job runner.

## Development

See [Development](https://docs.openpreflight.xyz/contributing/development/) for
CSS rebuilds and layout rules. The two Astro sites build from their own
repositories — [docs](https://github.com/openpreflight/docs) and
[website](https://github.com/openpreflight/website).

```bash
go build ./...
go test ./...
go vet ./...

CI_SECRET_KEY="$(openssl rand -base64 48)" DATA_DIR=./data WORKSPACE_DIR=./workspace \
  LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

Tests need no network and no credentials: the Coolify and GitHub APIs are faked,
and clone/pipeline tests run against a real `git-http-backend` server over a
fixture repository.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
