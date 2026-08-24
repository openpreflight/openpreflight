package api

import (
	"net/http"
	"strings"

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
	// Fork PRs are always skipped in v1 (README "Not in v1"); the field exists so the
	// setting is visible, not adjustable.
	next.SkipForkPRs = true

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

	if err := s.store.SaveSettings(next); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, next, "/settings", "Settings saved.", "ok")
}
