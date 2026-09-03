// SPDX-License-Identifier: Apache-2.0

package store

import "fmt"

// migrations are applied in order; each is wrapped in a transaction and
// recorded in schema_migrations. Never edit an applied migration — append.
var migrations = []struct {
	name string
	stmt string
}{
	{"0001_init", `
CREATE TABLE settings (
	id                      INTEGER PRIMARY KEY CHECK (id = 1),
	public_base_url         TEXT    NOT NULL DEFAULT '',
	default_check_name      TEXT    NOT NULL DEFAULT 'Coolify CI',
	default_pipeline_file   TEXT    NOT NULL DEFAULT '.ci.yml',
	default_timeout_seconds INTEGER NOT NULL DEFAULT 900,
	max_concurrent_jobs     INTEGER NOT NULL DEFAULT 1,
	max_log_bytes           INTEGER NOT NULL DEFAULT 10485760,
	max_workspace_bytes     INTEGER NOT NULL DEFAULT 1073741824,
	log_retention_days      INTEGER NOT NULL DEFAULT 14,
	skip_fork_prs           INTEGER NOT NULL DEFAULT 1,
	created_at              TEXT    NOT NULL,
	updated_at              TEXT    NOT NULL
);

CREATE TABLE users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT NOT NULL UNIQUE,
	password_hash TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL
);

CREATE TABLE sessions (
	token      TEXT PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TEXT NOT NULL,
	expires_at TEXT NOT NULL
);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

CREATE TABLE coolify_instances (
	id                   INTEGER PRIMARY KEY AUTOINCREMENT,
	name                 TEXT NOT NULL,
	base_url             TEXT NOT NULL,
	api_token_enc        TEXT NOT NULL,
	team_id              TEXT NOT NULL DEFAULT '',
	team_name            TEXT NOT NULL DEFAULT '',
	default_server_uuid  TEXT NOT NULL DEFAULT '',
	default_project_uuid TEXT NOT NULL DEFAULT '',
	last_seen_at         TEXT,
	last_error           TEXT NOT NULL DEFAULT '',
	created_at           TEXT NOT NULL,
	updated_at           TEXT NOT NULL
);

CREATE TABLE github_apps (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	name               TEXT NOT NULL,
	slug               TEXT NOT NULL UNIQUE,
	app_id             INTEGER NOT NULL,
	pem_enc            TEXT NOT NULL,
	webhook_secret_enc TEXT NOT NULL,
	api_url            TEXT NOT NULL DEFAULT 'https://api.github.com',
	check_name         TEXT NOT NULL DEFAULT '',
	last_seen_at       TEXT,
	last_error         TEXT NOT NULL DEFAULT '',
	created_at         TEXT NOT NULL,
	updated_at         TEXT NOT NULL
);

CREATE TABLE repo_bindings (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	github_app_id       INTEGER NOT NULL REFERENCES github_apps(id) ON DELETE CASCADE,
	coolify_instance_id INTEGER REFERENCES coolify_instances(id) ON DELETE SET NULL,
	repo                TEXT    NOT NULL,
	enabled             INTEGER NOT NULL DEFAULT 1,
	branches            TEXT    NOT NULL DEFAULT '',
	check_name          TEXT    NOT NULL DEFAULT '',
	pipeline_file       TEXT    NOT NULL DEFAULT '',
	timeout_seconds     INTEGER NOT NULL DEFAULT 0,
	install_cmd         TEXT    NOT NULL DEFAULT '',
	test_cmd            TEXT    NOT NULL DEFAULT '',
	build_cmd           TEXT    NOT NULL DEFAULT '',
	shareable_logs      INTEGER NOT NULL DEFAULT 0,
	created_at          TEXT    NOT NULL,
	updated_at          TEXT    NOT NULL,
	UNIQUE (github_app_id, repo)
);
CREATE INDEX idx_bindings_repo ON repo_bindings(repo);

CREATE TABLE deliveries (
	delivery_id   TEXT PRIMARY KEY,
	github_app_id INTEGER NOT NULL,
	event         TEXT NOT NULL,
	action        TEXT NOT NULL DEFAULT '',
	job_id        TEXT,
	received_at   TEXT NOT NULL
);

CREATE TABLE jobs (
	id              TEXT PRIMARY KEY,
	binding_id      INTEGER REFERENCES repo_bindings(id) ON DELETE SET NULL,
	github_app_id   INTEGER NOT NULL,
	repo            TEXT    NOT NULL,
	sha             TEXT    NOT NULL,
	ref             TEXT    NOT NULL DEFAULT '',
	event           TEXT    NOT NULL DEFAULT '',
	delivery_id     TEXT    NOT NULL DEFAULT '',
	installation_id INTEGER NOT NULL DEFAULT 0,
	check_run_id    INTEGER NOT NULL DEFAULT 0,
	check_name      TEXT    NOT NULL DEFAULT '',
	status          TEXT    NOT NULL DEFAULT 'queued',
	conclusion      TEXT    NOT NULL DEFAULT '',
	steps_json      TEXT    NOT NULL DEFAULT '[]',
	error           TEXT    NOT NULL DEFAULT '',
	log_bytes       INTEGER NOT NULL DEFAULT 0,
	shareable_logs  INTEGER NOT NULL DEFAULT 0,
	created_at      TEXT    NOT NULL,
	started_at      TEXT,
	finished_at     TEXT
);
CREATE INDEX idx_jobs_created ON jobs(created_at DESC);
CREATE INDEX idx_jobs_status ON jobs(status);
CREATE INDEX idx_jobs_delivery ON jobs(delivery_id);
CREATE INDEX idx_jobs_repo_ref ON jobs(repo, ref);
`},
	{"0002_wave4", `
ALTER TABLE settings ADD COLUMN default_runtime TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN is_fork INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN pull_number INTEGER NOT NULL DEFAULT 0;
`},
	// The index is deliberately NOT unique. CancelJob returns as soon as it has
	// cancelled the context; the 'cancelled' status is written later by the
	// job's own goroutine, so a partial unique index on the in-flight statuses
	// would intermittently reject the rerun insert. The one-live-run-per-commit
	// invariant is an application guard (ADR 005); this index only serves its
	// lookup.
	{"0003_check_suite", `
ALTER TABLE jobs ADD COLUMN check_suite_id INTEGER NOT NULL DEFAULT 0;
CREATE INDEX idx_jobs_app_repo_sha ON jobs(github_app_id, repo, sha, status);
`},
	// max_workspace_bytes was accepted, stored and editable, and nothing ever
	// read it: no code path measured a checkout. Keeping a setting that does
	// nothing is worse than not offering it, so the column goes rather than
	// growing an enforcement path nobody asked for.
	{"0004_drop_max_workspace_bytes", `
ALTER TABLE settings DROP COLUMN max_workspace_bytes;
`},
	{"0005_binding_paths", `
ALTER TABLE repo_bindings ADD COLUMN paths TEXT NOT NULL DEFAULT '';
`},
	// skip_reason separates the kinds of skip that used to be indistinguishable.
	// A path-filter miss is intentional; an empty pipeline is a configuration
	// problem; a fork PR was refused by policy. All three concluded `skipped`
	// with no way to tell which, and the fork case did not even produce a
	// Check Run. See queue.Runner.runJob.
	//
	// max_workspace_bytes comes back after 0004 dropped it, and this time it is
	// enforced: workspace.Usage is measured after clone and between steps. The
	// column is only worth having with that enforcement, which is exactly what
	// 0004's comment said.
	{"0006_skip_reason_and_workspace_cap", `
ALTER TABLE jobs ADD COLUMN skip_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE repo_bindings ADD COLUMN on_empty_pipeline TEXT NOT NULL DEFAULT '';
ALTER TABLE settings ADD COLUMN max_workspace_bytes INTEGER NOT NULL DEFAULT 1073741824;
`},
	// The runner resolves the executor and where the commands came from after
	// the clone, printed both to the log, and threw them away. They are the two
	// questions an operator asks about a run they did not watch — "what ran
	// this?" and "where did these commands come from?" — so they are recorded
	// the same way check_run_id and check_name already are.
	{"0007_job_plan", `
ALTER TABLE jobs ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN plan_source TEXT NOT NULL DEFAULT '';
`},
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: migrations table: %w", err)
	}
	for _, m := range migrations {
		var seen int
		if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations WHERE name = ?`, m.name).Scan(&seen); err != nil {
			return fmt.Errorf("store: check migration %s: %w", m.name, err)
		}
		if seen > 0 {
			continue
		}
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("store: begin %s: %w", m.name, err)
		}
		if _, err := tx.Exec(m.stmt); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: apply %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`, m.name, now().Format(timeFmt)); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: record %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: commit %s: %w", m.name, err)
		}
	}
	return nil
}
