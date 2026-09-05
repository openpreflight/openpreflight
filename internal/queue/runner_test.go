// SPDX-License-Identifier: Apache-2.0

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
	"github.com/openpreflight/openpreflight/internal/githubapp"
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
	return h.drainJob(t, job.ID)
}

// drainJob is runOne for a job that is already enqueued, so a test can set up
// row state (a Check Run id from a previous attempt, say) before the runner
// claims it.
func (h *harness) drainJob(t *testing.T, jobID string) store.Job {
	t.Helper()
	job := store.Job{ID: jobID}
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha,
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo ok\ntest: exit 3\nbuild: echo never\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha,
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{"README.md": "just docs"})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSkipped {
		t.Fatalf("a repo with no pipeline and no package.json should be skipped, got %q", job.Status)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "skipped" {
		t.Fatalf("conclusion: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
}

func TestRunnerSkipsWhenNoPathMatches(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	h.github.SetCommitFiles(sha, []map[string]any{{"filename": "backend/main.go"}}, false)
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true, Paths: "frontend/**"})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSkipped {
		t.Fatalf("path miss should skip, got %q %q", job.Status, job.Error)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "skipped" {
		t.Fatalf("required checks need a skipped conclusion: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	// The bare "no changed path matched" sentence was replaced by a diagnostic
	// that shows the counts and the filter, so an operator can tell a filter
	// that is too narrow from one that is simply not matching this commit.
	for _, want := range []string{"Changed files: 1", "Matched files: 0", "Filter: frontend/**", "Result: SKIP"} {
		if !strings.Contains(body, want) {
			t.Fatalf("log should contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "checked out") {
		t.Fatal("path miss cloned the repo")
	}
	if job.SkipReason != store.SkipReasonPathFilter {
		t.Fatalf("skip_reason %q, want %q", job.SkipReason, store.SkipReasonPathFilter)
	}
	// The Check Run has to carry the diagnostic too: a reader on GitHub cannot
	// see the worker's log file.
	summary, _ := h.github.CompletedCheckRuns()[0].Body["output"].(map[string]any)
	if s, _ := summary["summary"].(string); !strings.Contains(s, "Result: SKIP") {
		t.Fatalf("check run summary should carry the diagnostic: %v", summary)
	}
}

func TestRunnerRunsWhenAPathMatches(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	h.github.SetCommitFiles(sha, []map[string]any{
		{"filename": "frontend/app.ts"},
		{"filename": "frontend/renamed.ts", "previous_filename": "lib/old.ts"},
	}, false)
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true, Paths: "frontend/**"})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("path hit should run, got %q %q", job.Status, job.Error)
	}
}

func TestRunnerFailOpenWhenCommitFilesFail(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	h.github.FailNext("commit-files")
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true, Paths: "frontend/**"})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("commit-files error should fail-open, got %q %q", job.Status, job.Error)
	}
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	if !strings.Contains(body, "fail-open") {
		t.Fatalf("log should say fail-open:\n%s", body)
	}
}

func TestRunnerFailOpenWhenFileListIsTruncated(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "install: echo installing\ntest: echo testing\nbuild: echo building\n",
	})
	h.github.SetCommitFiles(sha, []map[string]any{{"filename": "docs/only.md"}}, true)
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true, Paths: "frontend/**"})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("truncated list should fail-open, got %q %q", job.Status, job.Error)
	}
}

func TestRunnerUsesBindingOverridesAndCheckName(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{"README.md": "no pipeline file"})
	b := h.binding(t, store.BindingInput{
		Repo: "acme/api", Enabled: true, CheckName: "Acme CI", TestCmd: "echo override-ran",
	})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha,
		InstallationID: 101, CheckName: b.CheckName,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("status %q error %q", job.Status, job.Error)
	}
	if name := h.github.CreatedCheckRuns()[0].Body["name"]; name != "Acme CI" {
		t.Fatalf("binding check name should win: %v", name)
	}
	if job.CheckName != "Acme CI" {
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		// No lockfile and no install-worthy dependencies: npm install works
		// offline for an empty dependency set.
		"package.json": `{"name":"api","private":true,"scripts":{"test":"echo node-test-ran"}}`,
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: sleep 60\ntimeout: 2s\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobCancelled || job.Conclusion != "timed_out" {
		t.Fatalf("status %q conclusion %q", job.Status, job.Conclusion)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "timed_out" {
		t.Fatalf("GitHub was not told about the timeout: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
}

// TestRunnerReusesCheckRunAfterRequeue is the regression test for the orphaned
// Check Run. A job interrupted by a crash or a Coolify redeploy is requeued with
// its check_run_id intact; if the runner created a second Check Run, the first
// would stay in_progress forever and a required check on that commit would never
// resolve.
func TestRunnerReusesCheckRunAfterRequeue(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: echo testing\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	// Stand in for "the worker died mid-job": the row already carries the Check
	// Run id that the first attempt created.
	job, err := h.store.EnqueueJob(store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetJobCheckRun(job.ID, 555); err != nil {
		t.Fatal(err)
	}

	settled := h.drainJob(t, job.ID)
	if settled.Status != store.JobSuccess {
		t.Fatalf("status %q err %q", settled.Status, settled.Error)
	}
	if got := h.github.CreatedCheckRuns(); len(got) != 0 {
		t.Fatalf("a requeued job must reuse its Check Run, not create one: %+v", got)
	}
	patches := h.github.CompletedCheckRuns()
	if len(patches) != 2 {
		t.Fatalf("expected a reopen then a completion, got %d: %+v", len(patches), patches)
	}
	if patches[0].Body["status"] != "in_progress" {
		t.Fatalf("first PATCH should reopen the run: %+v", patches[0].Body)
	}
	// A run left both completed and in_progress renders as finished on the
	// Checks tab, so the previous conclusion has to be cleared.
	if patches[0].Body["conclusion"] != nil {
		t.Fatalf("reopen must clear the old conclusion: %+v", patches[0].Body)
	}
	for i, pc := range patches {
		if pc.ID != "555" {
			t.Fatalf("PATCH %d addressed check run %q, want 555", i, pc.ID)
		}
	}
	if settled.CheckRunID != 555 {
		t.Fatalf("job kept check_run_id %d, want 555", settled.CheckRunID)
	}
}

// TestRunnerCreatesCheckRunWhenReopenFails covers the run being deleted or the
// App reinstalled. Falling back to create is what this code did before the
// reuse existed, so the worst case is the old behaviour, not a failed job.
func TestRunnerCreatesCheckRunWhenReopenFails(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: echo testing\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job, err := h.store.EnqueueJob(store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetJobCheckRun(job.ID, 999); err != nil {
		t.Fatal(err)
	}
	h.github.FailNext("reopen-check")

	settled := h.drainJob(t, job.ID)
	if settled.Status != store.JobSuccess {
		t.Fatalf("a failed reopen must not fail the job: %q %q", settled.Status, settled.Error)
	}
	if len(h.github.CreatedCheckRuns()) != 1 {
		t.Fatalf("expected one create after the reopen failed: %+v", h.github.CreatedCheckRuns())
	}
}

// TestRunnerReportsForkSkipAsACompletedCheck is the regression test for fork
// pull requests hanging a required check. The webhook used to answer 202 and
// drop the delivery, so no Check Run existed and branch protection waited
// forever with nothing on screen explaining why.
func TestRunnerReportsForkSkipAsACompletedCheck(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: echo testing\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
		IsFork: true, PullNumber: 7, SkipReason: store.SkipReasonForkDisabled,
	})
	if job.Status != store.JobSkipped {
		t.Fatalf("status %q err %q", job.Status, job.Error)
	}
	if job.SkipReason != store.SkipReasonForkDisabled {
		t.Fatalf("skip_reason %q", job.SkipReason)
	}
	completions := h.github.CompletedCheckRuns()
	if len(completions) != 1 {
		t.Fatalf("expected exactly one completion, got %d", len(completions))
	}
	if completions[0].Body["conclusion"] != "skipped" {
		t.Fatalf("conclusion: %+v", completions[0].Body)
	}
	// An operator reading the PR needs to know what to change, not just that it
	// was refused.
	out, _ := completions[0].Body["output"].(map[string]any)
	summary, _ := out["summary"].(string)
	for _, want := range []string{"skip_fork_prs", "default_runtime"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary should name %q so an operator can act:\n%s", want, summary)
		}
	}
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	if strings.Contains(body, "checked out") {
		t.Fatal("a pre-flight skip must not clone")
	}
}

func TestRunnerForkSkipReasonsAreDistinct(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{store.SkipReasonForkNoDocker, "no Docker engine is reachable"},
		{store.SkipReasonForkNoRuntime, "default_runtime is empty"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			h := newHarness(t)
			sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{".ci.yml": "test: echo t\n"})
			b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})
			job := h.runOne(t, store.JobInput{
				BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha,
				InstallationID: 101, IsFork: true, SkipReason: tc.reason,
			})
			if job.Status != store.JobSkipped {
				t.Fatalf("status %q", job.Status)
			}
			out, _ := h.github.CompletedCheckRuns()[0].Body["output"].(map[string]any)
			summary, _ := out["summary"].(string)
			if !strings.Contains(summary, tc.want) {
				t.Fatalf("summary should say %q:\n%s", tc.want, summary)
			}
		})
	}
}

// TestRunnerEmptyPipelineCanFail covers on_empty_pipeline. An empty pipeline
// used to be indistinguishable from an intentional path-filter skip; an operator
// who considers it a misconfiguration can now make it loud.
func TestRunnerEmptyPipelineCanFail(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{"README.md": "just docs"})
	b := h.binding(t, store.BindingInput{
		Repo: "acme/api", Enabled: true, OnEmptyPipeline: store.OnEmptyPipelineFail,
	})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobError {
		t.Fatalf("on_empty_pipeline: fail should not skip, got %q", job.Status)
	}
	if h.github.CompletedCheckRuns()[0].Body["conclusion"] != "failure" {
		t.Fatalf("conclusion: %+v", h.github.CompletedCheckRuns()[0].Body)
	}
}

func TestRunnerEmptyPipelineSkipsByDefault(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{"README.md": "just docs"})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSkipped {
		t.Fatalf("the default must stay today's behaviour, got %q", job.Status)
	}
	// Distinguishable from a path-filter skip, which is the whole point.
	if job.SkipReason != store.SkipReasonNoPipeline {
		t.Fatalf("skip_reason %q, want %q", job.SkipReason, store.SkipReasonNoPipeline)
	}
}

// TestRunnerFailsWhenWorkspaceExceedsCap proves max_workspace_bytes is enforced
// this time. Migration 0004 dropped the setting precisely because it was stored,
// editable and never read.
func TestRunnerFailsWhenWorkspaceExceedsCap(t *testing.T) {
	h := newHarness(t)
	settings, _ := h.store.Settings()
	// Large enough that the checkout itself fits, so this exercises the
	// between-steps check rather than the one right after clone.
	settings.MaxWorkspaceBytes = 64 << 10
	if err := h.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: dd if=/dev/zero of=big.bin bs=1024 count=256 2>/dev/null\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobFailure {
		t.Fatalf("a workspace over the cap should fail, got %q err %q", job.Status, job.Error)
	}
	if !strings.Contains(job.Error, "max_workspace_bytes") {
		t.Fatalf("error should name the setting: %q", job.Error)
	}
}

// TestRunnerFailsWhenCheckoutAlreadyExceedsCap covers the other enforcement
// point: a repository too big for the cap fails before any step runs, and
// reports the same status as the between-steps case.
func TestRunnerFailsWhenCheckoutAlreadyExceedsCap(t *testing.T) {
	h := newHarness(t)
	settings, _ := h.store.Settings()
	settings.MaxWorkspaceBytes = 512
	if err := h.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: echo testing\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobFailure {
		t.Fatalf("status %q err %q", job.Status, job.Error)
	}
	body, _ := logs.Read(h.cfg.LogDir(), job.ID)
	if strings.Contains(body, "--- test ---") {
		t.Fatal("an oversized checkout must fail before running a step")
	}
}

func TestRunnerWorkspaceCapOfZeroDisablesTheCheck(t *testing.T) {
	h := newHarness(t)
	settings, _ := h.store.Settings()
	settings.MaxWorkspaceBytes = 0
	if err := h.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: dd if=/dev/zero of=big.bin bs=1024 count=64 2>/dev/null\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("zero means no cap, got %q err %q", job.Status, job.Error)
	}
}

// TestRunnerRecordsExecutorAndPlanSource pins the two facts the run page shows.
// Both are resolved after the clone and used to live only in the log file.
func TestRunnerRecordsExecutorAndPlanSource(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{
		".ci.yml": "test: echo testing\n",
	})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.Status != store.JobSuccess {
		t.Fatalf("status %q err %q", job.Status, job.Error)
	}
	// No runtime: in the pipeline file, so this ran in the worker process.
	if job.Runtime != "" {
		t.Fatalf("runtime %q, want empty for the process executor", job.Runtime)
	}
	if job.PlanSource != ".ci.yml" {
		t.Fatalf("plan_source %q, want %q", job.PlanSource, ".ci.yml")
	}
}

func TestRunnerRecordsBindingCommandsAsThePlanSource(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{"README.md": "docs"})
	b := h.binding(t, store.BindingInput{
		Repo: "acme/api", Enabled: true, TestCmd: "echo from-the-binding",
	})

	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
	})
	if job.PlanSource != "binding commands" {
		t.Fatalf("plan_source %q", job.PlanSource)
	}
}

func TestRunnerFailsWhenCheckRunCannotBeCreated(t *testing.T) {
	h := newHarness(t)
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{".ci.yml": "test: echo hi\n"})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})
	// Without checks:write the App cannot report anything; the job must not
	// silently look successful.
	h.github.FailNext("create-check")
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
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
	sha := testsupport.NewRepo(t, h.repos, "acme/api", map[string]string{".ci.yml": "test: echo hi\n"})
	b := h.binding(t, store.BindingInput{Repo: "acme/api", Enabled: true})
	job := h.runOne(t, store.JobInput{
		BindingID: b.ID, GitHubAppID: h.app.ID, Repo: "acme/api", SHA: sha, InstallationID: 101,
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

func TestCheckTitleNamesTheFailingStep(t *testing.T) {
	results := []executor.Result{
		{Name: "install", ExitCode: 0},
		{Name: "test", ExitCode: 1},
		{Name: "build", Skipped: true},
	}
	// The title is the only part visible in a pull request's collapsed check
	// list, so a bare "Failed" wastes the one line a reader gets.
	if got := titleFor("failure", results); got != "Failed: test (exit 1)" {
		t.Fatalf("title %q", got)
	}
	if got := titleFor("success", results); got != "Passed" {
		t.Fatalf("a passing run keeps the plain title, got %q", got)
	}
	timedOut := []executor.Result{{Name: "test", TimedOut: true, ExitCode: -1}}
	if got := titleFor("failure", timedOut); got != "Failed: test timed out" {
		t.Fatalf("title %q", got)
	}
	// A failure with no step results at all (a clone failure, say) must not
	// invent one.
	if got := titleFor("failure", nil); got != "Failed" {
		t.Fatalf("title %q", got)
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
		if got := githubapp.GitBaseURL(in); got != want {
			t.Errorf("GitBaseURL(%q) = %q want %q", in, got, want)
		}
	}
}
