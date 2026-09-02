// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// maxOutputBytes caps what we send in a Check Run output. Current docs do not
// state a limit (AUDIT.md); we stay well under the historical 65535 and keep the
// authoritative copy in the local log file.
const maxOutputBytes = 60 << 10

// CheckOutput is the summary panel GitHub renders on the Checks tab.
type CheckOutput struct {
	Title   string `json:"title,omitempty"`
	Summary string `json:"summary,omitempty"`
	Text    string `json:"text,omitempty"`
}

// CheckRun is the subset of the Check Run we create and read back.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

// CreateCheckRunInput starts an in-progress Check Run for a SHA.
type CreateCheckRunInput struct {
	Repo       string // owner/name
	Name       string
	HeadSHA    string
	DetailsURL string
	Status     string // queued | in_progress
	Output     *CheckOutput
}

// CreateCheckRun posts a new Check Run. Only a GitHub App can do this — user and
// OAuth tokens are refused by GitHub (AUDIT.md).
func (c *Client) CreateCheckRun(ctx context.Context, installationID int64, in CreateCheckRunInput) (CheckRun, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return CheckRun{}, err
	}
	if in.Status == "" {
		in.Status = "in_progress"
	}
	body := map[string]any{
		"name":     in.Name,
		"head_sha": in.HeadSHA,
		"status":   in.Status,
	}
	if in.DetailsURL != "" {
		body["details_url"] = in.DetailsURL
	}
	if in.Status == "in_progress" {
		body["started_at"] = c.nowFunc().UTC().Format(time.RFC3339)
	}
	if in.Output != nil {
		body["output"] = truncateOutput(*in.Output)
	}
	var out CheckRun
	path := fmt.Sprintf("/repos/%s/check-runs", in.Repo)
	if err := c.do(ctx, http.MethodPost, path, "Bearer "+token, body, &out); err != nil {
		return CheckRun{}, err
	}
	return out, nil
}

// CompleteCheckRunInput finishes a Check Run.
type CompleteCheckRunInput struct {
	Repo       string
	CheckRunID int64
	Conclusion string // success | failure | neutral | cancelled | timed_out | skipped
	DetailsURL string
	Output     *CheckOutput
}

// CompleteCheckRun patches a Check Run to completed.
func (c *Client) CompleteCheckRun(ctx context.Context, installationID int64, in CompleteCheckRunInput) error {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	body := map[string]any{
		"status":       "completed",
		"conclusion":   in.Conclusion,
		"completed_at": c.nowFunc().UTC().Format(time.RFC3339),
	}
	if in.DetailsURL != "" {
		body["details_url"] = in.DetailsURL
	}
	if in.Output != nil {
		body["output"] = truncateOutput(*in.Output)
	}
	path := fmt.Sprintf("/repos/%s/check-runs/%d", in.Repo, in.CheckRunID)
	return c.do(ctx, http.MethodPatch, path, "Bearer "+token, body, nil)
}

// ReopenCheckRunInput moves an existing Check Run back to in_progress.
type ReopenCheckRunInput struct {
	Repo       string
	CheckRunID int64
	DetailsURL string
	Output     *CheckOutput
}

// ReopenCheckRun patches a Check Run back to in_progress. A job requeued after a
// crash or a redeploy must land on the Check Run it already created, or the
// original is left in_progress forever and a required check never resolves.
//
// GitHub keeps `conclusion` and `completed_at` from the previous completion, so
// both are sent as null: a run that says completed and in_progress at once
// renders as finished on the Checks tab.
func (c *Client) ReopenCheckRun(ctx context.Context, installationID int64, in ReopenCheckRunInput) error {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return err
	}
	body := map[string]any{
		"status":       "in_progress",
		"started_at":   c.nowFunc().UTC().Format(time.RFC3339),
		"conclusion":   nil,
		"completed_at": nil,
	}
	if in.DetailsURL != "" {
		body["details_url"] = in.DetailsURL
	}
	if in.Output != nil {
		body["output"] = truncateOutput(*in.Output)
	}
	path := fmt.Sprintf("/repos/%s/check-runs/%d", in.Repo, in.CheckRunID)
	return c.do(ctx, http.MethodPatch, path, "Bearer "+token, body, nil)
}

// truncateOutput keeps the payload under maxOutputBytes and says so in place of
// what it dropped.
func truncateOutput(o CheckOutput) map[string]any {
	out := map[string]any{}
	if o.Title != "" {
		out["title"] = truncateUTF8(o.Title, 255)
	}
	if o.Summary != "" {
		out["summary"] = truncateUTF8(o.Summary, maxOutputBytes)
	}
	if o.Text != "" {
		out["text"] = truncateUTF8(o.Text, maxOutputBytes)
	}
	return out
}

// truncateUTF8 trims to at most limit bytes without splitting a rune, keeping
// the tail (the interesting end of a log) when it has to cut.
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const notice = "… output truncated; full log is on the details page …\n"
	if limit <= len(notice) {
		return s[:limit]
	}
	keep := limit - len(notice)
	tail := s[len(s)-keep:]
	// Drop a partial leading rune produced by the byte-offset cut.
	for len(tail) > 0 && !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	if i := strings.IndexByte(tail, '\n'); i >= 0 && i < 200 {
		tail = tail[i+1:]
	}
	return notice + tail
}
