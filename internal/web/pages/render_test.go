// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"

	"github.com/openpreflight/openpreflight/internal/pipeline"
	"github.com/openpreflight/openpreflight/internal/store"
	"github.com/openpreflight/openpreflight/internal/web"
)

// renderPage runs a page against realistic data. templ generate catches a
// broken parse; nothing catches a page that renders but has lost a section, a
// confirm, or an empty state, which is what these assert.
func renderPage(t *testing.T, name string, f func(web.Page) templ.Component, p web.Page) string {
	t.Helper()
	var sb strings.Builder
	if err := f(p).Render(context.Background(), &sb); err != nil {
		t.Fatalf("%s: render: %v", name, err)
	}
	out := sb.String()
	if len(out) < 500 {
		t.Fatalf("%s: rendered %d bytes, expected a page", name, len(out))
	}
	return out
}

func page(data map[string]any) web.Page {
	return web.Page{
		Title:     "test",
		User:      &store.User{Username: "op"},
		CSRFToken: "t0ken",
		Data:      data,
	}
}

func TestRepoResolveRendersEverySection(t *testing.T) {
	out := renderPage(t, "RepoResolve", RepoResolve, page(map[string]any{
		"Binding": store.RepoBinding{ID: 1, Repo: "acme/api"},
		"Ref":     "main",
		"Resolution": pipeline.Resolution{
			SHA: "0123456789abcdef", Ref: "main", CheckName: "ci", Executor: "docker: node:24", Timeout: "600s",
			Steps:      []pipeline.ResolvedStep{{Name: "test", Command: "npm test", Source: ".ci.yml"}},
			Origins:    []pipeline.Origin{{Field: "timeout", Value: "600", Source: "settings"}},
			PathFilter: "frontend/**",
		},
	}))
	// Section order proves the nesting: a card that closes early swallows the
	// ones after it, and the missing marker is the failure.
	for _, want := range []string{"Where every value came from", "Path filter", "frontend/**"} {
		if !strings.Contains(out, want) {
			t.Errorf("RepoResolve: missing %q - a card probably closed too early", want)
		}
	}
}

func TestRepoResolveEmptyResolutionStillExplainsItself(t *testing.T) {
	out := renderPage(t, "RepoResolve", RepoResolve, page(map[string]any{
		"Binding":    store.RepoBinding{ID: 1, Repo: "acme/api"},
		"Resolution": pipeline.Resolution{},
	}))
	for _, want := range []string{"No steps", "Nothing was resolved"} {
		if !strings.Contains(out, want) {
			t.Errorf("RepoResolve with nothing resolved: missing the %q empty state", want)
		}
	}
}

func TestRunPageGuardsCancelAndNamesTheStreamState(t *testing.T) {
	running := store.Job{ID: "j1", Repo: "acme/api", SHA: "0123456789abcdef", Status: store.JobInProgress}
	out := renderPage(t, "Run", Run, page(map[string]any{
		"Job":   running,
		"Steps": []any{},
		"Log":   "",
	}))
	if !strings.Contains(out, "/api/v1/jobs/j1/cancel") {
		t.Fatal("Run: in-flight job offers no cancel at all")
	}
	// The POST must sit behind the dialog's affirmative button, not behind the
	// trigger: "Cancel the run" and "Keep running" only exist inside it.
	for _, want := range []string{"Cancel the run", "Keep running"} {
		if !strings.Contains(out, want) {
			t.Errorf("Run: cancel posts without a confirm dialog (no %q)", want)
		}
	}
	for _, want := range []string{"job-log-state", "job-log-error", "job-log-waiting"} {
		if !strings.Contains(out, want) {
			t.Errorf("Run: in-flight log is missing %q, so a dropped stream would be silent", want)
		}
	}
}

func TestSelectDefaults(t *testing.T) {
	apps := []store.GitHubApp{{ID: 7, Name: "first"}, {ID: 9, Name: "second"}}

	// A native <select> submitted its first option when nothing was chosen.
	// The registry Select submits only what is set, so a new binding must name
	// the first App explicitly or it posts an empty github_app_id.
	if got := editAppValue(false, 0, apps); got != "7" {
		t.Errorf("editAppValue(new) = %q, want the first App %q", got, "7")
	}
	if got := editAppValue(true, 9, apps); got != "9" {
		t.Errorf("editAppValue(editing) = %q, want the stored App %q", got, "9")
	}
	if got := editAppValue(false, 0, nil); got != "" {
		t.Errorf("editAppValue(no apps) = %q, want empty", got)
	}
	if got := firstAppName(nil); got != "- select -" {
		t.Errorf("firstAppName(nil) = %q, want the placeholder", got)
	}

	// "none" is a real choice on the Coolify select, and its value is "0" -
	// unlike an unset App, which is the empty string.
	if got := editInstValue(false, 0); got != "0" {
		t.Errorf("editInstValue(new) = %q, want %q", got, "0")
	}
	if got := editInstValue(true, 4); got != "4" {
		t.Errorf("editInstValue(editing) = %q, want %q", got, "4")
	}
	if got := idValue(0); got != "" {
		t.Errorf("idValue(0) = %q, want empty so the trigger shows its placeholder", got)
	}
	if got := idValue(12); got != "12" {
		t.Errorf("idValue(12) = %q, want %q", got, "12")
	}

	// Anything that is not an explicit "fail" is a skip, matching the binding
	// default the resolver applies.
	for in, want := range map[string]string{"": "skip", "skip": "skip", "fail": "fail", "nonsense": "skip"} {
		if got := onEmptyValue(in); got != want {
			t.Errorf("onEmptyValue(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJobPageNumber(t *testing.T) {
	for _, tc := range []struct {
		offset, limit, want int
	}{{0, 100, 1}, {100, 100, 2}, {250, 100, 3}, {0, 0, 1}} {
		if got := jobPageNumber(tc.offset, tc.limit); got != tc.want {
			t.Errorf("jobPageNumber(%d, %d) = %d, want %d", tc.offset, tc.limit, got, tc.want)
		}
	}
}
