// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openpreflight/openpreflight/internal/health"
	"github.com/openpreflight/openpreflight/internal/store"
)

// bearer gives a test an authenticated caller without a cookie jar.
func (ts *testServer) bearer(t *testing.T) string {
	t.Helper()
	user, err := ts.store.CreateUser("admin", "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := ts.store.CreateSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (ts *testServer) get(t *testing.T, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

func (ts *testServer) statusOf(t *testing.T, name string) health.Component {
	t.Helper()
	for _, c := range ts.buildStatus().Components {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q component", name)
	return health.Component{}
}

// TestHealthContractIsUnchanged is the test that matters most in this wave.
// Coolify polls this endpoint; if the body or the status code moves, a healthy
// container starts being restarted.
func TestHealthContractIsUnchanged(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.get(t, "/health", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("not JSON: %q", rec.Body.String())
	}
	if len(body) != 1 || body["status"] != "ok" {
		t.Fatalf(`body must stay exactly {"status":"ok"}, got %v`, body)
	}
}

// TestHealthVerboseNeedsAuth: the breakdown names the public base URL and the
// configured Apps, and /health is unguarded. An anonymous caller asking for
// detail gets liveness, not an error and not the detail.
func TestHealthVerboseNeedsAuth(t *testing.T) {
	ts := newTestServer(t)

	rec := ts.get(t, "/health?verbose=1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("anonymous verbose must still answer liveness: %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "components") {
		t.Fatalf("anonymous caller was given the breakdown: %s", rec.Body.String())
	}

	rec = ts.get(t, "/health?verbose=1", ts.bearer(t))
	var report health.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("not a report: %v\n%s", err, rec.Body.String())
	}
	if len(report.Components) == 0 {
		t.Fatal("authenticated verbose returned no components")
	}
	if report.Version == "" {
		t.Fatal("the report should say which build is running")
	}
}

// TestStatusDockerIsNotAnErrorWhenNothingNeedsIt keeps the severity line
// honest: an install with no runtime anywhere and forks skipped is healthy
// without an engine, and crying error there teaches operators to skim.
func TestStatusDockerIsNotAnErrorWhenNothingNeedsIt(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return false }

	settings, _ := ts.store.Settings()
	settings.DefaultRuntime = ""
	settings.SkipForkPRs = true
	if err := ts.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	c := ts.statusOf(t, "Docker")
	if c.State != health.StateOK {
		t.Fatalf("state %q: %s", c.State, c.Detail)
	}
	// It must still say a pipeline file could ask for one, because that file
	// lives in a commit this server has not seen.
	if !strings.Contains(c.Detail, "runtime:") {
		t.Fatalf("detail should keep the caveat: %q", c.Detail)
	}
}

func TestStatusDockerWarnsWhenForksAreEnabled(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return false }

	settings, _ := ts.store.Settings()
	settings.SkipForkPRs = false
	if err := ts.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}

	c := ts.statusOf(t, "Docker")
	if c.State != health.StateWarn {
		t.Fatalf("fork jobs always need a container: state %q", c.State)
	}
	if c.Action == "" {
		t.Fatal("a warning with no next step is half an answer")
	}
}

// TestStatusWorkerShowsRowsWithNothingRunning is the diagnosis this page exists
// for: a queue that has stopped moving because a crash left rows behind.
func TestStatusWorkerShowsRowsWithNothingRunning(t *testing.T) {
	ts := newTestServer(t)
	job, err := ts.store.EnqueueJob(store.JobInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", SHA: strings.Repeat("a", 40),
		Ref: "main", Event: "check_suite", InstallationID: 101,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.store.ClaimNextJob(); err != nil {
		t.Fatal(err)
	}
	_ = job

	c := ts.statusOf(t, "Worker")
	if c.State != health.StateWarn {
		t.Fatalf("state %q: %s", c.State, c.Detail)
	}
	if !strings.Contains(c.Detail, "nothing running") {
		t.Fatalf("detail should name the gap: %q", c.Detail)
	}
	// RequeueStaleJobs runs at startup and not on a timer, so telling an
	// operator to wait would be telling them to wait for nothing.
	if !strings.Contains(c.Action, "restart") {
		t.Fatalf("the action must say a restart is what clears these: %q", c.Action)
	}
}

func TestStatusReportsSchemaVersion(t *testing.T) {
	ts := newTestServer(t)
	c := ts.statusOf(t, "Database")
	if c.State != health.StateOK {
		t.Fatalf("state %q: %s", c.State, c.Detail)
	}
	if !strings.Contains(c.Detail, "schema 0") {
		t.Fatalf("an operator upgrading needs the schema version: %q", c.Detail)
	}
}

// TestStatusRepositoriesWarnsWhenNoneEnabled covers the most common reason a
// correct install reports nothing at all.
func TestStatusRepositoriesWarnsWhenNoneEnabled(t *testing.T) {
	ts := newTestServer(t)
	c := ts.statusOf(t, "Repositories")
	if c.State != health.StateWarn {
		t.Fatalf("state %q: %s", c.State, c.Detail)
	}

	if _, err := ts.store.UpsertBinding(store.BindingInput{
		GitHubAppID: ts.app.ID, Repo: "acme/api", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if c := ts.statusOf(t, "Repositories"); c.State != health.StateOK {
		t.Fatalf("one enabled binding should be ok: %q %s", c.State, c.Detail)
	}
}

func TestStatusPageRenders(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.get(t, "/status", ts.bearer(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Database", "Webhook", "Repositories", "Worker", "Docker", "verbose=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not mention %q", want)
		}
	}
}

func TestStatusPageNeedsASession(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.get(t, "/status", "")
	if rec.Code == http.StatusOK {
		t.Fatalf("the status page must not be anonymous: %d", rec.Code)
	}
}
