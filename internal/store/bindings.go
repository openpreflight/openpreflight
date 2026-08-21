package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

const bindingCols = `id, github_app_id, COALESCE(coolify_instance_id, 0), repo, enabled, branches,
	check_name, pipeline_file, timeout_seconds, install_cmd, test_cmd, build_cmd,
	shareable_logs, created_at, updated_at`

// repoPattern is GitHub's owner/name shape.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9-_.]+/[A-Za-z0-9-_.]+$`)

func scanBinding(sc interface{ Scan(...any) error }) (RepoBinding, error) {
	var (
		b               RepoBinding
		enabled, shared int
		ca, ua          string
	)
	if err := sc.Scan(&b.ID, &b.GitHubAppID, &b.CoolifyInstanceID, &b.Repo, &enabled, &b.Branches,
		&b.CheckName, &b.PipelineFile, &b.TimeoutSeconds, &b.InstallCmd, &b.TestCmd, &b.BuildCmd,
		&shared, &ca, &ua); err != nil {
		return RepoBinding{}, err
	}
	b.Enabled = enabled != 0
	b.ShareableLogs = shared != 0
	b.CreatedAt = parseTime(ca)
	b.UpdatedAt = parseTime(ua)
	return b, nil
}

// ListBindings returns every binding, enabled or not.
func (s *Store) ListBindings() ([]RepoBinding, error) {
	rows, err := s.db.Query(`SELECT ` + bindingCols + ` FROM repo_bindings ORDER BY repo, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list bindings: %w", err)
	}
	defer rows.Close()
	var out []RepoBinding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan binding: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Binding loads one binding by id.
func (s *Store) Binding(id int64) (RepoBinding, error) {
	b, err := scanBinding(s.db.QueryRow(`SELECT `+bindingCols+` FROM repo_bindings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return RepoBinding{}, ErrNotFound
	}
	if err != nil {
		return RepoBinding{}, fmt.Errorf("store: binding %d: %w", id, err)
	}
	return b, nil
}

// EnabledBinding is the worker's allow-list lookup: an App plus a repo must have
// an enabled row or nothing runs. Repo matching is case-insensitive because
// GitHub treats owner/name that way.
func (s *Store) EnabledBinding(githubAppID int64, repo string) (RepoBinding, error) {
	b, err := scanBinding(s.db.QueryRow(`SELECT `+bindingCols+` FROM repo_bindings
		WHERE github_app_id = ? AND lower(repo) = lower(?) AND enabled = 1`, githubAppID, repo))
	if errors.Is(err, sql.ErrNoRows) {
		return RepoBinding{}, ErrNotFound
	}
	if err != nil {
		return RepoBinding{}, fmt.Errorf("store: enabled binding %s: %w", repo, err)
	}
	return b, nil
}

// BindingInput is the writable shape of a binding.
type BindingInput struct {
	GitHubAppID       int64
	CoolifyInstanceID int64
	Repo              string
	Enabled           bool
	Branches          string
	CheckName         string
	PipelineFile      string
	TimeoutSeconds    int
	InstallCmd        string
	TestCmd           string
	BuildCmd          string
	ShareableLogs     bool
}

func (in *BindingInput) normalise() error {
	in.Repo = strings.Trim(strings.TrimSpace(in.Repo), "/")
	in.Branches = strings.TrimSpace(in.Branches)
	in.CheckName = strings.TrimSpace(in.CheckName)
	in.PipelineFile = strings.TrimSpace(in.PipelineFile)
	if in.GitHubAppID <= 0 {
		return errors.New("a CI GitHub App is required: it verifies the webhook and writes the Check Run")
	}
	if !repoPattern.MatchString(in.Repo) {
		return errors.New("repo must be owner/name")
	}
	if in.PipelineFile != "" && (path.IsAbs(in.PipelineFile) || strings.Contains(in.PipelineFile, "..")) {
		return errors.New("pipeline file must be a path inside the repo")
	}
	if in.TimeoutSeconds < 0 {
		return errors.New("timeout must not be negative")
	}
	return nil
}

// UpsertBinding creates or updates the binding for (app, repo). The UI's repo
// checkboxes and the API's PUT both land here.
func (s *Store) UpsertBinding(in BindingInput) (RepoBinding, error) {
	if err := in.normalise(); err != nil {
		return RepoBinding{}, err
	}
	if _, err := s.GitHubApp(in.GitHubAppID); err != nil {
		return RepoBinding{}, fmt.Errorf("github app %d: %w", in.GitHubAppID, err)
	}
	if in.CoolifyInstanceID != 0 {
		if _, err := s.CoolifyInstance(in.CoolifyInstanceID); err != nil {
			return RepoBinding{}, fmt.Errorf("coolify instance %d: %w", in.CoolifyInstanceID, err)
		}
	}
	ts := formatTime(now())
	_, err := s.db.Exec(`INSERT INTO repo_bindings
		(github_app_id, coolify_instance_id, repo, enabled, branches, check_name, pipeline_file,
		 timeout_seconds, install_cmd, test_cmd, build_cmd, shareable_logs, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (github_app_id, repo) DO UPDATE SET
			coolify_instance_id = excluded.coolify_instance_id,
			enabled = excluded.enabled,
			branches = excluded.branches,
			check_name = excluded.check_name,
			pipeline_file = excluded.pipeline_file,
			timeout_seconds = excluded.timeout_seconds,
			install_cmd = excluded.install_cmd,
			test_cmd = excluded.test_cmd,
			build_cmd = excluded.build_cmd,
			shareable_logs = excluded.shareable_logs,
			updated_at = excluded.updated_at`,
		in.GitHubAppID, nullInt64(in.CoolifyInstanceID), in.Repo, boolInt(in.Enabled), in.Branches,
		in.CheckName, in.PipelineFile, in.TimeoutSeconds, in.InstallCmd, in.TestCmd, in.BuildCmd,
		boolInt(in.ShareableLogs), ts, ts)
	if err != nil {
		return RepoBinding{}, fmt.Errorf("store: upsert binding: %w", err)
	}
	b, err := scanBinding(s.db.QueryRow(`SELECT `+bindingCols+` FROM repo_bindings
		WHERE github_app_id = ? AND repo = ?`, in.GitHubAppID, in.Repo))
	if err != nil {
		return RepoBinding{}, fmt.Errorf("store: reload binding: %w", err)
	}
	return b, nil
}

// DeleteBinding removes a binding, which also removes the repo from the
// allow-list.
func (s *Store) DeleteBinding(id int64) error {
	res, err := s.db.Exec(`DELETE FROM repo_bindings WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete binding: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// BranchAllowed applies the optional branch allow-list. An empty list allows
// everything; entries may be exact names or a trailing-* prefix like release/*.
// An unknown ref (fork PR, tag) is only allowed when the list is empty.
func (b RepoBinding) BranchAllowed(branch string) bool {
	list := splitList(b.Branches)
	if len(list) == 0 {
		return true
	}
	branch = strings.TrimPrefix(branch, "refs/heads/")
	for _, pattern := range list {
		if pattern == branch {
			return true
		}
		if strings.HasSuffix(pattern, "*") && strings.HasPrefix(branch, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// splitList parses a comma or newline separated field.
func splitList(v string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	}) {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
