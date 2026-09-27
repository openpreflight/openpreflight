// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openpreflight/openpreflight/internal/store"
)

// loginAttemptsPerMinute is the sign-in budget per client address. Someone who
// mistypes needs two or three; a guesser needs millions.
const loginAttemptsPerMinute = 5

// loginLimiter counts sign-in attempts per client address in a fixed one-minute
// window. An attempt is counted before the bcrypt compare, so a burst of
// concurrent guesses cannot all get in ahead of the first failure, and an
// attempt over budget costs no bcrypt at all.
//
// ponytail: keyed on RemoteAddr, so behind a reverse proxy every client shares
// one budget. For a single admin that global limit is the point; key on a
// trusted X-Forwarded-For if a shared budget ever locks a real operator out.
type loginLimiter struct {
	mu    sync.Mutex
	start time.Time
	count map[string]int
}

// allow records an attempt from addr and reports whether it is within budget,
// and how long until the window resets.
func (l *loginLimiter) allow(addr string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.count == nil || now.Sub(l.start) >= time.Minute {
		l.start, l.count = now, map[string]int{}
	}
	l.count[addr]++
	return l.count[addr] <= loginAttemptsPerMinute, l.start.Add(time.Minute).Sub(now)
}

// clear forgets addr's attempts once it has signed in.
func (l *loginLimiter) clear(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.count, addr)
}

// clientAddr is the peer's IP, without the port.
func clientAddr(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

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
	p := s.page(w, r, nil, "Setup", "", map[string]any{
		// Seed the field from the env hint so a Coolify deployment can prefill it.
		"PublicBaseURL": s.cfg.PublicBaseURL,
	})
	p.Narrow = true
	s.render(w, "setup", p)
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
	// An empty setupToken means this process booted with an admin in place; it
	// must refuse rather than match an empty field.
	if s.setupToken == "" || subtle.ConstantTimeCompare([]byte(in.Str("setup_token")), []byte(s.setupToken)) != 1 {
		s.reply(w, r, http.StatusForbidden,
			map[string]string{"error": "setup_token is missing or wrong; the server prints it to its log at startup"},
			"/setup", "That setup token is wrong. Copy it from the server log, where it is printed at startup.", "err")
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
	p := s.page(w, r, nil, "Sign in", "", nil)
	p.Narrow = true
	s.render(w, "login", p)
}

// handleLogin exchanges credentials for a session. The JSON surface gets the
// token back so a CLI can use it as a Bearer credential.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	addr := clientAddr(r)
	if ok, wait := s.logins.allow(addr); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		s.reply(w, r, http.StatusTooManyRequests, map[string]string{"error": "too many sign-in attempts; retry later"},
			"/login", "Too many sign-in attempts. Wait a minute and try again.", "err")
		return
	}
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
	s.logins.clear(addr)
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
// looking at the cookie alone left that token valid for the rest of its life.
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
	// SetPassword revoked every session, including the one this request came
	// in on. Hand the browser a fresh cookie so changing your own password does
	// not bounce you to the login form; a Bearer caller ignores the Set-Cookie
	// and signs in again for a new token, which is the point.
	if err := s.setSession(w, r, user.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.reply(w, r, http.StatusOK, map[string]string{"status": "password updated; other sessions signed out"},
		"/settings/admin", "Password updated. Every other signed-in session was signed out.", "ok")
}
