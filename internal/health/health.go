// SPDX-License-Identifier: Apache-2.0

// Package health holds the shape of an instance's self-report: what is wrong,
// in the words an operator would use, and what to do about it.
//
// The types live here rather than in the API package so the JSON endpoint and
// the /status page share one definition. A mirror struct on the other side of
// the renderer is one more thing that can drift, and this is the page people
// will read when they already believe something is broken.
package health

// Component states, worst last. A report's overall status is its worst
// component.
//
// `Error` means checks cannot be reported at all. `Warn` means something is
// configured in a way that will bite, or has not been verified. Keeping that
// line strict matters: a page that cries error over a Docker engine an install
// does not use is a page operators learn to ignore.
const (
	StateOK    = "ok"
	StateWarn  = "warn"
	StateError = "error"
)

// Component is one thing that can be wrong.
type Component struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	// Action is what to do about it, and is empty when the state is ok. A
	// diagnosis with no next step is only half an answer.
	Action string `json:"action,omitempty"`
}

// Report is the whole picture: GET /health?verbose=1, and the /status page.
type Report struct {
	Status     string      `json:"status"`
	Version    string      `json:"version"`
	Components []Component `json:"components"`
}

// Worse returns the more serious of two states.
func Worse(a, b string) string {
	rank := map[string]int{StateOK: 0, StateWarn: 1, StateError: 2}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// OK reports whether nothing needs attention.
func (r Report) OK() bool { return r.Status == StateOK }
