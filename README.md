<div align="center">

<picture>
  <source
    media="(prefers-color-scheme: dark)"
    srcset="https://openpreflight.xyz/banner-dark.png"
  />
  <img
    src="https://openpreflight.xyz/banner-light.png"
    alt="openpreflight — Self-hosted CI without the CI platform. One Go binary, one SQLite file: every commit gets a native GitHub Check Run."
    width="880"
  />
</picture>

[![Website](https://img.shields.io/badge/website-openpreflight.xyz-2f6f4f?style=flat-square)](https://openpreflight.xyz)
[![Docs](https://img.shields.io/badge/docs-docs.openpreflight.xyz-2f6f4f?style=flat-square)](https://docs.openpreflight.xyz)
[![License](https://img.shields.io/badge/license-Apache--2.0-8a8a84?style=flat-square)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)

**This repository is checked by openpreflight.** `go vet ./...` and `go test ./...`
run on a self-hosted instance at [ci.openpreflight.xyz](https://ci.openpreflight.xyz),
and the Check Run on each commit comes from a GitHub App registered the same way
you would register yours. GitHub Actions is kept for one job: building and
publishing release images on a `v*` tag, which this tool does not do.

One Go binary that is both a **configurator** — GitHub Apps and repo bindings in a web UI or over JSON — and a **worker** that receives webhooks, runs install/test/build on the exact commit, and reports one Check Run with full logs.

[Website](https://openpreflight.xyz) · [Documentation](https://docs.openpreflight.xyz) · [Quickstart](https://docs.openpreflight.xyz/start/quickstart/)

</div>

## Why

It is the smallest useful version of GitHub-native CI: a self-hosted Check Runs runner for teams that want CI on their own server, without Actions and without learning a pipeline DSL. Full platforms, hosted control planes, and Kubernetes-oriented runners already fill this slot. This one is a binary and a SQLite file on a box you already pay for.

Runs are gated on the commit the way Zuul does it — trigger on the check suite, build the immutable SHA, one live run per commit, result written back as a Check Run. See [ADR 005](https://docs.openpreflight.xyz/adr/005-check-suite-gating/) for what that borrows, what it rejects, and where the ceiling is.

## What you get

| | |
| --- | --- |
| **One process** | UI, JSON API, webhook receiver, and job runner in a single Go binary. No broker, no separate frontend. |
| **One file of state** | SQLite, with every secret column AES-256-GCM encrypted at rest. |
| **Configured in a UI** | Register GitHub Apps, bind repos, mint tokens — without a pile of env vars for every installation. |

## Documentation

Published at **[docs.openpreflight.xyz](https://docs.openpreflight.xyz)** ([openpreflight/docs](https://github.com/openpreflight/docs)). A behaviour change here needs a docs PR alongside it.

| | |
| --- | --- |
| Start | [Quickstart](https://docs.openpreflight.xyz/start/quickstart/) · [Configuration](https://docs.openpreflight.xyz/start/configuration/) · [FAQ](https://docs.openpreflight.xyz/start/faq/) |
| Setup | [GitHub App](https://docs.openpreflight.xyz/setup/github-app/) · [Bindings](https://docs.openpreflight.xyz/setup/bindings/) · [Coolify](https://docs.openpreflight.xyz/setup/coolify/) |
| Using | [Pipelines](https://docs.openpreflight.xyz/using/pipelines/) · [Logs](https://docs.openpreflight.xyz/using/logs/) · [API](https://docs.openpreflight.xyz/using/api/) · [Troubleshooting](https://docs.openpreflight.xyz/using/troubleshooting/) |
| Understanding | [Architecture](https://docs.openpreflight.xyz/understanding/architecture/) · [Security model](https://docs.openpreflight.xyz/understanding/security-model/) · [Deployment](https://docs.openpreflight.xyz/understanding/deployment/) |
| Project | [ADRs](https://docs.openpreflight.xyz/adr/001-database/) · [Development](https://docs.openpreflight.xyz/contributing/development/) · [Contributing](CONTRIBUTING.md) · [Security](SECURITY.md) · [Changelog](CHANGELOG.md) |

## Run it

`CI_SECRET_KEY` is the only variable you must set. Everything else has a default or is asked for in the setup wizard.

```bash
curl -O https://raw.githubusercontent.com/openpreflight/openpreflight/main/compose.prod.yaml
export CI_SECRET_KEY="$(openssl rand -base64 48)"   # keep it forever
docker compose -f compose.prod.yaml up -d
```

That pulls the published image; no checkout is needed. Open <http://localhost:8080>, complete the wizard, register your GitHub App, and enable bindings. Full walkthrough: [Quickstart](https://docs.openpreflight.xyz/start/quickstart/).

To build from source instead:

```bash
git clone https://github.com/openpreflight/openpreflight
cd openpreflight
export CI_SECRET_KEY="$(openssl rand -base64 48)"
docker compose up --build
```

| File | For | Image |
| --- | --- | --- |
| `compose.prod.yaml` | Running it | Pulls `ghcr.io/openpreflight/openpreflight` |
| `compose.yaml` | Working on it | Builds from the checkout |

`runtime:` jobs and fork PRs also need `DOCKER_GID` set to the docker socket's group **as the container sees it** — `0` on Docker Desktop, usually `998` or `999` on Linux. Read it with `docker compose exec openpreflight stat -c %g /var/run/docker.sock`. Nothing else requires it.

### Requirements

- A GitHub App you own ([permissions and events](https://docs.openpreflight.xyz/setup/github-app/))
- A public HTTPS URL GitHub can reach
- `git` in the worker image (clone happens here). Node is needed in this image only when a job has no `runtime:` and runs as a process
- A reachable Docker engine (`CI_DOCKER_HOST` or a mounted `docker.sock`) if you use `runtime:` or opt into fork PRs
- Optionally, a Coolify API token — inventory, the repo picker, and install-worker

## Repository layout

This repository is the binary and nothing else. The prose and the two sites are their own repositories under [openpreflight](https://github.com/openpreflight).

```text
cmd/server/          entrypoint
internal/            the whole implementation — no pkg/, nothing importable
  web/               templ layouts/pages, copied shadcn-templ components, embedded CSS
examples/            a sample .ci.yml
.github/workflows/   ci.yml gates merges; release.yml publishes on a v* tag
```

| Repo | What it is |
| --- | --- |
| **openpreflight** (here) | The Go binary — configurator and worker |
| [docs](https://github.com/openpreflight/docs) | Astro Starlight → [docs.openpreflight.xyz](https://docs.openpreflight.xyz) |
| [website](https://github.com/openpreflight/website) | Astro marketing → [openpreflight.xyz](https://openpreflight.xyz) |
| [.github](https://github.com/openpreflight/.github) | The org landing page |

`internal/web` carries the one `package.json` in this repo, and it exists only to compile Tailwind for the Go UI. The Dockerfile installs it in an isolated stage, so it must stay standalone rather than becoming part of a workspace.

## Out of scope

GitHub Actions YAML, `actions/runner`, matrices, caches, and artifacts. Jobs on another machine use `CI_DOCKER_HOST` / `DOCKER_HOST` (a Docker engine), not Coolify's API as a job runner. GitHub Apps can be created with GitHub's review screen or pasted.

## Development

See [Development](https://docs.openpreflight.xyz/contributing/development/) for CSS rebuilds and layout rules. The Astro sites build from [docs](https://github.com/openpreflight/docs) and [website](https://github.com/openpreflight/website).

```bash
go build ./...
go test ./...
go vet ./...

CI_SECRET_KEY="$(openssl rand -base64 48)" DATA_DIR=./data WORKSPACE_DIR=./workspace \
  LISTEN_ADDR=127.0.0.1:8080 go run ./cmd/server
```

Tests need no network and no credentials: the Coolify and GitHub APIs are faked, and clone/pipeline tests run against a real `git-http-backend` server over a fixture repository.

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
