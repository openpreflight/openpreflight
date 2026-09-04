package pages

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/openpreflight/openpreflight/internal/coolify"
	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/githubapp"
	"github.com/openpreflight/openpreflight/internal/health"
	"github.com/openpreflight/openpreflight/internal/pipeline"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/web"
	"github.com/openpreflight/openpreflight/internal/web/components/badge"
)

const setupPasswordHint = "At least 12 characters. There is no password reset in v1, so keep it somewhere you can find."

const setupURLHint = "GitHub must reach this host over HTTPS. Webhooks go to /webhook/{slug}; Check Run links go to /runs/{id}."

func settingsURLHint() string {
	return "Used for webhook URLs and the details_url GitHub puts on a check."
}

func dockerHint(ok bool, host string) string {
	s := "Used when a pipeline sets runtime:, and required to run fork PRs. Docker is "
	if ok {
		s += "reachable"
	} else {
		s += "not reachable"
	}
	if host != "" {
		s += " (" + host + ")"
	}
	return s + "."
}

func asMap(data any) map[string]any {
	switch m := data.(type) {
	case map[string]any:
		return m
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	default:
		return map[string]any{}
	}
}

func get[T any](m map[string]any, key string) T {
	var zero T
	v, ok := m[key]
	if !ok || v == nil {
		return zero
	}
	if t, ok := v.(T); ok {
		return t
	}
	b, err := json.Marshal(v)
	if err != nil {
		return zero
	}
	var out T
	if json.Unmarshal(b, &out) != nil {
		return zero
	}
	return out
}

func str(m map[string]any, key string) string {
	return get[string](m, key)
}

func asInt(m map[string]any, key string) int {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return get[int](m, key)
	}
}

func asInt64(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return get[int64](m, key)
	}
}

func asBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return get[bool](m, key)
}

func has(m map[string]any, key string) bool {
	v, ok := m[key]
	return ok && v != nil
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
func itoa(n int) string     { return strconv.Itoa(n) }

func timeoutVal(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func agoPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return web.Ago(t.UTC())
}

type dashRepo struct {
	ID      int64
	Repo    string
	LastJob store.Job
}

type bindingRow struct {
	Binding     store.RepoBinding
	AppName     string
	CoolifyName string
	LastJob     store.Job
}

type pickerRepo struct {
	FullName string
	Private  bool
	Bound    bool
}

func jobVariant(j store.Job) badge.Variant {
	switch j.Status {
	case store.JobSuccess:
		return badge.VariantDefault
	case store.JobFailure, store.JobError:
		return badge.VariantDestructive
	case store.JobInProgress:
		return badge.VariantOutline
	case store.JobQueued:
		return badge.VariantSecondary
	default:
		return badge.VariantGhost
	}
}

func healthVariant(lastError string, seen bool) badge.Variant {
	if lastError != "" {
		return badge.VariantDestructive
	}
	if seen {
		return badge.VariantDefault
	}
	return badge.VariantSecondary
}

func healthLabel(lastError string, seen bool) string {
	if lastError != "" {
		return "failed"
	}
	if seen {
		return "ok"
	}
	return "never"
}

const selectClass = "border-input bg-transparent dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring/50 h-8 w-full min-w-0 rounded-lg border px-2.5 py-1 text-sm outline-none focus-visible:ring-3"

const selectToolbarClass = "border-input bg-transparent dark:bg-input/30 focus-visible:border-ring focus-visible:ring-ring/50 h-8 w-[11rem] shrink-0 rounded-lg border px-2.5 py-1 text-sm outline-none focus-visible:ring-3"

const emptyInCardClass = "border-0 bg-transparent px-4 py-10"

const emptyPageClass = "mb-6"

// Keep the glob out of .templ files: templ treats /* as a comment opener.
const branchesPlaceholder = "main, release/*"

func bindingFormTitle(editing bool) string {
	if editing {
		return "Edit binding"
	}
	return "Add a binding manually"
}

func bindingFilterLine(row bindingRow) string {
	var b strings.Builder
	b.WriteString(row.AppName)
	if row.CoolifyName != "" {
		b.WriteString(" · ")
		b.WriteString(row.CoolifyName)
	}
	b.WriteString(" · ")
	if row.Binding.Branches != "" {
		b.WriteString(row.Binding.Branches)
	} else {
		b.WriteString("all branches")
	}
	if row.Binding.Paths != "" {
		b.WriteString(" · ")
		b.WriteString(row.Binding.Paths)
	}
	return b.String()
}

func bindingSubmitLabel(editing bool) string {
	if editing {
		return "Save binding"
	}
	return "Add binding"
}

func pickerError(m map[string]any) string {
	return str(m, "PickerError")
}

func webhookHint(base string) string {
	if base == "" {
		return "{public base URL}/webhook/{slug}"
	}
	return strings.TrimRight(base, "/") + "/webhook/{slug}"
}

func appFormAction(editing bool, id int64) string {
	if editing {
		return "/api/v1/github-apps/" + itoa64(id)
	}
	return "/api/v1/github-apps"
}

func appFormTitle(editing bool, name string) string {
	if editing {
		if name != "" {
			return "Edit " + name
		}
		return "Edit App"
	}
	return "Add an App"
}

func appFormLede(editing bool) string {
	if editing {
		return "Leave the webhook secret and PEM blank to keep the stored values."
	}
	return "Create with GitHub, or paste credentials under Advanced. GitHub Enterprise uses paste."
}

func appIDValue(editing bool, id int64) string {
	if !editing || id == 0 {
		return ""
	}
	return itoa64(id)
}

func apiURLValue(editing bool, u string) string {
	if !editing && u == "" {
		return "https://api.github.com"
	}
	return u
}

func pemAttrs(editing bool) templ.Attributes {
	if editing {
		return nil
	}
	return templ.Attributes{"required": ""}
}

func settingsOf(m map[string]any) store.Settings { return get[store.Settings](m, "Settings") }

func settingsSectionOf(m map[string]any) string {
	s := str(m, "Section")
	if s == "" {
		return "configuration"
	}
	return s
}
func appsOf(m map[string]any) []store.GitHubApp {
	return get[[]store.GitHubApp](m, "Apps")
}
func coolifyOf(m map[string]any) []store.CoolifyInstance {
	return get[[]store.CoolifyInstance](m, "Coolify")
}
func instancesOf(m map[string]any) []store.CoolifyInstance {
	return get[[]store.CoolifyInstance](m, "Instances")
}
func bindingsOf(m map[string]any) []store.RepoBinding {
	return get[[]store.RepoBinding](m, "Bindings")
}
func jobsOf(m map[string]any) []store.Job { return get[[]store.Job](m, "Jobs") }

func jobRepoOf(m map[string]any) string   { return str(m, "Repo") }
func jobStatusOf(m map[string]any) string { return str(m, "Status") }
func jobLimitOf(m map[string]any) int {
	n := asInt(m, "Limit")
	if n <= 0 || n > 500 {
		return 100
	}
	return n
}
func jobOffsetOf(m map[string]any) int { return asInt(m, "Offset") }

func jobPrevOffset(offset, limit int) int {
	n := offset - limit
	if n < 0 {
		return 0
	}
	return n
}

// jobFiltered is true when the jobs index is constrained by repo, status, or
// a non-zero offset, so an empty table is "no matches" rather than "no jobs yet".
func jobFiltered(m map[string]any) bool {
	return jobRepoOf(m) != "" || jobStatusOf(m) != "" || jobOffsetOf(m) > 0
}

func jobsPath(repo, status string, limit, offset int) string {
	q := url.Values{}
	if repo != "" {
		q.Set("repo", repo)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 && limit != 100 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if enc := q.Encode(); enc != "" {
		return "/jobs?" + enc
	}
	return "/jobs"
}
func inflightOf(m map[string]any) []store.Job {
	return get[[]store.Job](m, "InFlight")
}
func cardsOf(m map[string]any) []dashRepo  { return get[[]dashRepo](m, "RepoCards") }
func rowsOf(m map[string]any) []bindingRow { return get[[]bindingRow](m, "Bindings") }
func pickerOf(m map[string]any) []pickerRepo {
	return get[[]pickerRepo](m, "PickerRepos")
}
func appOf(m map[string]any, key string) store.GitHubApp {
	return get[store.GitHubApp](m, key)
}
func instOf(m map[string]any, key string) store.CoolifyInstance {
	return get[store.CoolifyInstance](m, key)
}
func bindingOf(m map[string]any) store.RepoBinding { return get[store.RepoBinding](m, "Edit") }

// bindingKeyOf reads a binding stored under an explicit key, so a page can hold
// more than one.
func bindingKeyOf(m map[string]any, key string) store.RepoBinding {
	return get[store.RepoBinding](m, key)
}
func jobOf(m map[string]any) store.Job { return get[store.Job](m, "Job") }
func stepsOf(m map[string]any) []executor.Result {
	return get[[]executor.Result](m, "Steps")
}
func installsOf(m map[string]any) []githubapp.Installation {
	return get[[]githubapp.Installation](m, "Installations")
}
func ghReposOf(m map[string]any) []githubapp.Repository {
	return get[[]githubapp.Repository](m, "Repos")
}
func serversOf(m map[string]any) []coolify.Server {
	return get[[]coolify.Server](m, "Servers")
}
func connectorsOf(m map[string]any) []coolify.GitHubApp {
	return get[[]coolify.GitHubApp](m, "Connectors")
}

func dashNeedApp(n int) bool { return n == 0 }

// dashSetupDone reports whether the whole first-run arc is complete: configured,
// wired, and proved by a check that actually passed. Only then does the setup
// card retire — a finished install should not be told how to install.
//
// It comes back if a step regresses (the App is deleted, every binding is
// disabled), because then it is true again.
func dashSetupDone(baseURL string, apps, enabled int, passedAny bool) bool {
	return baseURL != "" && apps > 0 && enabled > 0 && passedAny
}
func dashNeedRepo(apps, enabled int) bool {
	return apps > 0 && enabled == 0
}
func dashHaveSetup(apps, enabled int) bool {
	return apps > 0 && enabled > 0
}

func editAppSelected(editing bool, id, editID int64) bool {
	return editing && id == editID
}

func editInstSelected(editing bool, id, editID int64) bool {
	return editing && id == editID
}

func bindingEnabledChecked(editing bool, enabled bool) bool {
	return !editing || enabled
}

func shareableChecked(editing bool, shareable bool) bool {
	return editing && shareable
}

func jobInFlight(j store.Job) bool { return j.InFlight() }

func jobRefLine(j store.Job) string {
	ref := j.Ref
	sha := web.ShortSHA(j.SHA)
	switch {
	case ref != "" && sha != "":
		return ref + " · " + sha
	case ref != "":
		return ref
	case sha != "":
		return sha
	default:
		return "-"
	}
}

func coolifyFormAction(editing bool, id int64) string {
	if editing {
		return "/api/v1/coolify/" + itoa64(id)
	}
	return "/api/v1/coolify"
}

// repoSubtitle is the one-line description under a repository's name.
func repoSubtitle(b store.RepoBinding) string {
	if !b.Enabled {
		return "This binding is disabled, so webhooks for it are dropped."
	}
	return "What this repository is configured to run, and what it has been running."
}

// repoFailureReason is the short "why" for a run that did not pass. A skip says
// which kind of skip, since those used to be indistinguishable.
func repoFailureReason(j store.Job) string {
	if j.SkipReason != "" {
		return skipReasonLabel(j.SkipReason)
	}
	return j.Error
}

// skipReasonLabel turns a stored skip reason into something an operator reads.
func skipReasonLabel(reason string) string {
	switch reason {
	case store.SkipReasonPathFilter:
		return "no changed path matched the filter"
	case store.SkipReasonNoPipeline:
		return "nothing to run: no pipeline, commands, or recognisable project"
	case store.SkipReasonForkDisabled:
		return "fork pull requests are not run"
	case store.SkipReasonForkNoDocker:
		return "fork pull requests need a reachable Docker engine"
	case store.SkipReasonForkNoRuntime:
		return "fork pull requests need settings.default_runtime"
	default:
		return reason
	}
}

func orDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

func orAll(v string) string {
	if strings.TrimSpace(v) == "" {
		return "all"
	}
	return v
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// resolutionOf is the dry run's result. It is the pipeline package's own type
// rather than a mirror struct here, so the page and the JSON endpoint cannot
// drift apart.
func resolutionOf(m map[string]any) pipeline.Resolution {
	return get[pipeline.Resolution](m, "Resolution")
}

// originsOf decodes the per-value provenance recorded on a job. A job from
// before migration 0008, or one that never got as far as resolving a plan, has
// none — an empty list, not an error, because the run page still has everything
// else worth showing.
func originsOf(j store.Job) []pipeline.Origin {
	if strings.TrimSpace(j.PlanOrigins) == "" {
		return nil
	}
	var out []pipeline.Origin
	if err := json.Unmarshal([]byte(j.PlanOrigins), &out); err != nil {
		return nil
	}
	return out
}

// reportOf is the instance self-report rendered by the status page. It is the
// health package's own type rather than a mirror here, for the same reason the
// dry run's is: two definitions of "what is wrong" would drift.
func reportOf(m map[string]any) health.Report {
	return get[health.Report](m, "Report")
}
