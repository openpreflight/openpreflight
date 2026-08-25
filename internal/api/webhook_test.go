package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openpreflight/openpreflight/internal/config"
	"github.com/openpreflight/openpreflight/internal/queue"
	"github.com/openpreflight/openpreflight/internal/secret"
	"github.com/openpreflight/openpreflight/internal/store"
)

const (
	fixtureKey    = "fixture-key-for-tests-only-1234567890"
	webhookSecret = "webhook-secret-value"
	appNumericID  = 4242
)

type testServer struct {
	*Server
	handler http.Handler
	store   *store.Store
	app     store.GitHubApp
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	box, err := secret.New(fixtureKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := config.Config{ListenAddr: ":0", DataDir: filepath.Join(dir, "data"), SecretKey: fixtureKey}
	t.Setenv("WORKSPACE_DIR", filepath.Join(dir, "workspace"))
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath(), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := queue.New(st, cfg, log)
	srv, err := New(st, cfg, runner, log)
	if err != nil {
		t.Fatal(err)
	}
	app, err := st.CreateGitHubApp(store.GitHubAppInput{
		Name: "ci", Slug: "ci", AppID: appNumericID,
		PEM:           "-----BEGIN RSA PRIVATE KEY-----\nfixture\n-----END RSA PRIVATE KEY-----",
		WebhookSecret: webhookSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &testServer{Server: srv, handler: srv.Handler(), store: st, app: app}
}

// post delivers a signed webhook the way GitHub would.
func (ts *testServer) post(t *testing.T, slug, event, deliveryID, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook/"+slug, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(body))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	return rec
}

func checkSuiteBody(repo, sha, branch string) string {
	return fmt.Sprintf(`{
	  "action": "requested",
	  "check_suite": {"head_sha": %q, "head_branch": %q, "pull_requests": [], "app": {"id": %d}},
	  "repository": {"id": 10, "full_name": %q, "private": true},
	  "installation": {"id": 101},
	  "sender": {"login": "tester"}
	}`, sha, branch, appNumericID, repo)
}

// checkSuiteBodyAction is checkSuiteBody with the action spelled out, for the
// rerequested (human pressed Re-run) path.
func checkSuiteBodyAction(repo, sha, branch, action string) string {
	return fmt.Sprintf(`{
	  "action": %q,
	  "check_suite": {"id": 5150, "head_sha": %q, "head_branch": %q, "pull_requests": [], "app": {"id": %d}},
	  "repository": {"id": 10, "full_name": %q, "private": true},
	  "installation": {"id": 101},
	  "sender": {"login": "tester"}
	}`, action, sha, branch, appNumericID, repo)
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response was not JSON: %q", rec.Body.String())
	}
	return out
}

func (ts *testServer) jobCount(t *testing.T) int {
	t.Helper()
	jobs, err := ts.store.ListJobs(100)
	if err != nil {
		t.Fatal(err)
	}
	return len(jobs)
}

func TestWebhookRejectsBadSignature(t *testing.T) {
	ts := newTestServer(t)
	body := checkSuiteBody("winpra/api", "abc1234", "main")
	req := httptest.NewRequest(http.MethodPost, "/webhook/ci", strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", "check_suite")
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	rec := httptest.NewRecorder()
	ts.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", rec.Code)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("an unsigned delivery must never enqueue a job")
	}
}

func TestWebhookUnknownSlugIsUnauthorized(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.post(t, "no-such-app", "check_suite", "d-1", checkSuiteBody("winpra/api", "abc", "main"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
}

func TestWebhookWithoutBindingIsIgnored(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "abc1234", "main"))
	// 202 keeps GitHub's delivery log green: the signature was fine, we just
	// have nothing to do.
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if reason := decodeBody(t, rec)["reason"]; !strings.Contains(reason, "no enabled binding") {
		t.Fatalf("reason: %q", reason)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("a repo with no binding must not run")
	}
}

func TestWebhookDisabledBindingIsIgnored(t *testing.T) {
	ts := newTestServer(t)
	if _, err := ts.store.UpsertBinding(store.BindingInput{
		GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}
	ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "abc1234", "main"))
	if ts.jobCount(t) != 0 {
		t.Fatal("a disabled binding must not run")
	}
}

func TestWebhookEnabledBindingEnqueues(t *testing.T) {
	ts := newTestServer(t)
	binding, err := ts.store.UpsertBinding(store.BindingInput{
		GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true, ShareableLogs: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "abc1234", "main"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	jobID := decodeBody(t, rec)["job"]
	if jobID == "" {
		t.Fatalf("no job id in %q", rec.Body.String())
	}
	job, err := ts.store.Job(jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Repo != "winpra/api" || job.SHA != "abc1234" || job.Ref != "main" {
		t.Fatalf("job: %+v", job)
	}
	if job.BindingID != binding.ID || job.InstallationID != 101 {
		t.Fatalf("job not linked to its binding/installation: %+v", job)
	}
	if !job.ShareableLogs {
		t.Fatal("the binding's shareable_logs must be copied onto the job")
	}
	if job.Event != "check_suite.requested" {
		t.Fatalf("event: %q", job.Event)
	}
}

func TestWebhookDeliveryDedupOnlyWhileInFlight(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := checkSuiteBody("winpra/api", "abc1234", "main")

	first := decodeBody(t, ts.post(t, "ci", "check_suite", "delivery-1", body))["job"]
	if ts.jobCount(t) != 1 {
		t.Fatal("first delivery should enqueue")
	}
	// Same delivery id while the job is queued: no second job.
	again := ts.post(t, "ci", "check_suite", "delivery-1", body)
	if got := decodeBody(t, again)["status"]; got != "already queued" {
		t.Fatalf("status %q body %q", got, again.Body.String())
	}
	if ts.jobCount(t) != 1 {
		t.Fatal("an in-flight delivery must not enqueue twice")
	}

	// Once it completes, GitHub's Redeliver button has to work again — it reuses
	// the same delivery id.
	if err := ts.store.FinishJob(first, store.JobSuccess, "success", "[]", ""); err != nil {
		t.Fatal(err)
	}
	rec := ts.post(t, "ci", "check_suite", "delivery-1", body)
	if got := decodeBody(t, rec)["status"]; got != "queued" {
		t.Fatalf("redeliver after completion: status %q body %q", got, rec.Body.String())
	}
	if ts.jobCount(t) != 2 {
		t.Fatal("redelivering a completed delivery should enqueue a new job")
	}
}

func TestWebhookSkipsForkPRs(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := fmt.Sprintf(`{
	  "action": "requested",
	  "check_suite": {
	    "head_sha": "abc1234", "head_branch": "patch-1",
	    "pull_requests": [{"head": {"repo": {"id": 99, "full_name": "outsider/api"}},
	                       "base": {"repo": {"id": 10, "full_name": "winpra/api"}}}],
	    "app": {"id": %d}
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 101}
	}`, appNumericID)

	rec := ts.post(t, "ci", "check_suite", "d-fork", body)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if reason := decodeBody(t, rec)["reason"]; !strings.Contains(reason, "fork") {
		t.Fatalf("reason: %q", reason)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("a fork PR must never run: it is untrusted code on this host")
	}
}

func TestWebhookRunsForkWhenEnabled(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return true }
	settings, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.SkipForkPRs = false
	settings.DefaultRuntime = "node:24"
	if err := ts.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := fmt.Sprintf(`{
	  "action": "requested",
	  "check_suite": {
	    "head_sha": "abc1234", "head_branch": "patch-1",
	    "pull_requests": [{"number": 12, "head": {"ref": "patch-1", "repo": {"id": 99, "full_name": "outsider/api"}},
	                       "base": {"repo": {"id": 10, "full_name": "winpra/api"}}}],
	    "app": {"id": %d}
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 101}
	}`, appNumericID)
	rec := ts.post(t, "ci", "check_suite", "d-fork-run", body)
	if got := decodeBody(t, rec)["status"]; got != "queued" {
		t.Fatalf("status %q body %s", got, rec.Body.String())
	}
	jobs, err := ts.store.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !jobs[0].IsFork || jobs[0].PullNumber != 12 {
		t.Fatalf("jobs: %+v", jobs)
	}
}

func TestWebhookForkRequiresDockerEvenWhenEnabled(t *testing.T) {
	ts := newTestServer(t)
	ts.dockerOK = func() bool { return false }
	settings, err := ts.store.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.SkipForkPRs = false
	settings.DefaultRuntime = "node:24"
	if err := ts.store.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := fmt.Sprintf(`{
	  "action": "requested",
	  "check_suite": {
	    "head_sha": "abc1234", "head_branch": "patch-1",
	    "pull_requests": [{"number": 12, "head": {"repo": {"id": 99, "full_name": "outsider/api"}},
	                       "base": {"repo": {"id": 10, "full_name": "winpra/api"}}}],
	    "app": {"id": %d}
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 101}
	}`, appNumericID)
	rec := ts.post(t, "ci", "check_suite", "d-fork-nodocker", body)
	if reason := decodeBody(t, rec)["reason"]; !strings.Contains(reason, "Docker") {
		t.Fatalf("reason: %q", reason)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("a fork PR must not run without Docker")
	}
}

func TestWebhookBranchAllowList(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{
		GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true, Branches: "main, release/*",
	})
	if rec := ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "a1", "develop")); rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("a branch outside the allow-list must not run")
	}
	ts.post(t, "ci", "check_suite", "d-2", checkSuiteBody("winpra/api", "a2", "release/2026"))
	if ts.jobCount(t) != 1 {
		t.Fatal("a branch matching release/* should run")
	}
}

func TestWebhookIgnoresOtherAppsCheckRunRerequest(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := `{
	  "action": "rerequested",
	  "check_run": {"id": 1, "head_sha": "abc1234", "app": {"id": 999999},
	                "check_suite": {"head_sha":"abc1234","head_branch": "main", "pull_requests": []}},
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 101}
	}`
	rec := ts.post(t, "ci", "check_run", "d-1", body)
	if reason := decodeBody(t, rec)["reason"]; !strings.Contains(reason, "different GitHub App") {
		t.Fatalf("reason: %q", reason)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("we must not answer another App's check")
	}
}

func TestWebhookOwnCheckRunRerequestEnqueues(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := fmt.Sprintf(`{
	  "action": "rerequested",
	  "check_run": {"id": 1, "head_sha": "abc1234", "app": {"id": %d},
	                "check_suite": {"head_sha":"abc1234","head_branch": "main", "pull_requests": []}},
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 101}
	}`, appNumericID)
	rec := ts.post(t, "ci", "check_run", "d-1", body)
	if got := decodeBody(t, rec)["status"]; got != "queued" {
		t.Fatalf("status %q body %q", got, rec.Body.String())
	}
}

func TestWebhookPing(t *testing.T) {
	ts := newTestServer(t)
	rec := ts.post(t, "ci", "ping", "d-1", `{"zen":"Keep it logically awesome."}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("a ping should succeed so the GitHub App UI shows a green delivery: %d", rec.Code)
	}
}

func TestWebhookNewCommitCancelsOlderJobOnSameRef(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	first := decodeBody(t, ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "aaa1111", "main")))["job"]
	ts.post(t, "ci", "check_suite", "d-2", checkSuiteBody("winpra/api", "bbb2222", "main"))

	older, err := ts.store.Job(first)
	if err != nil {
		t.Fatal(err)
	}
	if older.InFlight() {
		t.Fatalf("the superseded job should have been cancelled, status %q", older.Status)
	}
	if older.Status != store.JobCancelled {
		t.Fatalf("status %q", older.Status)
	}
}

func TestWebhookMalformedPayloadIsAccepted(t *testing.T) {
	ts := newTestServer(t)
	// A retry cannot fix a broken payload, so 202 rather than 4xx keeps the
	// App's delivery history clean.
	rec := ts.post(t, "ci", "check_suite", "d-1", `{"action":"requested"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d", rec.Code)
	}
	if ts.jobCount(t) != 0 {
		t.Fatal("a malformed payload must not enqueue")
	}
}

// One live run per commit: two "requested" deliveries for one SHA are GitHub
// asking twice, not two builds to run. Without the guard each would enqueue and
// the commit would carry two Check Runs with the same name (ADR 005).
func TestWebhookDuplicateSuiteRequestIsNotRunTwice(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	body := checkSuiteBody("winpra/api", "abc1234", "main")

	first := decodeBody(t, ts.post(t, "ci", "check_suite", "delivery-1", body))["job"]
	if ts.jobCount(t) != 1 {
		t.Fatal("the first delivery should enqueue")
	}
	// A distinct delivery id, so the delivery dedup cannot catch this one.
	rec := ts.post(t, "ci", "check_suite", "delivery-2", body)
	got := decodeBody(t, rec)
	if got["status"] != "already queued" {
		t.Fatalf("status %q body %q", got["status"], rec.Body.String())
	}
	if got["job"] != first {
		t.Fatalf("should point at the run already in flight: %q vs %q", got["job"], first)
	}
	if ts.jobCount(t) != 1 {
		t.Fatal("two requested deliveries for one commit must produce one job")
	}
}

// A rerequest is a human pressing Re-run, so it supersedes the run in flight
// rather than being dropped. The commit ends up with one cancelled run and one
// fresh run — never two live ones.
func TestWebhookRerequestCancelsInFlightRunForSameSHA(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})

	first := decodeBody(t, ts.post(t, "ci", "check_suite", "d-1",
		checkSuiteBody("winpra/api", "abc1234", "main")))["job"]

	rec := ts.post(t, "ci", "check_suite", "d-2",
		checkSuiteBodyAction("winpra/api", "abc1234", "main", "rerequested"))
	if got := decodeBody(t, rec)["status"]; got != "queued" {
		t.Fatalf("a rerequest should enqueue: status %q body %q", got, rec.Body.String())
	}

	older, err := ts.store.Job(first)
	if err != nil {
		t.Fatal(err)
	}
	if older.Status != store.JobCancelled {
		t.Fatalf("the superseded run should be cancelled, status %q", older.Status)
	}

	jobs, err := ts.store.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	var live int
	for _, j := range jobs {
		if j.InFlight() {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("exactly one run may be live for a commit, found %d in %+v", live, jobs)
	}
}

// The same commit can arrive on a second branch — a branch cut from the tip, or
// a PR opened on an existing commit. cancelSuperseded keys on (repo, ref) and
// cannot see this; the suite guard can.
func TestWebhookSameSHAOnSecondRefDoesNotDuplicate(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})

	ts.post(t, "ci", "check_suite", "d-1", checkSuiteBody("winpra/api", "abc1234", "main"))
	rec := ts.post(t, "ci", "check_suite", "d-2", checkSuiteBody("winpra/api", "abc1234", "release/2026"))

	if got := decodeBody(t, rec)["status"]; got != "already queued" {
		t.Fatalf("status %q body %q", got, rec.Body.String())
	}
	if ts.jobCount(t) != 1 {
		t.Fatal("one commit means one run, whichever ref it arrives on")
	}
}

// The suite id travels from the payload onto the job row.
func TestWebhookRecordsCheckSuiteID(t *testing.T) {
	ts := newTestServer(t)
	ts.store.UpsertBinding(store.BindingInput{GitHubAppID: ts.app.ID, Repo: "winpra/api", Enabled: true})
	id := decodeBody(t, ts.post(t, "ci", "check_suite", "d-1",
		checkSuiteBodyAction("winpra/api", "abc1234", "main", "requested")))["job"]
	job, err := ts.store.Job(id)
	if err != nil {
		t.Fatal(err)
	}
	if job.CheckSuiteID != 5150 {
		t.Fatalf("check suite id not recorded on the job: %d", job.CheckSuiteID)
	}
}
