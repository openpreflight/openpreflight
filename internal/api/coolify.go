package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openpreflight/openpreflight/internal/coolify"
	"github.com/openpreflight/openpreflight/internal/store"
)

// coolifyClient builds a client for a stored instance, decrypting its token.
func (s *Server) coolifyClient(id int64) (*coolify.Client, store.CoolifyInstance, error) {
	inst, err := s.store.CoolifyInstance(id)
	if err != nil {
		return nil, store.CoolifyInstance{}, err
	}
	token, err := s.store.DecryptCoolifyToken(inst)
	if err != nil {
		return nil, inst, fmt.Errorf("could not decrypt the stored token for %q: %w", inst.Name, err)
	}
	return coolify.New(inst.BaseURL, token), inst, nil
}

func (s *Server) listCoolify(w http.ResponseWriter, r *http.Request, _ store.User) {
	items, err := s.store.ListCoolifyInstances()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": items})
}

func (s *Server) createCoolify(w http.ResponseWriter, r *http.Request, _ store.User) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	inst, err := s.store.CreateCoolifyInstance(store.CoolifyInput{
		Name:               in.Str("name"),
		BaseURL:            in.Str("base_url"),
		APIToken:           in.Str("api_token"),
		DefaultServerUUID:  in.Str("default_server_uuid"),
		DefaultProjectUUID: in.Str("default_project_uuid"),
	})
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	// Testing on create tells the user immediately whether the token works,
	// which is the whole point of the row.
	msg, kind := s.runCoolifyTest(r.Context(), inst.ID)
	s.reply(w, r, http.StatusCreated, map[string]any{"instance": inst, "test": msg},
		"/coolify", "Added "+inst.Name+". "+msg, kind)
}

func (s *Server) updateCoolify(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	existing, err := s.store.CoolifyInstance(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	next := store.CoolifyInput{
		Name:               existing.Name,
		BaseURL:            existing.BaseURL,
		DefaultServerUUID:  existing.DefaultServerUUID,
		DefaultProjectUUID: existing.DefaultProjectUUID,
	}
	if in.Has("name") {
		next.Name = in.Str("name")
	}
	if in.Has("base_url") {
		next.BaseURL = in.Str("base_url")
	}
	if in.Has("default_server_uuid") {
		next.DefaultServerUUID = in.Str("default_server_uuid")
	}
	if in.Has("default_project_uuid") {
		next.DefaultProjectUUID = in.Str("default_project_uuid")
	}
	// An empty token means "keep the stored one": the UI only ever showed a
	// redacted value.
	next.APIToken = in.Str("api_token")
	inst, err := s.store.UpdateCoolifyInstance(id, next)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, map[string]any{"instance": inst}, "/coolify", "Saved.", "ok")
}

// runCoolifyTest hits both endpoints the plan calls for and records the outcome.
func (s *Server) runCoolifyTest(ctx context.Context, id int64) (string, string) {
	client, inst, err := s.coolifyClient(id)
	if err != nil {
		s.store.RecordCoolifyTest(id, "", "", err.Error())
		return err.Error(), "err"
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	team, err := client.CurrentTeam(ctx)
	if err != nil {
		s.store.RecordCoolifyTest(id, "", "", err.Error())
		return err.Error(), "err"
	}
	servers, err := client.Servers(ctx)
	if err != nil {
		// The team call already proved the token; a servers failure is worth
		// recording but the team label is real.
		s.store.RecordCoolifyTest(id, team.ID.String(), team.Name, err.Error())
		return fmt.Sprintf("token works for team %q but listing servers failed: %v", team.Name, err), "err"
	}
	if err := s.store.RecordCoolifyTest(id, team.ID.String(), team.Name, ""); err != nil {
		return err.Error(), "err"
	}
	return fmt.Sprintf("Connected to %s as team %q (%d server(s) visible to this token).",
		inst.BaseURL, team.Name, len(servers)), "ok"
}

func (s *Server) testCoolify(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	msg, kind := s.runCoolifyTest(r.Context(), id)
	status := http.StatusOK
	if kind == "err" {
		status = http.StatusBadGateway
	}
	s.reply(w, r, status, map[string]any{"ok": kind == "ok", "message": msg}, "/coolify", msg, kind)
}

func (s *Server) coolifyServers(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	client, _, err := s.coolifyClient(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	servers, err := client.Servers(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (s *Server) coolifyConnectors(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	client, _, err := s.coolifyClient(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	apps, err := client.GitHubApps(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"github_apps": apps,
		"note": "These are Coolify deploy sources. They cannot write commit checks and their webhook " +
			"belongs to Coolify; use them here only as a repository list.",
	})
}

// coolifyRepos lists the repos this instance's connectors can see, which feeds
// the bindings picker.
func (s *Server) coolifyRepos(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	repos, err := s.loadCoolifyRepos(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

// loadCoolifyRepos gathers repos across every connector on an instance.
func (s *Server) loadCoolifyRepos(ctx context.Context, id int64) ([]coolify.Repository, error) {
	client, _, err := s.coolifyClient(id)
	if err != nil {
		return nil, err
	}
	connectors, err := client.GitHubApps(ctx)
	if err != nil {
		return nil, err
	}
	if len(connectors) == 0 {
		return nil, errors.New("this Coolify team has no GitHub connector, so there are no repositories to list")
	}
	seen := map[string]bool{}
	var out []coolify.Repository
	var unsupported int
	for _, c := range connectors {
		repos, err := client.Repositories(ctx, c.UUID)
		if errors.Is(err, coolify.ErrReposUnsupported) {
			unsupported++
			continue
		}
		if err != nil {
			return out, err
		}
		for _, repo := range repos {
			if repo.FullName == "" || seen[repo.FullName] {
				continue
			}
			seen[repo.FullName] = true
			out = append(out, repo)
		}
	}
	if len(out) == 0 && unsupported > 0 {
		return nil, fmt.Errorf("%w — pick the CI App's own installation list instead", coolify.ErrReposUnsupported)
	}
	return out, nil
}

func (s *Server) deleteCoolify(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.store.DeleteCoolifyInstance(id); err != nil {
		s.notFound(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, map[string]string{"status": "deleted"},
		"/coolify", "Instance removed.", "ok")
}

func (s *Server) installWorker(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	client, inst, err := s.coolifyClient(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	serverUUID := in.Str("server_uuid")
	if serverUUID == "" {
		serverUUID = inst.DefaultServerUUID
	}
	projectUUID := in.Str("project_uuid")
	if projectUUID == "" {
		projectUUID = inst.DefaultProjectUUID
	}
	if serverUUID == "" || projectUUID == "" {
		s.badRequest(w, r, errors.New("server_uuid and project_uuid are required"))
		return
	}
	name := in.Str("name")
	envName := in.Str("environment_name")
	uuid, err := client.CreateComposeApplication(r.Context(), coolify.ComposeApplicationInput{
		Name:            name,
		ProjectUUID:     projectUUID,
		ServerUUID:      serverUUID,
		EnvironmentName: envName,
		InstantDeploy:   false,
	})
	if err != nil {
		s.reply(w, r, http.StatusBadGateway, map[string]string{"error": err.Error()},
			"/coolify?inspect="+fmt.Sprintf("%d", id), err.Error(), "err")
		return
	}
	msg := "Created Coolify application " + uuid + ". Set CI_SECRET_KEY on it before deploying."
	s.reply(w, r, http.StatusCreated, map[string]string{"uuid": uuid},
		"/coolify?inspect="+fmt.Sprintf("%d", id), msg, "ok")
}

// notFound answers a missing row on either surface.
func (s *Server) notFound(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.reply(w, r, http.StatusNotFound, map[string]string{"error": "not found"}, "", "Not found.", "err")
		return
	}
	s.fail(w, r, err)
}
