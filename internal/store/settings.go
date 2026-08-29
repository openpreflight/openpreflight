// SPDX-License-Identifier: Apache-2.0

package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// defaultSettings seed the settings row on first read, and are the only place
// the default check name comes from — the DEFAULT in migration 0001's DDL is
// never exercised because seedSettings always supplies the column.
//
// Changing this name therefore affects new installs only. That is deliberate:
// GitHub matches a required status check by its name string, so renaming a live
// install's check would leave its branch protection rule permanently
// unsatisfiable ("Expected — waiting for status to be reported"). Existing
// installs keep the name already in their database until an operator changes it
// in the UI. See https://docs.openpreflight.xyz/start/configuration/.
func defaultSettings() Settings {
	return Settings{
		DefaultCheckName:      "openpreflight",
		DefaultPipelineFile:   ".ci.yml",
		DefaultTimeoutSeconds: 900,
		MaxConcurrentJobs:     1,
		MaxLogBytes:           10 << 20,
		LogRetentionDays:      14,
		SkipForkPRs:           true,
		DefaultRuntime:        "",
	}
}

// Settings returns the single settings row, creating it on first read so the
// rest of the code never has to handle "not configured yet".
func (s *Store) Settings() (Settings, error) {
	var (
		out         Settings
		skipForks   int
		scanErr     error
		selectQuery = `SELECT public_base_url, default_check_name, default_pipeline_file,
			default_timeout_seconds, max_concurrent_jobs, max_log_bytes,
			log_retention_days, skip_fork_prs, default_runtime
			FROM settings WHERE id = 1`
	)
	scanErr = s.db.QueryRow(selectQuery).Scan(
		&out.PublicBaseURL, &out.DefaultCheckName, &out.DefaultPipelineFile,
		&out.DefaultTimeoutSeconds, &out.MaxConcurrentJobs, &out.MaxLogBytes,
		&out.LogRetentionDays, &skipForks, &out.DefaultRuntime,
	)
	if errors.Is(scanErr, sql.ErrNoRows) {
		d := defaultSettings()
		if err := s.seedSettings(d); err != nil {
			return Settings{}, err
		}
		return d, nil
	}
	if scanErr != nil {
		return Settings{}, fmt.Errorf("store: settings: %w", scanErr)
	}
	out.SkipForkPRs = skipForks != 0
	return out, nil
}

func (s *Store) seedSettings(v Settings) error {
	ts := formatTime(now())
	_, err := s.db.Exec(`INSERT INTO settings (id, public_base_url, default_check_name,
		default_pipeline_file, default_timeout_seconds, max_concurrent_jobs, max_log_bytes,
		log_retention_days, skip_fork_prs, default_runtime, created_at, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		v.PublicBaseURL, v.DefaultCheckName, v.DefaultPipelineFile, v.DefaultTimeoutSeconds,
		v.MaxConcurrentJobs, v.MaxLogBytes, v.LogRetentionDays,
		boolInt(v.SkipForkPRs), v.DefaultRuntime, ts, ts)
	if err != nil {
		return fmt.Errorf("store: seed settings: %w", err)
	}
	return nil
}

// SaveSettings writes the whole row. Callers read-modify-write so a PATCH of one
// field cannot silently reset the others.
func (s *Store) SaveSettings(v Settings) error {
	if _, err := s.Settings(); err != nil { // ensure the row exists
		return err
	}
	_, err := s.db.Exec(`UPDATE settings SET public_base_url = ?, default_check_name = ?,
		default_pipeline_file = ?, default_timeout_seconds = ?, max_concurrent_jobs = ?,
		max_log_bytes = ?, log_retention_days = ?,
		skip_fork_prs = ?, default_runtime = ?, updated_at = ? WHERE id = 1`,
		v.PublicBaseURL, v.DefaultCheckName, v.DefaultPipelineFile, v.DefaultTimeoutSeconds,
		v.MaxConcurrentJobs, v.MaxLogBytes, v.LogRetentionDays,
		boolInt(v.SkipForkPRs), v.DefaultRuntime, formatTime(now()))
	if err != nil {
		return fmt.Errorf("store: save settings: %w", err)
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
