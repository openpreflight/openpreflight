// Package coolify talks to one Coolify instance with one team-scoped API token.
//
// Every token is scoped to a single team, so a Client sees that team's servers
// and sources — not the whole host. See AUDIT.md.
package coolify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client is one (base URL, team token) pair.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client. baseURL is the Coolify root, e.g. https://coolify.example.com.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// Team is the team a token belongs to. Used to label the instance row so two
// tokens on the same host are distinguishable.
type Team struct {
	ID          json.Number `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
}

// Server is one machine in the token's team.
type Server struct {
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IP          string `json:"ip"`
	User        string `json:"user"`
	Port        int    `json:"port"`
}

// GitHubApp is a Coolify GitHub source (deploy connector). Coolify never returns
// the PEM or the webhook secret, so this is repo inventory only: it cannot be
// our Checks App.
type GitHubApp struct {
	UUID           string      `json:"uuid"`
	Name           string      `json:"name"`
	HTMLURL        string      `json:"html_url"`
	AppID          json.Number `json:"app_id"`
	InstallationID json.Number `json:"installation_id"`
	Organization   string      `json:"organization"`
}

// Repository is a repo Coolify's connector can see.
type Repository struct {
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
}

// APIError carries the HTTP status so callers can tell "bad token" (401) from
// "this Coolify version lacks the endpoint" (404).
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("Coolify rejected the token (%d) — check it is an API token for the right team and has at least read-only permission", e.Status)
	case http.StatusNotFound:
		return fmt.Sprintf("Coolify has no %s (%d) — this instance may be older than the endpoint", e.Path, e.Status)
	default:
		body := e.Body
		if len(body) > 300 {
			body = body[:300] + "…"
		}
		return fmt.Sprintf("Coolify %s returned %d: %s", e.Path, e.Status, body)
	}
}

// NotFound reports whether the endpoint is missing on this instance.
func (e *APIError) NotFound() bool { return e.Status == http.StatusNotFound }

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("coolify: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("coolify: %s: %w", path, err)
	}
	defer res.Body.Close()
	// Cap the read: a misconfigured base URL can point at anything.
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("coolify: read %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{Status: res.StatusCode, Path: path, Body: string(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("coolify: %s did not return the expected JSON (is the base URL the Coolify root?): %w", path, err)
	}
	return nil
}

// CurrentTeam is GET /api/v1/teams/current — the team this token can see.
func (c *Client) CurrentTeam(ctx context.Context) (Team, error) {
	var t Team
	if err := c.get(ctx, "/api/v1/teams/current", &t); err != nil {
		return Team{}, err
	}
	return t, nil
}

// Servers is GET /api/v1/servers — that team's servers, not the whole instance.
func (c *Client) Servers(ctx context.Context) ([]Server, error) {
	var out []Server
	if err := c.get(ctx, "/api/v1/servers", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GitHubApps is GET /api/v1/github-apps — the team's deploy connectors.
func (c *Client) GitHubApps(ctx context.Context) ([]GitHubApp, error) {
	var out []GitHubApp
	if err := c.get(ctx, "/api/v1/github-apps", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ErrReposUnsupported means this Coolify version exposes no repo listing for a
// connector over the API. The caller should fall back to the CI App's own
// installation repos.
var ErrReposUnsupported = errors.New("coolify: this instance does not expose connector repositories over the API")

// repoPaths are the shapes Coolify has used for connector repo listing. The
// endpoint is not pinned in the docs we verified (AUDIT.md), so we probe rather
// than hard-code and let the caller fall back.
func repoPaths(uuid string) []string {
	return []string{
		"/api/v1/github-apps/" + uuid + "/repositories",
		"/api/v1/github-apps/" + uuid + "/repos",
		"/api/v1/github-apps/" + uuid + "/load-repositories",
	}
}

// Repositories lists the repos a connector's installation can see. Returns
// ErrReposUnsupported when no known endpoint exists on this instance.
func (c *Client) Repositories(ctx context.Context, connectorUUID string) ([]Repository, error) {
	if connectorUUID == "" {
		return nil, errors.New("coolify: connector uuid is required")
	}
	var lastErr error
	for _, p := range repoPaths(connectorUUID) {
		// Coolify wraps some list responses in {"repositories": [...]}, so
		// decode into a shape that accepts either.
		var raw json.RawMessage
		err := c.get(ctx, p, &raw)
		if err == nil {
			return decodeRepositories(raw)
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.NotFound() {
			lastErr = err
			continue
		}
		return nil, err
	}
	if lastErr != nil {
		return nil, ErrReposUnsupported
	}
	return nil, ErrReposUnsupported
}

func decodeRepositories(raw json.RawMessage) ([]Repository, error) {
	var direct []Repository
	if err := json.Unmarshal(raw, &direct); err == nil {
		return direct, nil
	}
	var wrapped struct {
		Repositories []Repository `json:"repositories"`
		Data         []Repository `json:"data"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("coolify: unexpected repositories payload: %w", err)
	}
	if len(wrapped.Repositories) > 0 {
		return wrapped.Repositories, nil
	}
	return wrapped.Data, nil
}
