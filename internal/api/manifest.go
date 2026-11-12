// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/githubapp"
	"github.com/openpreflight/openpreflight/internal/store"
)

const githubNewAppURL = "https://github.com/settings/apps/new"

// githubLogin matches a GitHub user or org name.
var githubLogin = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// newAppURL is where the manifest is posted. A private App can only be
// installed on the account that owns it, so org repos need an org-owned App.
func newAppURL(org string) (string, error) {
	if org == "" {
		return githubNewAppURL, nil
	}
	if !githubLogin.MatchString(org) {
		return "", fmt.Errorf("%q is not a valid GitHub organization name", org)
	}
	return "https://github.com/organizations/" + org + "/settings/apps/new", nil
}

const manifestFormHTML = `<!doctype html>
<meta charset="utf-8">
<title>Continue to GitHub</title>
<form id="manifest" method="post" action="%s">
<input type="hidden" name="manifest" value="%s">
<input type="hidden" name="state" value="%s">
<button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById("manifest").submit()</script>
`

func (s *Server) manifestAPIURL() string {
	if s.manifestAPI != "" {
		return strings.TrimRight(s.manifestAPI, "/")
	}
	return "https://api.github.com"
}

func appManifest(base string) map[string]any {
	return map[string]any{
		"name": "openpreflight",
		"url":  base,
		"hook_attributes": map[string]any{
			"url": base + "/webhook/openpreflight",
		},
		"redirect_url": base + "/api/v1/github-apps/manifest/callback",
		"public":       false,
		"default_permissions": map[string]string{
			"checks":   "write",
			"contents": "read",
			"metadata": "read",
		},
		"default_events": []string{"check_suite", "check_run"},
	}
}

func (s *Server) startAppManifest(w http.ResponseWriter, r *http.Request, _ store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	base := strings.TrimRight(settings.PublicBaseURL, "/")
	if base == "" {
		s.badRequest(w, r, errors.New("set the public base URL in Settings before creating an App with GitHub"))
		return
	}
	action, err := newAppURL(strings.TrimSpace(r.FormValue("org")))
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	state := s.csrfToken(w, r)
	manifest := appManifest(base)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{
			"action":   action,
			"manifest": manifest,
			"state":    state,
		})
		return
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, manifestFormHTML, html.EscapeString(action), html.EscapeString(string(raw)), html.EscapeString(state))
}

func (s *Server) callbackAppManifest(w http.ResponseWriter, r *http.Request, _ store.User) {
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := r.URL.Query().Get("state")
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(c.Value)) != 1 {
		s.badRequest(w, r, errors.New("manifest callback is missing a valid state"))
		return
	}
	if code == "" {
		s.badRequest(w, r, errors.New("manifest callback is missing a code"))
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	conv, err := githubapp.ConvertManifest(ctx, s.manifestAPIURL(), code)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validatePEM(conv.PEM); err != nil {
		s.badRequest(w, r, err)
		return
	}
	app, err := s.store.CreateGitHubApp(store.GitHubAppInput{
		Name:          conv.Name,
		Slug:          conv.Slug,
		AppID:         conv.ID,
		PEM:           conv.PEM,
		WebhookSecret: conv.WebhookSecret,
		APIURL:        s.manifestAPIURL(),
	})
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if org := conv.OwnerOrg(); org != "" {
		s.store.SetGitHubAppOwnerOrg(app.ID, org)
	}
	hook := webhookURL(settings.PublicBaseURL, app.Slug)
	var hookErr error
	if hook != "" {
		client, _, err := s.appClient(app.ID)
		if err == nil {
			err = client.SetHookConfig(ctx, hook)
		}
		if err != nil {
			hookErr = err
			s.log.Warn("set app webhook url", "app", app.ID, "error", err)
		}
	}
	msg, kind := s.runAppTest(ctx, app.ID)
	if hookErr != nil {
		// Kept on the App, not only flashed: without the webhook URL GitHub
		// never calls this worker, and nothing else would say why.
		msg = fmt.Sprintf("GitHub did not take the webhook URL (%v). Set it on the App's settings page to %s", hookErr, hook)
		kind = "err"
		s.store.RecordGitHubAppTest(app.ID, msg)
	}
	s.reply(w, r, http.StatusCreated, map[string]any{"github_app": app, "test": msg},
		"/github-apps", msg, kind)
}
