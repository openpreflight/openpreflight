package api

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/webhook"
)

// maxWebhookBody caps what we read. GitHub payloads are well under this; the
// limit exists so an unauthenticated endpoint cannot be used to allocate memory.
const maxWebhookBody = 5 << 20

// handleWebhook is the only endpoint GitHub calls. It must answer within 10
// seconds or the delivery is recorded as failed, so it does no git or GitHub
// work: verify, resolve, enqueue, 202.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	app, err := s.store.GitHubAppBySlug(slug)
	if err != nil {
		// Do not distinguish "no such App" from "bad signature" to an
		// unauthenticated caller.
		s.log.Warn("webhook for unknown app slug", "slug", slug)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	secret, err := s.store.DecryptWebhookSecret(app)
	if err != nil {
		s.log.Error("decrypt webhook secret", "app", app.Slug, "error", err)
		http.Error(w, "server misconfigured", http.StatusInternalServerError)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	// The signature covers the exact bytes GitHub sent, so verification happens
	// before any parsing.
	if err := webhook.VerifySignature(secret, body, r.Header.Get("X-Hub-Signature-256")); err != nil {
		s.log.Warn("webhook signature rejected", "app", app.Slug)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	eventName := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")

	if eventName == "ping" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "pong", "app": app.Slug})
		return
	}

	ev, skip, err := webhook.Parse(eventName, body)
	if err != nil {
		// A malformed payload is not something a retry fixes; 202 keeps GitHub
		// from marking the App's deliveries as failing.
		s.log.Warn("webhook payload rejected", "app", app.Slug, "event", eventName, "error", err)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": err.Error()})
		return
	}
	if skip != "" {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "ignored", "reason": string(skip)})
		return
	}

	// A rerequest on another App's check is not ours to answer.
	if ev.Name == webhook.EventCheckRun && ev.CheckRunAppID != app.AppID {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "ignored",
			"reason": "the re-requested check run belongs to a different GitHub App",
		})
		return
	}

	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("webhook: read settings", "error", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}

	// Fork PRs run untrusted code on the host unless Docker isolation is on.
	if ev.IsFork && settings.SkipForkPRs {
		s.log.Info("webhook: fork skipped", "repo", ev.Repo, "sha", ev.SHA)
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "ignored", "reason": "fork pull requests are not run",
		})
		return
	}
	if ev.IsFork {
		if !s.dockerAvailable() {
			writeJSON(w, http.StatusAccepted, map[string]string{
				"status": "ignored",
				"reason": "fork pull requests require a working Docker executor",
			})
			return
		}
		if strings.TrimSpace(settings.DefaultRuntime) == "" {
			writeJSON(w, http.StatusAccepted, map[string]string{
				"status": "ignored",
				"reason": "fork pull requests require settings.default_runtime",
			})
			return
		}
	}

	binding, err := s.store.EnabledBinding(app.ID, ev.Repo)
	if errors.Is(err, store.ErrNotFound) {
		// The bindings table *is* the allow-list: an unknown or disabled repo
		// produces no job, however valid the signature was.
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "ignored",
			"reason": "no enabled binding for " + ev.Repo,
		})
		return
	}
	if err != nil {
		s.log.Error("webhook: binding lookup", "repo", ev.Repo, "error", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if !binding.BranchAllowed(ev.Branch) {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "ignored",
			"reason": "branch " + ev.Branch + " is not in this binding's branch list",
		})
		return
	}

	// Dedup only while a job for this delivery is still in flight. GitHub's
	// Redeliver button reuses the delivery id, so dedup-forever would break the
	// main debugging tool (AUDIT.md).
	if existing, err := s.store.InFlightJobForDelivery(deliveryID); err == nil {
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status": "already queued", "job": existing.ID,
		})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("webhook: delivery lookup", "error", err)
	}

	// A newer commit on the same ref makes the older run pointless.
	s.cancelSuperseded(ev.Repo, ev.Branch, ev.SHA)

	job, err := s.store.EnqueueJob(store.JobInput{
		BindingID:      binding.ID,
		GitHubAppID:    app.ID,
		Repo:           ev.Repo,
		SHA:            ev.SHA,
		Ref:            ev.Branch,
		Event:          ev.Name + "." + ev.Action,
		DeliveryID:     deliveryID,
		InstallationID: ev.InstallationID,
		CheckName:      binding.CheckName,
		ShareableLogs:  binding.ShareableLogs,
		IsFork:         ev.IsFork,
		PullNumber:     ev.PullNumber,
	})
	if err != nil {
		s.log.Error("webhook: enqueue", "repo", ev.Repo, "error", err)
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	s.runner.Notify()
	s.log.Info("job queued", "job", job.ID, "repo", job.Repo, "sha", job.SHA, "event", job.Event)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued", "job": job.ID})
}

// cancelSuperseded cancels in-flight jobs for the same repo and ref on a
// different commit. Same-SHA duplicates are left alone: those are re-runs the
// user asked for.
func (s *Server) cancelSuperseded(repo, ref, sha string) {
	if ref == "" {
		return
	}
	jobs, err := s.store.InFlightJobsForRef(repo, ref)
	if err != nil {
		s.log.Error("look up in-flight jobs", "repo", repo, "ref", ref, "error", err)
		return
	}
	for _, j := range jobs {
		if j.SHA == sha {
			continue
		}
		if s.runner.CancelJob(j.ID) {
			s.log.Info("cancelled superseded job", "job", j.ID, "repo", repo, "ref", ref)
		}
	}
}
