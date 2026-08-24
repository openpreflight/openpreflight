# Configuration

Two layers. The process reads a handful of env vars so it can start. Everything
else is a row in SQLite, edited in the UI or over the JSON API.

## Environment

| Variable | Required | Purpose |
|---|---|---|
| `CI_SECRET_KEY` | yes | AES-256-GCM key material (32+ bytes). Losing it makes stored PEMs and tokens unreadable. |
| `CI_SECRET_KEY_OLD` | no | Previous key. On boot, secret columns are re-sealed under `CI_SECRET_KEY`. Unset it and restart after a successful rotate. |
| `CI_PUBLIC_BASE_URL` | no | Seeds the public base URL on first boot only; the UI owns it afterwards. |
| `CI_BOOTSTRAP_ADMIN_PASSWORD` | no | Creates the `admin` user on first boot so a headless deploy can be driven over the API. Ignored once a user exists. |
| `CI_DOCKER_HOST` | no | Docker engine for `runtime:` and fork PRs. Falls back to `DOCKER_HOST`, then the engine default (typically the mounted socket). |
| `LISTEN_ADDR` | no | Default `:8080`. |
| `DATA_DIR` | no | Default `/data` — `ci.db` and `logs/`. Must be a persistent volume. |
| `WORKSPACE_DIR` | no | Default `/workspace` — per-job checkouts, disposable. |

There is no `GITHUB_APP_ID` and no `CI_ALLOWED_REPOS`. Those live in
`github_apps` and `repo_bindings`.

Generate a key with:

```bash
openssl rand -base64 48
```

To rotate: set `CI_SECRET_KEY` to the **new** key and `CI_SECRET_KEY_OLD` to
the previous one, start once, confirm the log line `re-sealed secret columns`,
then unset `CI_SECRET_KEY_OLD` and restart. A row that opens with neither key
fails startup.

## Settings (database)

Single row, `id = 1`. Changed from **Settings** in the UI or
`PATCH /api/v1/settings`.

| Field | Default | Purpose |
|---|---|---|
| `public_base_url` | empty (or seeded from env) | Webhook URLs and Check Run `details_url` |
| `default_check_name` | `Coolify CI` | Check Run name unless the App or binding overrides |
| `default_pipeline_file` | `.ci.yml` | Path in the repo |
| `default_timeout_seconds` | `900` | Per-job timeout |
| `max_concurrent_jobs` | `1` | Runner concurrency |
| `max_log_bytes` | 10 MiB | Cap on the on-disk log |
| `max_workspace_bytes` | 1 GiB | Checkout size cap |
| `log_retention_days` | `14` | Prune old logs and job rows |
| `default_runtime` | empty | Docker image used when a fork job's pipeline has no `runtime:` |
| `skip_fork_prs` | `true` | Fork PRs are ignored. Saving `false` requires Docker plus `default_runtime`. |

## Binding overrides

Per repo, highest first at run time: **binding → App → settings**.

A binding can override branches, check name, pipeline file, timeout,
install/test/build commands, and whether logs are shareable. The bindings
table **is** the allow-list: a signed webhook for a repo with no enabled
binding is dropped.

## Pipeline file

Committed to the repo (default `.ci.yml`):

```yaml
runtime: node:24
install: npm ci
test: npm test
build: npm run build
timeout: 15m
```

`runtime` is a Docker image. Empty means the worker process. A non-empty value
(or a fork job) uses `docker run`; a missing engine fails the job. Image names
are allow-listed (no shell metacharacters, no leading `-`). A file that only
sets `runtime:` / `timeout:` still applies those while commands come from the
binding or `package.json`.

Resolution order, highest first:

1. the repo's pipeline file
2. the binding's command overrides
3. Node defaults inferred from `package.json` — `npm ci` / `pnpm` / `yarn` by
   lockfile, then `test` and `build` **only if those scripts exist**
4. nothing to run → the check is reported as **skipped**, not failed
