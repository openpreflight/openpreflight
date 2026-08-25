package queue

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openpreflight/openpreflight/internal/config"
	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/logs"
	"github.com/openpreflight/openpreflight/internal/secret"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/testsupport"
)

type harness struct {
	store  *store.Store
	cfg    config.Config
	runner *Runner
	github *testsupport.GitHub
	app    store.GitHubApp
	repos  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	box, err := secret.New("fixture-key-for-tests-only-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := config.Config{
		ListenAddr: ":0",
		DataDir:    filepath.Join(dir, "data"),
		SecretKey:  "fixture-key-for-tests-only-1234567890",
	}
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
		Name: "ci", AppID: 4242, PEM: testsupport.AppPEM(t),
		WebhookSecret: "webhook-secret-value", APIURL: gh.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := st.Settings()
	settings.PublicBaseURL = "https://ci.example.com"
	settings.DefaultTimeoutSeconds = 120
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &harness{store: st, cfg: cfg, runner: New(st, cfg, log), github: gh, app: app, repos: repoRoot}
}

// runOne enqueues a job and drives the runner until it reaches a terminal state.
//
// Terminal status alone is not enough to assert on. runJob writes the terminal
// status before it PATCHes the Check Run and before the deferred
// SetJobLogBytes, so returning the moment InFlight() goes false races both.
// activeCount() drops to zero only after runJob has returned, so that is the
// signal that every write for this job has landed.
func (h *harness) runOne(t *testing.T, in store.JobInput) store.Job {
	t.Helper()
	job, err := h.store.EnqueueJob(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	go h.runner.Start(ctx)
	h.runner.Notify()

	deadline := time.Now().Add(80 * time.Second)
	for time.Now().Before(deadline) {
		current, err := h.store.Job(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !current.InFlight() && h.runner.activeCount() == 0 {
			// Re-read: the trailing writes landed after the load above.
			settled, err := h.store.Job(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			return settled
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", job.ID)
	return store.Job{}
}

func (h *harness) binding(t *testing.T, in store.BindingInput) store.RepoBinding {
	t.Helper()
	in.GitHubAppID = h.app.ID
	b, err := h.store.UpsertBinding(in)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunnerHappyPath(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha,
		Ref: "main", Event: "check_suite.requested", DeliveryID: "d-1", InstallationID: 101,
	})

	if job.Status != store.JobSuccess {
		t.Fatalf("status %q conclusion %q error %q", job.Status, job.Conclusion, job.Error)
	}
	if job.CheckRunID != 555 {
		t.Fatalf("check run id not recorded: %d", job.CheckRunID)
	}
	if job.CheckName != "openpreflight" {
		t.Fatalf("resolved check name not written on the job: %q", job.CheckName)
	}

	created := h.github.CreatedCheckRuns()
	if len(created) != 1 {
		t.Fatalf("expected one Check Run, got %d", len(created))
	}
	if created[0].Body["name"] != "openpreflight" {
		t.Fatalf("check name should fall back to the global default: %v", created[0].Body["name"])
	}
	if created[0].Body["head_sha"] != sha {
		t.Fatalf("check run is not against the webhook SHA: %v", created[0].Body["head_sha"])
	}
	details, _ := created[0].Body["details_url"].(string)
	if details != "https://ci.example.com/runs/"+job.ID {
		t.Fatalf("details_url should point at the job's log page, got %q", details)
	}

	completed := h.github.CompletedCheckRuns()
	if len(completed) != 1 || completed[0].Body["conclusion"] != "success" {
		t.Fatalf("completion: %+v", completed)
	}

	var steps []executor.Result
	if err := json.Unmarshal([]byte(job.StepsJSON), &steps); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps, got %+v", steps)
	}
	for _, s := range steps {
		if !s.OK() {
			t.Fatalf("step %s failed: %+v", s.Name, s)
		}
	}

	body, err := logs.Read(h.cfg.LogDir(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"installing", "testing", "building", "checked out"} {
		if !strings.Contains(body, want) {
			t.Errorf("log is missing %q:\n%s", want, body)
		}
	}
	if job.LogBytes == 0 {
		t.Fatal("log size was not recorded")
	}
}

func TestRunnerFailingStepStopsAndSkipsTheRest(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{
		".ci.yml": "install: echo ok\ntest: exit 3\nbuild: echo never\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha,
		Ref: "main", InstallationID: 101,
	})
	if job.Status != store.JobFailure {
		t.Fatalf("status %q", job.Status)
	}
	var steps []executor.Result
	json.Unmarshal([]byte(job.StepsJSON), &steps)
	if len(steps) != 3 {
		t.Fatalf("steps: %+v", steps)
	}
	if steps[1].ExitCode != 3 {
		t.Fatalf("failing step exit code: %+v", steps[1])
	}
	if !steps[2].Skipped {
		t.Fatal("a step after a failure must be skipped, not run")
	}
	completed := h.github.CompletedCheckRuns()
	if completed[0].Body["conclusion"] != "failure" {
		t.Fatalf("conclusion: %v", completed[0].Body["conclusion"])
	}
	// The summary is what a reviewer sees on the commit; it should name the steps.
	output, _ := completed[0].Body["output"].(map[string]any)
	summary, _ := output["summary"].(string)
	if !strings.Contains(summary, "install") || !strings.Contains(summary, "Failed") {
		t.Fatalf("unhelpful summary: %q", summary)
	}
}

func TestRunnerSkipsRepoWithNothingToRun(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{"README.md": "just docs"})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSkipped {
		t.Fatalf("a repo with no pipeline and no package.json should be skipped, got %q", job.Status)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "skipped" {
		t.Fatalf("conclusion: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
}

func TestRunnerUsesBindingOverridesAndCheckName(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{"README.md": "no pipeline file"})
	b := h.binding(t, store.BindingInput{
		Repo: "winpra/api", Enabled: true, CheckName: "Winpra CI", TestCmd: "echo override-ran",
	})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha,
		InstallationID: 101, CheckName: b.CheckName,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("status %q error %q", job.Status, job.Error)
	}
	if name := h.github.CreatedCheckRuns()[0].Body["name"]; name != "Winpra CI" {
		t.Fatalf("binding check name should win: %v", name)
	}
	if job.CheckName != "Winpra CI" {
		t.Fatalf("binding check name not written on the job: %q", job.CheckName)
	}
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	if !strings.Contains(body, "override-ran") {
		t.Fatalf("binding command did not run:\n%s", body)
	}
	if !strings.Contains(body, "binding commands") {
		t.Fatalf("log should say where the plan came from:\n%s", body)
	}
}

func TestRunnerNodeDefaults(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{
		// No lockfile and no install-worthy dependencies: npm install works
		// offline for an empty dependency set.
		"package.json": `{"name":"api","private":true,"scripts":{"test":"echo node-test-ran"}}`,
	})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha, InstallationID: 101,
	})
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	if !strings.Contains(body, "Node defaults from package.json") {
		t.Fatalf("expected Node defaults:\n%s", body)
	}
	if job.Status != store.JobSuccess {
		t.Fatalf("status %q error %q\n%s", job.Status, job.Error, body)
	}
	if !strings.Contains(body, "node-test-ran") {
		t.Fatalf("npm test did not run:\n%s", body)
	}
}

func TestRunnerTimeoutIsReported(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{
		".ci.yml": "test: sleep 60\ntimeout: 2s\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobCancelled || job.Conclusion != "timed_out" {
		t.Fatalf("status %q conclusion %q", job.Status, job.Conclusion)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "timed_out" {
		t.Fatalf("GitHub was not told about the timeout: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
}

func TestRunnerFailsWhenCheckRunCannotBeCreated(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{".ci.yml": "test: echo hi\n"})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})
	// Without checks:write the App cannot report anything; the job must not
	// silently look successful.
	h.github.FailNext("create-check")
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobError {
		t.Fatalf("status %q", job.Status)
	}
	if !strings.Contains(job.Error, "Check Run") {
		t.Fatalf("error should name the failure: %q", job.Error)
	}
}

func TestRunnerCleansUpWorkspace(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "winpra/api", map[string]string{".ci.yml": "test: echo hi\n"})
	b := h.binding(t, store.BindingInput{Repo: "winpra/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "winpra/api", SHA: sha, InstallationID: 101,
	})
	entries, err := filepath.Glob(filepath.Join(h.cfg.WorkspaceDir(), job.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace was left behind: %v", entries)
	}
}

func TestPruneDeletesLogFiles(t *testing.T) {
	h := newHarness(t)
	job, err := h.store.EnqueueJob(store.JobInput{GitHubAppID: h.app.ID, Repo: "o/r", SHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := logs.Create(h.cfg.LogDir(), job.ID, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	w.Printf("some output\n")
	w.Close()
	h.store.FinishJob(job.ID, store.JobSuccess, "success", "[]", "")
	// Backdate past the retention window.
	if _, err := h.store.DB().Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-30*24*time.Hour).Format(time.RFC3339Nano), job.ID); err != nil {
		t.Fatal(err)
	}
	h.runner.pruneOnce()

	if body, _ := logs.Read(h.cfg.LogDir(), job.ID); body != "" {
		t.Fatalf("log file survived pruning: %q", body)
	}
}

func TestGitBaseURLDerivation(t *testing.T) {
	cases := map[string]string{
		"":                               "https://github.com",
		"https://api.github.com":         "https://github.com",
		"https://ghe.example.com/api/v3": "https://ghe.example.com",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080",
		"nonsense":                       "https://github.com",
	}
	for in, want := range cases {
		if got := gitBaseURL(in); got != want {
			t.Errorf("gitBaseURL(%q) = %q want %q", in, got, want)
		}
	}
}
