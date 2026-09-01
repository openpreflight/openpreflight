# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [1.1.0] - 2026-09-01

Operator chrome and the docker.sock workspace mount rewrite. Image
`ghcr.io/openpreflight/openpreflight:1.1.0`.

### Changed

- Configurator chrome is a shadcn-templ operator shell: Sidebar (Workspace /
  Setup / Settings), resource cards, Inset breadcrumbs. **templ** + copied
  shadcn-templ 2.0 components on Tailwind v4 (nova / olive, forest green). Theme
  follows `localStorage.theme` (`system` default). Sidebar collapse persists in
  the `sidebar_state` cookie.

### Added

- `.ci.yml` so this repository's Check Runs go through ci.openpreflight.xyz.

### Fixed

- `runtime:` jobs using the host `docker.sock` mount the checkout from the
  host path behind `WORKSPACE_DIR` (via `/proc/self/mountinfo`, or
  `CI_WORKSPACE_HOST`). Sibling containers no longer see an empty `/work`.

## [1.0.0] - 2026-08-29

v1 of the configurator and worker in one Go binary.

### Added

- Web UI and JSON API to register Coolify team tokens, GitHub Apps, and repo
  bindings.
- GitHub App webhook receiver that enqueues install/test/build on the exact
  commit and reports one Check Run with logs.
- SQLite store (`modernc.org/sqlite`) with AES-256-GCM for PEM, webhook secret,
  and Coolify token columns.
- First-boot setup wizard, optional `CI_BOOTSTRAP_ADMIN_PASSWORD` for headless
  deploys, session cookie plus Bearer token from `POST /api/v1/login`.
- Docker image and Compose file: persistent `/data`, disposable `/workspace`,
  non-root runtime, `git`, Node, and `docker-cli` in the image. Compose mounts
  the host docker socket for `runtime:` jobs.
- `POST /api/v1/coolify/{id}/install-worker` creates a Coolify compose
  application with `instant_deploy: false`.
- `CI_SECRET_KEY_OLD` re-seals secret columns under `CI_SECRET_KEY` on boot.
- Graceful shutdown waits up to 30 seconds for in-flight jobs to record
  themselves cancelled before the process exits, so a redeploy leaves a
  cancelled Check Run rather than a job stranded `in_progress`. A job still
  running past that, or a hard kill, is requeued on the next boot.
- Settings `default_runtime`. `skip_fork_prs` is writable when Docker is
  reachable and a default runtime is set; fork jobs always use Docker.
- `check_suite_id` recorded on every job, from both the `check_suite` and the
  `check_run` payload, as a stable handle back to GitHub's suite.
- One live run per `(app, repo, sha)`. A `check_suite.requested` delivery for a
  commit already in flight is answered `already queued`; a `rerequested` one
  cancels that run and enqueues a fresh one.
- [ADR 005](https://docs.openpreflight.xyz/adr/005-check-suite-gating/) records the trigger model:
  `check_suite`/`check_run` only, one Check Run per job, Zuul as the referenced
  model with its architecture rejected.

### Changed

- `install-worker` falls back to Coolify `POST /api/v1/services` when
  `/api/v1/applications/dockercompose` is missing (self-hosted 4.3.x).

### Fixed

- Duplicate Check Runs with the same name on one commit. Dedup was keyed on
  `delivery_id` and cancellation on `(repo, ref)`, so the same commit arriving on
  a second ref — or a rerequest landing while a job ran — started a second
  concurrent job, and branch protection read whichever finished last.
- Intermittent failures in the `internal/queue` tests: the harness asserted as
  soon as a job's status went terminal, which is written before the Check Run
  PATCH and the final log-size write.

### Security

- HMAC webhook verification per App slug.
- Fork PRs skipped by default; opt-in requires Docker isolation.
- CSRF on cookie-authenticated writes; Bearer callers skip CSRF.
- Clone credential passed via `GIT_CONFIG_*`, never the remote URL; remote
  stripped before pipeline steps run.
- Job containers: `--security-opt no-new-privileges`, `--cap-drop ALL`, no
  engine socket.

[unreleased]: https://github.com/openpreflight/openpreflight/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/openpreflight/openpreflight/releases/tag/v1.1.0
[1.0.0]: https://github.com/openpreflight/openpreflight/releases/tag/v1.0.0
