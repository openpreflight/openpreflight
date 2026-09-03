// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/openpreflight/openpreflight/internal/store"
)

// errNoApp is returned when the picker posts without an App: a binding cannot
// exist without one, because the App is what verifies the webhook and writes
// the Check Run.
var errNoApp = errors.New("select a CI GitHub App before saving repositories")

func (s *Server) listBindings(w http.ResponseWriter, r *http.Request, _ store.User) {
	bindings, err := s.store.ListBindings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

// upsertBinding creates or updates one repo binding.
func (s *Server) upsertBinding(w http.ResponseWriter, r *http.Request, _ store.User) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	binding, err := s.store.UpsertBinding(store.BindingInput{
		GitHubAppID:       in.Int64("github_app_id", 0),
		CoolifyInstanceID: in.Int64("coolify_instance_id", 0),
		Repo:              in.Str("repo"),
		Enabled:           in.Bool("enabled"),
		Branches:          in.Str("branches"),
		Paths:             in.Str("paths"),
		CheckName:         in.Str("check_name"),
		PipelineFile:      in.Str("pipeline_file"),
		TimeoutSeconds:    in.Int("timeout_seconds", 0),
		InstallCmd:        in.Str("install_cmd"),
		TestCmd:           in.Str("test_cmd"),
		BuildCmd:          in.Str("build_cmd"),
		ShareableLogs:     in.Bool("shareable_logs"),
		OnEmptyPipeline:   in.Str("on_empty_pipeline"),
	})
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	msg := "Binding saved for " + binding.Repo + "."
	if !binding.Enabled {
		msg += " It is disabled, so webhooks for it are ignored."
	}
	s.reply(w, r, http.StatusOK, map[string]any{"binding": binding}, "/repos", msg, "ok")
}

// bulkBindings applies the repo picker's checkboxes: checked repos are enabled
// bindings, and a previously bound repo that is now unchecked is unbound.
func (s *Server) bulkBindings(w http.ResponseWriter, r *http.Request, _ store.User) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	appID := in.Int64("github_app_id", 0)
	if appID == 0 {
		s.badRequest(w, r, errNoApp)
		return
	}
	coolifyID := in.Int64("coolify_instance_id", 0)
	wanted := map[string]bool{}
	for _, repo := range in.StrList("repo") {
		if repo = strings.TrimSpace(repo); repo != "" {
			wanted[repo] = true
		}
	}

	existing, err := s.store.ListBindings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Only touch bindings that belong to the App whose list was displayed.
	var added, removed int
	have := map[string]store.RepoBinding{}
	for _, b := range existing {
		if b.GitHubAppID == appID {
			have[b.Repo] = b
		}
	}
	for repo := range wanted {
		if b, ok := have[repo]; ok {
			// Keep the existing overrides; only make sure it is enabled and
			// pointed at the selected Coolify row.
			b.Enabled = true
			if coolifyID != 0 {
				b.CoolifyInstanceID = coolifyID
			}
			if _, err := s.store.UpsertBinding(bindingInputFrom(b)); err != nil {
				s.badRequest(w, r, err)
				return
			}
			continue
		}
		if _, err := s.store.UpsertBinding(store.BindingInput{
			GitHubAppID:       appID,
			CoolifyInstanceID: coolifyID,
			Repo:              repo,
			Enabled:           true,
		}); err != nil {
			s.badRequest(w, r, err)
			return
		}
		added++
	}
	for repo, b := range have {
		if wanted[repo] {
			continue
		}
		if err := s.store.DeleteBinding(b.ID); err != nil {
			s.fail(w, r, err)
			return
		}
		removed++
	}
	msg := "Selection saved: nothing changed."
	if added > 0 || removed > 0 {
		msg = fmt.Sprintf("Selection saved: %d repo(s) enabled, %d unbound.", added, removed)
	}
	s.reply(w, r, http.StatusOK, map[string]any{"added": added, "removed": removed},
		"/repos", msg, "ok")
}

func bindingInputFrom(b store.RepoBinding) store.BindingInput {
	return store.BindingInput{
		GitHubAppID:       b.GitHubAppID,
		CoolifyInstanceID: b.CoolifyInstanceID,
		Repo:              b.Repo,
		Enabled:           b.Enabled,
		Branches:          b.Branches,
		Paths:             b.Paths,
		CheckName:         b.CheckName,
		PipelineFile:      b.PipelineFile,
		TimeoutSeconds:    b.TimeoutSeconds,
		InstallCmd:        b.InstallCmd,
		TestCmd:           b.TestCmd,
		BuildCmd:          b.BuildCmd,
		ShareableLogs:     b.ShareableLogs,
		OnEmptyPipeline:   b.OnEmptyPipeline,
	}
}

func (s *Server) toggleBinding(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	binding, err := s.store.Binding(id)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	next := bindingInputFrom(binding)
	next.Enabled = !binding.Enabled
	updated, err := s.store.UpsertBinding(next)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	state := "disabled"
	if updated.Enabled {
		state = "enabled"
	}
	s.reply(w, r, http.StatusOK, map[string]any{"binding": updated},
		"/repos", updated.Repo+" is now "+state+".", "ok")
}

func (s *Server) deleteBinding(w http.ResponseWriter, r *http.Request, _ store.User) {
	id, err := pathID(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.store.DeleteBinding(id); err != nil {
		s.notFound(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, map[string]string{"status": "deleted"},
		"/repos", "Binding removed.", "ok")
}
