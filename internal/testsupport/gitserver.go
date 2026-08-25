// SPDX-License-Identifier: Apache-2.0

// Package testsupport provides fixtures shared by tests in several packages: a
// real git-over-HTTP server and a fake GitHub API. It is test-only scaffolding
// and is never imported by the server binary.
package testsupport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// GitServer serves repositories over git's smart HTTP protocol by delegating to
// git-http-backend, so clone behaviour in tests is the real thing rather than a
// mock: the Basic auth header, the shallow fetch and fetching a bare SHA all
// have to actually work.
type GitServer struct {
	*httptest.Server
	root string

	mu    sync.Mutex
	auths []string
}

// NewGitServer starts a git HTTP server over root, which holds bare repos at
// <root>/<owner>/<name>.git.
func NewGitServer(t *testing.T, root string) *GitServer {
	t.Helper()
	gs := &GitServer{root: root}
	gs.Server = httptest.NewServer(http.HandlerFunc(gs.serve))
	t.Cleanup(gs.Close)
	return gs
}

// Auths returns the Authorization headers seen so far, so a test can assert that
// git sent Basic x-access-token rather than a Bearer token.
func (g *GitServer) Auths() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.auths...)
}

func (g *GitServer) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	g.mu.Unlock()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cmd := exec.Command(filepath.Join(gitExecPath(), "git-http-backend"))
	cmd.Env = append(os.Environ(),
		"GIT_PROJECT_ROOT="+g.root,
		"GIT_HTTP_EXPORT_ALL=1",
		"REQUEST_METHOD="+r.Method,
		"PATH_INFO="+r.URL.Path,
		"QUERY_STRING="+r.URL.RawQuery,
		"CONTENT_TYPE="+r.Header.Get("Content-Type"),
		"CONTENT_LENGTH="+strconv.Itoa(len(body)),
		"HTTP_CONTENT_ENCODING="+r.Header.Get("Content-Encoding"),
		"REMOTE_USER=tester",
		"REMOTE_ADDR=127.0.0.1",
	)
	cmd.Stdin = bytes.NewReader(body)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		http.Error(w, fmt.Sprintf("git-http-backend: %v: %s", err, stderr.String()),
			http.StatusInternalServerError)
		return
	}
	writeCGI(w, out.Bytes())
}

// writeCGI splits the CGI response git-http-backend produces into headers and
// body.
func writeCGI(w http.ResponseWriter, raw []byte) {
	head, body, found := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !found {
		head, body, found = bytes.Cut(raw, []byte("\n\n"))
	}
	status := http.StatusOK
	if found {
		for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if strings.EqualFold(key, "Status") {
				if code, err := strconv.Atoi(strings.Fields(value)[0]); err == nil {
					status = code
				}
				continue
			}
			w.Header().Add(key, value)
		}
	} else {
		body = raw
	}
	w.WriteHeader(status)
	w.Write(body)
}

func gitExecPath() string {
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		return "/usr/lib/git-core"
	}
	return strings.TrimSpace(string(out))
}

// NewRepo creates a bare repository at <root>/<repo>.git containing files, and
// returns the commit SHA. Fetching that SHA directly is enabled, which is what
// GitHub allows and what our clone relies on.
func NewRepo(t *testing.T, root, repo string, files map[string]string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o750); err != nil {
		t.Fatal(err)
	}
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(work, "init", "--quiet", "--initial-branch=main")
	for name, body := range files {
		full := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run(work, "add", "-A")
	run(work, "commit", "--quiet", "-m", "fixture")
	sha := run(work, "rev-parse", "HEAD")

	bare := filepath.Join(root, repo+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	run(work, "clone", "--bare", "--quiet", work, bare)
	// Our clone fetches an exact SHA, which a server only allows when this is on.
	run(bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	run(bare, "config", "http.receivepack", "false")
	return sha
}
