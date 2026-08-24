# coolify-github-ci

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
Run. See [ADR 005](docs/adr/005-check-suite-gating.md) for what that borrows,
what it rejects, and where the ceiling is.

## Documentation

- [Architecture](docs/architecture.md) and [ADRs](docs/adr/)
- [Configuration](docs/configuration.md)
- [Development](docs/development.md) · [Contributing](CONTRIBUTING.md)
- [Deployment](docs/deployment.md)
- [Security](SECURITY.md) · [Changelog](CHANGELOG.md)

```text
Coolify CI
────────────────────
✓ install    8s
✓ test      21s
✓ build     13s

Passed in 42s

View full logs →
```

## Why a GitHub App

Only a GitHub App can create Check Runs — user and OAuth tokens are refused. A
GitHub App also has exactly one webhook URL, which is why **Coolify's own GitHub
connector cannot do this job**: its webhook belongs to Coolify's deploy pipeline
and its manifest has no `checks` permission. Coolify is used here for server
inventory and as a repository picker; the checks come from an App you register.

## Requirements

- A GitHub App you own (permissions and events below)
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

Then open the UI, complete the wizard, and follow the setup order below.

### Environment

| Variable | Required | Purpose |
|---|---|---|
| `CI_SECRET_KEY` | yes | AES-256-GCM key material for every secret column. The process refuses to start without it. |
| `CI_SECRET_KEY_OLD` | no | Previous key. On boot, secret columns are re-sealed under `CI_SECRET_KEY`; then unset this and restart. |
| `CI_PUBLIC_BASE_URL` | no | Seeds the public base URL on first boot only; the UI owns it afterwards. |
| `CI_BOOTSTRAP_ADMIN_PASSWORD` | no | Creates the `admin` user on first boot so a headless deploy can be driven over the API. Ignored once a user exists. |
| `CI_DOCKER_HOST` | no | Docker engine for `runtime:` and fork PRs. Falls back to `DOCKER_HOST`, then the default socket. |
| `LISTEN_ADDR` | no | Default `:8080`. |
| `DATA_DIR` | no | Default `/data` — holds `ci.db` and `logs/`. Must be a persistent volume. |
| `WORKSPACE_DIR` | no | Default `/workspace` — per-job checkouts, disposable. |

There is deliberately no `GITHUB_APP_ID` or `CI_ALLOWED_REPOS`: that
configuration lives in the database, edited in the UI.

## Setting up

### 1. First boot

The first request with no admin user lands on the setup wizard: admin password
plus the public base URL. Both are needed before GitHub can reach you.

### 2. Register a GitHub App

GitHub → Settings → Developer settings → **GitHub Apps** → New GitHub App.

| Setting | Value |
|---|---|
| Webhook URL | `{public base URL}/webhook/{slug}` — the slug you choose when adding the App here |
| Webhook secret | any strong random string; use a **different one per App** |
| Repository permissions | `Checks: Read and write`, `Contents: Read-only`, `Metadata: Read-only` |
| Subscribe to events | **Check suite**, **Check run** |

Generate a private key, install the App on the account or org that owns the
repos, then add it under **GitHub Apps** here: name, slug, App ID, webhook
secret, PEM. **Test** mints an App JWT and lists installations.

The PEM and the webhook secret are encrypted at rest and never shown again — the
API returns a redacted marker, not the value.

### 3. Optional: add a Coolify instance

One row is one **(base URL, API token)** pair. A Coolify API token is scoped to a
single team, so a row covers that team, not the whole host; a second team on the
same host is a second row.

Get a token from Coolify's **Security → API Tokens** (older versions: **Keys &
Tokens**). Read-only is enough for inventory. Creating this worker as a Coolify
application needs a token that can write applications. **Test** calls
`/api/v1/teams/current` and `/api/v1/servers` and labels the row with the team
it can see. **Inspect** can also create the compose application
(`instant_deploy: false`) so you can set `CI_SECRET_KEY` before the first start.

Do **not** point Coolify's GitHub connector webhook at this service: an App has
one webhook URL, and repointing it would steal Coolify's deploys.

### 4. Enable repos

**Repos** → pick the CI App, optionally pick a Coolify instance as the source of
the repo list, then check the repositories to run checks for. Unchecking a repo
removes its binding.

The bindings table **is** the allow-list. A webhook for a repo with no enabled
binding is acknowledged and dropped, however valid its signature. Only enable
private repos you trust: a pipeline runs the repo's own commands, either in
this process or in a `docker run` container when `runtime:` is set.

Per binding you can override the branch list, the check name, the pipeline file,
the timeout, the install/test/build commands, and whether logs are shareable.

## Pipelines

Commit a pipeline file (default `.ci.yml`) to the repo. A copy lives in
[`examples/.ci.yml`](examples/.ci.yml):

```yaml
runtime: node:24
install: npm ci
test: npm test
build: npm run build
timeout: 15m
```

`runtime` is a Docker image. Omit it (or leave it empty) to run steps in this
worker process. A non-empty value uses `docker run --rm` against
`CI_DOCKER_HOST` (else `DOCKER_HOST`, else the default socket). Fork jobs always
use Docker: they take `runtime` from the pipeline file, or `default_runtime`
from settings. If the image is set but the engine is unreachable, the job fails
instead of falling back to the process executor.

Resolution order, highest first:

1. the repo's pipeline file
2. the binding's command overrides
3. Node defaults inferred from `package.json` — `npm ci` / `pnpm` / `yarn` by
   lockfile, then `test` and `build` **only if those scripts exist**
4. nothing to run → the check is reported as **skipped**, not failed

A step that fails stops the run; later steps are reported as skipped.

## Logs

Full logs are written to `/data/logs/<job-id>.log`, capped by `max_log_bytes`
(10 MiB by default) and pruned after `log_retention_days` (14 by default). The
Check Run carries a truncated tail; the full log is on the details page.

`GET /runs/{job-id}` is the `details_url` GitHub links to. **GitHub never fetches
it — the reader's browser does**, so it requires a session by default. A binding
can opt into shareable logs, which makes that one job's page readable by anyone
holding the link. The same rule applies to `GET /api/v1/jobs/{id}/logs`. Job ids
are random UUIDs, but treat such a link as a secret.

## API

Everything except `/health`, `/webhook/{slug}` and a shareable `/runs/{id}` needs
a session cookie (UI) or `Authorization: Bearer <token>` (CLI). Get a token by
posting JSON to `/api/v1/login`.

```text
POST   /api/v1/setup                      first-run wizard
POST   /api/v1/login                      → { token }
POST   /api/v1/logout
GET    /api/v1/settings
PATCH  /api/v1/settings
POST   /api/v1/password

GET    /api/v1/coolify
POST   /api/v1/coolify                    { name, base_url, api_token }
PATCH  /api/v1/coolify/{id}
POST   /api/v1/coolify/{id}/test          teams/current + servers
GET    /api/v1/coolify/{id}/servers
GET    /api/v1/coolify/{id}/github-apps   Coolify's deploy connectors
GET    /api/v1/coolify/{id}/repos         connector repositories (picker source)
POST   /api/v1/coolify/{id}/install-worker  compose app (`instant_deploy: false`)
DELETE /api/v1/coolify/{id}

GET    /api/v1/github-apps
POST   /api/v1/github-apps                { name, slug, app_id, pem, webhook_secret }
PATCH  /api/v1/github-apps/{id}
POST   /api/v1/github-apps/{id}/test      App JWT + installations
GET    /api/v1/github-apps/{id}/repos     installations + repositories
DELETE /api/v1/github-apps/{id}

GET    /api/v1/bindings
PUT    /api/v1/bindings                   upsert one repo binding
POST   /api/v1/bindings/bulk              picker checkboxes
POST   /api/v1/bindings/{id}/toggle
DELETE /api/v1/bindings/{id}

GET    /api/v1/jobs
GET    /api/v1/jobs/{id}
GET    /api/v1/jobs/{id}/logs             full log (session, or shareable opt-in)
POST   /api/v1/jobs/{id}/rerun            new job, new Check Run
POST   /api/v1/jobs/{id}/cancel

POST   /webhook/{slug}                    GitHub (public, HMAC verified)
GET    /runs/{id}                         log page (session, or shareable opt-in)
GET    /health
```

## How a run happens

```text
GitHub → POST /webhook/{slug}          must answer 2xx within 10s
  verify HMAC with that App's secret
  skip fork PRs by default (null head_branch, or a PR head repo ≠ the base repo);
    opt-in requires Docker plus settings.default_runtime; fork jobs always Docker
  require an enabled binding and an allowed branch
  drop it if a job for this X-GitHub-Delivery is still queued or running
  cancel any in-flight job for the same repo+ref on an older commit
  enqueue, return 202
        ↓
worker: mint an installation token from the payload's installation id
        create the Check Run (binding → App → global name)
        fetch exactly that commit (fork PRs fall back to refs/pull/N/head),
        detach onto it, strip the remote
        run the pipeline under a timeout, in a per-job workspace:
          runtime: set → docker run --rm (no docker.sock in the job container)
          else this process, as non-root
        complete the Check Run, keep the full log locally
```

GitHub's **Redeliver** button reuses the delivery id, so dedup applies only while
a job is in flight; redelivering a finished delivery starts a new job, which is
what makes it useful for debugging. `check_run.rerequested` is honoured only for
this App's own checks.

## Security notes

- Secret columns (App PEM, webhook secret, Coolify token) are AES-256-GCM
  encrypted. GET responses return a redacted marker, never the value.
- The clone credential is passed to git through `GIT_CONFIG_*` environment
  variables as **Basic `x-access-token`** — GitHub's git endpoint wants Basic,
  not the REST API's Bearer. It never enters the remote URL, `.git/config`, or a
  command line, and the remote is removed before any pipeline step runs.
- Job environments are built from scratch: no `CI_SECRET_KEY`, no PEMs, no
  webhook secrets, no Coolify tokens, no installation token.
- Fork pull requests are skipped by default. Turning that off requires a
  reachable Docker engine and `default_runtime`; fork jobs always run in
  Docker, never as a process on this host.
- Session cookies are HttpOnly, `Secure` behind HTTPS, and browser writes require
  a CSRF token. Bearer callers carry no ambient cookie and so skip CSRF.

## Not in v1

GitHub Actions YAML, `actions/runner`, creating GitHub Apps for you, matrices,
caches, and artifacts. Jobs on another machine use `CI_DOCKER_HOST` /
`DOCKER_HOST` (a Docker engine), not Coolify's API as a job runner.

## Development

See [docs/development.md](docs/development.md) for CSS rebuilds and layout rules.

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

MIT. See [LICENSE](LICENSE).
