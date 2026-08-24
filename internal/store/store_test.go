package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trivedi-vatsal/openpreflight/internal/secret"
)

// testKey is a fixture key: tests never depend on a real CI_SECRET_KEY.
const testKey = "fixture-key-for-tests-only-1234567890"

func newTestStore(t *testing.T) *Store {
	t.Helper()
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(filepath.Join(t.TempDir(), "ci.db"), box)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMigrateIsIdempotent(t *testing.T) {
	box, _ := secret.New(testKey)
	path := filepath.Join(t.TempDir(), "ci.db")
	for i := 0; i < 2; i++ {
		st, err := Open(path, box)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}
}

func TestSettingsSeedAndSave(t *testing.T) {
	st := newTestStore(t)
	got, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultCheckName != "openpreflight" || got.DefaultPipelineFile != ".ci.yml" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	if !got.SkipForkPRs {
		t.Fatal("fork PRs must be skipped by default")
	}
	got.PublicBaseURL = "https://ci.example.com"
	got.LogRetentionDays = 3
	if err := st.SaveSettings(got); err != nil {
		t.Fatal(err)
	}
	again, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if again.PublicBaseURL != "https://ci.example.com" || again.LogRetentionDays != 3 {
		t.Fatalf("settings did not persist: %+v", again)
	}
}

// Renaming the product must not rename a live install's Check Run. GitHub
// matches a required status check by name string, so rewriting an existing row
// would leave that repo's branch protection waiting for a check that never
// reports again. The new default applies to fresh databases only.
func TestExistingInstallKeepsItsCheckName(t *testing.T) {
	box, _ := secret.New(testKey)
	path := filepath.Join(t.TempDir(), "ci.db")

	st, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for an install created before the rename.
	settings, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.DefaultCheckName = "Coolify CI"
	if err := st.SaveSettings(settings); err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Reopen: migrations re-run and the settings row is read, not reseeded.
	again, err := Open(path, box)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got, err := again.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultCheckName != "Coolify CI" {
		t.Fatalf("an existing install's check name was rewritten to %q", got.DefaultCheckName)
	}
}

func TestUsersAndSessions(t *testing.T) {
	st := newTestStore(t)
	if has, _ := st.HasUsers(); has {
		t.Fatal("fresh store should have no users")
	}
	if _, err := st.CreateUser("admin", "short"); err == nil {
		t.Fatal("expected short password to be rejected")
	}
	user, err := st.CreateUser("admin", "a-long-enough-password")
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := st.HasUsers(); !has {
		t.Fatal("user was not recorded")
	}
	if _, err := st.Authenticate("admin", "wrong-password-here"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong password: got %v", err)
	}
	if _, err := st.Authenticate("nobody", "a-long-enough-password"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user: got %v", err)
	}
	if _, err := st.Authenticate("admin", "a-long-enough-password"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	token, _, err := st.CreateSession(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if u, err := st.UserBySession(token); err != nil || u.ID != user.ID {
		t.Fatalf("session lookup: %v %+v", err, u)
	}
	if err := st.DeleteSession(token); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UserBySession(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session still valid: %v", err)
	}
}

func TestCoolifyTokenIsEncryptedAndRedacted(t *testing.T) {
	st := newTestStore(t)
	inst, err := st.CreateCoolifyInstance(CoolifyInput{
		Name: "prod", BaseURL: "https://coolify.example.com/", APIToken: "3|supersecrettokenvalue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inst.BaseURL != "https://coolify.example.com" {
		t.Fatalf("trailing slash not trimmed: %q", inst.BaseURL)
	}
	if strings.Contains(inst.APITokenRedacted, "supersecret") {
		t.Fatalf("redacted token leaks the secret: %q", inst.APITokenRedacted)
	}
	// The stored column must not contain the plaintext.
	var stored string
	if err := st.DB().QueryRow(`SELECT api_token_enc FROM coolify_instances WHERE id = ?`, inst.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "supersecret") {
		t.Fatal("token is stored in plaintext")
	}
	plain, err := st.DecryptCoolifyToken(inst)
	if err != nil || plain != "3|supersecrettokenvalue" {
		t.Fatalf("decrypt: %v %q", err, plain)
	}

	// An empty token on update keeps the stored one.
	updated, err := st.UpdateCoolifyInstance(inst.ID, CoolifyInput{Name: "prod-2", BaseURL: inst.BaseURL})
	if err != nil {
		t.Fatal(err)
	}
	if plain, _ := st.DecryptCoolifyToken(updated); plain != "3|supersecrettokenvalue" {
		t.Fatal("update with an empty token discarded the stored token")
	}
	if updated.Name != "prod-2" {
		t.Fatalf("name not updated: %q", updated.Name)
	}
}

func TestCoolifyTestResultResetsTeamOnTokenChange(t *testing.T) {
	st := newTestStore(t)
	inst, _ := st.CreateCoolifyInstance(CoolifyInput{Name: "a", BaseURL: "https://c.example.com", APIToken: "1|aaa"})
	if err := st.RecordCoolifyTest(inst.ID, "7", "Platform", ""); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := st.CoolifyInstance(inst.ID)
	if reloaded.TeamName != "Platform" || reloaded.LastSeenAt == nil {
		t.Fatalf("test result not stored: %+v", reloaded)
	}
	// A new token may belong to a different team, so the cached label must go.
	after, err := st.UpdateCoolifyInstance(inst.ID, CoolifyInput{Name: "a", BaseURL: reloaded.BaseURL, APIToken: "2|bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if after.TeamName != "" || after.TeamID != "" {
		t.Fatalf("stale team label survived a token change: %+v", after)
	}
}

func mustApp(t *testing.T, st *Store, name string) GitHubApp {
	t.Helper()
	app, err := st.CreateGitHubApp(GitHubAppInput{
		Name: name, AppID: 4242, PEM: "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----",
		WebhookSecret: "webhook-secret-value",
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func TestGitHubAppSecretsAndSlug(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "Winpra CI")
	if app.Slug != "winpra-ci" {
		t.Fatalf("slug not derived: %q", app.Slug)
	}
	if strings.Contains(app.PEMRedacted, "fake") {
		t.Fatalf("redacted PEM leaks content: %q", app.PEMRedacted)
	}
	if secret, err := st.DecryptWebhookSecret(app); err != nil || secret != "webhook-secret-value" {
		t.Fatalf("webhook secret round trip: %v %q", err, secret)
	}
	if _, err := st.GitHubAppBySlug("winpra-ci"); err != nil {
		t.Fatalf("lookup by slug: %v", err)
	}
	if _, err := st.CreateGitHubApp(GitHubAppInput{
		Name: "Other", Slug: "winpra-ci", AppID: 1, PEM: "x", WebhookSecret: "y",
	}); err == nil {
		t.Fatal("duplicate slug must be rejected: the webhook path has to be unique")
	}
	if _, err := st.CreateGitHubApp(GitHubAppInput{Name: "No id", PEM: "x", WebhookSecret: "y"}); err == nil {
		t.Fatal("missing app_id must be rejected")
	}
}

func TestBindingUpsertAndAllowList(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")

	if _, err := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "not-a-repo"}); err == nil {
		t.Fatal("repo must be owner/name")
	}
	if _, err := st.UpsertBinding(BindingInput{Repo: "o/r"}); err == nil {
		t.Fatal("a binding without an App must be rejected")
	}
	if _, err := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "o/r", PipelineFile: "../etc/passwd"}); err == nil {
		t.Fatal("pipeline file outside the repo must be rejected")
	}

	b, err := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "winpra/api", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if b.ShareableLogs {
		t.Fatal("shareable logs must default to off")
	}
	// Upsert is keyed on (app, repo): a second call updates rather than adds.
	again, err := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "winpra/api", Enabled: false, Branches: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != b.ID {
		t.Fatalf("upsert created a second row: %d vs %d", again.ID, b.ID)
	}
	all, _ := st.ListBindings()
	if len(all) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(all))
	}
	// Disabled is not in the allow-list.
	if _, err := st.EnabledBinding(app.ID, "winpra/api"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled binding is still in the allow-list: %v", err)
	}
	st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "winpra/api", Enabled: true})
	if _, err := st.EnabledBinding(app.ID, "WINPRA/API"); err != nil {
		t.Fatalf("repo lookup should be case-insensitive: %v", err)
	}
	if _, err := st.EnabledBinding(app.ID, "someone/else"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown repo must not resolve")
	}
}

func TestBranchAllowed(t *testing.T) {
	cases := []struct {
		list, branch string
		want         bool
	}{
		{"", "anything", true},
		{"main", "main", true},
		{"main", "develop", false},
		{"main, release/*", "release/2026-01", true},
		{"main\nrelease/*", "release/x", true},
		{"release/*", "releases/x", false},
		{"main", "refs/heads/main", true},
		{"main", "", false},
	}
	for _, c := range cases {
		b := RepoBinding{Branches: c.list}
		if got := b.BranchAllowed(c.branch); got != c.want {
			t.Errorf("branches %q vs %q: got %v want %v", c.list, c.branch, got, c.want)
		}
	}
}

func TestJobLifecycleAndDeliveryDedup(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	binding, _ := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "o/r", Enabled: true})

	job, err := st.EnqueueJob(JobInput{
		BindingID: binding.ID, GitHubAppID: app.ID, Repo: "o/r", SHA: "deadbeef",
		Ref: "main", Event: "check_suite.requested", DeliveryID: "delivery-1", InstallationID: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(job.ID) != 36 {
		t.Fatalf("job id should be a uuid, got %q", job.ID)
	}
	if job.Status != JobQueued {
		t.Fatalf("new job status %q", job.Status)
	}

	// Queued: the delivery is in flight, so a redelivery is a no-op.
	if _, err := st.InFlightJobForDelivery("delivery-1"); err != nil {
		t.Fatalf("queued job should hold its delivery: %v", err)
	}
	claimed, err := st.ClaimNextJob()
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim: %v %+v", err, claimed)
	}
	if claimed.StartedAt == nil {
		t.Fatal("claim must set started_at")
	}
	if _, err := st.ClaimNextJob(); !errors.Is(err, ErrNotFound) {
		t.Fatal("a claimed job must not be claimable twice")
	}
	// Running: still in flight.
	if _, err := st.InFlightJobForDelivery("delivery-1"); err != nil {
		t.Fatalf("running job should hold its delivery: %v", err)
	}
	if err := st.FinishJob(job.ID, JobSuccess, "success", `[{"name":"test"}]`, ""); err != nil {
		t.Fatal(err)
	}
	// Completed: GitHub's Redeliver must be able to start a new job.
	if _, err := st.InFlightJobForDelivery("delivery-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a completed job must release its delivery id: %v", err)
	}
	done, _ := st.Job(job.ID)
	if done.Status != JobSuccess || done.FinishedAt == nil || done.Duration() <= 0 {
		t.Fatalf("finished job: %+v", done)
	}
}

func TestRequeueStaleJobs(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	job, _ := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "abc"})
	st.ClaimNextJob()
	n, err := st.RequeueStaleJobs()
	if err != nil || n != 1 {
		t.Fatalf("requeue: %v n=%d", err, n)
	}
	reloaded, _ := st.Job(job.ID)
	if reloaded.Status != JobQueued || reloaded.StartedAt != nil {
		t.Fatalf("interrupted job was not requeued: %+v", reloaded)
	}
}

func TestInFlightJobsForRef(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	a, _ := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "aaa", Ref: "main"})
	st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "bbb", Ref: "other"})
	jobs, err := st.InFlightJobsForRef("o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].ID != a.ID {
		t.Fatalf("expected only the main job, got %+v", jobs)
	}
	st.FinishJob(a.ID, JobSuccess, "success", "[]", "")
	jobs, _ = st.InFlightJobsForRef("o/r", "main")
	if len(jobs) != 0 {
		t.Fatal("finished jobs are not in flight")
	}
}

func TestInFlightJobForSuite(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	other := mustApp(t, st, "ci-two")

	a, _ := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "aaa", Ref: "main"})
	// Same commit, different ref: still the same suite. This is the duplicate
	// InFlightJobsForRef cannot see.
	got, err := st.InFlightJobForSuite(app.ID, "o/r", "aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a.ID {
		t.Fatalf("expected %s, got %s", a.ID, got.ID)
	}

	// A different commit, repo or App is a different suite.
	for _, c := range []struct {
		name      string
		appID     int64
		repo, sha string
	}{
		{"other sha", app.ID, "o/r", "bbb"},
		{"other repo", app.ID, "o/other", "aaa"},
		{"other app", other.ID, "o/r", "aaa"},
	} {
		if _, err := st.InFlightJobForSuite(c.appID, c.repo, c.sha); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s should not match: %v", c.name, err)
		}
	}

	// An empty repo or sha must not match every row.
	if _, err := st.InFlightJobForSuite(app.ID, "", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty key should not match: %v", err)
	}

	st.FinishJob(a.ID, JobSuccess, "success", "[]", "")
	if _, err := st.InFlightJobForSuite(app.ID, "o/r", "aaa"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a finished job is not in flight: %v", err)
	}
}

// The suite id round-trips onto the job row. It is recorded for traceability,
// so a zero value must also be preserved rather than rejected.
func TestEnqueueJobRecordsCheckSuiteID(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	withID, err := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "aaa", CheckSuiteID: 5150})
	if err != nil {
		t.Fatal(err)
	}
	if withID.CheckSuiteID != 5150 {
		t.Fatalf("suite id not stored: %d", withID.CheckSuiteID)
	}
	reloaded, err := st.Job(withID.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CheckSuiteID != 5150 {
		t.Fatalf("suite id not reloaded: %d", reloaded.CheckSuiteID)
	}
	none, err := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	if none.CheckSuiteID != 0 {
		t.Fatalf("a job with no suite id should store zero: %d", none.CheckSuiteID)
	}
}

func TestPruneJobsRespectsInFlight(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	old, _ := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "old", DeliveryID: "d-old"})
	st.FinishJob(old.ID, JobSuccess, "success", "[]", "")
	running, _ := st.EnqueueJob(JobInput{GitHubAppID: app.ID, Repo: "o/r", SHA: "new"})

	// Backdate the finished job past the retention window.
	past := formatTime(time.Now().UTC().Add(-48 * time.Hour))
	if _, err := st.DB().Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, past, old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE deliveries SET received_at = ? WHERE delivery_id = 'd-old'`, past); err != nil {
		t.Fatal(err)
	}
	ids, err := st.PruneJobs(24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != old.ID {
		t.Fatalf("expected the old job pruned, got %v", ids)
	}
	if _, err := st.Job(old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("pruned job still present")
	}
	if _, err := st.Job(running.ID); err != nil {
		t.Fatal("a queued job must never be pruned")
	}
	var deliveries int
	st.DB().QueryRow(`SELECT count(*) FROM deliveries WHERE delivery_id = 'd-old'`).Scan(&deliveries)
	if deliveries != 0 {
		t.Fatal("old delivery row was not pruned")
	}
}

func TestDeletingAppRemovesBindings(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "o/r", Enabled: true})
	if err := st.DeleteGitHubApp(app.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListBindings()
	if len(all) != 0 {
		t.Fatalf("bindings outlived their App: %+v", all)
	}
}

func TestDeletingCoolifyKeepsBinding(t *testing.T) {
	st := newTestStore(t)
	app := mustApp(t, st, "ci")
	inst, _ := st.CreateCoolifyInstance(CoolifyInput{Name: "c", BaseURL: "https://c.example.com", APIToken: "1|x"})
	b, err := st.UpsertBinding(BindingInput{GitHubAppID: app.ID, Repo: "o/r", CoolifyInstanceID: inst.ID, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteCoolifyInstance(inst.ID); err != nil {
		t.Fatal(err)
	}
	// The Coolify row is inventory, not a dependency of running a check.
	reloaded, err := st.Binding(b.ID)
	if err != nil {
		t.Fatalf("binding disappeared with its Coolify row: %v", err)
	}
	if reloaded.CoolifyInstanceID != 0 {
		t.Fatalf("dangling coolify reference: %d", reloaded.CoolifyInstanceID)
	}
}
