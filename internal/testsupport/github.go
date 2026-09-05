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

	mu          sync.Mutex
	created     []CheckRunCall
	patched     []CheckRunCall
	tokens      int
	failNext    map[string]int
	commitFiles map[string]commitFilesStub
	// refs maps a branch or tag to the SHA it points at, so a dry run can ask
	// for "main" and get an immutable commit back the way real GitHub does.
	refs          map[string]string
	defaultBranch string
}

type commitFilesStub struct {
	files     []map[string]any
	truncated bool
}

// CheckRunCall records one Check Runs API call. ID is the Check Run the call
// addressed, and is zero for a create (the id does not exist yet).
type CheckRunCall struct {
	Repo string
	ID   string
	Body map[string]any
}

// NewGitHub starts the fake. Bare repos live at <root>/<owner>/<name>.git.
func NewGitHub(t *testing.T, root string) *GitHub {
	t.Helper()
	f := &GitHub{
		failNext:      map[string]int{},
		commitFiles:   map[string]commitFilesStub{},
		refs:          map[string]string{},
		defaultBranch: "main",
	}
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
		id := r.PathValue("id")
		f.mu.Lock()
		f.patched = append(f.patched, CheckRunCall{Repo: repo, ID: id, Body: body})
		f.mu.Unlock()
		// A reopen is a PATCH like a completion; let a test make it fail so the
		// create-instead fallback can be exercised.
		if body["status"] == "in_progress" && f.shouldFail("reopen-check") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 555})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if f.shouldFail("repo") {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		f.mu.Lock()
		branch := f.defaultBranch
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"full_name":      r.PathValue("owner") + "/" + r.PathValue("repo"),
			"default_branch": branch,
			"private":        true,
		})
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		if f.shouldFail("commit-files") {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"server error"}`))
			return
		}
		ref := r.PathValue("sha")
		f.mu.Lock()
		// A ref registered with SetRef resolves to its SHA, the way the real
		// endpoint accepts "a SHA, a branch or a tag".
		sha, isRef := f.refs[ref]
		if !isRef {
			sha = ref
		}
		stub, ok := f.commitFiles[sha]
		if !ok {
			stub, ok = f.commitFiles[ref]
		}
		f.mu.Unlock()
		files := stub.files
		if files == nil {
			files = []map[string]any{}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"sha":       sha,
			"files":     files,
			"truncated": ok && stub.truncated,
		})
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

// FailNext makes the named operation fail once ("token", "create-check",
// "reopen-check", "commit-files", "repo").
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

// SetRef points a branch or tag at a SHA, so GET /repos/{}/commits/{ref}
// resolves it the way GitHub does.
func (f *GitHub) SetRef(ref, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[ref] = sha
}

// SetDefaultBranch changes what GET /repos/{owner}/{repo} reports.
func (f *GitHub) SetDefaultBranch(branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defaultBranch = branch
}

// SetCommitFiles is the fake GET /repos/{}/commits/{sha} body for a SHA.
func (f *GitHub) SetCommitFiles(sha string, files []map[string]any, truncated bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitFiles[sha] = commitFilesStub{files: files, truncated: truncated}
}
