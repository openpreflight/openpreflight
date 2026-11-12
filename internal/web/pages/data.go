package pages

import (
	"encoding/json"
	"net/url"
	"sort"
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
	"github.com/openpreflight/openpreflight/internal/web/components/icon"
	"github.com/openpreflight/openpreflight/internal/web/layouts"
)

const setupTokenHint = "Printed in the server log when it started, as setup_token=… on the line that begins \"no admin yet\"."

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

// get reads one key. Every value in the map is put there by internal/api as
// the exact type the page asks for, so a failed assertion is a typo in the key
// or the type, and the zero value renders an empty section.
func get[T any](m map[string]any, key string) T {
	v, _ := m[key].(T)
	return v
}

func str(m map[string]any, key string) string {
	return get[string](m, key)
}

func asInt(m map[string]any, key string) int { return get[int](m, key) }

func asInt64(m map[string]any, key string) int64 { return get[int64](m, key) }

func asBool(m map[string]any, key string) bool { return get[bool](m, key) }

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

type (
	dashRepo   = web.DashRepo
	bindingRow = web.BindingRow
	pickerRepo = web.PickerRepo
)

// jobTone is a job's status in the app's five-colour vocabulary. Skipped and
// cancelled are outcomes, not problems, so they stay neutral.
func jobTone(j store.Job) layouts.Tone {
	switch j.Status {
	case store.JobSuccess:
		return layouts.ToneSuccess
	case store.JobFailure, store.JobError:
		return layouts.ToneDanger
	case store.JobInProgress:
		return layouts.ToneLive
	case store.JobQueued:
		return layouts.ToneInfo
	default:
		return layouts.ToneNeutral
	}
}

// jobRefLabel is the ref as a person says it: "main", "PR #47". GitHub hands
// over either a bare branch or a full ref depending on the event.
func jobRefLabel(j store.Job) string {
	ref := j.Ref
	if rest, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		n, _, _ := strings.Cut(rest, "/")
		return "PR #" + n
	}
	if j.PullNumber != 0 && ref == "" {
		return "PR #" + itoa(j.PullNumber)
	}
	for _, pre := range []string{"refs/heads/", "refs/tags/"} {
		if b, ok := strings.CutPrefix(ref, pre); ok {
			return b
		}
	}
	return orDash(ref)
}

// eventLabel names what started the run, in words rather than webhook
// vocabulary.
func eventLabel(j store.Job) string {
	switch j.Event {
	case "check_suite.requested":
		if j.PullNumber != 0 {
			return "pull request"
		}
		return "push"
	case "check_suite.rerequested", "check_run.rerequested":
		return "re-run from GitHub"
	case "manual.rerun":
		return "re-run"
	case "":
		return "—"
	}
	return strings.ReplaceAll(j.Event, "_", " ")
}

func stepMarkClass(s executor.Result) string {
	return "font-mono " + stepTone(s)
}

func stepTone(s executor.Result) string {
	switch {
	case s.Skipped:
		return "text-muted-foreground"
	case s.OK():
		return "text-primary"
	default:
		return "text-destructive"
	}
}

// healthStatus is a Coolify instance's last check as a status pill.
func healthStatus(lastError string, seen bool) (string, layouts.Tone) {
	switch {
	case lastError != "":
		return "failed", layouts.ToneDanger
	case seen:
		return "connected", layouts.ToneSuccess
	}
	return "not tested", layouts.ToneNeutral
}

// filterLabelClass is the label beside a control on a toolbar row, where the
// label is a hint rather than the caption of a stacked field.
const filterLabelClass = "text-muted-foreground text-xs font-medium"

const allStatuses = "All statuses"

const appInstallationsLabel = "This App's installations"

const emptyInCardClass = "border-0 bg-transparent px-6 py-12"

const emptyPageClass = "mb-6"

// Keep the glob out of .templ files: templ treats /* as a comment opener.
const branchesPlaceholder = "main, release/*"

func bindingFormTitle(editing bool, repo string) string {
	if editing && repo != "" {
		return repo
	}
	if editing {
		return "Edit binding"
	}
	return "Add a repository"
}

func bindingFormLede(editing bool) string {
	if editing {
		return "Edit binding: when this repository is checked, who can read its logs, and what it overrides."
	}
	return "Bind one repository to the GitHub App that receives its webhooks. Only the repository is required."
}

func repoHint(editing bool) string {
	if editing {
		return "Fixed. To check a different repository, remove this binding and add another."
	}
	return "Owner and name exactly as GitHub spells them."
}

// bindingOverrideCount is how many of the folded-away overrides a binding
// sets; the Overrides panel opens by itself when it is not zero.
func bindingOverrideCount(b store.RepoBinding) int {
	n := 0
	for _, v := range []string{b.CheckName, b.PipelineFile, b.InstallCmd, b.TestCmd, b.BuildCmd} {
		if strings.TrimSpace(v) != "" {
			n++
		}
	}
	if b.TimeoutSeconds > 0 {
		n++
	}
	return n
}

func bindingOverrides(b store.RepoBinding) bool { return bindingOverrideCount(b) > 0 }

// secondsLabel is a duration in seconds in the largest whole unit: 900 is
// "15 minutes", 3600 is "1 hour", 90 stays "90 seconds".
func secondsLabel(n int) string {
	unit := func(v int, one string) string {
		if v == 1 {
			return "1 " + one
		}
		return itoa(v) + " " + one + "s"
	}
	switch {
	case n > 0 && n%3600 == 0:
		return unit(n/3600, "hour")
	case n > 0 && n%60 == 0:
		return unit(n/60, "minute")
	}
	return unit(n, "second")
}

// cancelHref goes back where the operator came from: the repo's own page when
// editing, the list when adding.
func cancelHref(editing bool, id int64) string {
	if editing {
		return "/repos/" + itoa64(id)
	}
	return "/repos"
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
	return "Add repository"
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
	return "Let GitHub create the App and hand back its credentials, or paste the credentials of one you registered yourself."
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

// Browser autofill has nothing useful to offer an App slug or a Coolify
// instance id, and a password manager offering to fill "Webhook secret" with
// the operator's login is worse than unhelpful. Shared so every editor field
// opts out the same way.
var (
	noAutofill = templ.Attributes{"autocomplete": "off", "spellcheck": "false"}
	slugAttrs  = templ.Attributes{"autocomplete": "off", "spellcheck": "false", "autocapitalize": "none", "pattern": "[a-z0-9-]+"}
	// new-password rather than off: Chrome ignores off on password inputs.
	secretAttrs = templ.Attributes{"autocomplete": "new-password", "spellcheck": "false"}
)

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
		return jobPageSize
	}
	return n
}

// jobPageSize is the Jobs page's default; jobPageSizes are what the rows-per-
// page control offers.
const jobPageSize = 25

var jobPageSizes = []int{25, 50, 100}

// jobPages is the page numbers to show for a list of total rows: the first,
// the last, and the current one with a neighbour either side; 0 marks a gap.
func jobPages(total, limit, offset int) []int {
	if limit <= 0 || total <= 0 {
		return []int{1}
	}
	last := (total + limit - 1) / limit
	cur := jobPageNumber(offset, limit)
	var out []int
	for n := 1; n <= last; n++ {
		if n == 1 || n == last || (n >= cur-1 && n <= cur+1) {
			if len(out) > 0 && out[len(out)-1] != 0 && n-out[len(out)-1] > 1 {
				out = append(out, 0)
			}
			out = append(out, n)
		}
	}
	return out
}

// jobRange is "26–50 of 87": which rows this page holds, out of how many.
func jobRange(total, limit, offset, shown int) string {
	if total == 0 || shown == 0 {
		return "0 of " + itoa(total)
	}
	return itoa(offset+1) + "–" + itoa(offset+shown) + " of " + itoa(total)
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
	if limit > 0 && limit != jobPageSize {
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
func cardsOf(m map[string]any) []dashRepo   { return get[[]dashRepo](m, "RepoCards") }
func recentOf(m map[string]any) []store.Job { return get[[]store.Job](m, "Recent") }
func rowsOf(m map[string]any) []bindingRow  { return get[[]bindingRow](m, "Bindings") }
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

func bindingEnabledChecked(editing bool, enabled bool) bool {
	return !editing || enabled
}

func shareableChecked(editing bool, shareable bool) bool {
	return editing && shareable
}

func jobInFlight(j store.Job) bool { return j.InFlight() }

func statusLabel(st string) string { return web.JobStatusLabel(store.Job{Status: st}) }

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

// dashNoRepoHint says why the checklist is still waiting: bound but switched
// off reads differently from nothing bound at all.
func dashNoRepoHint(bound int) string {
	if bound > 0 {
		return itoa(bound) + " bound, none enabled"
	}
	return "the worker runs nothing without one"
}

// dashTile is one number on Overview: a label, the value with its unit set
// apart, a chip that says which way it is going, and two lines of context.
// The window is always named: these come from the recent runs the page
// loaded, not an all-time figure.
type dashTile struct {
	// Icon is the component, not its name: a name that no longer exists is a
	// render error at request time, a missing component is a build failure.
	Icon     func(...icon.Props) templ.Component
	Label    string
	Value    string
	Unit     string
	Chip     string
	ChipTone layouts.Tone
	ChipHint string
	Lines    [][2]string // {lead, emphasised}
}

// finished is a run with a result: passed, failed or errored. Skipped and
// cancelled runs have none, so the pass rate, its weekly change and run times
// all leave them out alike.
func finished(j store.Job) bool {
	return !j.InFlight() && j.Status != store.JobSkipped && j.Status != store.JobCancelled
}

func failedJob(j store.Job) bool { return j.Status == store.JobFailure || j.Status == store.JobError }

// weekRates is the pass rate of the last 7 days and the 7 before, from the
// daily counts; ok is false when either week has no finished run.
func weekRates(daily []store.DayCount) (last, prev int, ok bool) {
	if len(daily) < 14 {
		return 0, 0, false
	}
	rate := func(days []store.DayCount) (int, bool) {
		p, f := 0, 0
		for _, d := range days {
			p, f = p+d.Passed, f+d.Failed
		}
		if p+f == 0 {
			return 0, false
		}
		return p * 100 / (p + f), true
	}
	n := len(daily)
	a, okA := rate(daily[n-7:])
	b, okB := rate(daily[n-14 : n-7])
	return a, b, okA && okB
}

// dashTiles reads the same recent-jobs slice the repo list is built from, so
// the numbers and the rows under them describe the same runs. Skipped,
// cancelled and in-flight jobs are not results, so they count towards nothing.
func dashTiles(recent []store.Job, daily []store.DayCount, cards []dashRepo, enabled, bound, inflight int) []dashTile {
	done, passed := 0, 0
	var durs []time.Duration
	var lastFail, slowest store.Job
	for _, j := range recent {
		if !finished(j) {
			continue
		}
		done++
		if j.Status == store.JobSuccess {
			passed++
		}
		if failedJob(j) && lastFail.ID == "" {
			lastFail = j
		}
		if d := j.Duration(); d > 0 {
			durs = append(durs, d)
			if d > slowest.Duration() {
				slowest = j
			}
		}
	}

	rate := dashTile{Icon: icon.CircleCheck, Label: "Pass rate", Value: "—"}
	if done > 0 {
		rate.Value, rate.Unit = itoa(passed*100/done), "%"
		rate.Lines = append(rate.Lines, [2]string{"", itoa(passed) + " of " + itoa(done) + " finished runs passed."})
	} else {
		rate.Lines = append(rate.Lines, [2]string{"", "No finished runs yet."})
	}
	if last, prev, ok := weekRates(daily); ok {
		d := last - prev
		rate.Chip, rate.ChipHint = signed(d)+" pts", "this week against the week before"
		rate.ChipTone = layouts.ToneSuccess
		if d < 0 {
			rate.ChipTone = layouts.ToneDanger
		}
	}
	if lastFail.ID != "" {
		rate.Lines = append(rate.Lines, [2]string{"Last failure: ", lastFail.Repo + " · " + web.Ago(lastFail.CreatedAt)})
	} else if done > 0 {
		rate.Lines = append(rate.Lines, [2]string{"", "No failures in these runs."})
	}

	// Median, not mean: one job that hung for the full timeout would drag an
	// average somewhere no run has ever been.
	run := dashTile{Icon: icon.Timer, Label: "Run time", Value: "—"}
	if len(durs) > 0 {
		sort.Slice(durs, func(a, b int) bool { return durs[a] < durs[b] })
		run.Value = web.HumanDuration(durs[len(durs)/2])
		run.Chip, run.ChipTone, run.ChipHint = "p90 "+web.HumanDuration(durs[len(durs)*9/10]), layouts.ToneNeutral, "nine in ten runs finish within this"
		run.Lines = append(run.Lines, [2]string{"", "Median of " + itoa(len(durs)) + " runs."})
		run.Lines = append(run.Lines, [2]string{"Slowest: ", slowest.Repo + " · " + web.HumanDuration(slowest.Duration())})
	} else {
		run.Lines = append(run.Lines, [2]string{"", "Nothing timed yet."})
	}

	failing, ran := 0, 0
	for _, c := range cards {
		if c.LastJob.ID != "" {
			ran++
			if failedJob(c.LastJob) {
				failing++
			}
		}
	}
	repos := dashTile{Icon: icon.FolderGit, Label: "Repos", Value: itoa(enabled), Unit: " enabled"}
	switch {
	case failing > 0:
		repos.Chip, repos.ChipTone, repos.ChipHint = itoa(failing)+" failing", layouts.ToneDanger, "repos whose last run failed"
	case ran > 0:
		repos.Chip, repos.ChipTone, repos.ChipHint = "all passing", layouts.ToneSuccess, "every repo's last run"
	}
	repos.Lines = append(repos.Lines, [2]string{"Running now: ", itoa(inflight)})
	repos.Lines = append(repos.Lines, [2]string{"Bound but off: ", itoa(bound - enabled)})

	return []dashTile{rate, run, repos}
}

func signed(n int) string {
	if n >= 0 {
		return "+" + itoa(n)
	}
	return itoa(n)
}

// chartBar is one day in the activity chart. Heights are a share of the
// chart's top value, so the tallest day does not always touch the top.
type chartBar struct {
	Label     string // "Nov 3", shown on every third bar and the last
	ShowLabel bool
	Title     string
	Pass      int
	Fail      int
	Other     int
	Height    string // CSS percentage of the plot height
}

// activityChart lays the daily counts out as bars on a nice scale: the top
// of the axis is 1, 2 or 5 times a power of ten, so the gridlines read as
// round numbers.
func activityChart(daily []store.DayCount) (bars []chartBar, top, total, failed int) {
	most := 0
	for _, d := range daily {
		n := d.Passed + d.Failed + d.Other
		total += n
		failed += d.Failed
		most = max(most, n)
	}
	top = niceCeil(most)
	for i, d := range daily {
		n := d.Passed + d.Failed + d.Other
		b := chartBar{
			Label:     d.Day.Format("Jan 2"),
			ShowLabel: (len(daily)-1-i)%3 == 0,
			Pass:      d.Passed,
			Fail:      d.Failed,
			Other:     d.Other,
			Height:    strconv.FormatFloat(float64(n)*100/float64(top), 'f', 1, 64) + "%",
		}
		b.Title = b.Label + ": " + itoa(d.Passed) + " passed, " + itoa(d.Failed) + " failed, " + itoa(d.Other) + " skipped or cancelled"
		bars = append(bars, b)
	}
	return bars, top, total, failed
}

func niceCeil(n int) int {
	if n <= 4 {
		return 4
	}
	for p := 1; ; p *= 10 {
		for _, m := range []int{1, 2, 5} {
			if m*p >= n {
				return m * p
			}
		}
	}
}

// repoHistory is each repo's last ten completed runs (skipped and cancelled
// included; running ones left out), oldest first, from the
// recent jobs the page already loaded.
// ponytail: bounded by those 100 recent jobs; a quiet repo next to a busy one
// shows fewer squares. A per-repo query fixes that if it matters.
func repoHistory(recent []store.Job) map[string][]store.Job {
	out := map[string][]store.Job{}
	for _, j := range recent {
		if j.InFlight() || len(out[j.Repo]) >= 10 {
			continue
		}
		out[j.Repo] = append([]store.Job{j}, out[j.Repo]...)
	}
	return out
}

func dailyOf(m map[string]any) []store.DayCount { return get[[]store.DayCount](m, "Daily") }

// splitApps puts Apps that cannot do their job (not installed, or failing)
// apart from the ones that can, so the broken ones are listed first.
func splitApps(apps []store.GitHubApp) (attention, connected []store.GitHubApp) {
	for _, a := range apps {
		if a.LastError != "" {
			attention = append(attention, a)
		} else {
			connected = append(connected, a)
		}
	}
	return attention, connected
}

func appStatus(a store.GitHubApp) (string, layouts.Tone) {
	switch {
	case a.LastError == store.AppNotInstalled:
		return "not installed", layouts.ToneWarning
	case a.LastError != "":
		return "failed", layouts.ToneDanger
	case a.LastSeenAt != nil:
		return "connected", layouts.ToneSuccess
	}
	return "not tested", layouts.ToneNeutral
}

// appSettingsURL is the App's settings page on GitHub, where it is edited or
// deleted. Org-owned Apps live under the org; OwnerOrg is filled by Test.
// ponytail: github.com only, like the install link.
func appSettingsURL(a store.GitHubApp) string {
	if a.OwnerOrg != "" {
		return "https://github.com/organizations/" + a.OwnerOrg + "/settings/apps/" + a.Slug
	}
	return "https://github.com/settings/apps/" + a.Slug
}

func appOwnerSubtitle(a store.GitHubApp) string {
	if a.OwnerOrg != "" {
		return appSubtitle(a) + " · " + a.OwnerOrg
	}
	return appSubtitle(a)
}

// appSubtitle names an App without repeating its own title: the slug is only
// worth a line when it differs from the name it was derived from.
func appSubtitle(a store.GitHubApp) string {
	if a.Slug != "" && a.Slug != a.Name {
		return a.Slug + " · App ID " + itoa64(a.AppID)
	}
	return "App ID " + itoa64(a.AppID)
}

// idValue renders a foreign key for a Select's DefaultValue. Zero means "not
// chosen", which the component spells as the empty string so the trigger shows
// its placeholder.
func idValue(id int64) string {
	if id == 0 {
		return ""
	}
	return strconv.FormatInt(id, 10)
}

// editAppValue picks the App a binding form opens on: the stored one when
// editing, otherwise the first App in the list. A native select did this
// implicitly by selecting its first option; this says it out loud.
func editAppValue(editing bool, editID int64, apps []store.GitHubApp) string {
	if editing && editID != 0 {
		return strconv.FormatInt(editID, 10)
	}
	if len(apps) > 0 {
		return strconv.FormatInt(apps[0].ID, 10)
	}
	return ""
}

func firstAppName(apps []store.GitHubApp) string {
	if len(apps) > 0 {
		return apps[0].Name
	}
	return "- select -"
}

// editInstValue mirrors editAppValue for the optional Coolify instance, where
// "0" is a real option meaning none rather than an absent choice.
func editInstValue(editing bool, editID int64) string {
	if editing && editID != 0 {
		return strconv.FormatInt(editID, 10)
	}
	return "0"
}

func onEmptyValue(v string) string {
	if v == "fail" {
		return "fail"
	}
	return "skip"
}

// jobPageNumber is 1-based and derived, not stored: the jobs index pages by
// offset, and the number exists only to tell the operator how far in they are.
func jobPageNumber(offset, limit int) int {
	if limit <= 0 {
		return 1
	}
	return offset/limit + 1
}

// cancelJobDescription names the run being cancelled. The jobs table can show
// two in-flight runs of the same repo on adjacent rows, so the dialog repeats
// the commit rather than asking "cancel this run?" about an unnamed one.
func cancelJobDescription(j store.Job) string {
	return "Cancelling stops " + j.Repo + " at " + web.ShortSHA(j.SHA) +
		" where it is, and the Check Run is reported as cancelled. Re-running starts the whole pipeline again."
}

// retentionLabel says how long runs are kept, in the largest whole unit.
func retentionLabel(days int) string {
	switch {
	case days <= 0:
		return ""
	case days == 365:
		return "Kept for a year."
	case days%365 == 0:
		return "Kept for " + itoa(days/365) + " years."
	case days == 1:
		return "Kept for a day."
	}
	return "Kept for " + itoa(days) + " days."
}

// humanBytes is a byte count in binary units: 10485760 → "10 MiB".
func humanBytes(n int64) string {
	units := []string{"bytes", "KiB", "MiB", "GiB", "TiB"}
	f, u := float64(n), 0
	for f >= 1024 && u < len(units)-1 {
		f /= 1024
		u++
	}
	if u == 0 {
		return itoa64(n) + " bytes"
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + " " + units[u]
}

// dashLede says what Overview is for: how to finish setting up, until setup is
// finished, then what the page shows.
func dashLede(done bool) string {
	if done {
		return "What this worker is running, and how it has been going."
	}
	return "Set the public URL, add your CI GitHub App, then enable the repos it should check."
}

// stepResultWord is the step's mark read aloud, for screen readers.
func stepResultWord(s executor.Result) string {
	switch {
	case s.Skipped:
		return "skipped"
	case s.OK():
		return "passed"
	}
	return "failed"
}
