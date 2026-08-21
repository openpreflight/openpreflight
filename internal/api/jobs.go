package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
)

// errNoInstallation means we never learned which installation to authenticate
// as, so there is no way to clone or write a check.
var errNoInstallation = errors.New("this job has no installation id, so it cannot be re-run")

// atoiDefault parses a query parameter with a fallback.
func atoiDefault(v string, def int) int {
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, _ store.User) {
	jobs, err := s.store.ListJobs(atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, _ store.User) {
	job, err := s.store.Job(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// rerunJob queues a fresh job for the same commit. Per PLAN.md this is a new job
// and a new Check Run, never a mutation of the old one.
func (s *Server) rerunJob(w http.ResponseWriter, r *http.Request, _ store.User) {
	old, err := s.store.Job(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	if old.InstallationID == 0 {
		s.badRequest(w, r, errNoInstallation)
		return
	}
	// Re-read the binding: overrides may have changed since the first run, and a
	// disabled binding must not be runnable from the Jobs page either.
	binding, err := s.store.EnabledBinding(old.GitHubAppID, old.Repo)
	if err != nil {
		s.reply(w, r, http.StatusConflict,
			map[string]string{"error": "no enabled binding for " + old.Repo},
			"/jobs", old.Repo+" has no enabled binding, so it cannot be re-run.", "err")
		return
	}
	job, err := s.store.EnqueueJob(store.JobInput{
		BindingID:   binding.ID,
		GitHubAppID: old.GitHubAppID,
		Repo:        old.Repo,
		SHA:         old.SHA,
		Ref:         old.Ref,
		Event:       "manual.rerun",
		// No delivery id: this run is ours, not GitHub's, so it must not
		// occupy a delivery's dedup slot.
		InstallationID: old.InstallationID,
		CheckName:      binding.CheckName,
		ShareableLogs:  binding.ShareableLogs,
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.runner.Notify()
	s.reply(w, r, http.StatusAccepted, map[string]any{"job": job},
		"/jobs", "Re-run queued for "+job.Repo+".", "ok")
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request, _ store.User) {
	id := r.PathValue("id")
	if _, err := s.store.Job(id); err != nil {
		s.notFound(w, r, err)
		return
	}
	if !s.runner.CancelJob(id) {
		s.reply(w, r, http.StatusConflict, map[string]string{"error": "job is not running"},
			"/jobs", "That job is not running.", "err")
		return
	}
	s.reply(w, r, http.StatusOK, map[string]string{"status": "cancelling"},
		"/jobs", "Cancelling.", "ok")
}
