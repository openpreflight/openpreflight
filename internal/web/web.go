// SPDX-License-Identifier: Apache-2.0

// Package web renders the server-side HTML configurator. Styles are Tailwind,
// compiled into static/app.css and inlined at render time. There is no separate
// frontend and no JavaScript beyond tiny attributes (onchange, onsubmit, onfocus).
package web

import (
	"embed"
	"fmt"
	"html/template"
	"io"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/store"
)

//go:generate npm run css

//go:embed templates/*.html
var files embed.FS

//go:embed static/app.css
var appCSS string

// pages are the content templates, each compiled together with the layout.
var pages = []string{
	"setup", "login", "dashboard", "settings", "coolify",
	"githubapps", "repos", "jobs", "run", "error",
}

// Renderer holds the compiled templates.
type Renderer struct {
	tmpl map[string]*template.Template
}

// Page is the data every template receives.
type Page struct {
	Title          string
	Nav            string
	User           *store.User
	Flash          string
	FlashKind      string // ok | err
	CSRFField      template.HTML
	CSS            template.CSS // compiled Tailwind; set by Render
	Data           any
	Narrow         bool // login / setup: narrower main column
	RefreshSeconds int  // meta-refresh while a job is in flight
}

// New compiles the templates. It fails at startup rather than on first request.
func New() (*Renderer, error) {
	r := &Renderer{tmpl: map[string]*template.Template{}}
	for _, name := range pages {
		t, err := template.New("layout.html").Funcs(funcs()).ParseFS(files,
			"templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", name, err)
		}
		r.tmpl[name] = t
	}
	return r, nil
}

// Render writes one page.
func (r *Renderer) Render(w io.Writer, page string, data Page) error {
	t, ok := r.tmpl[page]
	if !ok {
		return fmt.Errorf("web: no template %q", page)
	}
	data.CSS = template.CSS(appCSS)
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		return fmt.Errorf("web: render %s: %w", page, err)
	}
	return nil
}

func funcs() template.FuncMap {
	return template.FuncMap{
		"shortSHA": func(sha string) string {
			if len(sha) > 8 {
				return sha[:8]
			}
			return sha
		},
		"ago":      ago,
		"duration": humanDuration,
		"stepMark": stepMark,
		"statusPill": func(j store.Job) template.HTML {
			class, label := "off", j.Status
			switch j.Status {
			case store.JobSuccess:
				class, label = "ok", "passed"
			case store.JobFailure:
				class, label = "bad", "failed"
			case store.JobError:
				class, label = "bad", "error"
			case store.JobInProgress:
				class, label = "warn", "running"
			case store.JobQueued:
				class, label = "off", "queued"
			case store.JobCancelled:
				class, label = "off", "cancelled"
			case store.JobSkipped:
				class, label = "off", "skipped"
			}
			return template.HTML(fmt.Sprintf(`<span class="pill %s">%s</span>`,
				class, template.HTMLEscapeString(label)))
		},
		// webhookURL builds the value the user pastes into the GitHub App.
		"webhookURL": func(base, slug string) string {
			base = strings.TrimRight(base, "/")
			if base == "" {
				return "(set the public base URL) /webhook/" + slug
			}
			return base + "/webhook/" + slug
		},
	}
}

// ago renders a coarse relative time; the exact instant is rarely what a
// configurator page needs.
func ago(t time.Time) string {
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

// stepMark matches queue/summary.go so the run page and the GitHub check
// output use the same ✓ / ✗ / – marks.
func stepMark(r executor.Result) string {
	switch {
	case r.Skipped:
		return "–"
	case r.OK():
		return "✓"
	default:
		return "✗"
	}
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
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
