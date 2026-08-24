# ADR 005: Check-suite-driven gating at single-server scale

- Status: accepted
- Date: 2026-08-24

## Context

The worker has always listened to `check_suite` and `check_run` and nothing
else. `internal/webhook.Parse` handles `check_suite` (`requested`,
`rerequested`) and `check_run` (`rerequested`); `push` and `pull_request` fall
through to the skip branch. That was never written down, so the most
consequential decision in the trigger path was the easiest one to undo — adding
a `push` case looks like a feature, not a reversal.

The model has prior art. Zuul gates on the commit, queues work against an
immutable SHA, attaches logs to the run, and reports the result back to the
forge. Its architecture does not travel: ZooKeeper for coordination, Nodepool
for node lifecycle, Ansible as the execution layer, a multi-tenant config model
and a scheduler separate from the executors. This project is one Go binary, one
SQLite file, one server.

Two duplicate-check bugs also came out of leaving the invariant unstated. The
handler deduped on `delivery_id` and cancelled on `(repo, ref)`, so a commit
arriving on a second ref, or a rerequest landing while a job ran, produced two
concurrent jobs — and two Check Runs with the same name on the same commit.
Branch protection then reads whichever finished last.

## Decision

- **Trigger on `check_suite` and `check_run` only.** Never `push`, never
  `pull_request`. GitHub creates the suite; a suite is the unit of work and it
  is already scoped to one commit and one App. `push` would fire for refs no
  one is reviewing, and `pull_request` would fire on metadata edits that do not
  change the tree.
- **One Check Run per job.** Steps are rendered as a table in that run's
  `output.summary` (`internal/queue/summary.go`), not as separate runs. One run
  means one required-status entry in a user's branch protection, and adding a
  step never breaks it.
- **One live run per `(github_app_id, repo, sha)`.** Enforced in the handler,
  not by a database constraint: `Runner.CancelJob` cancels the context and
  returns, while the `cancelled` status is written later by the job's own
  goroutine, so a partial unique index over the in-flight statuses would
  intermittently reject the follow-up insert. `check_suite_id` is recorded on
  the job for traceability but is not the key — it can be absent from a payload,
  and a missing id must not weaken the invariant.
- **`ev.Action` decides what a second delivery means.** `requested` is GitHub
  asking twice and is answered `already queued`. `rerequested` is a human
  pressing Re-run and supersedes the run in flight.
- **Zuul is the referenced model, not the implementation.** Suite/run semantics,
  immutable SHA, explicit gating, queueing, logs on the run, result written back
  to GitHub. No ZooKeeper, no Nodepool, no Ansible, no multi-tenant config, no
  second scheduler.

## Consequences

- The ceiling is real and worth stating: no build matrices, no dependency
  caching, no fan-out across machines, no cross-repo dependent pipelines. A
  queue depth of one server is the design, not a limitation to be worked around.
- Per-step Check Runs are rejected while the executor runs steps sequentially in
  one shell and one workspace. GitHub renders a Re-run button per check run, and
  a step that cannot be re-run alone must not advertise one. If the Docker
  executor ([ADR 004](004-docker-executor.md)) later makes steps independently
  schedulable, this is the decision to revisit — the recorded `check_suite_id`
  is what makes that cheap.
- A local `check_suites` table is rejected. Suites cannot be created through the
  API and GitHub computes suite status from the runs inside them, so a local
  aggregate would mirror state we do not own, with no reader that needs it.
- Anything that wants to trigger a run without a suite (a cron build, a manual
  "build this tag") needs a new decision here first, because it has no commit
  status to write to.
