// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client talks to one GitHub App's API. Safe for concurrent use.
type Client struct {
	appID   int64
	key     *rsa.PrivateKey
	apiURL  string
	http    *http.Client
	nowFunc func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken // installation id → token
}

type cachedToken struct {
	token   string
	expires time.Time
}

// New builds a client from an App id and its PEM.
func New(appID int64, pemData, apiURL string) (*Client, error) {
	key, err := ParsePrivateKey(pemData)
	if err != nil {
		return nil, err
	}
	if apiURL == "" {
		apiURL = "https://api.github.com"
	}
	return &Client{
		appID:   appID,
		key:     key,
		apiURL:  strings.TrimRight(apiURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
		nowFunc: time.Now,
		tokens:  map[int64]cachedToken{},
	}, nil
}

// Installation is one place the App is installed.
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
	TargetType          string `json:"target_type"`
}

// Repository is a repo an installation can see.
type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Name     string `json:"name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 400 {
		body = body[:400] + "…"
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return fmt.Sprintf("GitHub rejected the App credentials (401) on %s — check the App ID matches the private key: %s", e.Path, body)
	case http.StatusForbidden:
		return fmt.Sprintf("GitHub refused the request (403) on %s — the App may lack a permission: %s", e.Path, body)
	case http.StatusNotFound:
		return fmt.Sprintf("GitHub returned 404 on %s — the App may not be installed on that repository: %s", e.Path, body)
	default:
		return fmt.Sprintf("GitHub %s returned %d: %s", e.Path, e.Status, body)
	}
}

// Status exposes the HTTP status for callers that branch on it.
func (e *APIError) StatusCode() int { return e.Status }

// do performs a request with the given Authorization value.
func (c *Client) do(ctx context.Context, method, path, authz string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("githubapp: encode body: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	target := path
	if !strings.HasPrefix(target, "http") {
		target = c.apiURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("githubapp: build request: %w", err)
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "openpreflight")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("githubapp: %s %s: %w", method, path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("githubapp: read %s: %w", path, err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{Status: res.StatusCode, Path: path, Body: string(raw)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("githubapp: decode %s: %w", path, err)
	}
	return nil
}

func (c *Client) appAuth() (string, error) {
	tok, err := AppJWT(c.appID, c.key, c.nowFunc())
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

// Installations lists where the App is installed. This is the App-level Test.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	auth, err := c.appAuth()
	if err != nil {
		return nil, err
	}
	var out []Installation
	page := c.apiURL + "/app/installations?per_page=100"
	for page != "" && len(out) < 1000 {
		var batch []Installation
		if err := c.do(ctx, http.MethodGet, page, auth, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			break
		}
		page = nextPage(page, len(out))
	}
	return out, nil
}

// nextPage advances a per_page=100 cursor. GitHub sends Link headers, but the
// offset form is enough here and keeps the response reader simple.
func nextPage(current string, seen int) string {
	u, err := url.Parse(current)
	if err != nil {
		return ""
	}
	q := u.Query()
	q.Set("page", strconv.Itoa(seen/100+1))
	u.RawQuery = q.Encode()
	return u.String()
}

// InstallationToken mints (and caches) an installation access token. The
// installation id comes from the webhook payload, never from stored config, so a
// revoked install cannot be used against a stale id.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[installationID]; ok && c.nowFunc().Add(5*time.Minute).Before(t.expires) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()

	auth, err := c.appAuth()
	if err != nil {
		return "", err
	}
	var res struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := c.do(ctx, http.MethodPost, path, auth, map[string]any{}, &res); err != nil {
		return "", err
	}
	if res.Token == "" {
		return "", fmt.Errorf("githubapp: %s returned no token", path)
	}
	expires := c.nowFunc().Add(time.Hour)
	if res.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, res.ExpiresAt); err == nil {
			expires = t
		}
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: res.Token, expires: expires}
	c.mu.Unlock()
	return res.Token, nil
}

// InstallationRepositories lists the repos one installation can see.
func (c *Client) InstallationRepositories(ctx context.Context, installationID int64) ([]Repository, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	auth := "Bearer " + token
	var out []Repository
	page := c.apiURL + "/installation/repositories?per_page=100"
	for page != "" && len(out) < 2000 {
		var batch struct {
			TotalCount   int          `json:"total_count"`
			Repositories []Repository `json:"repositories"`
		}
		if err := c.do(ctx, http.MethodGet, page, auth, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch.Repositories...)
		if len(batch.Repositories) < 100 || len(out) >= batch.TotalCount {
			break
		}
		page = nextPage(page, len(out))
	}
	return out, nil
}
