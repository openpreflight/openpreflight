# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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
- Settings `default_runtime`. `skip_fork_prs` is writable when Docker is
  reachable and a default runtime is set; fork jobs always use Docker.
- `check_suite_id` recorded on every job, from both the `check_suite` and the
  `check_run` payload, as a stable handle back to GitHub's suite.
- One live run per `(app, repo, sha)`. A `check_suite.requested` delivery for a
  commit already in flight is answered `already queued`; a `rerequested` one
  cancels that run and enqueues a fresh one.
- [ADR 005](docs/adr/005-check-suite-gating.md) records the trigger model:
  `check_suite`/`check_run` only, one Check Run per job, Zuul as the referenced
  model with its architecture rejected.

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
