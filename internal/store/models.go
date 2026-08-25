// SPDX-License-Identifier: Apache-2.0

package store

import "time"

// Settings holds the global defaults. Precedence at run time is
// binding → github app → these.
type Settings struct {
	PublicBaseURL         string `json:"public_base_url"`
	DefaultCheckName      string `json:"default_check_name"`
	DefaultPipelineFile   string `json:"default_pipeline_file"`
	DefaultTimeoutSeconds int    `json:"default_timeout_seconds"`
	MaxConcurrentJobs     int    `json:"max_concurrent_jobs"`
	MaxLogBytes           int64  `json:"max_log_bytes"`
	MaxWorkspaceBytes     int64  `json:"max_workspace_bytes"`
	LogRetentionDays      int    `json:"log_retention_days"`
	SkipForkPRs           bool   `json:"skip_fork_prs"`
	DefaultRuntime        string `json:"default_runtime"`
}

// User is a configurator admin. v1 has exactly one.
type User struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`

	passwordHash string
}

// CoolifyInstance is one (base URL, team token) pair — not a whole host. The
// token is team-scoped, so a second team on the same host is a second row.
type CoolifyInstance struct {
	ID                 int64      `json:"id"`
	Name               string     `json:"name"`
	BaseURL            string     `json:"base_url"`
	TeamID             string     `json:"team_id"`
	TeamName           string     `json:"team_name"`
	DefaultServerUUID  string     `json:"default_server_uuid"`
	DefaultProjectUUID string     `json:"default_project_uuid"`
	LastSeenAt         *time.Time `json:"last_seen_at"`
	LastError          string     `json:"last_error"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`

	// APITokenRedacted is safe to serialise; the plaintext never leaves the
	// store except through DecryptAPIToken.
	APITokenRedacted string `json:"api_token"`

	apiTokenEnc string
}

// GitHubApp is our CI App: webhook receiver, Check Run author, clone credential.
type GitHubApp struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Slug       string     `json:"slug"`
	AppID      int64      `json:"app_id"`
	APIURL     string     `json:"api_url"`
	CheckName  string     `json:"check_name"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	LastError  string     `json:"last_error"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`

	PEMRedacted           string `json:"pem"`
	WebhookSecretRedacted string `json:"webhook_secret"`

	pemEnc           string
	webhookSecretEnc string
}

// RepoBinding is the allow-list entry for one repo. The worker runs nothing
// that does not have an enabled binding.
type RepoBinding struct {
	ID                int64     `json:"id"`
	GitHubAppID       int64     `json:"github_app_id"`
	CoolifyInstanceID int64     `json:"coolify_instance_id"`
	Repo              string    `json:"repo"`
	Enabled           bool      `json:"enabled"`
	Branches          string    `json:"branches"`
	CheckName         string    `json:"check_name"`
	PipelineFile      string    `json:"pipeline_file"`
	TimeoutSeconds    int       `json:"timeout_seconds"`
	InstallCmd        string    `json:"install_cmd"`
	TestCmd           string    `json:"test_cmd"`
	BuildCmd          string    `json:"build_cmd"`
	ShareableLogs     bool      `json:"shareable_logs"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Job statuses. queued/in_progress are the in-flight set used for delivery
// dedup; the rest are terminal.
const (
	JobQueued     = "queued"
	JobInProgress = "in_progress"
	JobSuccess    = "success"
	JobFailure    = "failure"
	JobSkipped    = "skipped"
	JobCancelled  = "cancelled"
	JobError      = "error"
)

// Job is one pipeline run against one SHA.
type Job struct {
	ID             string     `json:"id"`
	BindingID      int64      `json:"binding_id"`
	GitHubAppID    int64      `json:"github_app_id"`
	Repo           string     `json:"repo"`
	SHA            string     `json:"sha"`
	Ref            string     `json:"ref"`
	Event          string     `json:"event"`
	DeliveryID     string     `json:"delivery_id"`
	InstallationID int64      `json:"installation_id"`
	CheckSuiteID   int64      `json:"check_suite_id"`
	CheckRunID     int64      `json:"check_run_id"`
	CheckName      string     `json:"check_name"`
	Status         string     `json:"status"`
	Conclusion     string     `json:"conclusion"`
	StepsJSON      string     `json:"steps"`
	Error          string     `json:"error"`
	LogBytes       int64      `json:"log_bytes"`
	ShareableLogs  bool       `json:"shareable_logs"`
	IsFork         bool       `json:"is_fork"`
	PullNumber     int        `json:"pull_number"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
}

// InFlight reports whether the job still holds its delivery id.
func (j Job) InFlight() bool { return j.Status == JobQueued || j.Status == JobInProgress }

// Duration is the wall time of a finished job, or the elapsed time so far. It is
// unrounded: the view decides how to render it.
func (j Job) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	end := time.Now().UTC()
	if j.FinishedAt != nil {
		end = *j.FinishedAt
	}
	return end.Sub(*j.StartedAt)
}
