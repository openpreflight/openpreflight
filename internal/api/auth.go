package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/store"
)

// pageSetup shows the first-run wizard, or sends you on if setup already ran.
func (s *Server) pageSetup(w http.ResponseWriter, r *http.Request) {
	hasUsers, err := s.store.HasUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if hasUsers {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, "setup", s.page(w, r, nil, "Setup", "", map[string]any{
		// Seed the field from the env hint so a Coolify deployment can prefill it.
		"PublicBaseURL": s.cfg.PublicBaseURL,
	}))
}

// handleSetup creates the admin user. It is only reachable while no user exists,
// which is what keeps this endpoint from being an open door.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	hasUsers, err := s.store.HasUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if hasUsers {
		s.reply(w, r, http.StatusConflict, map[string]string{"error": "setup has already run"},
			"/login", "Setup has already run. Sign in instead.", "err")
		return
	}
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	username := in.Str("username")
	if username == "" {
		username = "admin"
	}
	user, err := s.store.CreateUser(username, in.Str("password"))
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if base := strings.TrimRight(in.Str("public_base_url"), "/"); base != "" {
		settings.PublicBaseURL = base
		if err := s.store.SaveSettings(settings); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.setSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, http.StatusCreated, map[string]any{"user": user, "settings": settings},
		"/", "Admin created. Add a Coolify instance or go straight to GitHub Apps.", "ok")
}

// pageLogin renders the sign-in form.
func (s *Server) pageLogin(w http.ResponseWriter, r *http.Request) {
	hasUsers, err := s.store.HasUsers()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !hasUsers {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if _, _, ok := s.authenticate(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login", s.page(w, r, nil, "Sign in", "", nil))
}

// handleLogin exchanges credentials for a session. The JSON surface gets the
// token back so a CLI can use it as a Bearer credential.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	user, err := s.store.Authenticate(in.Str("username"), in.Str("password"))
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.fail(w, r, err)
			return
		}
		s.reply(w, r, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"},
			"/login", "Invalid username or password.", "err")
		return
	}
	if wantsJSON(r) {
		token, expires, err := s.store.CreateSession(user.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      token,
			"expires_at": expires,
			"user":       user,
			"usage":      "send as: Authorization: Bearer <token>",
		})
		return
	}
	if err := s.setSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout drops every session credential the caller presented: the cookie
// and/or the Bearer token. JSON login issues a Bearer token and no cookie, so
// looking at the cookie alone left that token valid for its full 14 days.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		s.store.DeleteSession(c.Value)
	}
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		if token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer ")); token != "" {
			s.store.DeleteSession(token)
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode})
	s.reply(w, r, http.StatusOK, map[string]string{"status": "signed out"}, "/login", "", "")
}

// changePassword updates the admin password.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, user store.User) {
	in, err := readInput(r)
	if err != nil {
		s.badRequest(w, r, err)
		return
	}
	if err := s.store.SetPassword(user.ID, in.Str("password")); err != nil {
		s.badRequest(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, map[string]string{"status": "password updated"},
		"/settings", "Password updated.", "ok")
}
