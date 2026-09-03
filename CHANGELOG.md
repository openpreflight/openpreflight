# Changelog

All notable changes to this project are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Fixed

- **`on_empty_pipeline` is settable.** It shipped readable by the runner but
  absent from the JSON API and the binding form, so no operator could actually
  choose it. That is the same mistake migration `0004` exists to undo.
- **Cancel and timeout now actually stop a Docker job.** `Docker.Run` killed the
  `docker` CLI and waited on pipes the container still held, so a cancelled
  `runtime:` job reported cancelled on the Check Run and kept building. It also
  kept its concurrency slot and workspace, which stalled the queue on an
  instance running `max_concurrent_jobs: 1`. The container is now removed
  engine-side and the process group killed, matching the process executor.
- **A job requeued after a crash reuses its Check Run.** `RequeueStaleJobs`
  keeps `check_run_id`, and the runner reopens that run instead of creating a
  second one. Previously the first run was left `in_progress` with nothing ever
  completing it, so a required check on that commit never resolved. One
  repository + commit + pipeline is one logical Check Run, across restarts.
- **Fork pull requests now get a completed Check Run.** A fork PR refused by
  policy was answered at the webhook with no job and no check, so a required
  check waited forever with nothing explaining why. It is queued instead and
  concludes `skipped`, with a summary naming the settings an operator can
  change. Unbound repositories still produce nothing.
- **The Docker probe no longer fails a job on a busy host.** `Available` used a
  3-second budget and, like `Run`, ignored it when a child held the output
  pipes. A slow-but-healthy engine could fail a job with "the engine is not
  reachable". The budget is 15s and actually enforced.

### Added

- **A page per repository** at `/repos/{id}`: what the binding is configured to
  do, its last run with duration and the reason it did not pass, and that
  repository's recent runs. `/repos` is a management list and `/repos/{id}/edit`
  is a form; neither answered "what has this repo been doing".
- **Executor and plan source on the run page.** Whether a job ran in the worker
  process or `docker:<image>`, and whether its commands came from `.ci.yml`,
  binding overrides or inference, both existed only in the log body. They are
  recorded on the job now (migration `0007`) and shown.
- **The onboarding checklist tracks the whole arc**, adding *First run* and
  *Passing Check Run* to the four configuration steps, and **retires itself**
  once the arc is complete. A finished install should not still be shown
  instructions; it returns if a step regresses.
- **The Check Run title names the failing step** — `Failed: test (exit 1)`
  rather than `Failed`. The title is the only part visible in a pull request's
  collapsed check list.
- **`on_empty_pipeline`** on a binding: `skip` (the default, unchanged
  behaviour) or `fail`. A pipeline that resolves to no steps is usually a
  configuration mistake, and it used to be indistinguishable from an intentional
  path-filter skip.
- **`skip_reason`** on a job (`path_filter`, `no_pipeline`, `fork_disabled`,
  `fork_no_docker`, `fork_no_runtime`), exposed on the Jobs API. Every kind of
  skip concluded `skipped` with no way to tell which.
- **Path-filter diagnostics.** Changed count, matched count, the filter, and the
  decision now appear in the log on every outcome, and in the Check Run summary
  on a skip. A fail-open says so in words rather than only in the worker log.
- **`max_workspace_bytes`** is back, and enforced this time: measured after
  clone and between steps, failing the job rather than filling the disk. Default
  1 GiB; `0` disables. Migration `0004` dropped the column precisely because
  nothing read it.
- **A dry run.** `GET /repos/{id}/resolve` and
  `POST /api/v1/bindings/{id}/resolve` answer "what would this repository run,
  on this ref?" — checking out a real commit, resolving the plan the worker
  would resolve, evaluating the path filter, and reporting every configuration
  problem at once. It writes no Check Run, no job row and nothing to the queue.
  Until now the only way to find out what a configuration did was to push a
  commit and read the result afterwards.
- **Per-value provenance.** Every resolved value records which of the four
  layers supplied it — the pipeline file in the commit, the binding, settings,
  or a built-in default. Shown on the dry run and on the run page for a finished
  job (migration `0008`), and returned by the jobs API. One `plan_source` string
  could say where the *commands* came from but not that the timeout came from
  settings while the image came from `.ci.yml`.
- **Pre-flight validation.** A bad `timeout:`, a rejected `runtime:` image, an
  unreadable pipeline file and an empty plan are reported together by the dry
  run, before a real commit fails on the first of them.
- **Inference for Go, Rust and Python**, not only Node. `go.mod`, `Cargo.toml`
  and `pyproject.toml`/`requirements.txt` each yield at most the same three
  steps. Node is still checked first, so no repository that works today changes
  plan, and a repository matching two ecosystems gets a warning rather than a
  silent pick. Python's test step is emitted only where something says tests
  exist: `pytest` exits 5 on "no tests collected", which would fail a check for
  a repository that simply has none.
- **The fork runtime fallback names itself.** A fork commit inheriting
  `settings.default_runtime` used to be credited to the pipeline file. It is the
  one resolved value with security consequences, so it now says where it came
  from like every other value.

### Upgrade

Run migrations `0006`, `0007` and `0008` (automatic on boot). No configuration
change is required: `on_empty_pipeline` defaults to today's behaviour and
`max_workspace_bytes` defaults to 1 GiB. If a repository's checkout plus build
legitimately exceeds 1 GiB, raise it in Settings → Runner or set it to `0`.

A dry run clones to the workspace directory and deletes the checkout when it is
done. It does not take a concurrency slot, so it can run alongside a job; on a
disk-constrained host, note that it is one extra checkout for the duration of
the call.

## [2.0.2] - 2026-09-02

Operator UI refresh on the 2.0.0 binary. Image
`ghcr.io/openpreflight/openpreflight:2.0.2`.

### Changed

- Operator chrome is full width (the login/setup column stays 440px). The
  sidebar and wordmark use the website runway-check mark.
- Jobs is a denser table (Status, Repo with ref and SHA stacked, Event, Took,
  When, Actions). Filters sit in a toolbar on the table card.
- Empty lists use a dashed panel, an icon, and one primary action.
- Repos is a list. Pick repositories is `/repos/pick`; add is `/repos/new`.
  Edit is `/repos/{id}/edit` (same form as add). `/repos?edit=` redirects
  to that URL.
- Settings is four pages: Configuration (`/settings`), Runner, Logs, Admin.
  Each form saves only its fields and returns to that page.
- GitHub Apps is a list. Add is `/github-apps/new`; edit is
  `/github-apps/{id}/edit`. `/github-apps?edit=` redirects to the edit page.

## [2.0.0] - 2026-09-02

Job query, live logs, Create with GitHub, and path filters. Image
`ghcr.io/openpreflight/openpreflight:2.0.0`.

### Added

- Binding **paths** (`frontend/**`, comma or newline). Empty is every path. A
  complete file list with no match skips the job (Check Run conclusion
  `skipped`) before clone. A truncated or failed file list fail-opens and runs.
- **Create with GitHub** on the GitHub Apps page uses GitHub's App manifest
  flow (`POST /api/v1/github-apps/manifest/start` and the callback). Paste
  remains under Advanced, including for GitHub Enterprise.
- `GET /api/v1/jobs/{id}/logs/stream` tails an in-flight log over SSE. The run
  page opens EventSource while the job is running; meta-refresh stays for
  no-JS. If a reverse proxy swallows events, disable buffering on this path.
- `GET /jobs` and `GET /api/v1/jobs` accept `repo`, `status`, `limit`, and
  `offset` query parameters. Unknown `status` is 400. The Jobs page has a
  GET form for repo and status.

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

[unreleased]: https://github.com/openpreflight/openpreflight/compare/v2.0.2...HEAD
[2.0.2]: https://github.com/openpreflight/openpreflight/releases/tag/v2.0.2
[2.0.0]: https://github.com/openpreflight/openpreflight/releases/tag/v2.0.0
[1.1.0]: https://github.com/openpreflight/openpreflight/releases/tag/v1.1.0
[1.0.0]: https://github.com/openpreflight/openpreflight/releases/tag/v1.0.0
