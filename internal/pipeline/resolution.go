// SPDX-License-Identifier: Apache-2.0

package pipeline

import "fmt"

// Decisions a resolution can report. They are the three things that actually
// happen to a commit, not a status vocabulary of their own.
const (
	DecisionRun  = "run"
	DecisionSkip = "skip"
	DecisionFail = "fail"
)

// ResolvedStep is one planned command with the layer that supplied it.
type ResolvedStep struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Source  string `json:"source"`
}

// Resolution is the full answer to "what would this repository do, on this
// ref?" — the dry run's result, shared by the JSON endpoint and the page.
//
// Warnings are legal-but-probably-unintended. Errors are things that would fail
// or skip the real run. Both are collected rather than returned one at a time:
// mid-run the first error kills the job, but an operator checking a
// configuration wants all of them at once.
//
// It lives in this package rather than in the API so the endpoint and the
// template share one type. Its vocabulary is this package's — origins,
// decisions, sources — and a mirror struct on the other side of the renderer
// would be one more thing that can drift.
type Resolution struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`

	Decision    string `json:"decision"`
	SkipReason  string `json:"skip_reason"`
	Explanation string `json:"explanation"`

	PipelineFile string `json:"pipeline_file"`
	CheckName    string `json:"check_name"`
	Executor     string `json:"executor"`
	Timeout      string `json:"timeout"`

	Steps      []ResolvedStep `json:"steps"`
	Origins    []Origin       `json:"origins"`
	PathFilter string         `json:"path_filter"`

	Warnings []string `json:"warnings"`
	Errors   []string `json:"errors"`
}

// Warn records something legal but probably not intended.
func (r *Resolution) Warn(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Err records something that would fail or skip a real run.
func (r *Resolution) Err(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}
