package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const jobCols = `id, COALESCE(binding_id, 0), github_app_id, repo, sha, ref, event, delivery_id,
	installation_id, check_run_id, check_name, status, conclusion, steps_json, error,
	log_bytes, shareable_logs, created_at, started_at, finished_at`

func scanJob(sc interface{ Scan(...any) error }) (Job, error) {
	var (
		j                 Job
		shared            int
		created           string
		started, finished sql.NullString
	)
	if err := sc.Scan(&j.ID, &j.BindingID, &j.GitHubAppID, &j.Repo, &j.SHA, &j.Ref, &j.Event,
		&j.DeliveryID, &j.InstallationID, &j.CheckRunID, &j.CheckName, &j.Status, &j.Conclusion,
		&j.StepsJSON, &j.Error, &j.LogBytes, &shared, &created, &started, &finished); err != nil {
		return Job{}, err
	}
	j.ShareableLogs = shared != 0
	j.CreatedAt = parseTime(created)
	j.StartedAt = parseTimePtr(started)
	j.FinishedAt = parseTimePtr(finished)
	return j, nil
}

// NewJobID returns a random UUIDv4 string. Job ids are never sequential: they
// are the unguessable part of a shareable /runs/{id} link.
func NewJobID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; a panic here beats issuing a
		// predictable id that ends up in a shareable log URL.
		panic("store: crypto/rand failed: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// JobInput is what the webhook handler knows at enqueue time.
type JobInput struct {
	BindingID      int64
	GitHubAppID    int64
	Repo           string
	SHA            string
	Ref            string
	Event          string
	DeliveryID     string
	InstallationID int64
	CheckName      string
	ShareableLogs  bool
}

// EnqueueJob inserts a queued job and returns it.
func (s *Store) EnqueueJob(in JobInput) (Job, error) {
	if in.Repo == "" || in.SHA == "" {
		return Job{}, errors.New("store: job needs repo and sha")
	}
	id := NewJobID()
	_, err := s.db.Exec(`INSERT INTO jobs (id, binding_id, github_app_id, repo, sha, ref, event,
		delivery_id, installation_id, check_name, status, shareable_logs, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, nullInt64(in.BindingID), in.GitHubAppID, in.Repo, in.SHA, in.Ref, in.Event,
		in.DeliveryID, in.InstallationID, in.CheckName, JobQueued, boolInt(in.ShareableLogs),
		formatTime(now()))
	if err != nil {
		return Job{}, fmt.Errorf("store: enqueue job: %w", err)
	}
	if in.DeliveryID != "" {
		// Remember which job a delivery produced so a Redeliver while that job
		// is still running is a no-op, and after it finishes is a new job.
		if _, err := s.db.Exec(`INSERT INTO deliveries (delivery_id, github_app_id, event, action, job_id, received_at)
			VALUES (?, ?, ?, '', ?, ?)
			ON CONFLICT (delivery_id) DO UPDATE SET job_id = excluded.job_id, received_at = excluded.received_at`,
			in.DeliveryID, in.GitHubAppID, in.Event, id, formatTime(now())); err != nil {
			return Job{}, fmt.Errorf("store: record delivery: %w", err)
		}
	}
	return s.Job(id)
}

// Job loads one job.
func (s *Store) Job(id string) (Job, error) {
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: job %s: %w", id, err)
	}
	return j, nil
}

// ListJobs returns the most recent jobs first.
func (s *Store) ListJobs(limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs ORDER BY created_at DESC, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list jobs: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ClaimNextJob marks the oldest queued job in_progress and returns it. The
// single-writer connection makes the select-then-update atomic enough.
func (s *Store) ClaimNextJob() (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, fmt.Errorf("store: claim begin: %w", err)
	}
	defer tx.Rollback()
	j, err := scanJob(tx.QueryRow(`SELECT ` + jobCols + ` FROM jobs WHERE status = '` + JobQueued +
		`' ORDER BY created_at LIMIT 1`))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: claim select: %w", err)
	}
	started := now()
	if _, err := tx.Exec(`UPDATE jobs SET status = ?, started_at = ? WHERE id = ?`,
		JobInProgress, formatTime(started), j.ID); err != nil {
		return Job{}, fmt.Errorf("store: claim update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Job{}, fmt.Errorf("store: claim commit: %w", err)
	}
	j.Status = JobInProgress
	j.StartedAt = &started
	return j, nil
}

// InFlightJobForDelivery finds a queued or running job for a delivery id. A
// completed or missing one means GitHub's Redeliver should start a new job.
func (s *Store) InFlightJobForDelivery(deliveryID string) (Job, error) {
	if deliveryID == "" {
		return Job{}, ErrNotFound
	}
	j, err := scanJob(s.db.QueryRow(`SELECT `+jobCols+` FROM jobs
		WHERE delivery_id = ? AND status IN (?, ?) ORDER BY created_at DESC LIMIT 1`,
		deliveryID, JobQueued, JobInProgress))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: delivery lookup: %w", err)
	}
	return j, nil
}

// InFlightJobsForRef lists queued/running jobs for a repo+ref so a newer push
// can cancel them.
func (s *Store) InFlightJobsForRef(repo, ref string) ([]Job, error) {
	rows, err := s.db.Query(`SELECT `+jobCols+` FROM jobs
		WHERE repo = ? AND ref = ? AND status IN (?, ?)`, repo, ref, JobQueued, JobInProgress)
	if err != nil {
		return nil, fmt.Errorf("store: in-flight for ref: %w", err)
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan job: %w", err)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// SetJobCheckRun records the Check Run id we created for this job.
func (s *Store) SetJobCheckRun(id string, checkRunID int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET check_run_id = ? WHERE id = ?`, checkRunID, id)
	return err
}

// SetJobCheckName writes the name that was actually sent to GitHub, including
// the App/global fallback when the binding left it blank.
func (s *Store) SetJobCheckName(id, name string) error {
	_, err := s.db.Exec(`UPDATE jobs SET check_name = ? WHERE id = ?`, name, id)
	return err
}

// SetJobLogBytes records how much log the job wrote.
func (s *Store) SetJobLogBytes(id string, n int64) error {
	_, err := s.db.Exec(`UPDATE jobs SET log_bytes = ? WHERE id = ?`, n, id)
	return err
}

// FinishJob writes the terminal state.
func (s *Store) FinishJob(id, status, conclusion, stepsJSON, errMsg string) error {
	if stepsJSON == "" {
		stepsJSON = "[]"
	}
	_, err := s.db.Exec(`UPDATE jobs SET status = ?, conclusion = ?, steps_json = ?, error = ?,
		finished_at = ? WHERE id = ?`, status, conclusion, stepsJSON, errMsg, formatTime(now()), id)
	if err != nil {
		return fmt.Errorf("store: finish job: %w", err)
	}
	return nil
}

// RequeueStaleJobs moves jobs that were running when the process died back to
// queued. Coolify restarts the container on every deploy; without this, a job
// interrupted mid-run would sit in_progress forever and block its delivery id.
func (s *Store) RequeueStaleJobs() (int64, error) {
	res, err := s.db.Exec(`UPDATE jobs SET status = ?, started_at = NULL WHERE status = ?`,
		JobQueued, JobInProgress)
	if err != nil {
		return 0, fmt.Errorf("store: requeue stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneJobs deletes job rows older than the retention window and returns the ids
// removed so their log files can be deleted too.
func (s *Store) PruneJobs(olderThan time.Duration) ([]string, error) {
	cutoff := formatTime(now().Add(-olderThan))
	rows, err := s.db.Query(`SELECT id FROM jobs WHERE created_at < ? AND status NOT IN (?, ?)`,
		cutoff, JobQueued, JobInProgress)
	if err != nil {
		return nil, fmt.Errorf("store: prune select: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := s.db.Exec(`DELETE FROM jobs WHERE id = ?`, id); err != nil {
			return ids, fmt.Errorf("store: prune delete %s: %w", id, err)
		}
	}
	// Deliveries outlive their jobs otherwise, and the table only exists to
	// answer "is there a job in flight for this delivery".
	if _, err := s.db.Exec(`DELETE FROM deliveries WHERE received_at < ?`, cutoff); err != nil {
		return ids, fmt.Errorf("store: prune deliveries: %w", err)
	}
	return ids, nil
}
