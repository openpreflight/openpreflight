# Deployment

The image is a static Go binary plus `git` and Node. It runs as uid 10001.
`tini` is the entrypoint so pipeline shells get reaped.

```bash
export CI_SECRET_KEY="$(openssl rand -base64 48)"   # required, keep it forever
export CI_PUBLIC_BASE_URL="https://ci.example.com"  # optional seed
docker compose up --build
```

Compose maps `8080:8080` and two volumes:

| Volume | Mount | Must persist |
|---|---|---|
| `ci-data` | `/data` | yes — `ci.db`, encrypted secrets, job history, logs |
| `ci-workspace` | `/workspace` | no, but a volume keeps checkouts off the container's writable layer |

## Coolify (or any reverse proxy)

- Give the service a public HTTPS URL. GitHub must reach `POST /webhook/{slug}`.
- Point the domain at this container's port 8080.
- Set `CI_SECRET_KEY` as a secret / env var on the application, not in git.
- Optionally set `CI_PUBLIC_BASE_URL` to the public origin for first boot, and
  `CI_BOOTSTRAP_ADMIN_PASSWORD` if you will configure over the API instead of
  the wizard.
- Honour `X-Forwarded-Proto`: session and CSRF cookies set `Secure` when that
  header is `https` (Coolify's Traefik does this).
- Health check: `GET /health` (the image already defines one). A 503 means the
  process cannot read SQLite.

Do **not** point Coolify's own GitHub connector webhook at this service. An App
has one webhook URL; repointing it steals Coolify's deploys. See
[ADR 003](adr/003-github-app.md).

## After first boot

1. Complete setup (admin password + public base URL) if you did not bootstrap.
2. Register a GitHub App (permissions in the README) and paste it under
   **GitHub Apps**.
3. Optionally add a Coolify instance (team token) as a repo-picker source.
4. Enable bindings. Only enabled private repos you trust: a pipeline runs the
   repo's own commands on this host.

## Backups

Copy `/data` (or the `ci-data` volume) and keep `CI_SECRET_KEY` with it. The
database without the key is not enough to recover PEMs and tokens. The key
without the database is not enough to recover configuration.

Redeploys interrupt in-flight jobs; on start the runner requeues them.
