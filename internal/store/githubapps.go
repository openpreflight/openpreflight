package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/openpreflight/openpreflight/internal/secret"
)

const ghCols = `id, name, slug, app_id, pem_enc, webhook_secret_enc, api_url, check_name,
	last_seen_at, last_error, created_at, updated_at`

// slugPattern keeps the slug safe to put in the webhook path.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func (s *Store) scanGitHubApp(sc interface{ Scan(...any) error }) (GitHubApp, error) {
	var (
		a        GitHubApp
		lastSeen sql.NullString
		ca, ua   string
	)
	if err := sc.Scan(&a.ID, &a.Name, &a.Slug, &a.AppID, &a.pemEnc, &a.webhookSecretEnc,
		&a.APIURL, &a.CheckName, &lastSeen, &a.LastError, &ca, &ua); err != nil {
		return GitHubApp{}, err
	}
	a.LastSeenAt = parseTimePtr(lastSeen)
	a.CreatedAt = parseTime(ca)
	a.UpdatedAt = parseTime(ua)
	if pem, err := s.box.Open(a.pemEnc); err == nil {
		a.PEMRedacted = secret.Redact(pem)
	} else {
		a.PEMRedacted = "(undecryptable — wrong CI_SECRET_KEY?)"
	}
	if ws, err := s.box.Open(a.webhookSecretEnc); err == nil {
		a.WebhookSecretRedacted = secret.Redact(ws)
	} else {
		a.WebhookSecretRedacted = "(undecryptable — wrong CI_SECRET_KEY?)"
	}
	return a, nil
}

// ListGitHubApps returns every configured CI App.
func (s *Store) ListGitHubApps() ([]GitHubApp, error) {
	rows, err := s.db.Query(`SELECT ` + ghCols + ` FROM github_apps ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list github apps: %w", err)
	}
	defer rows.Close()
	var out []GitHubApp
	for rows.Next() {
		a, err := s.scanGitHubApp(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan github app: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GitHubApp loads one App by id.
func (s *Store) GitHubApp(id int64) (GitHubApp, error) {
	a, err := s.scanGitHubApp(s.db.QueryRow(`SELECT `+ghCols+` FROM github_apps WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	if err != nil {
		return GitHubApp{}, fmt.Errorf("store: github app %d: %w", id, err)
	}
	return a, nil
}

// GitHubAppBySlug resolves the /webhook/{slug} path segment.
func (s *Store) GitHubAppBySlug(slug string) (GitHubApp, error) {
	a, err := s.scanGitHubApp(s.db.QueryRow(`SELECT `+ghCols+` FROM github_apps WHERE slug = ?`, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubApp{}, ErrNotFound
	}
	if err != nil {
		return GitHubApp{}, fmt.Errorf("store: github app %q: %w", slug, err)
	}
	return a, nil
}

// DecryptPEM returns the App private key. Callers must not log or export it.
func (s *Store) DecryptPEM(a GitHubApp) (string, error) { return s.box.Open(a.pemEnc) }

// DecryptWebhookSecret returns the HMAC secret for this App's webhook path.
func (s *Store) DecryptWebhookSecret(a GitHubApp) (string, error) {
	return s.box.Open(a.webhookSecretEnc)
}

// GitHubAppInput is the writable shape of a GitHub App row.
type GitHubAppInput struct {
	Name          string
	Slug          string
	AppID         int64
	PEM           string
	WebhookSecret string
	APIURL        string
	CheckName     string
}

func (in *GitHubAppInput) normalise() error {
	in.Name = strings.TrimSpace(in.Name)
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	in.PEM = strings.TrimSpace(in.PEM)
	in.WebhookSecret = strings.TrimSpace(in.WebhookSecret)
	in.APIURL = strings.TrimRight(strings.TrimSpace(in.APIURL), "/")
	in.CheckName = strings.TrimSpace(in.CheckName)
	if in.Name == "" {
		return errors.New("name is required")
	}
	if in.Slug == "" {
		in.Slug = slugify(in.Name)
	}
	if !slugPattern.MatchString(in.Slug) {
		return errors.New("slug must be lowercase letters, digits and dashes")
	}
	if in.AppID <= 0 {
		return errors.New("app_id must be the numeric App ID from GitHub")
	}
	if in.APIURL == "" {
		in.APIURL = "https://api.github.com"
	}
	return nil
}

// slugify turns a display name into a URL-safe webhook path segment.
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// CreateGitHubApp stores a new CI App with its PEM and webhook secret encrypted.
func (s *Store) CreateGitHubApp(in GitHubAppInput) (GitHubApp, error) {
	if err := in.normalise(); err != nil {
		return GitHubApp{}, err
	}
	if in.PEM == "" {
		return GitHubApp{}, errors.New("private key (PEM) is required")
	}
	if in.WebhookSecret == "" {
		return GitHubApp{}, errors.New("webhook secret is required")
	}
	pemEnc, err := s.box.Seal(in.PEM)
	if err != nil {
		return GitHubApp{}, err
	}
	wsEnc, err := s.box.Seal(in.WebhookSecret)
	if err != nil {
		return GitHubApp{}, err
	}
	ts := formatTime(now())
	res, err := s.db.Exec(`INSERT INTO github_apps
		(name, slug, app_id, pem_enc, webhook_secret_enc, api_url, check_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Name, in.Slug, in.AppID, pemEnc, wsEnc, in.APIURL, in.CheckName, ts, ts)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return GitHubApp{}, fmt.Errorf("slug %q is already used by another App", in.Slug)
		}
		return GitHubApp{}, fmt.Errorf("store: create github app: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.GitHubApp(id)
}

// UpdateGitHubApp patches a row; empty PEM / secret keep the stored values.
func (s *Store) UpdateGitHubApp(id int64, in GitHubAppInput) (GitHubApp, error) {
	existing, err := s.GitHubApp(id)
	if err != nil {
		return GitHubApp{}, err
	}
	if err := in.normalise(); err != nil {
		return GitHubApp{}, err
	}
	pemEnc, wsEnc := existing.pemEnc, existing.webhookSecretEnc
	if in.PEM != "" {
		if pemEnc, err = s.box.Seal(in.PEM); err != nil {
			return GitHubApp{}, err
		}
	}
	if in.WebhookSecret != "" {
		if wsEnc, err = s.box.Seal(in.WebhookSecret); err != nil {
			return GitHubApp{}, err
		}
	}
	_, err = s.db.Exec(`UPDATE github_apps SET name = ?, slug = ?, app_id = ?, pem_enc = ?,
		webhook_secret_enc = ?, api_url = ?, check_name = ?, updated_at = ? WHERE id = ?`,
		in.Name, in.Slug, in.AppID, pemEnc, wsEnc, in.APIURL, in.CheckName, formatTime(now()), id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return GitHubApp{}, fmt.Errorf("slug %q is already used by another App", in.Slug)
		}
		return GitHubApp{}, fmt.Errorf("store: update github app: %w", err)
	}
	return s.GitHubApp(id)
}

// RecordGitHubAppTest stores the outcome of a Test click.
func (s *Store) RecordGitHubAppTest(id int64, errMsg string) error {
	var seen any
	if errMsg == "" {
		seen = formatTime(now())
	}
	_, err := s.db.Exec(`UPDATE github_apps SET last_seen_at = COALESCE(?, last_seen_at),
		last_error = ?, updated_at = ? WHERE id = ?`, seen, errMsg, formatTime(now()), id)
	if err != nil {
		return fmt.Errorf("store: record github app test: %w", err)
	}
	return nil
}

// DeleteGitHubApp removes an App and (by cascade) its bindings.
func (s *Store) DeleteGitHubApp(id int64) error {
	res, err := s.db.Exec(`DELETE FROM github_apps WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete github app: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
