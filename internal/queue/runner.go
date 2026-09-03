// SPDX-License-Identifier: Apache-2.0

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
	//
	// A job requeued after a crash or a redeploy already has a Check Run id, and
	// it must be reused: creating a second one leaves the first in_progress
	// forever, and a required check on that commit never resolves. This is the
	// invariant "one repository + commit + pipeline = one logical Check Run",
	// and it has to survive SIGKILL to mean anything.
	checkRunID := job.CheckRunID
	if checkRunID != 0 {
		err := client.ReopenCheckRun(ctx, job.InstallationID, githubapp.ReopenCheckRunInput{
			Repo:       job.Repo,
			CheckRunID: checkRunID,
			DetailsURL: detailsURL,
			Output:     &githubapp.CheckOutput{Title: "Running", Summary: "Restarted after an interruption; re-running the pipeline…"},
		})
		if err != nil {
			// The run may have been deleted, or the App reinstalled. Falling
			// back to create is exactly what this code did before, so the worst
			// case is the old behaviour rather than a failed job.
			r.log.Warn("could not reopen check run; creating a new one",
				"job", job.ID, "check_run", checkRunID, "error", err)
			w.Printf("could not reopen check run %d (%v); creating a new one\n", checkRunID, err)
			checkRunID = 0
		} else {
			w.Printf("reusing check run %d (%s) after an interruption\n\n", checkRunID, checkName)
		}
	}
	if checkRunID == 0 {
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
		checkRunID = run.ID
		if err := r.store.SetJobCheckRun(job.ID, checkRunID); err != nil {
			r.log.Error("record check run id", "job", job.ID, "error", err)
		}
		w.Printf("check run %d (%s)\n\n", checkRunID, checkName)
	}

	complete := func(conclusion string, results []executor.Result, note string) {
		body, _ := logs.Tail(r.cfg.LogDir(), job.ID, 50<<10)
		out := githubapp.CheckOutput{
			Title:   titleFor(conclusion, results),
			Summary: summarise(conclusion, results, note, detailsURL),
		}
		if body != "" {
			out.Text = "```\n" + body + "\n```"
		}
		if err := client.CompleteCheckRun(ctx, job.InstallationID, githubapp.CompleteCheckRunInput{
			Repo:       job.Repo,
			CheckRunID: checkRunID,
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

	// The webhook decided this job cannot run — a fork PR under the current
	// policy. It is enqueued anyway rather than dropped, so the Check Run exists
	// and *completes*: a required check with no check at all hangs the pull
	// request with nothing on screen to explain it.
	if job.SkipReason != "" {
		msg := skipExplanation(job.SkipReason)
		w.Printf("%s\n", msg)
		if err := r.store.FinishJobWithSkipReason(
			job.ID, store.JobSkipped, "skipped", "[]", "", job.SkipReason); err != nil {
			r.log.Error("finish pre-flight skip", "job", job.ID, "error", err)
		}
		complete("skipped", nil, msg)
		return nil
	}

	binding := store.RepoBinding{}
	if job.BindingID != 0 {
		if b, err := r.store.Binding(job.BindingID); err == nil {
			binding = b
		}
	}

	var token string
	var filterNote string
	if strings.TrimSpace(binding.Paths) != "" {
		var err error
		token, err = client.InstallationToken(ctx, job.InstallationID)
		if err != nil {
			msg := fmt.Sprintf("could not mint an installation token: %v", err)
			w.Printf("%s\n", msg)
			r.fail(job, nil, "", msg)
			complete("failure", nil, msg)
			return err
		}
		files, ferr := client.CommitFiles(ctx, job.InstallationID, job.Repo, job.SHA)
		switch {
		case ferr != nil || files.Truncated:
			// Fail open: skipping a commit because we could not see the file
			// list is worse than an extra run. Say so on the Check Run, not
			// only in this log — a reader on GitHub cannot see this file.
			why := "the file list was truncated by GitHub"
			if ferr != nil {
				why = "the changed-file lookup failed"
			}
			filterNote = fmt.Sprintf(
				"Path filter could not be evaluated (%s); the pipeline was executed.", why)
			w.Printf("path filter: fail-open (%s); running the job\n", why)
			w.Printf("  filter: %s\n\n", binding.Paths)
			r.log.Warn("path filter fail-open", "job", job.ID, "error", ferr, "truncated", files.Truncated)
		default:
			changed := files.ChangedPaths()
			matched := binding.MatchedPaths(changed)
			allowed := len(matched) > 0
			diag := PathFilterDiagnostic(binding.Paths, len(changed), len(matched), allowed)
			// Log the diagnostic on every outcome, not only on a skip: "why did
			// this run?" is as common a question as "why did it not?".
			w.Printf("%s\n", diag)
			if !allowed {
				filterNote = diag
				if err := r.store.FinishJobWithSkipReason(
					job.ID, store.JobSkipped, "skipped", "[]", "", store.SkipReasonPathFilter); err != nil {
					r.log.Error("finish path-filter skip", "job", job.ID, "error", err)
				}
				complete("skipped", nil, diag)
				return nil
			}
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

	if token == "" {
		var err error
		token, err = client.InstallationToken(ctx, job.InstallationID)
		if err != nil {
			msg := fmt.Sprintf("could not mint an installation token: %v", err)
			w.Printf("%s\n", msg)
			r.fail(job, nil, "", msg)
			complete("failure", nil, msg)
			return err
		}
	}

	cloneStart := time.Now()
	if err := ws.Clone(ctx, workspace.CloneOptions{
		Repo:       job.Repo,
		SHA:        job.SHA,
		Token:      token,
		BaseURL:    githubapp.GitBaseURL(app.APIURL),
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

	// A repository larger than the cap fails here rather than part way through a
	// build. Filling the server's disk takes down the whole worker, not one job.
	//
	// JobFailure rather than JobError, matching the between-steps check below:
	// it is the same condition and an operator should not have to know which
	// check caught it to find the job in the list.
	if err := ws.CheckSize(settings.MaxWorkspaceBytes); err != nil {
		w.Printf("%v\n", err)
		if ferr := r.store.FinishJob(job.ID, store.JobFailure, "failure", "[]", err.Error()); ferr != nil {
			r.log.Error("finish oversized workspace", "job", job.ID, "error", ferr)
		}
		complete("failure", nil, err.Error())
		return err
	}

	pipelineFile := firstNonEmpty(binding.PipelineFile, settings.DefaultPipelineFile, ".ci.yml")
	plan, err := pipeline.Resolve(ws.Repo, pipeline.Inputs{
		PipelineFile:       pipelineFile,
		PipelineFileSource: pipeline.Layer(binding.PipelineFile != "", settings.DefaultPipelineFile != ""),
		Overrides: pipeline.Overrides{
			Install: binding.InstallCmd,
			Test:    binding.TestCmd,
			Build:   binding.BuildCmd,
		},
		DefaultTimeout:       timeout,
		DefaultTimeoutSource: pipeline.Layer(binding.TimeoutSeconds > 0, settings.DefaultTimeoutSeconds > 0),
		DefaultRuntime:       settings.DefaultRuntime,
		IsFork:               job.IsFork,
	})
	if errors.Is(err, pipeline.ErrNothingToRun) {
		msg := fmt.Sprintf("no %s, no binding commands and no recognisable project — nothing to run", pipelineFile)
		w.Printf("%s\n", msg)
		// An empty pipeline is a different thing from a path-filter skip: the
		// filter case is intentional, this one is usually a misconfiguration.
		// Which of the two happened is now recorded, and an operator who wants
		// the mistake to be loud can set on_empty_pipeline: fail.
		if binding.OnEmptyPipeline == store.OnEmptyPipelineFail {
			note := msg + "\n\nThis binding sets on_empty_pipeline: fail."
			r.fail(job, nil, "", msg)
			complete("failure", nil, note)
			return nil
		}
		if err := r.store.FinishJobWithSkipReason(
			job.ID, store.JobSkipped, "skipped", "[]", "", store.SkipReasonNoPipeline); err != nil {
			r.log.Error("finish empty-pipeline skip", "job", job.ID, "error", err)
		}
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
	for _, warning := range plan.Warnings {
		w.Printf("warning: %s\n", warning)
	}
	// plan.Runtime already includes the fork fallback to settings.default_runtime,
	// applied inside Resolve so that it carries an origin like every other value.
	image := strings.TrimSpace(plan.Runtime)
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
	// Record what was resolved. These values are decided here, after the clone,
	// and used to exist only in this log file. The origins are what let the run
	// page say the timeout came from settings while the image came from the
	// pipeline file, which one summary string cannot.
	origins, err := json.Marshal(plan.Origins)
	if err != nil {
		r.log.Error("encode plan origins", "job", job.ID, "error", err)
		origins = nil
	}
	if err := r.store.SetJobPlan(job.ID, image, plan.Source, string(origins)); err != nil {
		r.log.Error("record job plan", "job", job.ID, "error", err)
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
	var workspaceErr error
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
			continue
		}
		// Between steps rather than during: a walk is cheap next to a compile,
		// and an install step is what usually blows the budget.
		if err := ws.CheckSize(settings.MaxWorkspaceBytes); err != nil {
			w.Printf("\n%v\n", err)
			res.Err = err.Error()
			res.ExitCode = -1
			results[len(results)-1] = res
			failed = true
			workspaceErr = err
		}
	}

	stepsJSON, _ := json.Marshal(results)
	switch {
	case ctx.Err() != nil || anyTimedOut(results):
		r.finishCancelled(job, results, firstNonNilErr(ctx.Err(), errors.New("timed out")))
		complete(conclusionForResults(ctx, results), results, filterNote)
	case failed:
		note := filterNote
		if workspaceErr != nil {
			note = workspaceErr.Error()
		}
		r.store.FinishJob(job.ID, store.JobFailure, "failure", string(stepsJSON), errString(workspaceErr))
		complete("failure", results, note)
	default:
		r.store.FinishJob(job.ID, store.JobSuccess, "success", string(stepsJSON), "")
		complete("success", results, filterNote)
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

// errString is "" for a nil error, so a job row's error column stays empty on an
// ordinary build failure.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
