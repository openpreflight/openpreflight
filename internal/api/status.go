// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/build"
	"github.com/openpreflight/openpreflight/internal/health"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/web"
)

// buildStatus assembles the report.
//
// Deliberately not a component: "OpenPreflight is running". It is served by the
// process it would describe, so it can only ever say yes, and a row that cannot
// fail teaches a reader to skim.
func (s *Server) buildStatus() health.Report {
	out := health.Report{Status: health.StateOK, Version: build.Version}
	settings, settingsErr := s.store.Settings()
	for _, c := range []health.Component{
		s.statusDatabase(settingsErr),
		s.statusWebhook(settings),
		s.statusGitHub(),
		s.statusRepositories(),
		s.statusWorker(settings),
		s.statusDocker(settings),
	} {
		out.Components = append(out.Components, c)
		out.Status = health.Worse(out.Status, c.State)
	}
	return out
}

func (s *Server) statusDatabase(settingsErr error) health.Component {
	if settingsErr != nil {
		return health.Component{Name: "Database", State: health.StateError,
			Detail: settingsErr.Error(),
			Action: "Check that the data directory is writable and that the disk is not full."}
	}
	schema, err := s.store.SchemaVersion()
	if err != nil {
		return health.Component{Name: "Database", State: health.StateError, Detail: err.Error(),
			Action: "Check that the data directory is writable and that the disk is not full."}
	}
	if !schema.UpToDate() {
		return health.Component{Name: "Database", State: health.StateError,
			Detail: fmt.Sprintf("at %d of %d migrations (last applied: %s)",
				schema.Count, schema.Expected, orNone(schema.Applied)),
			Action: "Restart the server: migrations are applied on boot. If it will not start, the log says which one failed."}
	}
	return health.Component{Name: "Database", State: health.StateOK,
		Detail: fmt.Sprintf("schema %s, %d migrations applied", schema.Applied, schema.Count)}
}

func (s *Server) statusWebhook(settings store.Settings) health.Component {
	base := strings.TrimSpace(settings.PublicBaseURL)
	switch {
	case base == "":
		return health.Component{Name: "Webhook", State: health.StateWarn,
			Detail: "no public base URL is set",
			Action: "Set it in Settings → Configuration. GitHub cannot deliver webhooks and Check Run links have nowhere to point."}
	case !strings.HasPrefix(base, "https://"):
		return health.Component{Name: "Webhook", State: health.StateWarn,
			Detail: base + " is not HTTPS",
			Action: "GitHub requires HTTPS for webhook delivery. Put a reverse proxy with a certificate in front, or fix the URL if one is already there."}
	}
	// Reachability is not knowable from inside: only GitHub can say whether its
	// POST arrives. Report what is configured and say which is which.
	return health.Component{Name: "Webhook", State: health.StateOK,
		Detail: base + "/webhook/{slug} (configured; delivery is verified by GitHub, not here)"}
}

func (s *Server) statusGitHub() health.Component {
	apps, err := s.store.ListGitHubApps()
	if err != nil {
		return health.Component{Name: "GitHub", State: health.StateError, Detail: err.Error()}
	}
	if len(apps) == 0 {
		return health.Component{Name: "GitHub", State: health.StateWarn,
			Detail: "no GitHub App configured",
			Action: "Add one at GitHub Apps. Only an App can write a Check Run, so nothing can be reported without it."}
	}
	var failing []string
	var unverified int
	newest := time.Time{}
	for _, a := range apps {
		if strings.TrimSpace(a.LastError) != "" {
			failing = append(failing, a.Name)
			continue
		}
		if a.LastSeenAt == nil {
			unverified++
			continue
		}
		if a.LastSeenAt.After(newest) {
			newest = *a.LastSeenAt
		}
	}
	switch {
	case len(failing) > 0:
		return health.Component{Name: "GitHub", State: health.StateWarn,
			Detail: fmt.Sprintf("%s last reported an error", strings.Join(failing, ", ")),
			Action: "Open the App and press Test. A rotated private key or a changed App ID is the usual cause."}
	case unverified == len(apps):
		return health.Component{Name: "GitHub", State: health.StateWarn,
			Detail: plural(len(apps), "App") + ", never verified",
			Action: "Press Test on the App to confirm the credentials work before a commit depends on them."}
	}
	// Stored, not live: /health is polled on a timer and must not spend GitHub
	// rate limit or turn a GitHub outage into an unhealthy container.
	return health.Component{Name: "GitHub", State: health.StateOK,
		Detail: fmt.Sprintf("%s, last verified %s", plural(len(apps), "App"), web.Ago(newest))}
}

func (s *Server) statusRepositories() health.Component {
	bindings, err := s.store.ListBindings()
	if err != nil {
		return health.Component{Name: "Repositories", State: health.StateError, Detail: err.Error()}
	}
	enabled := 0
	for _, b := range bindings {
		if b.Enabled {
			enabled++
		}
	}
	// The most common reason a correct install reports nothing: a signed
	// webhook for a repository with no enabled binding is dropped on purpose.
	if enabled == 0 {
		detail := "no repository is enabled"
		if len(bindings) > 0 {
			detail = fmt.Sprintf("%s, none enabled", plural(len(bindings), "binding"))
		}
		return health.Component{Name: "Repositories", State: health.StateWarn, Detail: detail,
			Action: "Enable one under Repos. Webhooks for repositories with no enabled binding are ignored by design."}
	}
	return health.Component{Name: "Repositories", State: health.StateOK,
		Detail: fmt.Sprintf("%d of %s enabled", enabled, plural(len(bindings), "binding"))}
}

func (s *Server) statusWorker(settings store.Settings) health.Component {
	inflight, err := s.store.CountInFlight()
	if err != nil {
		return health.Component{Name: "Worker", State: health.StateError, Detail: err.Error()}
	}
	running := 0
	if s.runner != nil {
		running = s.runner.Active()
	}
	limit := settings.MaxConcurrentJobs
	detail := fmt.Sprintf("%d running, %d in flight, limit %d", running, inflight, limit)
	// Two numbers, on purpose. Rows and goroutines disagree when a job was
	// killed mid-run, and that gap is the most useful thing this page can say
	// about a queue that has stopped moving.
	if inflight > running {
		// Stale rows are requeued by RequeueStaleJobs, which runs once at
		// startup and deliberately not on a timer — it requeues every
		// in_progress row unconditionally, so running it against live jobs
		// would clobber them. Telling an operator to wait would be telling
		// them to wait for something that never happens.
		return health.Component{Name: "Worker", State: health.StateWarn,
			Detail: detail + fmt.Sprintf(" (%d marked in flight with nothing running)", inflight-running),
			Action: "Left over from a crash or a kill. Rows are requeued at startup, so restarting the server clears them; nothing requeues them while it is running."}
	}
	return health.Component{Name: "Worker", State: health.StateOK, Detail: detail}
}

func (s *Server) statusDocker(settings store.Settings) health.Component {
	if s.dockerAvailable() {
		return health.Component{Name: "Docker", State: health.StateOK, Detail: "engine reachable"}
	}
	// Whether an engine is *required* is not fully knowable from here: any
	// repository's pipeline file can ask for a runtime, and that file lives in
	// a commit we have not seen. So this is a warning when the configuration
	// needs one, and still a note when it does not.
	needs := []string{}
	if strings.TrimSpace(settings.DefaultRuntime) != "" {
		needs = append(needs, "default_runtime is set")
	}
	if !settings.SkipForkPRs {
		needs = append(needs, "fork pull requests are enabled, and those always run in a container")
	}
	if len(needs) > 0 {
		return health.Component{Name: "Docker", State: health.StateWarn,
			Detail: "no engine reachable, and " + strings.Join(needs, "; "),
			Action: "Set CI_DOCKER_HOST or mount the engine socket. Jobs that need a container fail rather than falling back to the host."}
	}
	return health.Component{Name: "Docker", State: health.StateOK,
		Detail: "no engine reachable, and nothing in this configuration needs one — a pipeline file that sets runtime: would still fail"}
}

func orNone(v string) string {
	if strings.TrimSpace(v) == "" {
		return "none"
	}
	return v
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// pageStatus renders the same report as an operator-facing page.
func (s *Server) pageStatus(w http.ResponseWriter, r *http.Request, user store.User) {
	report := s.buildStatus()
	p := s.page(w, r, &user, "Status", "status", map[string]any{"Report": report})
	s.render(w, "status", p)
}
