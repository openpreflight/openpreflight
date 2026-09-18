// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// A login is idle-expiring rather than fixed-length. A flat 14-day TTL meant a
// cookie copied off a laptop stayed good for a fortnight whether or not anyone
// used it, and nothing but an explicit logout ever shortened that. This console
// can run arbitrary repo code on the host, so a credential nobody is using
// should stop working.
//
// SessionIdleTTL is how long a session survives without a request;
// SessionMaxTTL is the ceiling no amount of activity raises, so even a session
// in daily use is re-authenticated weekly.
const (
	SessionIdleTTL = 24 * time.Hour
	SessionMaxTTL  = 7 * 24 * time.Hour
)

// HasUsers reports whether the setup wizard still needs to run.
func (s *Store) HasUsers() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		return false, fmt.Errorf("store: count users: %w", err)
	}
	return n > 0, nil
}

// CreateUser stores an admin with a bcrypt password hash.
func (s *Store) CreateUser(username, password string) (User, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return User{}, errors.New("store: username required")
	}
	if len(password) < 12 {
		return User{}, errors.New("store: password must be at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, fmt.Errorf("store: hash password: %w", err)
	}
	ts := formatTime(now())
	res, err := s.db.Exec(`INSERT INTO users (username, password_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?)`, username, string(hash), ts, ts)
	if err != nil {
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return User{ID: id, Username: username, CreatedAt: parseTime(ts)}, nil
}

// SetPassword replaces a user's password.
func (s *Store) SetPassword(userID int64, password string) error {
	if len(password) < 12 {
		return errors.New("store: password must be at least 12 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("store: hash password: %w", err)
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
		string(hash), formatTime(now()), userID)
	if err != nil {
		return fmt.Errorf("store: set password: %w", err)
	}
	// A password change is how you lock someone out, so it has to invalidate
	// the credentials they already hold. Leaving them live meant a stolen
	// cookie or bearer token outlived the password it was obtained with. The
	// caller re-issues a session for whoever is standing at the keyboard.
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("store: revoke sessions: %w", err)
	}
	return nil
}

// Authenticate checks a username/password pair.
func (s *Store) Authenticate(username, password string) (User, error) {
	var (
		u  User
		ca string
	)
	err := s.db.QueryRow(`SELECT id, username, password_hash, created_at FROM users WHERE username = ?`,
		strings.TrimSpace(username)).Scan(&u.ID, &u.Username, &u.passwordHash, &ca)
	if errors.Is(err, sql.ErrNoRows) {
		// Spend the same time as a real comparison so the response does not
		// distinguish "no such user" from "wrong password".
		bcrypt.CompareHashAndPassword([]byte("$2a$10$"+strings.Repeat("x", 53)), []byte(password))
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: authenticate: %w", err)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.passwordHash), []byte(password)); err != nil {
		return User{}, ErrNotFound
	}
	u.CreatedAt = parseTime(ca)
	u.passwordHash = ""
	return u, nil
}

// FirstUser returns the single admin, used by bootstrap and session lookup.
func (s *Store) FirstUser() (User, error) {
	var (
		u  User
		ca string
	)
	err := s.db.QueryRow(`SELECT id, username, created_at FROM users ORDER BY id LIMIT 1`).
		Scan(&u.ID, &u.Username, &ca)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: first user: %w", err)
	}
	u.CreatedAt = parseTime(ca)
	return u, nil
}

// CreateSession issues an opaque session token.
func (s *Store) CreateSession(userID int64) (string, time.Time, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("store: session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	created := now()
	expires := created.Add(SessionIdleTTL)
	if _, err := s.db.Exec(`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, formatTime(created), formatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("store: create session: %w", err)
	}
	return token, expires, nil
}

// UserBySession resolves a session cookie or bearer token, dropping it if it
// has gone idle or hit the absolute ceiling, and sliding the idle window
// forward otherwise.
func (s *Store) UserBySession(token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	var (
		u              User
		expires        string
		sessionCreated string
		ca             string
	)
	err := s.db.QueryRow(`SELECT u.id, u.username, u.created_at, s.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token = ?`, token).
		Scan(&u.ID, &u.Username, &ca, &sessionCreated, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: session lookup: %w", err)
	}
	var (
		t        = now()
		idleEnds = parseTime(expires)
		hardEnds = parseTime(sessionCreated).Add(SessionMaxTTL)
	)
	if idleEnds.Before(t) || hardEnds.Before(t) {
		s.DeleteSession(token)
		return User{}, ErrNotFound
	}
	want := t.Add(SessionIdleTTL)
	if want.After(hardEnds) {
		want = hardEnds
	}
	// Write only when the stored deadline is off by more than half a window.
	// This runs on every authenticated request and a write per request buys
	// nothing: half a day of imprecision on when an idle session dies is not
	// worth the churn. Correcting in both directions also pulls sessions issued
	// under the old flat 14-day TTL back onto the idle window without a
	// migration.
	slack := SessionIdleTTL / 2
	if idleEnds.Before(want.Add(-slack)) || idleEnds.After(want.Add(slack)) {
		if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ? WHERE token = ?`,
			formatTime(want), token); err != nil {
			return User{}, fmt.Errorf("store: session refresh: %w", err)
		}
	}
	u.CreatedAt = parseTime(ca)
	return u, nil
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

// PruneSessions removes expired sessions.
func (s *Store) PruneSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, formatTime(now()))
	return err
}
