package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/openpreflight/openpreflight/internal/config"
	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/githubapp"
	"github.com/openpreflight/openpreflight/internal/logs"
	"github.com/openpreflight/openpreflight/internal/pipeline"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/workspace"
)

// pollInterval is the fallback for the notify channel: a job enqueued while the
// loop was busy still gets picked up.
const pollInterval = 5 * time.Second

// pruneInterval is how often retention runs. Retention itself is in days, so
// hourly is plenty.
const pruneInterval = time.Hour

// Runner is the data plane: it claims queued jobs and runs them.
type Runner struct {
	store   *store.Store
	cfg     config.Config
	exec    executor.Executor
	clients *clientCache
	log     *slog.Logger

	notify chan struct{}

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// New builds a runner. Jobs with an empty runtime use the process executor;
// a pipeline `runtime:` (or a fork job) switches to Docker for that job.
func New(st *store.Store, cfg config.Config, log *slog.Logger) *Runner {
	return &Runner{
		store:   st,
		cfg:     cfg,
		exec:    executor.Process{},
		clients: newClientCache(),
		log:     log,
		notify:  make(chan struct{}, 1),
		running: map[string]context.CancelFunc{},
	}
}

// Notify wakes the loop after an enqueue.
func (r *Runner) Notify() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// DropClient forgets a cached client for a deleted App.
func (r *Runner) DropClient(appID int64) { r.clients.Drop(appID) }

// Start runs the claim loop until ctx is done. A job interrupted by a restart is
// requeued first: Coolify redeploys restart the container mid-job.
func (r *Runner) Start(ctx context.Context) {
	if n, err := r.store.RequeueStaleJobs(); err != nil {
		r.log.Error("requeue interrupted jobs", "error", err)
	} else if n > 0 {
		r.log.Info("requeued jobs interrupted by a restart", "count", n)
	}
	go r.pruneLoop(ctx)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		r.drain(ctx)
		select {
		case <-ctx.Done():
			r.waitForRunning()
			return
		case <-r.notify:
		case <-ticker.C:
		}
	}
}

// drain claims as many jobs as the concurrency limit allows.
func (r *Runner) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		settings, err := r.store.Settings()
		if err != nil {
			r.log.Error("read settings", "error", err)
			return
		}
		limit := settings.MaxConcurrentJobs
		if limit < 1 {
			limit = 1
		}
		if r.activeCount() >= limit {
			return
		}
		job, err := r.store.ClaimNextJob()
		if errors.Is(err, store.ErrNotFound) {
			return
		}
		if err != nil {
			r.log.Error("claim job", "error", err)
			return
		}
		r.startJob(ctx, job, settings)
	}
}

func (r *Runner) activeCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}

func (r *Runner) waitForRunning() {
	for r.activeCount() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
}

// startJob launches one job in its own goroutine with its own timeout.
func (r *Runner) startJob(parent context.Context, job store.Job, settings store.Settings) {
	timeout := r.resolveTimeout(job, settings)
	// The job outlives a cancelled parent only long enough to be marked
	// cancelled; context.WithoutCancel would let a shutdown leave a running
	// build orphaned, so the parent is respected.
	ctx, cancel := context.WithTimeout(parent, timeout)
	r.mu.Lock()
	r.running[job.ID] = cancel
	r.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			r.mu.Lock()
			delete(r.running, job.ID)
			r.mu.Unlock()
			// A finished job frees a slot; look for the next one.
			r.Notify()
		}()
		if err := r.runJob(ctx, job, settings, timeout); err != nil {
			r.log.Error("job failed", "job", job.ID, "repo", job.Repo, "error", err)
		}
	}()
}

// resolveTimeout applies binding → settings precedence. The pipeline file can
// still lower (or raise) it once the checkout exists.
func (r *Runner) resolveTimeout(job store.Job, settings store.Settings) time.Duration {
	if job.BindingID != 0 {
		if b, err := r.store.Binding(job.BindingID); err == nil && b.TimeoutSeconds > 0 {
			return time.Duration(b.TimeoutSeconds) * time.Second
		}
	}
	if settings.DefaultTimeoutSeconds > 0 {
		return time.Duration(settings.DefaultTimeoutSeconds) * time.Second
	}
	return 15 * time.Minute
}

// CancelJob stops a running job and marks it cancelled if it is only queued.
func (r *Runner) CancelJob(id string) bool {
	r.mu.Lock()
	cancel, ok := r.running[id]
	r.mu.Unlock()
	if ok {
		cancel()
		return true
	}
	job, err := r.store.Job(id)
	if err != nil || !job.InFlight() {
		return false
	}
	if err := r.store.FinishJob(id, store.JobCancelled, "cancelled", "[]", "superseded"); err != nil {
		r.log.Error("cancel queued job", "job", id, "error", err)
		return false
	}
	return true
}

// runJob is the whole data plane for one job.
func (r *Runner) runJob(ctx context.Context, job store.Job, settings store.Settings, timeout time.Duration) error {
	w, err := logs.Create(r.cfg.LogDir(), job.ID, settings.MaxLogBytes)
	if err != nil {
		r.fail(job, nil, "", fmt.Sprintf("could not open log file: %v", err))
		return err
	}
	defer func() {
		r.store.SetJobLogBytes(job.ID, w.Bytes())
		w.Close()
	}()

	w.Printf("%s  %s @ %s\n", job.Repo, orDash(job.Ref), shortSHA(job.SHA))
	w.Printf("job %s  event %s  delivery %s\n", job.ID, job.Event, orDash(job.DeliveryID))
	w.Printf("timeout %s\n\n", timeout)

	app, err := r.store.GitHubApp(job.GitHubAppID)
	if err != nil {
		msg := fmt.Sprintf("github app %d is gone: %v", job.GitHubAppID, err)
		w.Printf("%s\n", msg)
		r.fail(job, nil, "", msg)
		return err
	}
	client, err := r.clients.For(r.store, app)
	if err != nil {
		w.Printf("%v\n", err)
		r.fail(job, nil, "", err.Error())
		return err
	}

	checkName := job.CheckName
	if checkName == "" {
		checkName = firstNonEmpty(app.CheckName, settings.DefaultCheckName, "openpreflight")
	}
	// Persist the resolved name so the Jobs API matches what GitHub shows.
	if err := r.store.SetJobCheckName(job.ID, checkName); err != nil {
		r.log.Error("record check name", "job", job.ID, "error", err)
	}
	detailsURL := r.detailsURL(settings, job.ID)

	// The Check Run goes up before any git work so a slow clone still shows a
	// running check on the commit rather than nothing at all.
	run, err := client.CreateCheckRun(ctx, job.InstallationID, githubapp.CreateCheckRunInput{
		Repo:       job.Repo,
		Name:       checkName,
		HeadSHA:    job.SHA,
		DetailsURL: detailsURL,
		Status:     "in_progress",
		Output:     &githubapp.CheckOutput{Title: "Running", Summary: "Starting pipeline…"},
	})
	if err != nil {
		msg := fmt.Sprintf("could not create the Check Run: %v", err)
		w.Printf("%s\n", msg)
		r.fail(job, nil, "", msg)
		return err
	}
	if err := r.store.SetJobCheckRun(job.ID, run.ID); err != nil {
		r.log.Error("record check run id", "job", job.ID, "error", err)
	}
	w.Printf("check run %d (%s)\n\n", run.ID, checkName)

	complete := func(conclusion string, results []executor.Result, note string) {
		body, _ := logs.Tail(r.cfg.LogDir(), job.ID, 50<<10)
		out := githubapp.CheckOutput{
			Title:   checkTitle(conclusion),
			Summary: summarise(conclusion, results, note, detailsURL),
		}
		if body != "" {
			out.Text = "```\n" + body + "\n```"
		}
		if err := client.CompleteCheckRun(ctx, job.InstallationID, githubapp.CompleteCheckRunInput{
			Repo:       job.Repo,
			CheckRunID: run.ID,
			Conclusion: conclusion,
			DetailsURL: detailsURL,
			Output:     &out,
		}); err != nil {
			// The build result is already in the database and the log; a failed
			// PATCH must not be reported as a build failure.
			r.log.Error("complete check run", "job", job.ID, "error", err)
			w.Printf("\nwarning: could not update the Check Run: %v\n", err)
		}
	}

	ws, err := workspace.Prepare(r.cfg.WorkspaceDir(), job.ID)
	if err != nil {
		w.Printf("%v\n", err)
		r.fail(job, nil, "", err.Error())
		complete("failure", nil, err.Error())
		return err
	}
	defer func() {
		if err := ws.Cleanup(); err != nil {
			r.log.Error("workspace cleanup", "job", job.ID, "error", err)
		}
	}()

	token, err := client.InstallationToken(ctx, job.InstallationID)
	if err != nil {
		msg := fmt.Sprintf("could not mint an installation token: %v", err)
		w.Printf("%s\n", msg)
		r.fail(job, nil, "", msg)
		complete("failure", nil, msg)
		return err
	}

	cloneStart := time.Now()
	if err := ws.Clone(ctx, workspace.CloneOptions{
		Repo:       job.Repo,
		SHA:        job.SHA,
		Token:      token,
		BaseURL:    gitBaseURL(app.APIURL),
		PullNumber: job.PullNumber,
	}, w); err != nil {
		msg := fmt.Sprintf("clone failed: %v", err)
		w.Printf("%s\n", msg)
		if ctx.Err() != nil {
			r.finishCancelled(job, nil, ctx.Err())
			complete(conclusionForCtx(ctx), nil, "cancelled or timed out during clone")
			return ctx.Err()
		}
		r.fail(job, nil, "", msg)
		complete("failure", nil, msg)
		return err
	}
	w.Printf("checked out %s in %s\n\n", shortSHA(job.SHA), time.Since(cloneStart).Round(time.Millisecond))

	binding := store.RepoBinding{}
	if job.BindingID != 0 {
		if b, err := r.store.Binding(job.BindingID); err == nil {
			binding = b
		}
	}
	pipelineFile := firstNonEmpty(binding.PipelineFile, settings.DefaultPipelineFile, ".ci.yml")
	plan, err := pipeline.Resolve(ws.Repo, pipelineFile, pipeline.Overrides{
		Install: binding.InstallCmd,
		Test:    binding.TestCmd,
		Build:   binding.BuildCmd,
	}, timeout)
	if errors.Is(err, pipeline.ErrNothingToRun) {
		msg := fmt.Sprintf("no %s, no binding commands and no package.json — nothing to run", pipelineFile)
		w.Printf("%s\n", msg)
		r.store.FinishJob(job.ID, store.JobSkipped, "skipped", "[]", "")
		complete("skipped", nil, msg)
		return nil
	}
	if err != nil {
		w.Printf("%v\n", err)
		r.fail(job, nil, "", err.Error())
		complete("failure", nil, err.Error())
		return err
	}

	w.Printf("plan from %s\n", plan.Source)
	image := strings.TrimSpace(plan.Runtime)
	if job.IsFork && image == "" {
		image = strings.TrimSpace(settings.DefaultRuntime)
	}
	exec := r.exec
	if image != "" {
		if err := executor.ValidImage(image); err != nil {
			w.Printf("%v\n", err)
			r.fail(job, nil, "", err.Error())
			complete("failure", nil, err.Error())
			return err
		}
		d := executor.Docker{Host: r.cfg.DockerHost, Image: image}
		if !d.Available(ctx) {
			msg := fmt.Sprintf("runtime %q needs Docker, but the engine is not reachable (set CI_DOCKER_HOST or mount docker.sock)", image)
			w.Printf("%s\n", msg)
			r.fail(job, nil, "", msg)
			complete("failure", nil, msg)
			return errors.New(msg)
		}
		exec = d
		w.Printf("runtime %s via docker\n", image)
	} else {
		w.Printf("runtime: worker process\n")
	}
	// A pipeline file may set its own timeout, which the outer context does not
	// know about. Tighten (or extend) here.
	stepCtx := ctx
	if plan.Timeout > 0 && plan.Timeout != timeout {
		var cancel context.CancelFunc
		stepCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), plan.Timeout)
		defer cancel()
		// Keep honouring shutdown/cancellation of the outer context.
		go func() {
			<-ctx.Done()
			cancel()
		}()
		w.Printf("timeout from %s: %s\n", plan.Source, plan.Timeout)
	}
	w.Printf("\n")

	env := executor.BaseEnv(ws.Root, map[string]string{
		"GITHUB_REPOSITORY": job.Repo,
		"GITHUB_SHA":        job.SHA,
		"GITHUB_REF_NAME":   job.Ref,
		"CI_JOB_ID":         job.ID,
	})

	var results []executor.Result
	failed := false
	for _, step := range plan.Steps {
		if failed {
			results = append(results, executor.Result{Name: step.Name, Command: step.Command, Skipped: true})
			w.Printf("\n--- %s: skipped (an earlier step failed) ---\n", step.Name)
			continue
		}
		w.Printf("\n--- %s ---\n$ %s\n", step.Name, step.Command)
		res := exec.Run(stepCtx, executor.Step{
			Name:    step.Name,
			Command: step.Command,
			Dir:     ws.Repo,
			Env:     env,
		}, w)
		results = append(results, res)
		w.Printf("\n--- %s: %s in %s ---\n", step.Name, outcome(res), res.Duration.Round(time.Millisecond))
		if !res.OK() {
			failed = true
		}
	}

	stepsJSON, _ := json.Marshal(results)
	switch {
	case ctx.Err() != nil || anyTimedOut(results):
		r.finishCancelled(job, results, firstNonNilErr(ctx.Err(), errors.New("timed out")))
		complete(conclusionForResults(ctx, results), results, "")
	case failed:
		r.store.FinishJob(job.ID, store.JobFailure, "failure", string(stepsJSON), "")
		complete("failure", results, "")
	default:
		r.store.FinishJob(job.ID, store.JobSuccess, "success", string(stepsJSON), "")
		complete("success", results, "")
	}
	return nil
}

// fail records a terminal error that is ours, not the build's.
func (r *Runner) fail(job store.Job, results []executor.Result, conclusion, msg string) {
	steps, _ := json.Marshal(results)
	if err := r.store.FinishJob(job.ID, store.JobError, firstNonEmpty(conclusion, "failure"), string(steps), msg); err != nil {
		r.log.Error("finish job", "job", job.ID, "error", err)
	}
}

func (r *Runner) finishCancelled(job store.Job, results []executor.Result, cause error) {
	steps, _ := json.Marshal(results)
	conclusion := "cancelled"
	if errors.Is(cause, context.DeadlineExceeded) || anyTimedOut(results) {
		conclusion = "timed_out"
	}
	if err := r.store.FinishJob(job.ID, store.JobCancelled, conclusion, string(steps), conclusion); err != nil {
		r.log.Error("finish cancelled job", "job", job.ID, "error", err)
	}
}

// detailsURL is what GitHub links to from the Checks tab. The browser fetches
// it, not GitHub, so it points at our session-gated log page (AUDIT.md).
func (r *Runner) detailsURL(settings store.Settings, jobID string) string {
	base := strings.TrimRight(firstNonEmpty(settings.PublicBaseURL, r.cfg.PublicBaseURL), "/")
	if base == "" {
		return ""
	}
	return base + "/runs/" + url.PathEscape(jobID)
}

// pruneLoop enforces log retention.
func (r *Runner) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		r.pruneOnce()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Runner) pruneOnce() {
	settings, err := r.store.Settings()
	if err != nil {
		r.log.Error("prune: read settings", "error", err)
		return
	}
	if err := r.store.PruneSessions(); err != nil {
		r.log.Error("prune sessions", "error", err)
	}
	if settings.LogRetentionDays <= 0 {
		return
	}
	ids, err := r.store.PruneJobs(time.Duration(settings.LogRetentionDays) * 24 * time.Hour)
	if err != nil {
		r.log.Error("prune jobs", "error", err)
	}
	for _, id := range ids {
		if err := logs.Delete(r.cfg.LogDir(), id); err != nil {
			r.log.Error("prune log", "job", id, "error", err)
		}
	}
	if len(ids) > 0 {
		r.log.Info("pruned jobs past retention", "count", len(ids), "retention_days", settings.LogRetentionDays)
	}
}

// gitBaseURL derives the git origin from the App's API URL so GitHub Enterprise
// works: api.github.com → https://github.com, ghe.example.com/api/v3 →
// https://ghe.example.com.
func gitBaseURL(apiURL string) string {
	if apiURL == "" {
		return "https://github.com"
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return "https://github.com"
	}
	if u.Host == "api.github.com" {
		return "https://github.com"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func firstNonNilErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func anyTimedOut(results []executor.Result) bool {
	for _, r := range results {
		if r.TimedOut {
			return true
		}
	}
	return false
}

func conclusionForCtx(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timed_out"
	}
	return "cancelled"
}

func conclusionForResults(ctx context.Context, results []executor.Result) string {
	if anyTimedOut(results) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timed_out"
	}
	return "cancelled"
}

func outcome(r executor.Result) string {
	switch {
	case r.Skipped:
		return "skipped"
	case r.TimedOut:
		return "timed out"
	case r.OK():
		return "passed"
	default:
		return fmt.Sprintf("failed (exit %d)", r.ExitCode)
	}
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func orDash(v string) string {
	if v == "" {
		return "—"
	}
	return v
}
