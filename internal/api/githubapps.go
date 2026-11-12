// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/githubapp"
	"github.com/openpreflight/openpreflight/internal/store"
)

// appClient builds a GitHub client for a stored App row.
func (s *Server) appClient(id int64) (*githubapp.Client, store.GitHubApp, error) {
	app, err := s.store.GitHubApp(id)
	if err != nil {
		return nil, store.GitHubApp{}, err
	}
	pem, err := s.store.DecryptPEM(app)
	if err != nil {
		return nil, app, fmt.Errorf("could not decrypt the private key for %q: %w", app.Name, err)
	}
	client, err := githubapp.New(app.AppID, pem, app.APIURL)
	if err != nil {
		return nil, app, err
	}
	return client, app, nil
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request, _ store.User) {
	apps, err := s.store.ListGitHubApps()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	type row struct {
		store.GitHubApp
		WebhookURL string `json:"webhook_url"`
	}
	out := make([]row, 0, len(apps))
	for _, a := range apps {
		out = append(out, row{GitHubApp: a, WebhookURL: webhookURL(settings.PublicBaseURL, a.Slug)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"github_apps": out})
}

func webhookURL(base, slug string) string {
	if base == "" {
		return ""
	}
	return base + "/webhook/" + slug
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request, _ store.User) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	// Parse the key before storing it: a row whose PEM cannot be read is
	// useless, and the failure is much clearer here than on the first webhook.
	if err := validatePEM(in.Str("pem")); err != nil {
		s.badRequest(w, r, err)
		return
	}
	app, err := s.store.CreateGitHubApp(store.GitHubAppInput{
		Name:          in.Str("name"),
		Slug:          in.Str("slug"),
		AppID:         in.Int64("app_id", 0),
		PEM:           in.Str("pem"),
		WebhookSecret: in.Str("webhook_secret"),
		APIURL:        in.Str("api_url"),
		CheckName:     in.Str("check_name"),
	})
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	msg, kind := s.runAppTest(r.Context(), app.ID)
	settings, _ := s.store.Settings()
	hint := ""
	if u := webhookURL(settings.PublicBaseURL, app.Slug); u != "" {
		hint = " Webhook URL: " + u
	}
	s.reply(w, r, http.StatusCreated, map[string]any{"github_app": app, "test": msg},
		"/github-apps", msg+hint, kind)
}

func (s *Server) updateApp(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	existing, err := s.store.GitHubApp(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	next := store.GitHubAppInput{
		Name:      existing.Name,
		Slug:      existing.Slug,
		AppID:     existing.AppID,
		APIURL:    existing.APIURL,
		CheckName: existing.CheckName,
		// Blank keeps the stored secrets.
		PEM:           in.Str("pem"),
		WebhookSecret: in.Str("webhook_secret"),
	}
	if in.Has("name") {
		next.Name = in.Str("name")
	}
	if in.Has("slug") {
		next.Slug = in.Str("slug")
	}
	if in.Has("app_id") {
		next.AppID = in.Int64("app_id", existing.AppID)
	}
	if in.Has("api_url") {
		next.APIURL = in.Str("api_url")
	}
	if in.Has("check_name") {
		next.CheckName = in.Str("check_name")
	}
	if err := validatePEM(next.PEM); err != nil {
		s.badRequest(w, r, err)
		return
	}
	app, err := s.store.UpdateGitHubApp(id, next)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	// A re-saved PEM invalidates any cached client for this App.
	s.runner.DropClient(id)
	s.reply(w, r, http.StatusOK, map[string]any{"github_app": app}, "/github-apps", "Saved.", "ok")
}

// runAppTest mints an App JWT and lists installations. Every outcome is stored
// on the App, so the page still shows it after the flash message is gone.
func (s *Server) runAppTest(ctx context.Context, id int64) (string, string) {
	client, app, err := s.appClient(id)
	if err != nil {
		s.store.RecordGitHubAppTest(id, err.Error())
		return err.Error(), "err"
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	installs, health, err := s.recordInstallations(ctx, client, app)
	if err != nil {
		return err.Error(), "err"
	}
	if health != "" && health != store.AppNotInstalled {
		return health, "err"
	}
	// Only for the settings link; a failure here is not a failed test. An
	// org owner, once known, is not asked again.
	// ponytail: a personal App stores "" and so is asked on every Test (one
	// cheap call per click); an owner_type column would end that.
	if app.OwnerOrg == "" {
		if org, err := client.OwnerOrg(ctx); err == nil && org != "" {
			s.store.SetGitHubAppOwnerOrg(id, org)
		}
	}
	if len(installs) == 0 {
		return fmt.Sprintf("App %q authenticated, but it is not installed anywhere yet. Install it on the account that owns the repos you want checked.", app.Name), "warn"
	}
	accounts := make([]string, 0, len(installs))
	for _, i := range installs {
		accounts = append(accounts, i.Account.Login)
	}
	return fmt.Sprintf("App %q authenticated. Installed on: %s.", app.Name, joinMax(accounts, 6)), "ok"
}

// recordInstallations lists the App's installations and checks where GitHub
// sends its webhooks, then stores the App's health: an error, a webhook going
// to the wrong place, not installed, or fine. It returns that health.
func (s *Server) recordInstallations(ctx context.Context, client *githubapp.Client, app store.GitHubApp) ([]githubapp.Installation, string, error) {
	installs, err := client.Installations(ctx)
	if err != nil {
		// A request the browser abandoned says nothing about the App; a
		// timeout does (GitHub, or the API URL, is not answering).
		if !errors.Is(ctx.Err(), context.Canceled) {
			s.store.RecordGitHubAppTest(app.ID, err.Error())
		}
		return nil, "", err
	}
	health, known := s.hookProblem(ctx, client, app)
	if health == "" && len(installs) == 0 {
		health = store.AppNotInstalled
	}
	// A webhook URL that could not be read must not wipe a stored hook
	// problem; anything else (not installed, an old error) still updates.
	if !known && strings.HasPrefix(app.LastError, hookProblemPrefix) {
		return installs, app.LastError, nil
	}
	if err := s.store.RecordGitHubAppTest(app.ID, health); err != nil {
		// The installations are real; a failed write must not hide them.
		s.log.Warn("record app health", "app", app.ID, "error", err)
	}
	return installs, health, nil
}

const hookProblemPrefix = "GitHub sends this App's webhooks to"

// hookProblem says why GitHub would not deliver this App's webhooks here, or
// "" when it would; known is false when the URL could not be read. Without
// it, a webhook URL GitHub refused at creation would be wiped from the page by the next
// successful Test, while GitHub still sends nothing.
func (s *Server) hookProblem(ctx context.Context, client *githubapp.Client, app store.GitHubApp) (problem string, known bool) {
	settings, err := s.store.Settings()
	if err != nil || settings.PublicBaseURL == "" {
		return "", err == nil
	}
	want := webhookURL(settings.PublicBaseURL, app.Slug)
	got, err := client.HookURL(ctx)
	if err != nil {
		s.log.Warn("read app webhook url", "app", app.ID, "error", err)
		return "", false
	}
	// ponytail: a URL on another host is a relay the operator chose (smee, a
	// tunnel); only a missing URL or one on this host with the wrong slug is
	// certainly wrong.
	if got == want || (got != "" && !strings.HasPrefix(got, settings.PublicBaseURL+"/")) {
		return "", true
	}
	if got == "" {
		got = "nowhere"
	}
	return fmt.Sprintf(hookProblemPrefix+" %s, not %s. Change the webhook URL on the App's settings page on GitHub.", got, want), true
}

func (s *Server) testApp(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	msg, kind := s.runAppTest(r.Context(), id)
	status := http.StatusOK
	if kind == "err" {
		status = http.StatusBadGateway
	}
	s.reply(w, r, status, map[string]any{"ok": kind == "ok", "message": msg},
		"/github-apps", msg, kind)
}

// appRepos lists every repo every installation of this App can see. This is the
// fallback picker when no Coolify connector is selected.
func (s *Server) appRepos(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	installs, repos, err := s.loadAppRepos(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installations": installs, "repositories": repos})
}

// loadAppRepos returns installations and the union of their repositories.
func (s *Server) loadAppRepos(ctx context.Context, id int64) ([]githubapp.Installation, []githubapp.Repository, error) {
	client, app, err := s.appClient(id)
	if err != nil {
		return nil, nil, err
	}
	// Loading repos is a test too: installing the App on GitHub and loading
	// again clears the "not installed" state without a separate Test click.
	installs, _, err := s.recordInstallations(ctx, client, app)
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	var repos []githubapp.Repository
	for _, inst := range installs {
		batch, err := client.InstallationRepositories(ctx, inst.ID)
		if err != nil {
			// One broken installation should not hide the others.
			s.log.Warn("list installation repositories", "installation", inst.ID, "error", err)
			continue
		}
		for _, repo := range batch {
			if repo.FullName == "" || seen[repo.FullName] {
				continue
			}
			seen[repo.FullName] = true
			repos = append(repos, repo)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
	return installs, repos, nil
}

func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.store.DeleteGitHubApp(id); err != nil {
		s.notFound(w, r, err)
		return
	}
	s.runner.DropClient(id)
	s.reply(w, r, http.StatusOK, map[string]string{"status": "deleted"},
		"/github-apps", "App removed, along with its repo bindings.", "ok")
}

// validatePEM checks a private key the user pasted. An empty value means "keep
// the stored key" on update, so it passes.
func validatePEM(pem string) error {
	if pem == "" {
		return nil
	}
	if _, err := githubapp.ParsePrivateKey(pem); err != nil {
		return err
	}
	return nil
}

func joinMax(items []string, max int) string {
	if len(items) <= max {
		return joinComma(items)
	}
	return fmt.Sprintf("%s and %d more", joinComma(items[:max]), len(items)-max)
}

func joinComma(items []string) string {
	out := ""
	for i, v := range items {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}
