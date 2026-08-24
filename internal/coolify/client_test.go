package coolify

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCoolify stands in for a Coolify instance. Requests must carry the team
// token as a Bearer credential or they are rejected, as the real API does.
func fakeCoolify(t *testing.T, token string, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Unauthenticated."}`))
			return
		}
		body, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			body, ok = routes[r.URL.Path]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not found."}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCurrentTeamAndServers(t *testing.T) {
	srv := fakeCoolify(t, "3|secret", map[string]string{
		"/api/v1/teams/current": `{"id":7,"name":"Platform","description":"prod"}`,
		"/api/v1/servers":       `[{"uuid":"srv-1","name":"hetzner-1","ip":"10.0.0.1"},{"uuid":"srv-2","name":"hetzner-2"}]`,
	})
	c := New(srv.URL, "3|secret")

	team, err := c.CurrentTeam(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if team.Name != "Platform" || team.ID.String() != "7" {
		t.Fatalf("team: %+v", team)
	}
	servers, err := c.Servers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Name != "hetzner-1" {
		t.Fatalf("servers: %+v", servers)
	}
}

func TestBadTokenIsReportedClearly(t *testing.T) {
	srv := fakeCoolify(t, "3|secret", map[string]string{"/api/v1/servers": `[]`})
	c := New(srv.URL, "3|wrong")
	_, err := c.Servers(context.Background())
	if err == nil {
		t.Fatal("expected an error for a bad token")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("expected a 401 APIError, got %v", err)
	}
	// The message has to tell the user what to fix.
	if !strings.Contains(err.Error(), "token") {
		t.Fatalf("unhelpful message: %v", err)
	}
}

func TestTrailingSlashInBaseURL(t *testing.T) {
	srv := fakeCoolify(t, "t", map[string]string{"/api/v1/teams/current": `{"id":1,"name":"x"}`})
	c := New(srv.URL+"/", "t")
	if _, err := c.CurrentTeam(context.Background()); err != nil {
		t.Fatalf("a trailing slash must not produce a double slash: %v", err)
	}
}

func TestGitHubApps(t *testing.T) {
	srv := fakeCoolify(t, "t", map[string]string{
		"/api/v1/github-apps": `[{"uuid":"gh-1","name":"winpra","app_id":123,"installation_id":456}]`,
	})
	c := New(srv.URL, "t")
	apps, err := c.GitHubApps(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].UUID != "gh-1" || apps[0].AppID.String() != "123" {
		t.Fatalf("connectors: %+v", apps)
	}
}

func TestRepositoriesProbesKnownPaths(t *testing.T) {
	// Only the second candidate path exists on this instance.
	srv := fakeCoolify(t, "t", map[string]string{
		"/api/v1/github-apps/gh-1/repos": `{"repositories":[{"full_name":"winpra/api","private":true}]}`,
	})
	c := New(srv.URL, "t")
	repos, err := c.Repositories(context.Background(), "gh-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "winpra/api" || !repos[0].Private {
		t.Fatalf("repos: %+v", repos)
	}
}

func TestRepositoriesUnsupported(t *testing.T) {
	srv := fakeCoolify(t, "t", map[string]string{})
	c := New(srv.URL, "t")
	// An instance with none of the candidate endpoints must say so, so the
	// caller can fall back to the CI App's own installation list.
	if _, err := c.Repositories(context.Background(), "gh-1"); !errors.Is(err, ErrReposUnsupported) {
		t.Fatalf("got %v want ErrReposUnsupported", err)
	}
}

func TestRepositoriesAcceptsPlainArray(t *testing.T) {
	srv := fakeCoolify(t, "t", map[string]string{
		"/api/v1/github-apps/gh-1/repositories": `[{"full_name":"a/b"}]`,
	})
	c := New(srv.URL, "t")
	repos, err := c.Repositories(context.Background(), "gh-1")
	if err != nil || len(repos) != 1 {
		t.Fatalf("%v %+v", err, repos)
	}
}

func TestNonJSONResponseIsExplained(t *testing.T) {
	// Pointing the base URL at something that is not Coolify is a common
	// mistake; the error should say so rather than "unexpected character".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>hello</html>"))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	_, err := c.CurrentTeam(context.Background())
	if err == nil || !strings.Contains(err.Error(), "base URL") {
		t.Fatalf("expected a base-URL hint, got %v", err)
	}
}

func TestCreateComposeApplication(t *testing.T) {
	srv := fakeCoolify(t, "t", map[string]string{
		"POST /api/v1/applications/dockercompose": `{"uuid":"app-9"}`,
	})
	c := New(srv.URL, "t")
	uuid, err := c.CreateComposeApplication(context.Background(), ComposeApplicationInput{
		ProjectUUID: "proj", ServerUUID: "srv", Name: "ci",
	})
	if err != nil {
		t.Fatal(err)
	}
	if uuid != "app-9" {
		t.Fatalf("uuid %q", uuid)
	}
}

func TestCreateComposeApplicationRequiresUUIDs(t *testing.T) {
	c := New("http://example.invalid", "t")
	if _, err := c.CreateComposeApplication(context.Background(), ComposeApplicationInput{}); err == nil {
		t.Fatal("expected an error without project and server uuids")
	}
}
