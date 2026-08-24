# ADR 003: Our GitHub App, not Coolify's GitHub connector

- Status: accepted
- Date: 2026-08-24

## Context

Coolify already talks to GitHub for deploys. It is tempting to reuse that
connector for commit checks so operators do not register another App.

GitHub only lets a **GitHub App** create Check Runs. User and OAuth tokens
are refused. A GitHub App has exactly one webhook URL. Coolify's connector
webhook belongs to Coolify's deploy pipeline, and its manifest has no
`checks` permission.

## Decision

Operators register a GitHub App they own, with `Checks: Read and write`,
`Contents: Read-only`, `Metadata: Read-only`, subscribed to **Check suite**
and **Check run**. This process is that App's webhook (`/webhook/{slug}`).

Coolify is optional inventory: a team-scoped API token so the UI can list
servers and offer a repo picker. Coolify is not the Check Run author and
must not have its GitHub webhook pointed here.

## Consequences

- Setup is longer (create an App, paste PEM + webhook secret). That is the
  cost of writing the same commit checks a PR already shows.
- Repointing Coolify's GitHub connector at this service would steal deploys.
  Docs warn against it.
- Clone credentials are installation tokens from *our* App, passed to git
  via `GIT_CONFIG_*` as Basic `x-access-token`, then the remote is stripped
  before any pipeline step runs.
