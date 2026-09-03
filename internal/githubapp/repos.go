// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// RepoInfo is the part of GET /repos/{owner}/{repo} we read. DefaultBranch is
// what a dry run resolves against when the caller names no ref: deriving one
// from a binding's branch patterns would be guesswork, since those are globs.
type RepoInfo struct {
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Fork          bool   `json:"fork"`
}

// Repo reads one repository's metadata (installation token, metadata:read).
func (c *Client) Repo(ctx context.Context, installationID int64, repo string) (RepoInfo, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return RepoInfo{}, errors.New("githubapp: repo is required")
	}
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return RepoInfo{}, err
	}
	var out RepoInfo
	if err := c.do(ctx, http.MethodGet, "/repos/"+repo, "Bearer "+token, nil, &out); err != nil {
		return RepoInfo{}, err
	}
	return out, nil
}

// GitBaseURL derives the git origin from the App's API URL so GitHub Enterprise
// works: api.github.com → https://github.com, ghe.example.com/api/v3 →
// https://ghe.example.com.
func GitBaseURL(apiURL string) string {
	if apiURL == "" {
		return "https://github.com"
	}
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return "https://github.com"
	}
	if u.Host == "api.github.com" {
		return "https://github.com"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

// InstallationForRepo finds which installation of this App can see a repository.
//
// A binding records the App but not the installation — only a job does — so a
// dry run for a repository that has never run has to look it up. That is the
// case a dry run matters most in, which is why this exists rather than reading
// the newest job row and giving up when there isn't one.
func (c *Client) InstallationForRepo(ctx context.Context, repo string) (int64, error) {
	repo = strings.TrimSpace(repo)
	installs, err := c.Installations(ctx)
	if err != nil {
		return 0, err
	}
	for _, inst := range installs {
		repos, err := c.InstallationRepositories(ctx, inst.ID)
		if err != nil {
			// One broken installation should not hide the others.
			continue
		}
		for _, r := range repos {
			if strings.EqualFold(r.FullName, repo) {
				return inst.ID, nil
			}
		}
	}
	return 0, fmt.Errorf("githubapp: no installation of this App can see %s — install it on that repository", repo)
}
