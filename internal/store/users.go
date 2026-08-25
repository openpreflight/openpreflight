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

// sessionTTL is how long a configurator login lasts.
const sessionTTL = 14 * 24 * time.Hour

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
	expires := created.Add(sessionTTL)
	if _, err := s.db.Exec(`INSERT INTO sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, formatTime(created), formatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("store: create session: %w", err)
	}
	return token, expires, nil
}

// UserBySession resolves a session cookie, dropping it if expired.
func (s *Store) UserBySession(token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	var (
		u       User
		expires string
		ca      string
	)
	err := s.db.QueryRow(`SELECT u.id, u.username, u.created_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token = ?`, token).
		Scan(&u.ID, &u.Username, &ca, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: session lookup: %w", err)
	}
	if parseTime(expires).Before(now()) {
		s.DeleteSession(token)
		return User{}, ErrNotFound
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
