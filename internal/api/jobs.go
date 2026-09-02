// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/openpreflight/openpreflight/internal/logs"
	"github.com/openpreflight/openpreflight/internal/store"
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

// parseJobList reads repo, status, limit, and offset from the query string.
func parseJobList(r *http.Request) store.JobList {
	q := r.URL.Query()
	return store.JobList{
		Repo:   strings.TrimSpace(q.Get("repo")),
		Status: strings.TrimSpace(q.Get("status")),
		Limit:  atoiDefault(q.Get("limit"), 100),
		Offset: atoiOffset(q.Get("offset")),
	}
}

// atoiOffset parses a page offset. Empty, invalid, or negative values are 0.
func atoiOffset(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request, _ store.User) {
	jobs, _, ok := s.loadJobs(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// loadJobs applies the query filter. Unknown status is 400; other store errors
// are 500. The returned JobList is clamped the same way ListJobs clamps.
func (s *Server) loadJobs(w http.ResponseWriter, r *http.Request) ([]store.Job, store.JobList, bool) {
	f := parseJobList(r)
	jobs, err := s.store.ListJobs(f)
	if err != nil {
		if errors.Is(err, store.ErrInvalidJobStatus) {
			s.badRequest(w, r, err)
			return nil, f, false
		}
		s.fail(w, r, err)
		return nil, f, false
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	return jobs, f, true
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request, _ store.User) {
	job, err := s.store.Job(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// jobLogAccess loads a job and enforces the same rule as GET /runs/{id}:
// session, or the binding opted into shareable logs.
func (s *Server) jobLogAccess(w http.ResponseWriter, r *http.Request) (store.Job, bool) {
	job, err := s.store.Job(r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, err)
		return store.Job{}, false
	}
	_, _, authed := s.authenticate(r)
	if !authed && !job.ShareableLogs {
		if wantsJSON(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return store.Job{}, false
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return store.Job{}, false
	}
	return job, true
}

// getJobLogs is the JSON equivalent of GET /runs/{id}. Unguarded so a shareable
// job is readable without a session, matching that page's rule.
func (s *Server) getJobLogs(w http.ResponseWriter, r *http.Request) {
	job, ok := s.jobLogAccess(w, r)
	if !ok {
		return
	}
	body, err := logs.Read(s.cfg.LogDir(), job.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    job.ID,
		"log":   body,
		"bytes": int64(len(body)),
	})
}

// rerunJob queues a fresh job for the same commit. Per the README this is a new job
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
