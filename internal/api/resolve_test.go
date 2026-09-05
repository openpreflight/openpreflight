// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/openpreflight/openpreflight/internal/config"
	"github.com/openpreflight/openpreflight/internal/pipeline"
	"github.com/openpreflight/openpreflight/internal/queue"
	"github.com/openpreflight/openpreflight/internal/secret"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/testsupport"
)

// resolveHarness is a server whose App can actually reach GitHub: the webhook
// tests use a placeholder PEM, which is fine for HMAC but cannot mint a JWT, and
// a dry run has to talk to the API and clone over git.
type resolveHarness struct {
	*Server
	handler http.Handler
	store   *store.Store
	github  *testsupport.GitHub
	repos   string
	app     store.GitHubApp
	bearer  string
}

func newResolveHarness(t *testing.T) *resolveHarness {
	t.Helper()
	const key = "fixture-key-for-tests-only-1234567890"
	box, err := secret.New(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := config.Config{ListenAddr: ":0", DataDir: filepath.Join(dir, "data"), SecretKey: key}
	t.Setenv("WORKSPACE_DIR", filepath.Join(dir, "workspace"))
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	repoRoot := filepath.Join(dir, "repos")
	gh := testsupport.NewGitHub(t, repoRoot)

	app, err := st.CreateGitHubApp(store.GitHubAppInput{
		Name: "ci", Slug: "ci", AppID: appNumericID, PEM: testsupport.AppPEM(t),
		WebhookSecret: webhookSecret, APIURL: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := st.Settings()
	settings.PublicBaseURL = "https://ci.example.com"
	settings.DefaultTimeoutSeconds = 600
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(st, cfg, queue.New(st, cfg, log), log)
	if err != nil {
		t.Fatal(err)
	}
	srv.dockerOK = func() bool { return true }

	user, err := st.CreateUser("admin", "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &resolveHarness{
		Server: srv, handler: srv.Handler(), store: st,
		github: gh, repos: repoRoot, app: app, bearer: token,
	}
}

// seed creates a bare repo, points main at it and binds it.
func (h *resolveHarness) seed(t *testing.T, files map[string]string, in store.BindingInput) (store.RepoBinding, string) {
	t.Helper()
	sha := testsupport.NewRepo(t, h.repos, "acme/api", files)
	h.github.SetRef("main", sha)
	in.GitHubAppID = h.app.ID
	in.Repo = "acme/api"
	if !in.Enabled {
		in.Enabled = true
	}
	binding, err := h.store.UpsertBinding(in)
	if err != nil {
		t.Fatal(err)
	}
	return binding, sha
}

func (h *resolveHarness) resolve(t *testing.T, id int64, ref string) pipeline.Resolution {
	t.Helper()
	res, err := h.resolveBinding(context.Background(), id, ref)
	if err != nil {
		t.Fatalf("dry run could not be attempted: %v", err)
	}
	return res
}

func originSource(t *testing.T, res pipeline.Resolution, field string) string {
	t.Helper()
	for _, o := range res.Origins {
		if o.Field == field {
			return o.Source
		}
	}
	t.Fatalf("no origin for %q: %+v", field, res.Origins)
	return ""
}

// TestResolveReportsPerValueOrigins is the wave's headline: two values in one
// answer coming from two different layers, each saying so.
func TestResolveReportsPerValueOrigins(t *testing.T) {
	h := newResolveHarness(t)
	binding, sha := h.seed(t, map[string]string{
		".ci.yml": "runtime: node:22\ntest: npm test\n",
	}, store.BindingInput{TimeoutSeconds: 120})

	res := h.resolve(t, binding.ID, "")
	if res.Decision != pipeline.DecisionRun {
		t.Fatalf("decision: %+v", res)
	}
	if res.SHA != sha {
		t.Fatalf("sha: %q want %q", res.SHA, sha)
	}
	if res.Ref != "main" {
		t.Fatalf("an absent ref should fall back to the default branch: %q", res.Ref)
	}
	if res.Executor != "docker: node:22" {
		t.Fatalf("executor: %q", res.Executor)
	}
	if got := originSource(t, res, "test"); got != ".ci.yml" {
		t.Errorf("test origin: %q", got)
	}
	if got := originSource(t, res, "runtime"); got != ".ci.yml" {
		t.Errorf("runtime origin: %q", got)
	}
	if got := originSource(t, res, "timeout"); got != pipeline.SourceBinding {
		t.Errorf("timeout origin: %q — the binding set 120s, not settings", got)
	}
	if len(res.Steps) != 1 || res.Steps[0].Source != ".ci.yml" {
		t.Fatalf("steps: %+v", res.Steps)
	}
	if len(res.Errors) != 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
}

// TestResolveWritesNoCheckRun is the property that makes this a dry run. A
// resolve that could report a status on a commit would be a run.
func TestResolveWritesNoCheckRun(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{".ci.yml": "test: echo hi\n"}, store.BindingInput{})

	h.resolve(t, binding.ID, "main")

	if n := len(h.github.CreatedCheckRuns()); n != 0 {
		t.Errorf("a dry run created %d Check Runs", n)
	}
	if n := len(h.github.CompletedCheckRuns()); n != 0 {
		t.Errorf("a dry run PATCHed %d Check Runs", n)
	}
	jobs, err := h.store.ListJobs(store.JobList{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 0 {
		t.Errorf("a dry run enqueued %d jobs", len(jobs))
	}
}

// TestResolveCleansUpItsWorkspace: a dry run clones, and a leaked checkout per
// call would fill the disk the workspace cap exists to protect.
func TestResolveCleansUpItsWorkspace(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{".ci.yml": "test: echo hi\n"}, store.BindingInput{})

	h.resolve(t, binding.ID, "main")

	entries, err := filepath.Glob(filepath.Join(h.cfg.WorkspaceDir(), "resolve-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("dry-run workspaces left behind: %v", entries)
	}
}

// TestResolveReportsEveryProblemAtOnce is item 16. Mid-run the first error
// kills the job; pre-flight an operator wants the whole list.
func TestResolveReportsEveryProblemAtOnce(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{
		".ci.yml": "runtime: \"node:22; id\"\ntimeout: soon\ntest: npm test\n",
	}, store.BindingInput{})

	res := h.resolve(t, binding.ID, "main")
	if res.Decision != pipeline.DecisionFail {
		t.Fatalf("decision: %+v", res)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("expected the bad timeout and the bad image together, got %v", res.Errors)
	}
	joined := strings.Join(res.Errors, "\n")
	if !strings.Contains(joined, "timeout") {
		t.Errorf("no timeout problem reported: %v", res.Errors)
	}
	if !strings.Contains(joined, "runtime") {
		t.Errorf("no image problem reported: %v", res.Errors)
	}
}

// TestResolveSkipsWhenNoPathMatches answers "why did nothing happen?" without
// pushing a commit to find out.
func TestResolveSkipsWhenNoPathMatches(t *testing.T) {
	h := newResolveHarness(t)
	binding, sha := h.seed(t, map[string]string{
		".ci.yml":   "test: echo hi\n",
		"README.md": "docs",
	}, store.BindingInput{Paths: "frontend/**"})
	h.github.SetCommitFiles(sha, []map[string]any{{"filename": "README.md", "status": "modified"}}, false)

	res := h.resolve(t, binding.ID, "main")
	if res.Decision != pipeline.DecisionSkip {
		t.Fatalf("decision: %+v", res)
	}
	if res.SkipReason != store.SkipReasonPathFilter {
		t.Fatalf("skip reason: %q", res.SkipReason)
	}
	if !strings.Contains(res.PathFilter, "Result: SKIP") {
		t.Fatalf("path filter diagnostic: %q", res.PathFilter)
	}
}

// TestResolveReportsInferredPlanForAGoRepo is item 18 reaching the surface an
// operator actually looks at. Before this wave a Go repository with no .ci.yml
// and no binding commands had nothing to run.
func TestResolveReportsInferredPlanForAGoRepo(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{
		"go.mod":  "module example.com/x\n\ngo 1.24\n",
		"main.go": "package main\n\nfunc main() {}\n",
	}, store.BindingInput{})

	res := h.resolve(t, binding.ID, "main")
	if res.Decision != pipeline.DecisionRun {
		t.Fatalf("decision: %+v", res)
	}
	if len(res.Steps) != 3 {
		t.Fatalf("steps: %+v", res.Steps)
	}
	if res.Steps[1].Command != "go test ./..." {
		t.Fatalf("test step: %q", res.Steps[1].Command)
	}
	if res.Steps[0].Source != "Go defaults from go.mod" {
		t.Fatalf("step source should name the marker file: %q", res.Steps[0].Source)
	}
}

// TestResolveWarnsOnEmptyPlan: the binding default is skip, so this is a
// warning and a skip rather than an error.
func TestResolveWarnsOnEmptyPlan(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{"README.md": "just docs"}, store.BindingInput{})

	res := h.resolve(t, binding.ID, "main")
	if res.Decision != pipeline.DecisionSkip {
		t.Fatalf("decision: %+v", res)
	}
	if res.SkipReason != store.SkipReasonNoPipeline {
		t.Fatalf("skip reason: %q", res.SkipReason)
	}
	if len(res.Warnings) == 0 {
		t.Fatal("an empty plan should say so")
	}

	// on_empty_pipeline: fail turns the same commit into a failure, which is
	// the whole point of that setting.
	in := store.BindingInput{
		GitHubAppID: h.app.ID, Repo: "acme/api", Enabled: true,
		OnEmptyPipeline: store.OnEmptyPipelineFail,
	}
	if _, err := h.store.UpsertBinding(in); err != nil {
		t.Fatal(err)
	}
	res = h.resolve(t, binding.ID, "main")
	if res.Decision != pipeline.DecisionFail {
		t.Fatalf("on_empty_pipeline: fail should fail: %+v", res)
	}
}

// TestResolveEndpointReturnsJSON checks the API surface, not just the core.
func TestResolveEndpointReturnsJSON(t *testing.T) {
	h := newResolveHarness(t)
	binding, sha := h.seed(t, map[string]string{".ci.yml": "test: npm test\n"}, store.BindingInput{})

	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/bindings/"+strconv.FormatInt(binding.ID, 10)+"/resolve?ref=main", nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out pipeline.Resolution
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body.String())
	}
	if out.SHA != sha || out.Decision != pipeline.DecisionRun {
		t.Fatalf("body: %+v", out)
	}
	if len(out.Origins) == 0 {
		t.Fatal("the JSON must carry the per-value origins; that is the point of the endpoint")
	}
}

// TestResolvePageRendersTheVerdict is the operator-facing half.
func TestResolvePageRendersTheVerdict(t *testing.T) {
	h := newResolveHarness(t)
	binding, _ := h.seed(t, map[string]string{".ci.yml": "test: npm test\n"}, store.BindingInput{})

	req := httptest.NewRequest(http.MethodGet,
		"/repos/"+strconv.FormatInt(binding.ID, 10)+"/resolve?ref=main", nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"This commit would run", "npm test", "Where every value came from"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q", want)
		}
	}
}

// TestResolveUnknownBindingRedirects matches pageRepo and pageReposEdit: a
// stale id on an HTML page lands on the list, not a dead end.
func TestResolveUnknownBindingRedirects(t *testing.T) {
	h := newResolveHarness(t)
	req := httptest.NewRequest(http.MethodGet, "/repos/9999/resolve", nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d want 303", rec.Code)
	}
}
