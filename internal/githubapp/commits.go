// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GitHub's Get a commit endpoint truncates files after this many entries.
const maxCommitFiles = 300

// CommitFile is one path from GET /repos/{}/commits/{sha}.
type CommitFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
}

// CommitFiles is the changed-path list for one SHA. Truncated means the list
// is incomplete (explicit flag or the 300-file cap) and callers should fail-open.
type CommitFiles struct {
	Files     []CommitFile
	Truncated bool
}

// ChangedPaths is filename plus previous_filename (renames), leading slashes
// stripped, de-duplicated.
func (c CommitFiles) ChangedPaths() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range c.Files {
		for _, name := range []string{f.Filename, f.PreviousFilename} {
			name = strings.TrimPrefix(strings.TrimSpace(name), "/")
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// CommitFiles lists paths changed in a commit (installation token, contents:read).
func (c *Client) CommitFiles(ctx context.Context, installationID int64, repo, sha string) (CommitFiles, error) {
	repo = strings.TrimSpace(repo)
	sha = strings.TrimSpace(sha)
	if repo == "" || sha == "" {
		return CommitFiles{}, errors.New("githubapp: repo and sha are required")
	}
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return CommitFiles{}, err
	}
	var raw struct {
		Files     []CommitFile `json:"files"`
		Truncated bool         `json:"truncated"`
	}
	path := fmt.Sprintf("/repos/%s/commits/%s", repo, sha)
	if err := c.do(ctx, http.MethodGet, path, "Bearer "+token, nil, &raw); err != nil {
		return CommitFiles{}, err
	}
	return CommitFiles{
		Files:     raw.Files,
		Truncated: raw.Truncated || len(raw.Files) >= maxCommitFiles,
	}, nil
}
