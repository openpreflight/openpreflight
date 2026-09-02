// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/logs"
	"github.com/openpreflight/openpreflight/internal/store"
)

// jsonReq builds an API-surface request.
func jsonReq(method, path, body string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req
}

// htmlReq builds a browser-surface request.
func htmlReq(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	return req
}

func (ts *testServer) do(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

// login creates an admin (if needed) and returns a Bearer token.
func (ts *testServer) login(t *testing.T) string {
	t.Helper()
	if has, _ := ts.store.HasUsers(); !has {
		rec := ts.do(jsonReq(http.MethodPost, "/api/v1/setup",
			`{"username":"admin","password":"a-long-enough-password","public_base_url":"https://ci.example.com"}`))
		if rec.Code != http.StatusCreated {
			t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := ts.do(jsonReq(http.MethodPost, "/api/v1/login",
		`{"username":"admin","password":"a-long-enough-password"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	var out struct{ Token string }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Token == "" {
		t.Fatalf("no token in %s", rec.Body.String())
	}
	return out.Token
}

func (ts *testServer) authed(t *testing.T, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := jsonReq(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	return ts.do(req)
}

func TestHealth(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(jsonReq(http.MethodGet, "/health", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSetupIsOnlyAvailableOnce(t *testing.T) {
	ts := newTestServer(t)
	ts.login(t)
	// A second setup call must not be able to create another admin.
	rec := ts.do(jsonReq(http.MethodPost, "/api/v1/setup",
		`{"username":"intruder","password":"another-long-password"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if _, err := ts.store.Authenticate("intruder", "another-long-password"); err == nil {
		t.Fatal("a second admin was created")
	}
}

func TestUnauthenticatedAPIIsRejected(t *testing.T) {
	ts := newTestServer(t)
	ts.login(t)
	for _, path := range []string{"/api/v1/settings", "/api/v1/coolify", "/api/v1/github-apps",
		"/api/v1/bindings", "/api/v1/jobs"} {
		rec := ts.do(jsonReq(http.MethodGet, path, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, expected 401", path, rec.Code)
		}
	}
}

func TestBrowserPagesRedirectToSetupThenLogin(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(htmlReq(http.MethodGet, "/"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("expected a redirect to /setup, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
	ts.login(t)
	rec = ts.do(htmlReq(http.MethodGet, "/"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("expected a redirect to /login, got %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestBearerTokenIsAcceptedForAPI(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodGet, "/api/v1/settings", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	var settings store.Settings
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatal(err)
	}
	if settings.PublicBaseURL != "https://ci.example.com" {
		t.Fatalf("settings: %+v", settings)
	}
}

func TestSettingsPatchIsPartial(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/settings", `{"default_check_name":"Acme CI"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	settings, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultCheckName != "Acme CI" {
		t.Fatalf("check name not applied: %+v", settings)
	}
	// The fields the PATCH did not mention must survive.
	if settings.PublicBaseURL != "https://ci.example.com" || settings.LogRetentionDays != 14 {
		t.Fatalf("a partial PATCH reset other fields: %+v", settings)
	}
}

func TestSettingsRejectsNonsenseValues(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	ts.authed(t, token, http.MethodPatch, "/api/v1/settings",
		`{"max_concurrent_jobs":0,"log_retention_days":0,"default_timeout_seconds":1,"max_log_bytes":1}`)
	settings, _ := ts.store.Settings()
	if settings.MaxConcurrentJobs < 1 || settings.LogRetentionDays < 1 ||
		settings.DefaultTimeoutSeconds < 30 || settings.MaxLogBytes < 4096 {
		t.Fatalf("values were not clamped: %+v", settings)
	}
}

func TestForkSkippingCannotBeTurnedOffWithoutDocker(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return false }
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/settings",
		`{"skip_fork_prs":false,"default_runtime":"node:24"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	settings, _ := ts.store.Settings()
	if !settings.SkipForkPRs {
		t.Fatal("fork PRs must stay skipped until Docker is available")
	}
}

func TestForkSkippingCanBeTurnedOffWithDockerAndRuntime(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return true }
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/settings",
		`{"skip_fork_prs":false,"default_runtime":"node:24"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	settings, _ := ts.store.Settings()
	if settings.SkipForkPRs {
		t.Fatal("fork skipping should be off when Docker and a runtime are configured")
	}
	if settings.DefaultRuntime != "node:24" {
		t.Fatalf("runtime %q", settings.DefaultRuntime)
	}
}

func TestForkSkippingCannotBeTurnedOffWithoutRuntime(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return true }
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/settings", `{"skip_fork_prs":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	settings, _ := ts.store.Settings()
	if !settings.SkipForkPRs {
		t.Fatal("fork PRs must stay skipped without default_runtime")
	}
}

func TestGitHubAppRejectsInvalidPEM(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPost, "/api/v1/github-apps",
		`{"name":"broken","app_id":5,"pem":"not a key","webhook_secret":"s"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	apps, _ := ts.store.ListGitHubApps()
	for _, a := range apps {
		if a.Name == "broken" {
			t.Fatal("an App with an unusable private key was stored")
		}
	}
}

func TestListGitHubAppsRedactsSecretsAndShowsWebhookURL(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodGet, "/api/v1/github-apps", "")
	body := rec.Body.String()
	if strings.Contains(body, webhookSecret) {
		t.Fatal("the webhook secret is exposed by the API")
	}
	if strings.Contains(body, "BEGIN RSA PRIVATE KEY") {
		t.Fatal("the private key is exposed by the API")
	}
	if !strings.Contains(body, "https://ci.example.com/webhook/ci") {
		t.Fatalf("webhook URL missing from %s", body)
	}
}

func TestBindingsUpsertAndDeleteOverAPI(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPut, "/api/v1/bindings",
		`{"github_app_id":`+itoa(ts.app.ID)+`,"repo":"acme/api","enabled":true,"branches":"main","paths":"frontend/**","shareable_logs":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	bindings, _ := ts.store.ListBindings()
	if len(bindings) != 1 || !bindings[0].Enabled || !bindings[0].ShareableLogs || bindings[0].Paths != "frontend/**" {
		t.Fatalf("binding: %+v", bindings)
	}
	rec = ts.authed(t, token, http.MethodDelete, "/api/v1/bindings/"+itoa(bindings[0].ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	bindings, _ = ts.store.ListBindings()
	if len(bindings) != 0 {
		t.Fatal("binding was not deleted")
	}
}

func TestBindingRequiresAnApp(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	rec := ts.authed(t, token, http.MethodPut, "/api/v1/bindings", `{"repo":"acme/api","enabled":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func manifestTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}))
}

func TestAppManifestCreatesAnApp(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	pemData := manifestTestPEM(t)

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/app-manifests/") && strings.HasSuffix(r.URL.Path, "/conversions"):
			if !strings.Contains(r.URL.Path, "/good-code/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": 9191, "slug": "openpreflight-ci", "name": "openpreflight-ci",
				"pem": pemData, "webhook_secret": "from-github-manifest",
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/app/hook/config":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			json.NewEncoder(w).Encode([]any{})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	ts.manifestAPI = gh.URL

	rec := ts.authed(t, token, http.MethodPost, "/api/v1/github-apps/manifest/start", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var start struct {
		Action   string         `json:"action"`
		State    string         `json:"state"`
		Manifest map[string]any `json:"manifest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatal(err)
	}
	if start.Action != githubNewAppURL || start.State == "" {
		t.Fatalf("start payload: %+v", start)
	}
	if start.Manifest["url"] != "https://ci.example.com" {
		t.Fatalf("manifest url: %+v", start.Manifest)
	}
	perms, _ := start.Manifest["default_permissions"].(map[string]any)
	if perms["checks"] != "write" || perms["contents"] != "read" || perms["metadata"] != "read" {
		t.Fatalf("permissions: %+v", perms)
	}
	var csrf string
	for _, c := range rec.Result().Cookies() {
		if c.Name == csrfCookie {
			csrf = c.Value
		}
	}
	if csrf == "" {
		t.Fatal("start did not mint a CSRF cookie to use as state")
	}

	bad := jsonReq(http.MethodGet, "/api/v1/github-apps/manifest/callback?code=good-code&state=nope", "")
	bad.Header.Set("Authorization", "Bearer "+token)
	bad.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	rec = ts.do(bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state: %d %s", rec.Code, rec.Body.String())
	}
	apps, _ := ts.store.ListGitHubApps()
	if len(apps) != 1 {
		t.Fatalf("bad state created an App: %+v", apps)
	}

	ok := jsonReq(http.MethodGet, "/api/v1/github-apps/manifest/callback?code=good-code&state="+start.State, "")
	ok.Header.Set("Authorization", "Bearer "+token)
	ok.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	rec = ts.do(ok)
	if rec.Code != http.StatusCreated {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "BEGIN RSA PRIVATE KEY") || strings.Contains(rec.Body.String(), pemData) {
		t.Fatal("callback leaked the PEM")
	}
	apps, _ = ts.store.ListGitHubApps()
	if len(apps) != 2 {
		t.Fatalf("apps after callback: %+v", apps)
	}
	var created store.GitHubApp
	for _, a := range apps {
		if a.Slug == "openpreflight-ci" {
			created = a
		}
	}
	if created.ID == 0 || created.AppID != 9191 {
		t.Fatalf("created app: %+v", created)
	}
	gotPEM, err := ts.store.DecryptPEM(created)
	if err != nil || strings.TrimSpace(gotPEM) != strings.TrimSpace(pemData) {
		t.Fatalf("stored pem: %v", err)
	}
	secret, err := ts.store.DecryptWebhookSecret(created)
	if err != nil || secret != "from-github-manifest" {
		t.Fatalf("stored webhook secret: %v %q", err, secret)
	}
}

func TestGitHubAppsPageOffersManifest(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	req := htmlReq(http.MethodGet, "/github-apps")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := ts.do(req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(body, "Create with GitHub") {
		t.Fatal("list page should not embed the add form")
	}
	if !strings.Contains(body, "/github-apps/new") {
		t.Fatal("list page is missing Add an App")
	}

	req = htmlReq(http.MethodGet, "/github-apps?edit="+itoa(ts.app.ID))
	req.Header.Set("Authorization", "Bearer "+token)
	rec = ts.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("edit query: %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/github-apps/"+itoa(ts.app.ID)+"/edit" {
		t.Fatalf("edit query location: %s", loc)
	}

	req = htmlReq(http.MethodGet, "/github-apps/new")
	req.Header.Set("Authorization", "Bearer "+token)
	rec = ts.do(req)
	body = rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("new status %d", rec.Code)
	}
	if !strings.Contains(body, "Create with GitHub") || !strings.Contains(body, "Advanced") {
		t.Fatal("add page is missing the manifest button")
	}
	if strings.Contains(body, "never creates Apps") {
		t.Fatal("add page still says we never create Apps")
	}
}

func TestRunPageRequiresSessionUnlessShareable(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)

	private, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "aaa", ShareableLogs: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "bbb", ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{private.ID, shared.ID} {
		w, err := logs.Create(ts.cfg.LogDir(), id, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		w.Printf("secret build output for %s\n", id)
		w.Close()
	}

	// Anonymous: the private job's logs must not be readable.
	rec := ts.do(htmlReq(http.MethodGet, "/runs/"+private.ID))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("private log page leaked: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if strings.Contains(rec.Body.String(), "secret build output") {
		t.Fatal("private build output was served to an anonymous visitor")
	}

	// Anonymous: the opted-in job is readable, because that is the point.
	rec = ts.do(htmlReq(http.MethodGet, "/runs/"+shared.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("shareable log page: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "secret build output for "+shared.ID) {
		t.Fatal("shareable log page did not show the log")
	}

	// With a session, both are readable.
	req := jsonReq(http.MethodGet, "/runs/"+private.ID, "")
	req.Header.Set("Authorization", "Bearer "+token)
	rec = ts.do(req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret build output") {
		t.Fatalf("authenticated read failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRunPageHidesWhetherAJobExists(t *testing.T) {
	ts := newTestServer(t)
	ts.login(t)
	rec := ts.do(htmlReq(http.MethodGet, "/runs/1e6b1f9c-0000-4000-8000-000000000000"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestRerunRequiresEnabledBinding(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	job, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "aaa", InstallationID: 101,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No binding: a job in the list must not be a way around the allow-list.
	rec := ts.authed(t, token, http.MethodPost, "/api/v1/jobs/"+job.ID+"/rerun", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}

	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "acme/api", Enabled: true})
	rec = ts.authed(t, token, http.MethodPost, "/api/v1/jobs/"+job.ID+"/rerun", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rerun: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := ts.store.ListJobs(store.JobList{Limit: 10})
	if len(jobs) != 2 {
		t.Fatalf("expected a new job, got %d", len(jobs))
	}
	// A re-run must not claim the original delivery id, or the next redelivery
	// would be deduped against it.
	for _, j := range jobs {
		if j.ID != job.ID && j.DeliveryID != "" {
			t.Fatalf("re-run job carries a delivery id: %+v", j)
		}
	}
}

func TestAuthenticatedPagesRender(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	binding, err := ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "acme/api", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ts.store.EnqueueJob(store.JobInput{GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "abc1234", Ref: "main"})
	inst, err := ts.store.CreateCoolifyInstance(store.CoolifyInput{
		Name: "prod", BaseURL: "https://coolify.example.com", APIToken: "1|secret-token-value",
	})
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"/", "/settings", "/settings/runner", "/settings/logs", "/settings/admin",
		"/coolify", "/github-apps", "/github-apps/new",
		"/github-apps/" + itoa(ts.app.ID) + "/edit",
		"/repos", "/repos/pick", "/repos/new",
		"/repos/" + itoa(binding.ID) + "/edit",
		"/jobs",
		"/coolify?edit=" + itoa(inst.ID),
	}
	for _, path := range paths {
		req := htmlReq(http.MethodGet, path)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := ts.do(req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status %d", path, rec.Code)
			continue
		}
		body := rec.Body.String()
		if !strings.Contains(body, "<!doctype html>") {
			t.Errorf("%s: not an HTML page", path)
		}
		if strings.Contains(body, webhookSecret) {
			t.Errorf("%s: leaks the webhook secret", path)
		}
		if strings.Contains(body, "-----END RSA PRIVATE KEY-----") {
			t.Errorf("%s: leaks a private key", path)
		}
		if strings.Contains(body, "1|secret-token-value") {
			t.Errorf("%s: leaks the Coolify token", path)
		}
		if strings.Contains(body, "max-w-[1180px]") {
			t.Errorf("%s: still uses the old max-width shell", path)
		}
	}

	edit := htmlReq(http.MethodGet, "/repos?edit="+itoa(binding.ID))
	edit.Header.Set("Authorization", "Bearer "+token)
	if rec := ts.do(edit); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/repos/"+itoa(binding.ID)+"/edit" {
		t.Errorf("/repos?edit=: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}

	logo := ts.do(htmlReq(http.MethodGet, "/assets/brand/logo.svg"))
	if logo.Code != http.StatusOK || !strings.Contains(logo.Header().Get("Content-Type"), "image/svg+xml") {
		t.Errorf("logo: status %d type %q", logo.Code, logo.Header().Get("Content-Type"))
	}

	repos := authedHTML(t, ts, token, "/repos")
	if strings.Contains(repos, `name="app_id"`) {
		t.Error("/repos still embeds the picker")
	}
	if !strings.Contains(repos, "/repos/pick") || !strings.Contains(repos, "/repos/new") {
		t.Error("/repos missing pick/new links")
	}
	pick := authedHTML(t, ts, token, "/repos/pick")
	if !strings.Contains(pick, `name="app_id"`) {
		t.Error("/repos/pick missing the App picker")
	}
	editPage := authedHTML(t, ts, token, "/repos/"+itoa(binding.ID)+"/edit")
	if !strings.Contains(editPage, `role="dialog"`) || !strings.Contains(editPage, "Save binding") {
		t.Error("/repos/{id}/edit is not a drawer over the list")
	}
	if !strings.Contains(editPage, "Pick repositories") {
		t.Error("edit drawer should keep the repos list underneath")
	}
	settings := authedHTML(t, ts, token, "/settings")
	if strings.Contains(settings, `name="max_concurrent_jobs"`) || strings.Contains(settings, `name="password"`) {
		t.Error("/settings still shows runner or admin fields")
	}
	if !strings.Contains(settings, `name="public_base_url"`) {
		t.Error("/settings missing configuration fields")
	}
	runner := authedHTML(t, ts, token, "/settings/runner")
	if !strings.Contains(runner, `name="max_concurrent_jobs"`) || !strings.Contains(runner, `aria-current="page"`) {
		t.Error("/settings/runner missing runner fields or current nav")
	}
	admin := authedHTML(t, ts, token, "/settings/admin")
	if !strings.Contains(admin, `name="password"`) {
		t.Error("/settings/admin missing the password form")
	}

	req := htmlReq(http.MethodGet, "/")
	req.Header.Set("Authorization", "Bearer "+token)
	home := ts.do(req).Body.String()
	for _, want := range []string{"Setup", "Public base URL", "CI GitHub App", "Enabled repo", "Coolify", "Workspace"} {
		if !strings.Contains(home, want) {
			t.Errorf("overview missing %q", want)
		}
	}
}

func authedHTML(t *testing.T, ts *testServer, token, path string) string {
	t.Helper()
	req := htmlReq(http.MethodGet, path)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := ts.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d", path, rec.Code)
	}
	return rec.Body.String()
}

func TestSettingsFormRedirectsToSection(t *testing.T) {
	ts := newTestServer(t)
	session, csrf := ts.cookieAuth(t)
	req := htmlForm(http.MethodPost, "/api/v1/settings",
		"public_base_url=https://ci.example.com&default_check_name=openpreflight&default_pipeline_file=.ci.yml&default_timeout_seconds=900&next=/settings/logs&csrf="+csrf,
		session, csrf)
	rec := ts.do(req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings/logs" {
		t.Fatalf("allowed next: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}

	req = htmlForm(http.MethodPost, "/api/v1/settings",
		"public_base_url=https://ci.example.com&next=https://evil.example/phish&csrf="+csrf,
		session, csrf)
	rec = ts.do(req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("rejected next: status %d location %q", rec.Code, rec.Header().Get("Location"))
	}

	before, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	req = htmlForm(http.MethodPost, "/api/v1/settings",
		"public_base_url=https://ci.example.com&default_check_name=openpreflight&default_pipeline_file=.ci.yml&default_timeout_seconds=900&next=/settings&csrf="+csrf,
		session, csrf)
	rec = ts.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("configuration save: %d %s", rec.Code, rec.Body.String())
	}
	after, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.SkipForkPRs != before.SkipForkPRs || after.MaxConcurrentJobs != before.MaxConcurrentJobs {
		t.Fatalf("configuration save mutated runner fields: %+v -> %+v", before, after)
	}
}

func TestJobsAPIFilters(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	if _, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "aaa",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/web", SHA: "bbb",
	}); err != nil {
		t.Fatal(err)
	}

	rec := ts.authed(t, token, http.MethodGet, "/api/v1/jobs?repo=acme/api", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("repo filter: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Jobs []store.Job `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Jobs) != 1 || out.Jobs[0].Repo != "acme/api" {
		t.Fatalf("repo filter jobs: %+v", out.Jobs)
	}

	rec = ts.authed(t, token, http.MethodGet, "/api/v1/jobs?status=nope", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown status: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobsIndexFilterForm(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	req := htmlReq(http.MethodGet, "/jobs")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := ts.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `name="status"`) || !strings.Contains(body, `name="repo"`) {
		t.Fatal("jobs page is missing the filter form")
	}
	if !strings.Contains(body, "No jobs yet") {
		t.Fatal("unfiltered empty should still say no jobs yet")
	}
	if strings.Contains(body, "No jobs match this filter") {
		t.Fatal("unfiltered empty is not a filter miss")
	}
}

func TestJobsIndexFilteredEmpty(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	if _, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "abc1234",
	}); err != nil {
		t.Fatal(err)
	}
	req := htmlReq(http.MethodGet, "/jobs?repo=nobody/else")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := ts.do(req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, body)
	}
	if !strings.Contains(body, "No jobs match this filter") {
		t.Fatal("filtered empty should say no jobs match")
	}
	if strings.Contains(body, "Enable a repo") {
		t.Fatal("filtered empty must not send the operator to enable a repo")
	}
}

func TestCSRFRequiredForCookieWrites(t *testing.T) {
	ts := newTestServer(t)
	ts.login(t)
	// Sign in as a browser to obtain both cookies.
	form := httptest.NewRequest(http.MethodPost, "/api/v1/login",
		strings.NewReader("username=admin&password=a-long-enough-password"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.Header.Set("Accept", "text/html")
	rec := ts.do(form)
	var session string
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie issued")
	}
	// A cross-site form post carries the cookie but not the CSRF token.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bindings",
		strings.NewReader("repo=acme/api&enabled=1&github_app_id="+itoa(ts.app.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "the-real-token"})
	ts.do(req)
	bindings, _ := ts.store.ListBindings()
	if len(bindings) != 0 {
		t.Fatal("a request without a CSRF token created a binding")
	}

	// The same request with the token succeeds.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/bindings",
		strings.NewReader("repo=acme/api&enabled=1&csrf=the-real-token&github_app_id="+itoa(ts.app.ID)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "the-real-token"})
	ts.do(req)
	bindings, _ = ts.store.ListBindings()
	if len(bindings) != 1 {
		t.Fatalf("a valid form post did not create a binding: %+v", bindings)
	}
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	if rec := ts.authed(t, token, http.MethodGet, "/api/v1/settings", ""); rec.Code != http.StatusOK {
		t.Fatal("token should work before logout")
	}
	// JSON login issues a Bearer token and no cookie. Logout must revoke that
	// token; stuffing it into a cookie hid the bug.
	if rec := ts.authed(t, token, http.MethodPost, "/api/v1/logout", ""); rec.Code != http.StatusOK {
		t.Fatalf("logout: %d %s", rec.Code, rec.Body.String())
	}
	if rec := ts.authed(t, token, http.MethodGet, "/api/v1/settings", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token still valid after logout: %d", rec.Code)
	}
}

func TestJobLogsAPIHonoursShareableLogs(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	private, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "aaa", ShareableLogs: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "bbb", ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{private.ID, shared.ID} {
		w, err := logs.Create(ts.cfg.LogDir(), id, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		w.Printf("secret build output for %s\n", id)
		w.Close()
	}

	rec := ts.do(jsonReq(http.MethodGet, "/api/v1/jobs/"+private.ID+"/logs", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("private logs leaked: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do(jsonReq(http.MethodGet, "/api/v1/jobs/"+shared.ID+"/logs", ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret build output for "+shared.ID) {
		t.Fatalf("shareable logs: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.authed(t, token, http.MethodGet, "/api/v1/jobs/"+private.ID+"/logs", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret build output for "+private.ID) {
		t.Fatalf("authenticated logs: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.authed(t, token, http.MethodGet, "/api/v1/jobs/1e6b1f9c-0000-4000-8000-000000000000/logs", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing job: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobLogStreamHonoursShareableLogs(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	private, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "aaa", ShareableLogs: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "bbb", ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{private.ID, shared.ID} {
		w, err := logs.Create(ts.cfg.LogDir(), id, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		w.Printf("secret build output for %s\n", id)
		w.Close()
		if err := ts.store.FinishJob(id, store.JobSuccess, "success", "[]", ""); err != nil {
			t.Fatal(err)
		}
	}

	rec := ts.do(jsonReq(http.MethodGet, "/api/v1/jobs/"+private.ID+"/logs/stream", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("private stream leaked: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.do(jsonReq(http.MethodGet, "/api/v1/jobs/"+shared.ID+"/logs/stream", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("shareable stream: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "secret build output for "+shared.ID) {
		t.Fatalf("shareable stream missing log: %s", body)
	}
	if !strings.Contains(body, "event: finished") {
		t.Fatalf("shareable stream missing finished: %s", body)
	}

	rec = ts.authed(t, token, http.MethodGet, "/api/v1/jobs/"+private.ID+"/logs/stream", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "secret build output for "+private.ID) {
		t.Fatalf("authenticated stream: %d %s", rec.Code, rec.Body.String())
	}

	rec = ts.authed(t, token, http.MethodGet, "/api/v1/jobs/1e6b1f9c-0000-4000-8000-000000000000/logs/stream", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing job stream: %d %s", rec.Code, rec.Body.String())
	}
}

func TestJobLogStreamStopsWhenClientLeaves(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	job, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "ccc", ShareableLogs: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := jsonReq(http.MethodGet, "/api/v1/jobs/"+job.ID+"/logs/stream", "")
	req.Header.Set("Authorization", "Bearer "+token)
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	rec := ts.do(req)
	if rec.Code != http.StatusOK {
		t.Fatalf("in-flight stream: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLoginAndSetupHaveBrandChrome(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.do(htmlReq(http.MethodGet, "/setup"))
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="brand"`) || !strings.Contains(body, "max-w-[440px]") {
		t.Fatalf("setup is missing brand chrome: %s", body)
	}
	if strings.Contains(body, "Sign out") {
		t.Fatal("setup must not show sign-out")
	}

	ts.login(t)
	rec = ts.do(htmlReq(http.MethodGet, "/login"))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, `class="brand"`) || !strings.Contains(body, "max-w-[440px]") {
		t.Fatalf("login is missing brand chrome")
	}
	if strings.Contains(body, "Sign out") || strings.Contains(body, ">Overview<") {
		t.Fatal("login must not show the signed-in nav")
	}
}

func TestHTMLPostRenamesGitHubAppWithoutPEM(t *testing.T) {
	ts := newTestServer(t)
	session, csrf := ts.cookieAuth(t)
	pemBefore, err := ts.store.DecryptPEM(ts.app)
	if err != nil {
		t.Fatal(err)
	}

	req := htmlForm(http.MethodPost, "/api/v1/github-apps/"+itoa(ts.app.ID),
		"name=renamed-ci&slug=ci&app_id="+itoa(appNumericID)+"&api_url=https://api.github.com&csrf="+csrf,
		session, csrf)
	rec := ts.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}

	app, err := ts.store.GitHubApp(ts.app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if app.Name != "renamed-ci" {
		t.Fatalf("name not updated: %+v", app)
	}
	pemAfter, err := ts.store.DecryptPEM(app)
	if err != nil {
		t.Fatal(err)
	}
	if pemAfter != pemBefore {
		t.Fatal("an empty PEM field rotated the stored key")
	}

	get := htmlReq(http.MethodGet, "/github-apps/"+itoa(ts.app.ID)+"/edit")
	get.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	page := ts.do(get)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "renamed-ci") {
		t.Fatalf("edit page: %d", page.Code)
	}
	if !strings.Contains(body, "Leave blank to keep the stored key") {
		t.Fatal("edit form is missing the keep-PEM hint")
	}
	if !strings.Contains(body, "Changing the slug changes the webhook URL") {
		t.Fatal("edit form is missing the slug-change hint")
	}
	if strings.Contains(body, webhookSecret) || strings.Contains(body, "-----END RSA PRIVATE KEY-----") {
		t.Fatal("edit page leaks secret material")
	}
}

func TestJSONPatchGitHubAppStillWorks(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	pemBefore, _ := ts.store.DecryptPEM(ts.app)
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/github-apps/"+itoa(ts.app.ID),
		`{"name":"via-json"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	app, _ := ts.store.GitHubApp(ts.app.ID)
	if app.Name != "via-json" {
		t.Fatalf("json patch: %+v", app)
	}
	pemAfter, _ := ts.store.DecryptPEM(app)
	if pemAfter != pemBefore {
		t.Fatal("json patch rotated the PEM")
	}
}

func TestHTMLPostUpdatesCoolifyWithoutToken(t *testing.T) {
	ts := newTestServer(t)
	session, csrf := ts.cookieAuth(t)
	inst, err := ts.store.CreateCoolifyInstance(store.CoolifyInput{
		Name: "prod", BaseURL: "https://coolify.example.com", APIToken: "1|secret-token-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	tokenBefore, err := ts.store.DecryptCoolifyToken(inst)
	if err != nil {
		t.Fatal(err)
	}

	req := htmlForm(http.MethodPost, "/api/v1/coolify/"+itoa(inst.ID),
		"name=prod-2&base_url=https://coolify.example.com&csrf="+csrf,
		session, csrf)
	rec := ts.do(req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	updated, err := ts.store.CoolifyInstance(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "prod-2" {
		t.Fatalf("name not updated: %+v", updated)
	}
	tokenAfter, err := ts.store.DecryptCoolifyToken(updated)
	if err != nil {
		t.Fatal(err)
	}
	if tokenAfter != tokenBefore {
		t.Fatal("an empty token field rotated the stored token")
	}
}

func TestRunPageOperatorSurface(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)

	running, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "abc1234deadbeef", Ref: "main",
		ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: "def5678deadbeef",
		ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	steps, err := json.Marshal([]executor.Result{
		{Name: "install", Command: "npm ci", ExitCode: 0},
		{Name: "test", Command: "npm test", ExitCode: 1},
		{Name: "build", Skipped: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.FinishJob(done.ID, store.JobFailure, "failure", string(steps), ""); err != nil {
		t.Fatal(err)
	}

	req := htmlReq(http.MethodGet, "/runs/"+running.ID)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := ts.do(req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("authed running: %d", rec.Code)
	}
	if !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatal("in-flight run page is missing meta refresh")
	}
	if !strings.Contains(body, `id="job-log"`) || !strings.Contains(body, "EventSource") {
		t.Fatal("in-flight run page is missing the live log stream")
	}
	if !strings.Contains(body, "/api/v1/jobs/"+running.ID+"/rerun") ||
		!strings.Contains(body, "/api/v1/jobs/"+running.ID+"/cancel") {
		t.Fatal("authed run page is missing re-run/cancel")
	}
	if !strings.Contains(body, "https://github.com/acme/api/commit/abc1234deadbeef") {
		t.Fatal("github.com commit link missing")
	}

	anon := ts.do(htmlReq(http.MethodGet, "/runs/"+running.ID))
	anonBody := anon.Body.String()
	if anon.Code != http.StatusOK {
		t.Fatalf("shareable running: %d", anon.Code)
	}
	if !strings.Contains(anonBody, `http-equiv="refresh"`) {
		t.Fatal("anonymous in-flight view should still refresh")
	}
	if strings.Contains(anonBody, "/rerun") || strings.Contains(anonBody, "/cancel") {
		t.Fatal("anonymous shareable view must not offer re-run or cancel")
	}

	donePage := ts.do(htmlReq(http.MethodGet, "/runs/"+done.ID))
	doneBody := donePage.Body.String()
	if donePage.Code != http.StatusOK {
		t.Fatalf("shareable finished: %d", donePage.Code)
	}
	if strings.Contains(doneBody, `http-equiv="refresh"`) {
		t.Fatal("finished job should not refresh")
	}
	if strings.Contains(doneBody, "EventSource") {
		t.Fatal("finished job should not open EventSource")
	}
	if !strings.Contains(doneBody, "✓") || !strings.Contains(doneBody, "✗") || !strings.Contains(doneBody, "–") {
		t.Fatalf("step marks missing from %s", doneBody)
	}
}

func TestGitHubCommitURL(t *testing.T) {
	job := store.Job{Repo: "acme/api", SHA: "abc1234deadbeef"}
	if got := githubCommitURL(job, ""); got != "https://github.com/acme/api/commit/abc1234deadbeef" {
		t.Fatalf("empty api: %s", got)
	}
	if got := githubCommitURL(job, "https://api.github.com"); got != "https://github.com/acme/api/commit/abc1234deadbeef" {
		t.Fatalf("dotcom: %s", got)
	}
	if got := githubCommitURL(job, "https://ghe.example.com/api/v3"); got != "" {
		t.Fatalf("ghe should not link: %s", got)
	}
}

// cookieAuth signs in through the HTML form and returns the session cookie.
func (ts *testServer) cookieAuth(t *testing.T) (session, csrf string) {
	t.Helper()
	ts.login(t)
	form := httptest.NewRequest(http.MethodPost, "/api/v1/login",
		strings.NewReader("username=admin&password=a-long-enough-password"))
	form.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form.Header.Set("Accept", "text/html")
	rec := ts.do(form)
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie issued")
	}
	csrf = "test-csrf"
	return session, csrf
}

func htmlForm(method, path, body, session, csrf string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	return req
}

// itoa keeps the request-literal helpers readable.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
