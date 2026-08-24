# Architecture

One Go process is both the **configurator** (HTML UI + JSON API) and the
**worker** (webhook → queue → Check Run). There is no separate frontend, no
message broker, and no `pkg/` library.

```text
GitHub ──POST /webhook/{slug}──► api ──enqueue──► queue.Runner
                                      │                │
Browser / CLI ──session/Bearer──► api │                ├── githubapp (JWT, installation token, checks)
                                      │                ├── workspace (fetch SHA, detach, strip remote)
                                      └── store        ├── pipeline (.ci.yml + binding + package.json)
                                            │          └── executor (process, or docker run)
                                            └── secret (AES-256-GCM for PEM / webhook / Coolify token)
```

## Packages

| Package | Role |
|---|---|
| `cmd/server` | Load env, open SQLite, rotate secrets if asked, bootstrap admin, serve HTTP, start the runner |
| `internal/api` | Routes, session/CSRF, HTML and JSON from the same handlers |
| `internal/web` | Server-rendered templates; Tailwind compiled into `static/app.css` |
| `internal/store` | SQLite (`modernc.org/sqlite`), migrations, bindings, jobs |
| `internal/secret` | Seal/open secret columns |
| `internal/config` | Process env only: listen addr, dirs, `CI_SECRET_KEY`, `CI_DOCKER_HOST` |
| `internal/queue` | Claim queued jobs, concurrency cap, prune logs |
| `internal/githubapp` | App JWT, installation tokens, Check Runs |
| `internal/coolify` | Team-scoped Coolify API (inventory, repo picker, install-worker) |
| `internal/webhook` | HMAC and payload shape |
| `internal/pipeline` | Resolve install/test/build and `runtime` |
| `internal/workspace` | Per-job checkout |
| `internal/executor` | Run a step as a process or `docker run` |
| `internal/logs` | Per-job log files under `DATA_DIR/logs` |

Everything a GitHub App or Coolify host needs to know lives in the database,
edited in the UI. Env vars are only what the process needs before it can open
that database. See [configuration.md](configuration.md).

## How a run happens

1. `POST /webhook/{slug}` must return 2xx within GitHub's ~10s window.
2. HMAC is verified with that App's webhook secret.
3. Fork PRs are dropped unless `skip_fork_prs` is off, Docker is reachable, and
   `default_runtime` is set. A repo with no enabled binding is acknowledged and
   dropped. Delivery ids still in-flight are deduped.
4. One live run per commit is enforced: a `requested` delivery for a
   `(app, repo, sha)` already in flight is answered `already queued`, while a
   `rerequested` one cancels that run first. An older in-flight job for the same
   repo+ref on a *different* SHA is then cancelled, the new SHA is enqueued, and
   the handler returns 202.
5. The runner mints an installation token from the payload's installation id,
   creates the Check Run, fetches that commit (fork PRs fall back to
   `refs/pull/N/head`), detaches, strips the remote, runs the pipeline under a
   timeout — `docker run` when `runtime` is set or the job is a fork, otherwise
   a local process — completes the Check Run, keeps the full log locally.

`GET /runs/{id}` is the Check Run `details_url`. GitHub never fetches it — the
reader's browser does — so it needs a session unless the binding opted into
shareable logs.

## Why this shape

- [ADR 001](adr/001-database.md) — SQLite in-process, secrets encrypted at rest.
- [ADR 002](adr/002-authentication.md) — local admin + opaque sessions, not GitHub OAuth.
- [ADR 003](adr/003-github-app.md) — our GitHub App, not Coolify's GitHub connector.
- [ADR 004](adr/004-docker-executor.md) — `runtime:` is `docker run`; fork PRs stay off until that works.
- [ADR 005](adr/005-check-suite-gating.md) — `check_suite`/`check_run` only, one Check Run per job, one live run per commit.

### Prior art

The trigger model is Zuul's, at a single server's scale: gate on the commit,
queue against an immutable SHA, attach logs to the run, write the result back to
the forge. Zuul's architecture — ZooKeeper, Nodepool, Ansible, a scheduler apart
from its executors — is explicitly not adopted. [ADR 005](adr/005-check-suite-gating.md)
records what is borrowed, what is rejected, and where the ceiling is.
