// SPDX-License-Identifier: Apache-2.0

package testsupport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// GitHub is a fake GitHub App API that also serves the repositories over git,
// so one base URL can stand in for both api.github.com and github.com. That
// mirrors how the runner derives the git origin from the App's API URL.
type GitHub struct {
	*httptest.Server

	mu       sync.Mutex
	created  []CheckRunCall
	patched  []CheckRunCall
	tokens   int
	failNext map[string]int
}

// CheckRunCall records one Check Runs API call.
type CheckRunCall struct {
	Repo string
	Body map[string]any
}

// NewGitHub starts the fake. Bare repos live at <root>/<owner>/<name>.git.
func NewGitHub(t *testing.T, root string) *GitHub {
	t.Helper()
	f := &GitHub{failNext: map[string]int{}}
	git := &GitServer{root: root}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 101, "account": map[string]any{"login": "acme", "type": "Organization"},
				"repository_selection": "selected"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		if f.shouldFail("token") {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs-installation-token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"repositories": []map[string]any{
				{"id": 1, "full_name": "acme/api", "name": "api", "private": true},
			},
		})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		f.mu.Lock()
		f.created = append(f.created, CheckRunCall{Repo: repo, Body: body})
		f.mu.Unlock()
		if f.shouldFail("create-check") {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 555, "name": body["name"]})
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		repo := r.PathValue("owner") + "/" + r.PathValue("repo")
		f.mu.Lock()
		f.patched = append(f.patched, CheckRunCall{Repo: repo, Body: body})
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id": 555})
	})
	// Anything else is a git request.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, ".git/") {
			git.serve(w, r)
			return
		}
		http.NotFound(w, r)
	})

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// FailNext makes the named operation fail once ("token", "create-check").
func (f *GitHub) FailNext(op string) {
	f.mu.Lock()
	f.failNext[op]++
	f.mu.Unlock()
}

func (f *GitHub) shouldFail(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext[op] > 0 {
		f.failNext[op]--
		return true
	}
	return false
}

// CreatedCheckRuns returns the Check Runs created so far.
func (f *GitHub) CreatedCheckRuns() []CheckRunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CheckRunCall(nil), f.created...)
}

// CompletedCheckRuns returns the completion PATCHes seen so far.
func (f *GitHub) CompletedCheckRuns() []CheckRunCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CheckRunCall(nil), f.patched...)
}
