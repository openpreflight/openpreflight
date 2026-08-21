package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/logs"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
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
	rec := ts.authed(t, token, http.MethodPatch, "/api/v1/settings", `{"default_check_name":"Winpra CI"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	settings, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.DefaultCheckName != "Winpra CI" {
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

func TestForkSkippingCannotBeTurnedOff(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)
	ts.authed(t, token, http.MethodPatch, "/api/v1/settings", `{"skip_fork_prs":false}`)
	settings, _ := ts.store.Settings()
	if !settings.SkipForkPRs {
		t.Fatal("v1 must always skip fork PRs")
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
		`{"github_app_id":`+itoa(ts.app.ID)+`,"repo":"winpra/api","enabled":true,"branches":"main","shareable_logs":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	bindings, _ := ts.store.ListBindings()
	if len(bindings) != 1 || !bindings[0].Enabled || !bindings[0].ShareableLogs {
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
	rec := ts.authed(t, token, http.MethodPut, "/api/v1/bindings", `{"repo":"winpra/api","enabled":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}

func TestRunPageRequiresSessionUnlessShareable(t *testing.T) {
	ts := newTestServer(t)
	token := ts.login(t)

	private, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "winpra/api", SHA: "aaa", ShareableLogs: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "winpra/api", SHA: "bbb", ShareableLogs: true,
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
		GitHubAppID: ts.app.ID, Repo: "winpra/api", SHA: "aaa", InstallationID: 101,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No binding: a job in the list must not be a way around the allow-list.
	rec := ts.authed(t, token, http.MethodPost, "/api/v1/jobs/"+job.ID+"/rerun", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}

	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	rec = ts.authed(t, token, http.MethodPost, "/api/v1/jobs/"+job.ID+"/rerun", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("rerun: %d %s", rec.Code, rec.Body.String())
	}
	jobs, _ := ts.store.ListJobs(10)
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
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	ts.store.EnqueueJob(store.JobInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", SHA: "abc1234", Ref: "main"})

	for _, path := range []string{"/", "/settings", "/coolify", "/github-apps", "/repos", "/jobs"} {
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
		strings.NewReader("repo=winpra/api&enabled=1&github_app_id="+itoa(ts.app.ID)))
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
		strings.NewReader("repo=winpra/api&enabled=1&csrf=the-real-token&github_app_id="+itoa(ts.app.ID)))
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
	req := jsonReq(http.MethodPost, "/api/v1/logout", "")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	ts.do(req)
	if rec := ts.authed(t, token, http.MethodGet, "/api/v1/settings", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("token still valid after logout: %d", rec.Code)
	}
}

// itoa keeps the request-literal helpers readable.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
