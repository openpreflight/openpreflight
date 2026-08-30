// SPDX-License-Identifier: Apache-2.0

// Package web is the configurator HTML layer: page data, helpers, and the
// compiled Tailwind stylesheet. Markup lives in layouts/ and pages/ as templ.
package web

import (
	_ "embed"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/store"
)

//go:generate npm run css

//go:embed assets/css/output.css
var outputCSS string

// Crumb is one breadcrumb. Empty Href means the current page.
type Crumb struct {
	Label string
	Href  string
}

// Page is the data every view receives.
type Page struct {
	Title             string
	Nav               string
	User              *store.User
	Flash             string
	FlashKind         string // ok | err
	CSRFToken         string
	Data              any
	Narrow            bool // login / setup: narrower main column
	RefreshSeconds    int  // meta-refresh while a job is in flight
	InFlightCount     int
	DockerAvailable   bool
	DockerHost        string
	Crumbs            []Crumb
	SidebarCollapsed  bool
}

// Renderer is kept so api.New can fail closed if CSS failed to embed.
type Renderer struct{}

// New checks that the stylesheet was compiled into the binary.
func New() (*Renderer, error) {
	if len(outputCSS) < 500 {
		return nil, fmt.Errorf("web: embedded CSS is empty; from internal/web run: npm install && npm run css")
	}
	return &Renderer{}, nil
}

// CSSHandler serves the compiled stylesheet.
func CSSHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		io.WriteString(w, outputCSS)
	})
}

// ShortSHA is the first 8 characters of a commit, or the whole string if shorter.
func ShortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// WebhookURL builds the value the user pastes into the GitHub App.
func WebhookURL(base, slug string) string {
	base = strings.TrimRight(base, "/")
	if base == "" {
		return "(set the public base URL) /webhook/" + slug
	}
	return base + "/webhook/" + slug
}

// Ago renders a coarse relative time; the exact instant is rarely what a
// configurator page needs.
func Ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// StepMark matches queue/summary.go so the run page and the GitHub check
// output use the same marks.
func StepMark(r executor.Result) string {
	switch {
	case r.Skipped:
		return "–"
	case r.OK():
		return "✓"
	default:
		return "✗"
	}
}

// HumanDuration formats a step or job wall time.
func HumanDuration(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	if d < time.Second {
		// A step can finish in milliseconds; rounding would print "0s".
		return "<1s"
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// JobStatusLabel is the operator-facing word for a job row.
func JobStatusLabel(j store.Job) string {
	switch j.Status {
	case store.JobSuccess:
		return "passed"
	case store.JobFailure:
		return "failed"
	case store.JobError:
		return "error"
	case store.JobInProgress:
		return "running"
	case store.JobQueued:
		return "queued"
	case store.JobCancelled:
		return "cancelled"
	case store.JobSkipped:
		return "skipped"
	default:
		return j.Status
	}
}
