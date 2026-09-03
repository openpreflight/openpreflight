// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/githubapp"
	"github.com/openpreflight/openpreflight/internal/pipeline"
	"github.com/openpreflight/openpreflight/internal/queue"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/workspace"
)

// resolveDeadline bounds a dry run. It clones, so it is not instant, but an
// operator waiting on a page needs an answer or an error rather than a spinner.
const resolveDeadline = 90 * time.Second

// resolveBinding answers "what would this repository run, on this ref?" without
// pushing a commit.
//
// It clones. Precedence is decided by whether the pipeline file exists *in that
// commit*, which is only knowable from a checkout, so an answer derived from
// the binding row alone would not be the answer the worker gets — and being the
// same answer is the entire point.
//
// It never touches the Checks API, never writes a job row and never enqueues. A
// dry run that could report a status on a commit would not be a dry run.
//
// The returned error means the dry run could not be attempted at all (no such
// binding, no usable App, unreachable GitHub, failed clone). Everything wrong
// with the *configuration* comes back inside the result.
func (s *Server) resolveBinding(ctx context.Context, id int64, ref string) (pipeline.Resolution, error) {
	binding, err := s.store.Binding(id)
	if err != nil {
		return pipeline.Resolution{}, err
	}
	settings, err := s.store.Settings()
	if err != nil {
		return pipeline.Resolution{}, err
	}
	client, app, err := s.appClient(binding.GitHubAppID)
	if err != nil {
		return pipeline.Resolution{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, resolveDeadline)
	defer cancel()

	installID, err := s.installationFor(ctx, client, binding.Repo)
	if err != nil {
		return pipeline.Resolution{}, err
	}

	out := pipeline.Resolution{
		Repo:      binding.Repo,
		Ref:       strings.TrimSpace(ref),
		Decision:  pipeline.DecisionRun,
		CheckName: firstNonEmpty(binding.CheckName, settings.DefaultCheckName, "openpreflight"),
		Warnings:  []string{},
		Errors:    []string{},
		Steps:     []pipeline.ResolvedStep{},
	}
	if out.Ref == "" {
		info, err := client.Repo(ctx, installID, binding.Repo)
		if err != nil {
			return pipeline.Resolution{}, fmt.Errorf("could not read %s to find its default branch: %w", binding.Repo, err)
		}
		out.Ref = info.DefaultBranch
	}

	// One call answers both questions a dry run needs: which immutable SHA the
	// ref points at, and which paths that commit changed.
	files, err := client.CommitFiles(ctx, installID, binding.Repo, out.Ref)
	if err != nil {
		return pipeline.Resolution{}, fmt.Errorf("could not resolve %s@%s: %w", binding.Repo, out.Ref, err)
	}
	out.SHA = files.SHA
	if out.SHA == "" {
		return pipeline.Resolution{}, fmt.Errorf("%s has no commit at %q", binding.Repo, out.Ref)
	}

	s.resolvePathFilter(binding, files, &out)
	if err := s.resolvePlan(ctx, client, app, installID, binding, settings, &out); err != nil {
		return pipeline.Resolution{}, err
	}
	if !binding.Enabled {
		out.Warn("This binding is disabled, so webhooks for %s are ignored. "+
			"The plan below is what it would run if you enabled it.", binding.Repo)
	}
	return out, nil
}

// resolvePathFilter evaluates the binding's filter against the same commit the
// worker would see, using the runner's own diagnostic so the two cannot drift.
func (s *Server) resolvePathFilter(binding store.RepoBinding, files githubapp.CommitFiles, out *pipeline.Resolution) {
	if strings.TrimSpace(binding.Paths) == "" {
		return
	}
	if files.Truncated {
		// Same fail-open posture as a real run: skipping a commit because we
		// could not see its file list is worse than an extra run.
		out.PathFilter = "Path filter could not be evaluated (the file list was truncated by GitHub); the pipeline would be executed."
		out.Warn("GitHub truncated the changed-file list for this commit, so the path filter could not be evaluated. A real run would fail open and execute.")
		return
	}
	changed := files.ChangedPaths()
	matched := binding.MatchedPaths(changed)
	allowed := len(matched) > 0
	out.PathFilter = queue.PathFilterDiagnostic(binding.Paths, len(changed), len(matched), allowed)
	if !allowed {
		out.Decision = pipeline.DecisionSkip
		out.SkipReason = store.SkipReasonPathFilter
		out.Explanation = "No changed path in this commit matched the binding's path filter."
		out.Warn("Nothing in this commit matched the path filter %q. That is the filter working, "+
			"but if you expected a run, check the filter against the changed paths.", binding.Paths)
	}
}

// resolvePlan clones the commit and resolves the plan the worker would run.
func (s *Server) resolvePlan(
	ctx context.Context,
	client *githubapp.Client,
	app store.GitHubApp,
	installID int64,
	binding store.RepoBinding,
	settings store.Settings,
	out *pipeline.Resolution,
) error {
	token, err := client.InstallationToken(ctx, installID)
	if err != nil {
		return err
	}
	// "resolve-" prefixed so a dry run's directory can never collide with a
	// live job's, which Prepare would delete.
	ws, err := workspace.Prepare(s.cfg.WorkspaceDir(), "resolve-"+store.NewJobID())
	if err != nil {
		return err
	}
	defer func() {
		if err := ws.Cleanup(); err != nil {
			s.log.Error("dry-run workspace cleanup", "repo", binding.Repo, "error", err)
		}
	}()

	if err := ws.Clone(ctx, workspace.CloneOptions{
		Repo:    binding.Repo,
		SHA:     out.SHA,
		Token:   token,
		BaseURL: githubapp.GitBaseURL(app.APIURL),
	}, io.Discard); err != nil {
		return fmt.Errorf("could not check out %s: %w", shortSHA(out.SHA), err)
	}
	if err := ws.CheckSize(settings.MaxWorkspaceBytes); err != nil {
		// Not fatal to the dry run — the point is to report it before a real
		// run hits it.
		out.Err("%v", err)
	}

	pipelineFile := firstNonEmpty(binding.PipelineFile, settings.DefaultPipelineFile, ".ci.yml")
	out.PipelineFile = pipelineFile
	timeout := resolvedTimeout(binding, settings)

	// Validate before resolving. Resolve stops at the first problem because a
	// job cannot run on a broken file; an operator who is about to fix all of
	// them should not have to fix one to discover the next.
	spec, problems := pipeline.Validate(ws.Repo, pipelineFile)
	for _, p := range problems {
		out.Decision = pipeline.DecisionFail
		out.Err("%s", p)
	}
	if image := strings.TrimSpace(spec.Runtime); image != "" {
		out.Executor = "docker: " + image
		if err := executor.ValidImage(image); err != nil {
			out.Decision = pipeline.DecisionFail
			out.Err("%s: runtime: %v", pipelineFile, err)
		}
	}

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
		// A dry run resolves the repository's own branch, not a fork PR, so the
		// fork runtime fallback does not apply. Say so rather than implying the
		// answer covers fork pull requests too.
		IsFork: false,
	})
	out.Timeout = timeout.String()

	switch {
	case errors.Is(err, pipeline.ErrNothingToRun):
		out.SkipReason = store.SkipReasonNoPipeline
		out.Explanation = fmt.Sprintf(
			"Nothing to run: no %s in this commit, no binding commands, and no recognisable project.",
			pipelineFile)
		if binding.OnEmptyPipeline == store.OnEmptyPipelineFail {
			out.Decision = pipeline.DecisionFail
			out.Err("This binding sets on_empty_pipeline: fail, so an empty plan fails the check.")
			return nil
		}
		if out.Decision == pipeline.DecisionRun {
			out.Decision = pipeline.DecisionSkip
		}
		out.Warn("This commit has no plan. Add %s, set commands on the binding, "+
			"or leave it — the check will conclude `skipped`.", pipelineFile)
		return nil
	case err != nil:
		// A bad pipeline file. Report it and keep going: the operator still
		// wants the check name, the timeout and the filter verdict. Validate
		// above has usually already said the same thing in better words, so do
		// not say it twice.
		out.Decision = pipeline.DecisionFail
		if len(problems) == 0 {
			out.Err("%s", cleanPipelineError(err))
		}
		return nil
	}

	out.Origins = plan.Origins
	out.Warnings = append(out.Warnings, plan.Warnings...)
	for _, step := range plan.Steps {
		out.Steps = append(out.Steps, pipeline.ResolvedStep{
			Name: step.Name, Command: step.Command, Source: plan.OriginOf(step.Name),
		})
	}
	if plan.Timeout > 0 {
		out.Timeout = plan.Timeout.String()
	}
	s.resolveExecutor(plan, out)
	return nil
}

// resolveExecutor validates the runtime the plan asks for. An unreachable
// engine is a warning, not an error: it says something about this host right
// now and nothing about the commit.
func (s *Server) resolveExecutor(plan pipeline.Plan, out *pipeline.Resolution) {
	image := strings.TrimSpace(plan.Runtime)
	if image == "" {
		out.Executor = "worker process"
		return
	}
	out.Executor = "docker: " + image
	if err := executor.ValidImage(image); err != nil {
		// Already reported by the pre-flight validation when the image came
		// from the pipeline file; this catches the settings-derived case.
		out.Decision = pipeline.DecisionFail
		if len(out.Errors) == 0 {
			out.Err("%v", err)
		}
		return
	}
	if !s.dockerAvailable() {
		out.Warn("This plan needs Docker for %s, but no engine is reachable from this server right now. "+
			"Set CI_DOCKER_HOST or mount the engine socket.", image)
	}
}

// installationFor finds the installation to authenticate as. The newest job for
// the repository already recorded one, which saves a fan-out over every
// installation; a binding that has never run has no such row, and that is
// exactly when a dry run is most useful, so it falls back to asking GitHub.
func (s *Server) installationFor(ctx context.Context, client *githubapp.Client, repo string) (int64, error) {
	jobs, err := s.store.ListJobs(store.JobList{Repo: repo, Limit: 1})
	if err == nil && len(jobs) > 0 && jobs[0].InstallationID != 0 {
		return jobs[0].InstallationID, nil
	}
	return client.InstallationForRepo(ctx, repo)
}

// resolvedTimeout is the runner's binding → settings → built-in chain. Kept
// here rather than exported from queue because it reads two rows the API
// already has in hand.
func resolvedTimeout(binding store.RepoBinding, settings store.Settings) time.Duration {
	if binding.TimeoutSeconds > 0 {
		return time.Duration(binding.TimeoutSeconds) * time.Second
	}
	if settings.DefaultTimeoutSeconds > 0 {
		return time.Duration(settings.DefaultTimeoutSeconds) * time.Second
	}
	return 15 * time.Minute
}

// cleanPipelineError strips the package prefix so the message reads as a
// sentence about the operator's file rather than about our Go packages.
func cleanPipelineError(err error) string {
	return strings.TrimPrefix(err.Error(), "pipeline: ")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// resolveBindingAPI is POST /api/v1/bindings/{id}/resolve.
func (s *Server) resolveBindingAPI(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	if ref == "" {
		if in, err := readInput(r); err == nil {
			ref = in.Str("ref")
		}
	}
	result, err := s.resolveBinding(r.Context(), id, ref)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
