# ADR 004: Docker executor, remote engine, opt-in forks

- Status: accepted
- Date: 2026-08-24

## Context

v1 ran every pipeline step as a child of this process. `.ci.yml` `runtime:`
was parsed and ignored. Fork PRs were always skipped because they would
execute untrusted code with the worker's privileges.

Coolify's API can list servers and create applications. It cannot start an
arbitrary `docker run` on a host: there is no "run this container on server
UUID" job API. The remote-host path is whatever Docker already understands
(`DOCKER_HOST` / a mounted socket).

## Decision

- A non-empty pipeline `runtime` is a Docker image. The runner calls
  `docker run --rm` with the checkout at `/work`, the worker uid, `--security-opt
  no-new-privileges`, `--cap-drop ALL`. The job container never receives
  `docker.sock`. Image names are allow-listed so `runtime` cannot become extra
  CLI flags.
- Empty `runtime` keeps the process executor (Node/git in this image).
- If `runtime` is set (or the job is a fork) and the engine is unreachable,
  the job **fails**. There is no silent fallback to the process executor.
- `CI_DOCKER_HOST` (else `DOCKER_HOST`) is how jobs run on another Docker
  engine, including a Coolify server's. Installing *this* worker onto Coolify
  is a separate call: `POST /api/v1/applications/dockercompose` with
  `instant_deploy: false`.
- `skip_fork_prs` stays true by default. Saving `false` requires a reachable
  engine plus `settings.default_runtime` (the webhook has no pipeline file
  yet). Fork jobs always use Docker.

## Consequences

- The worker image ships `docker-cli`. Compose mounts `/var/run/docker.sock`
  and adds the socket's group (`DOCKER_GID`) so uid 10001 can talk to the
  engine. Job containers still do not see that socket.
- Operators who never set `runtime:` and never enable fork PRs still run as
  before, as a process.
- A Coolify API token cannot replace `CI_DOCKER_HOST`. Remote jobs are Docker
  remote API, not Coolify remote API.
- Fork isolation is "a sibling container with dropped caps", not a sandbox
  VM. That is the trade-off for staying one process plus `docker run`.
