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
  non-root runtime, `git` and Node in the image.

### Security

- HMAC webhook verification per App slug.
- Fork PRs skipped.
- CSRF on cookie-authenticated writes; Bearer callers skip CSRF.
- Clone credential passed via `GIT_CONFIG_*`, never the remote URL; remote
  stripped before pipeline steps run.
