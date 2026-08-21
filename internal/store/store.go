// Package store is the SQLite persistence layer. The driver is
// modernc.org/sqlite (pure Go) so the image can stay CGO_ENABLED=0.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/secret"
)

// ErrNotFound is returned by the Get* helpers when a row is absent.
var ErrNotFound = errors.New("store: not found")

// Store owns the database handle and the secret box used for secret columns.
type Store struct {
	db  *sql.DB
	box *secret.Box
}

// Open opens (creating if needed) the SQLite file and applies migrations.
func Open(path string, box *secret.Box) (*Store, error) {
	// WAL keeps the webhook handler's writes from blocking on a running job's,
	// and busy_timeout absorbs the rest.
	dsn := "file:" + url.PathEscape(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	// modernc/sqlite handles concurrency, but a single writer avoids
	// SQLITE_BUSY churn entirely for a workload this small.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{db: db, box: box}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests.
func (s *Store) DB() *sql.DB { return s.db }

// timeFmt is how every timestamp column is stored. Times are written as TEXT
// rather than left to the driver so the format is identical everywhere and
// string comparison in SQL (retention pruning) sorts chronologically.
const timeFmt = time.RFC3339Nano

func now() time.Time { return time.Now().UTC() }

func formatTime(t time.Time) string { return t.UTC().Format(timeFmt) }

// parseTime reads a stored timestamp. A malformed or empty value yields the zero
// time rather than an error: a bad timestamp must not hide the row.
func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(timeFmt, v)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// parseTimePtr is parseTime for nullable columns.
func parseTimePtr(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parseTime(v.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

// timeArg writes an optional timestamp, mapping nil to SQL NULL.
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// nullString maps "" to SQL NULL so unique indexes over optional columns behave.
func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// nullInt64 maps 0 to SQL NULL for optional foreign keys.
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
