package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/coolify"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/executor"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/githubapp"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/logs"
	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
)

func (s *Server) pageDashboard(w http.ResponseWriter, r *http.Request, user store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	instances, err := s.store.ListCoolifyInstances()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apps, err := s.store.ListGitHubApps()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	bindings, err := s.store.ListBindings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	jobs, err := s.store.ListJobs(8)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	enabled := 0
	for _, b := range bindings {
		if b.Enabled {
			enabled++
		}
	}
	s.render(w, "dashboard", s.page(w, r, &user, "Overview", "home", map[string]any{
		"Settings":        settings,
		"Coolify":         instances,
		"Apps":            apps,
		"Bindings":        bindings,
		"EnabledBindings": enabled,
		"Jobs":            jobs,
	}))
}

func (s *Server) pageSettings(w http.ResponseWriter, r *http.Request, user store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, "settings", s.page(w, r, &user, "Settings", "settings", map[string]any{
		"Settings":        settings,
		"DockerAvailable": s.dockerAvailable(),
		"DockerHost":      s.cfg.DockerHost,
	}))
}

func (s *Server) pageCoolify(w http.ResponseWriter, r *http.Request, user store.User) {
	instances, err := s.store.ListCoolifyInstances()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := map[string]any{"Instances": instances}

	if raw := r.URL.Query().Get("edit"); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		if inst, err := s.store.CoolifyInstance(id); err == nil {
			data["Edit"] = inst
		}
	}

	// ?inspect=N fetches that instance's servers and connectors server-side, so
	// the page needs no JavaScript.
	if raw := r.URL.Query().Get("inspect"); raw != "" {
		id, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil {
			s.badRequest(w, r, errors.New("invalid instance id"))
			return
		}
		inst, err := s.store.CoolifyInstance(id)
		if err != nil {
			s.notFound(w, r, err)
			return
		}
		data["Inspect"] = inst
		client, _, err := s.coolifyClient(id)
		if err != nil {
			data["InspectError"] = err.Error()
		} else {
			var problems []string
			servers, err := client.Servers(r.Context())
			if err != nil {
				problems = append(problems, err.Error())
			} else {
				data["Servers"] = servers
			}
			connectors, err := client.GitHubApps(r.Context())
			if err != nil {
				problems = append(problems, err.Error())
			} else {
				data["Connectors"] = connectors
			}
			if len(problems) > 0 {
				data["InspectError"] = joinComma(problems)
			}
		}
	}
	s.render(w, "coolify", s.page(w, r, &user, "Coolify", "coolify", data))
}

func (s *Server) pageGitHubApps(w http.ResponseWriter, r *http.Request, user store.User) {
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
	data := map[string]any{"Apps": apps, "Settings": settings}

	if raw := r.URL.Query().Get("edit"); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		if app, err := s.store.GitHubApp(id); err == nil {
			data["Edit"] = app
		}
	}

	if raw := r.URL.Query().Get("inspect"); raw != "" {
		id, convErr := strconv.ParseInt(raw, 10, 64)
		if convErr != nil {
			s.badRequest(w, r, errors.New("invalid app id"))
			return
		}
		app, err := s.store.GitHubApp(id)
		if err != nil {
			s.notFound(w, r, err)
			return
		}
		data["Inspect"] = app
		installs, repos, err := s.loadAppRepos(r.Context(), id)
		if err != nil {
			data["InspectError"] = err.Error()
		} else {
			data["Installations"] = installs
			data["Repos"] = repos
		}
	}
	s.render(w, "githubapps", s.page(w, r, &user, "GitHub Apps", "apps", data))
}

// pickerRepo is one row of the repo picker.
type pickerRepo struct {
	FullName string
	Private  bool
	Bound    bool
}

// bindingRow pairs a binding with the names it references, so the template does
// not have to look them up.
type bindingRow struct {
	Binding     store.RepoBinding
	AppName     string
	CoolifyName string
}

func (s *Server) pageRepos(w http.ResponseWriter, r *http.Request, user store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	apps, err := s.store.ListGitHubApps()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	instances, err := s.store.ListCoolifyInstances()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	bindings, err := s.store.ListBindings()
	if err != nil {
		s.fail(w, r, err)
		return
	}

	appNames := map[int64]string{}
	for _, a := range apps {
		appNames[a.ID] = a.Name
	}
	instNames := map[int64]string{}
	for _, i := range instances {
		instNames[i.ID] = i.Name
	}
	rows := make([]bindingRow, 0, len(bindings))
	for _, b := range bindings {
		rows = append(rows, bindingRow{
			Binding:     b,
			AppName:     orUnknown(appNames[b.GitHubAppID], "(deleted app)"),
			CoolifyName: instNames[b.CoolifyInstanceID],
		})
	}

	selectedApp := int64(0)
	if raw := r.URL.Query().Get("app_id"); raw != "" {
		selectedApp, _ = strconv.ParseInt(raw, 10, 64)
	}
	selectedCoolify := int64(0)
	if raw := r.URL.Query().Get("coolify_id"); raw != "" {
		selectedCoolify, _ = strconv.ParseInt(raw, 10, 64)
	}

	data := map[string]any{
		"Settings":          settings,
		"Apps":              apps,
		"Instances":         instances,
		"Bindings":          rows,
		"SelectedAppID":     selectedApp,
		"SelectedCoolifyID": selectedCoolify,
	}

	if raw := r.URL.Query().Get("edit"); raw != "" {
		id, _ := strconv.ParseInt(raw, 10, 64)
		if b, err := s.store.Binding(id); err == nil {
			data["Edit"] = b
		}
	}

	// Load the picker only once an App is chosen: it is the App that must be
	// able to see the repo, whichever list the names come from.
	if selectedApp != 0 {
		bound := map[string]bool{}
		for _, b := range bindings {
			if b.GitHubAppID == selectedApp {
				bound[b.Repo] = true
			}
		}
		var picker []pickerRepo
		if selectedCoolify != 0 {
			repos, err := s.loadCoolifyRepos(r.Context(), selectedCoolify)
			if err != nil {
				data["PickerError"] = err.Error()
			}
			picker = pickerFromCoolify(repos, bound)
		} else {
			_, repos, err := s.loadAppRepos(r.Context(), selectedApp)
			if err != nil {
				data["PickerError"] = err.Error()
			}
			picker = pickerFromGitHub(repos, bound)
		}
		// Bound repos the list no longer contains still need a checkbox, or
		// saving the form would silently unbind them.
		for repo := range bound {
			if !containsRepo(picker, repo) {
				picker = append(picker, pickerRepo{FullName: repo, Bound: true})
			}
		}
		sort.Slice(picker, func(i, j int) bool { return picker[i].FullName < picker[j].FullName })
		data["PickerRepos"] = picker
	}

	s.render(w, "repos", s.page(w, r, &user, "Repos", "repos", data))
}

func pickerFromCoolify(repos []coolify.Repository, bound map[string]bool) []pickerRepo {
	out := make([]pickerRepo, 0, len(repos))
	for _, r := range repos {
		out = append(out, pickerRepo{FullName: r.FullName, Private: r.Private, Bound: bound[r.FullName]})
	}
	return out
}

func pickerFromGitHub(repos []githubapp.Repository, bound map[string]bool) []pickerRepo {
	out := make([]pickerRepo, 0, len(repos))
	for _, r := range repos {
		if r.Archived {
			continue
		}
		out = append(out, pickerRepo{FullName: r.FullName, Private: r.Private, Bound: bound[r.FullName]})
	}
	return out
}

func containsRepo(list []pickerRepo, name string) bool {
	for _, r := range list {
		if r.FullName == name {
			return true
		}
	}
	return false
}

func (s *Server) pageJobs(w http.ResponseWriter, r *http.Request, user store.User) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	jobs, err := s.store.ListJobs(atoiDefault(r.URL.Query().Get("limit"), 100))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.render(w, "jobs", s.page(w, r, &user, "Jobs", "jobs", map[string]any{
		"Settings": settings,
		"Jobs":     jobs,
	}))
}

// pageRun is the details_url target. GitHub never fetches it — the reader's
// browser does — so it is session-gated unless the binding opted into shareable
// logs (AUDIT.md).
func (s *Server) pageRun(w http.ResponseWriter, r *http.Request) {
	job, err := s.store.Job(r.PathValue("id"))
	if err != nil {
		// Do not confirm whether an id exists to an anonymous caller.
		w.WriteHeader(http.StatusNotFound)
		s.render(w, "error", s.page(w, r, nil, "Not found", "", map[string]string{
			"Heading": "No such job",
			"Message": "That job does not exist, or its logs have been pruned.",
		}))
		return
	}
	user, _, authed := s.authenticate(r)
	if !authed && !job.ShareableLogs {
		if wantsJSON(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	body, err := logs.Read(s.cfg.LogDir(), job.ID)
	if err != nil {
		s.log.Error("read log", "job", job.ID, "error", err)
	}
	var steps []executor.Result
	if job.StepsJSON != "" {
		json.Unmarshal([]byte(job.StepsJSON), &steps)
	}

	var pageUser *store.User
	if authed {
		pageUser = &user
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"job": job, "steps": steps, "log": body})
		return
	}
	p := s.page(w, r, pageUser, job.Repo, "jobs", map[string]any{
		"Job":       job,
		"Steps":     steps,
		"Log":       body,
		"Shared":    !authed,
		"CommitURL": githubCommitURL(job, s.appAPIURL(job.GitHubAppID)),
	})
	if job.InFlight() {
		p.RefreshSeconds = 3
	}
	s.render(w, "run", p)
}

// appAPIURL is the GitHub API base for the App that owns a job. Missing Apps
// (deleted after the job ran) return "" which githubCommitURL treats as github.com.
func (s *Server) appAPIURL(appID int64) string {
	if appID == 0 {
		return ""
	}
	app, err := s.store.GitHubApp(appID)
	if err != nil {
		return ""
	}
	return app.APIURL
}

// githubCommitURL links a SHA on github.com only. GitHub Enterprise hosts are
// left as plain text because the HTML URL is not the API URL.
func githubCommitURL(job store.Job, apiURL string) string {
	if job.Repo == "" || job.SHA == "" {
		return ""
	}
	apiURL = strings.TrimRight(strings.TrimSpace(apiURL), "/")
	if apiURL != "" && apiURL != "https://api.github.com" {
		return ""
	}
	return "https://github.com/" + job.Repo + "/commit/" + job.SHA
}

func orUnknown(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
