package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/executor"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
)

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

// patchSettings writes the whole row after reading it, so a partial PATCH (or a
// form that omits a field) cannot silently reset the rest.
func (s *Server) patchSettings(w http.ResponseWriter, r *http.Request, _ store.User) {
	current, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	next := current
	if in.Has("public_base_url") {
		next.PublicBaseURL = strings.TrimRight(in.Str("public_base_url"), "/")
	}
	if in.Has("default_check_name") {
		next.DefaultCheckName = in.Str("default_check_name")
	}
	if in.Has("default_pipeline_file") {
		next.DefaultPipelineFile = in.Str("default_pipeline_file")
	}
	next.DefaultTimeoutSeconds = in.Int("default_timeout_seconds", current.DefaultTimeoutSeconds)
	next.MaxConcurrentJobs = in.Int("max_concurrent_jobs", current.MaxConcurrentJobs)
	next.MaxLogBytes = int64(in.Int("max_log_bytes", int(current.MaxLogBytes)))
	next.MaxWorkspaceBytes = int64(in.Int("max_workspace_bytes", int(current.MaxWorkspaceBytes)))
	next.LogRetentionDays = in.Int("log_retention_days", current.LogRetentionDays)
	if in.json != nil {
		if in.Has("skip_fork_prs") {
			next.SkipForkPRs = in.Bool("skip_fork_prs")
		}
		if in.Has("default_runtime") {
			next.DefaultRuntime = strings.TrimSpace(in.Str("default_runtime"))
		}
	} else {
		next.SkipForkPRs = in.Bool("skip_fork_prs")
		if in.Has("default_runtime") {
			next.DefaultRuntime = strings.TrimSpace(in.Str("default_runtime"))
		}
	}

	if next.DefaultCheckName == "" {
		next.DefaultCheckName = "Coolify CI"
	}
	if next.DefaultPipelineFile == "" {
		next.DefaultPipelineFile = ".ci.yml"
	}
	if next.DefaultTimeoutSeconds < 30 {
		next.DefaultTimeoutSeconds = 30
	}
	if next.MaxConcurrentJobs < 1 {
		next.MaxConcurrentJobs = 1
	}
	if next.MaxLogBytes < 4096 {
		next.MaxLogBytes = 4096
	}
	if next.LogRetentionDays < 1 {
		next.LogRetentionDays = 1
	}
	if next.DefaultRuntime != "" {
		if err := executor.ValidImage(next.DefaultRuntime); err != nil {
			s.badRequest(w, r, err)
			return
		}
	}
	if !next.SkipForkPRs {
		if next.DefaultRuntime == "" {
			s.badRequest(w, r, errors.New("default_runtime is required to run fork pull requests"))
			return
		}
		if !s.dockerAvailable() {
			s.badRequest(w, r, errors.New("fork pull requests require a working Docker executor"))
			return
		}
	}

	if err := s.store.SaveSettings(next); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, next, "/settings", "Settings saved.", "ok")
}
