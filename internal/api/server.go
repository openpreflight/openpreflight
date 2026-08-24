// Package api serves both surfaces: the HTML configurator and the JSON API,
// plus the public webhook and log endpoints.
package api

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trivedi-vatsal/openpreflight/internal/config"
	"github.com/trivedi-vatsal/openpreflight/internal/executor"
	"github.com/trivedi-vatsal/openpreflight/internal/queue"
	"github.com/trivedi-vatsal/openpreflight/internal/store"
	"github.com/trivedi-vatsal/openpreflight/internal/web"
)

const (
	sessionCookie = "ci_session"
	csrfCookie    = "ci_csrf"
	flashCookie   = "ci_flash"
)

// Server wires the store, the runner and the renderer into one http.Handler.
type Server struct {
	store    *store.Store
	cfg      config.Config
	runner   *queue.Runner
	renderer *web.Renderer
	log      *slog.Logger
	dockerOK func() bool
}

// New builds the server.
func New(st *store.Store, cfg config.Config, runner *queue.Runner, log *slog.Logger) (*Server, error) {
	renderer, err := web.New()
	if err != nil {
		return nil, err
	}
	return &Server{store: st, cfg: cfg, runner: runner, renderer: renderer, log: log}, nil
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public: no session, no CSRF.
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("POST /webhook/{slug}", s.handleWebhook)
	mux.HandleFunc("GET /runs/{id}", s.pageRun)

	// Unauthenticated entry points.
	mux.HandleFunc("GET /setup", s.pageSetup)
	mux.HandleFunc("POST /api/v1/setup", s.handleSetup)
	mux.HandleFunc("GET /login", s.pageLogin)
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/logout", s.handleLogout)

	// HTML pages.
	mux.HandleFunc("GET /{$}", s.guard(s.pageDashboard))
	mux.HandleFunc("GET /settings", s.guard(s.pageSettings))
	mux.HandleFunc("GET /coolify", s.guard(s.pageCoolify))
	mux.HandleFunc("GET /github-apps", s.guard(s.pageGitHubApps))
	mux.HandleFunc("GET /repos", s.guard(s.pageRepos))
	mux.HandleFunc("GET /jobs", s.guard(s.pageJobs))

	// JSON API / form targets.
	mux.HandleFunc("GET /api/v1/settings", s.guard(s.getSettings))
	mux.HandleFunc("PATCH /api/v1/settings", s.guard(s.patchSettings))
	mux.HandleFunc("POST /api/v1/settings", s.guard(s.patchSettings))
	mux.HandleFunc("POST /api/v1/password", s.guard(s.changePassword))

	mux.HandleFunc("GET /api/v1/coolify", s.guard(s.listCoolify))
	mux.HandleFunc("POST /api/v1/coolify", s.guard(s.createCoolify))
	mux.HandleFunc("PATCH /api/v1/coolify/{id}", s.guard(s.updateCoolify))
	mux.HandleFunc("POST /api/v1/coolify/{id}", s.guard(s.updateCoolify))
	mux.HandleFunc("POST /api/v1/coolify/{id}/test", s.guard(s.testCoolify))
	mux.HandleFunc("POST /api/v1/coolify/{id}/install-worker", s.guard(s.installWorker))
	mux.HandleFunc("GET /api/v1/coolify/{id}/servers", s.guard(s.coolifyServers))
	mux.HandleFunc("GET /api/v1/coolify/{id}/github-apps", s.guard(s.coolifyConnectors))
	mux.HandleFunc("GET /api/v1/coolify/{id}/repos", s.guard(s.coolifyRepos))
	mux.HandleFunc("DELETE /api/v1/coolify/{id}", s.guard(s.deleteCoolify))
	mux.HandleFunc("POST /api/v1/coolify/{id}/delete", s.guard(s.deleteCoolify))

	mux.HandleFunc("GET /api/v1/github-apps", s.guard(s.listApps))
	mux.HandleFunc("POST /api/v1/github-apps", s.guard(s.createApp))
	mux.HandleFunc("PATCH /api/v1/github-apps/{id}", s.guard(s.updateApp))
	mux.HandleFunc("POST /api/v1/github-apps/{id}", s.guard(s.updateApp))
	mux.HandleFunc("POST /api/v1/github-apps/{id}/test", s.guard(s.testApp))
	mux.HandleFunc("GET /api/v1/github-apps/{id}/repos", s.guard(s.appRepos))
	mux.HandleFunc("DELETE /api/v1/github-apps/{id}", s.guard(s.deleteApp))
	mux.HandleFunc("POST /api/v1/github-apps/{id}/delete", s.guard(s.deleteApp))

	mux.HandleFunc("GET /api/v1/bindings", s.guard(s.listBindings))
	mux.HandleFunc("PUT /api/v1/bindings", s.guard(s.upsertBinding))
	mux.HandleFunc("POST /api/v1/bindings", s.guard(s.upsertBinding))
	mux.HandleFunc("POST /api/v1/bindings/bulk", s.guard(s.bulkBindings))
	mux.HandleFunc("POST /api/v1/bindings/{id}/toggle", s.guard(s.toggleBinding))
	mux.HandleFunc("DELETE /api/v1/bindings/{id}", s.guard(s.deleteBinding))
	mux.HandleFunc("POST /api/v1/bindings/{id}/delete", s.guard(s.deleteBinding))

	mux.HandleFunc("GET /api/v1/jobs", s.guard(s.listJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.guard(s.getJob))
	mux.HandleFunc("GET /api/v1/jobs/{id}/logs", s.getJobLogs)
	mux.HandleFunc("POST /api/v1/jobs/{id}/rerun", s.guard(s.rerunJob))
	mux.HandleFunc("POST /api/v1/jobs/{id}/cancel", s.guard(s.cancelJob))

	return s.withRecovery(s.withLogging(mux))
}

func (s *Server) dockerAvailable() bool {
	if s.dockerOK != nil {
		return s.dockerOK()
	}
	return executor.Docker{Host: s.cfg.DockerHost}.Ping()
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	// Touch the database: a process that cannot reach its own store is not
	// healthy, and Coolify's healthcheck should say so.
	if _, err := s.store.Settings(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// withLogging records one line per request. Webhook bodies and secrets never
// appear: only method, path, status and duration.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("http", "method", r.Method, "path", r.URL.Path,
			"status", rec.status, "ms", time.Since(start).Milliseconds())
	})
}

// withRecovery keeps one bad request from taking down the worker.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "value", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// guard requires authentication, and CSRF for cookie-authenticated writes.
func (s *Server) guard(next func(http.ResponseWriter, *http.Request, store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hasUsers, err := s.store.HasUsers()
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if !hasUsers {
			// Nothing is configured yet: send a browser to the wizard rather
			// than a login form nobody can pass.
			if wantsJSON(r) {
				writeJSON(w, http.StatusPreconditionRequired,
					map[string]string{"error": "setup has not run; POST /api/v1/setup first"})
				return
			}
			http.Redirect(w, r, "/setup", http.StatusSeeOther)
			return
		}
		user, viaCookie, ok := s.authenticate(r)
		if !ok {
			if wantsJSON(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		// Bearer callers carry no ambient cookie, so they cannot be tricked by a
		// cross-site form; CSRF applies to cookie sessions only.
		if viaCookie && isUnsafe(r.Method) && !s.csrfOK(r) {
			s.reply(w, r, http.StatusForbidden, map[string]string{"error": "csrf token mismatch"},
				"", "Your session expired or the form was stale. Try again.", "err")
			return
		}
		next(w, r, user)
	}
}

func isUnsafe(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// authenticate resolves a session cookie or a Bearer session token.
func (s *Server) authenticate(r *http.Request) (store.User, bool, bool) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if u, err := s.store.UserBySession(c.Value); err == nil {
			return u, true, true
		}
	}
	// The CLI uses the same opaque session token as a Bearer credential; there
	// are no separate API tokens in v1.
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		if u, err := s.store.UserBySession(token); err == nil {
			return u, false, true
		}
	}
	return store.User{}, false, false
}

func (s *Server) csrfOK(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	sent := r.Header.Get("X-CSRF-Token")
	if sent == "" {
		// ParseForm is safe to call twice; handlers read the same parsed values.
		r.ParseForm()
		sent = r.PostFormValue("csrf")
	}
	return sent != "" && subtle.ConstantTimeCompare([]byte(sent), []byte(c.Value)) == 1
}

// csrfToken reads the CSRF cookie, minting one if the browser has none.
func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && c.Value != "" {
		return c.Value
	}
	raw := make([]byte, 24)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // read back only as a form field; not a credential on its own
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   14 * 24 * 3600,
	})
	return token
}

// isHTTPS reports whether the browser connection is secure, honouring the
// reverse proxy header Coolify's Traefik sets.
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	token, expires, err := s.store.CreateSession(userID)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	return nil
}

// ---- request input -------------------------------------------------------

// input reads either a JSON body or an HTML form, so every handler serves both
// the UI and the API without branching.
type input struct {
	form url.Values
	json map[string]any
}

func readInput(r *http.Request) (*input, error) {
	in := &input{}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		in.json = map[string]any{}
		dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
		if err := dec.Decode(&in.json); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		return in, nil
	}
	if err := r.ParseForm(); err != nil {
		return nil, fmt.Errorf("invalid form body: %w", err)
	}
	in.form = r.PostForm
	return in, nil
}

// Str returns a string value, or "" when absent.
func (in *input) Str(key string) string {
	if in.json != nil {
		if v, ok := in.json[key]; ok {
			switch t := v.(type) {
			case string:
				return strings.TrimSpace(t)
			case float64:
				return strconv.FormatFloat(t, 'f', -1, 64)
			case bool:
				return strconv.FormatBool(t)
			}
		}
		return ""
	}
	return strings.TrimSpace(in.form.Get(key))
}

// Has reports whether the key was supplied at all, which is how PATCH tells
// "set to empty" from "leave alone".
func (in *input) Has(key string) bool {
	if in.json != nil {
		_, ok := in.json[key]
		return ok
	}
	return in.form.Has(key)
}

// StrList returns every value for a repeated key (checkbox groups).
func (in *input) StrList(key string) []string {
	if in.json != nil {
		raw, ok := in.json[key]
		if !ok {
			return nil
		}
		if arr, ok := raw.([]any); ok {
			out := make([]string, 0, len(arr))
			for _, v := range arr {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		}
		if s, ok := raw.(string); ok && s != "" {
			return []string{s}
		}
		return nil
	}
	return in.form[key]
}

// Int returns an integer value, or def when absent or unparseable.
func (in *input) Int(key string, def int) int {
	v := in.Str(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// Int64 is Int for identifiers.
func (in *input) Int64(key string, def int64) int64 {
	v := in.Str(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

// Bool reads a checkbox or JSON boolean. An HTML checkbox that is unchecked
// sends nothing, so absence is false.
func (in *input) Bool(key string) bool {
	if in.json != nil {
		if v, ok := in.json[key]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return t == "1" || strings.EqualFold(t, "true") || strings.EqualFold(t, "on")
			case float64:
				return t != 0
			}
		}
		return false
	}
	v := in.form.Get(key)
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "on")
}

// pathID reads a numeric path parameter.
func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id in path")
	}
	return id, nil
}

// ---- responses -----------------------------------------------------------

// wantsJSON decides which surface is asking. Browsers post forms and accept
// HTML; the CLI sends JSON or asks for it.
func wantsJSON(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return true
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "application/json") {
		return true
	}
	if strings.Contains(accept, "text/html") {
		return false
	}
	// No signal at all: an API client's default. Form posts always carry a
	// Content-Type, so this does not catch the UI.
	return r.Header.Get("Content-Type") == ""
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if payload != nil {
		json.NewEncoder(w).Encode(payload)
	}
}

// reply answers both surfaces: JSON for the API, redirect-with-flash for a form.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, code int, payload any, redirect, flash, kind string) {
	if wantsJSON(r) {
		writeJSON(w, code, payload)
		return
	}
	if flash != "" {
		s.setFlash(w, r, flash, kind)
	}
	if redirect == "" {
		redirect = r.Header.Get("Referer")
	}
	if redirect == "" || strings.Contains(redirect, "://") && !sameOrigin(r, redirect) {
		redirect = "/"
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// sameOrigin keeps a Referer-derived redirect on this host.
func sameOrigin(r *http.Request, target string) bool {
	u, err := url.Parse(target)
	if err != nil {
		return false
	}
	return u.Host == "" || u.Host == r.Host
}

func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, message, kind string) {
	if kind == "" {
		kind = "ok"
	}
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(kind + "|" + message)),
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30,
	})
}

// takeFlash reads and clears the flash cookie.
func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) (string, string) {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return "", ""
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return "", ""
	}
	kind, msg, ok := strings.Cut(string(raw), "|")
	if !ok {
		return "", ""
	}
	return msg, kind
}

// page assembles the common template data.
func (s *Server) page(w http.ResponseWriter, r *http.Request, user *store.User, title, nav string, data any) web.Page {
	flash, kind := s.takeFlash(w, r)
	token := s.csrfToken(w, r)
	return web.Page{
		Title:     title,
		Nav:       nav,
		User:      user,
		Flash:     flash,
		FlashKind: kind,
		CSRFField: template.HTML(fmt.Sprintf(`<input type="hidden" name="csrf" value="%s">`,
			template.HTMLEscapeString(token))),
		Data: data,
	}
}

func (s *Server) render(w http.ResponseWriter, page string, data web.Page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.renderer.Render(w, page, data); err != nil {
		s.log.Error("render", "page", page, "error", err)
	}
}

// fail reports an unexpected server-side error on either surface.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("request failed", "path", r.URL.Path, "error", err)
	if wantsJSON(r) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var pageUser *store.User
	if user, _, ok := s.authenticate(r); ok {
		pageUser = &user
	}
	w.WriteHeader(http.StatusInternalServerError)
	s.render(w, "error", s.page(w, r, pageUser, "Error", "", map[string]string{
		"Heading": "Something went wrong",
		"Message": err.Error(),
	}))
}

// badRequest reports a caller mistake.
func (s *Server) badRequest(w http.ResponseWriter, r *http.Request, err error) {
	s.reply(w, r, http.StatusBadRequest, map[string]string{"error": err.Error()}, "", err.Error(), "err")
}
