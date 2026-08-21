package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/secret"
)

const coolifyCols = `id, name, base_url, api_token_enc, team_id, team_name,
	default_server_uuid, default_project_uuid, last_seen_at, last_error, created_at, updated_at`

func (s *Store) scanCoolify(sc interface{ Scan(...any) error }) (CoolifyInstance, error) {
	var (
		c        CoolifyInstance
		lastSeen sql.NullString
		ca, ua   string
	)
	if err := sc.Scan(&c.ID, &c.Name, &c.BaseURL, &c.apiTokenEnc, &c.TeamID, &c.TeamName,
		&c.DefaultServerUUID, &c.DefaultProjectUUID, &lastSeen, &c.LastError, &ca, &ua); err != nil {
		return CoolifyInstance{}, err
	}
	c.LastSeenAt = parseTimePtr(lastSeen)
	c.CreatedAt = parseTime(ca)
	c.UpdatedAt = parseTime(ua)
	// Redaction uses the plaintext length, so decrypt failures degrade to a
	// marker instead of taking the list page down.
	if tok, err := s.box.Open(c.apiTokenEnc); err == nil {
		c.APITokenRedacted = secret.Redact(tok)
	} else {
		c.APITokenRedacted = "(undecryptable — wrong CI_SECRET_KEY?)"
	}
	return c, nil
}

// ListCoolifyInstances returns every team-token row.
func (s *Store) ListCoolifyInstances() ([]CoolifyInstance, error) {
	rows, err := s.db.Query(`SELECT ` + coolifyCols + ` FROM coolify_instances ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list coolify: %w", err)
	}
	defer rows.Close()
	var out []CoolifyInstance
	for rows.Next() {
		c, err := s.scanCoolify(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan coolify: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CoolifyInstance loads one row.
func (s *Store) CoolifyInstance(id int64) (CoolifyInstance, error) {
	c, err := s.scanCoolify(s.db.QueryRow(`SELECT `+coolifyCols+` FROM coolify_instances WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CoolifyInstance{}, ErrNotFound
	}
	if err != nil {
		return CoolifyInstance{}, fmt.Errorf("store: coolify %d: %w", id, err)
	}
	return c, nil
}

// DecryptCoolifyToken returns the plaintext API token for outbound calls only.
func (s *Store) DecryptCoolifyToken(c CoolifyInstance) (string, error) {
	return s.box.Open(c.apiTokenEnc)
}

// CoolifyInput is the writable shape of a Coolify row.
type CoolifyInput struct {
	Name               string
	BaseURL            string
	APIToken           string
	DefaultServerUUID  string
	DefaultProjectUUID string
}

func (in *CoolifyInput) normalise() error {
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.APIToken = strings.TrimSpace(in.APIToken)
	if in.BaseURL == "" {
		return errors.New("base_url is required")
	}
	if !strings.HasPrefix(in.BaseURL, "http://") && !strings.HasPrefix(in.BaseURL, "https://") {
		return errors.New("base_url must start with http:// or https://")
	}
	if in.Name == "" {
		in.Name = in.BaseURL
	}
	return nil
}

// CreateCoolifyInstance adds a (base URL, team token) row.
func (s *Store) CreateCoolifyInstance(in CoolifyInput) (CoolifyInstance, error) {
	if err := in.normalise(); err != nil {
		return CoolifyInstance{}, err
	}
	if in.APIToken == "" {
		return CoolifyInstance{}, errors.New("api_token is required")
	}
	enc, err := s.box.Seal(in.APIToken)
	if err != nil {
		return CoolifyInstance{}, err
	}
	ts := formatTime(now())
	res, err := s.db.Exec(`INSERT INTO coolify_instances
		(name, base_url, api_token_enc, default_server_uuid, default_project_uuid, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		in.Name, in.BaseURL, enc, in.DefaultServerUUID, in.DefaultProjectUUID, ts, ts)
	if err != nil {
		return CoolifyInstance{}, fmt.Errorf("store: create coolify: %w", err)
	}
	id, _ := res.LastInsertId()
	return s.CoolifyInstance(id)
}

// UpdateCoolifyInstance patches a row. An empty APIToken keeps the stored one so
// the UI can re-save a form that only ever showed a redacted token.
func (s *Store) UpdateCoolifyInstance(id int64, in CoolifyInput) (CoolifyInstance, error) {
	existing, err := s.CoolifyInstance(id)
	if err != nil {
		return CoolifyInstance{}, err
	}
	if err := in.normalise(); err != nil {
		return CoolifyInstance{}, err
	}
	enc := existing.apiTokenEnc
	if in.APIToken != "" {
		if enc, err = s.box.Seal(in.APIToken); err != nil {
			return CoolifyInstance{}, err
		}
	}
	// Changing the token (or the host) invalidates the cached team label: the
	// token defines which team we can see.
	teamID, teamName := existing.TeamID, existing.TeamName
	if in.APIToken != "" || in.BaseURL != existing.BaseURL {
		teamID, teamName = "", ""
	}
	_, err = s.db.Exec(`UPDATE coolify_instances SET name = ?, base_url = ?, api_token_enc = ?,
		team_id = ?, team_name = ?, default_server_uuid = ?, default_project_uuid = ?, updated_at = ?
		WHERE id = ?`,
		in.Name, in.BaseURL, enc, teamID, teamName, in.DefaultServerUUID, in.DefaultProjectUUID,
		formatTime(now()), id)
	if err != nil {
		return CoolifyInstance{}, fmt.Errorf("store: update coolify: %w", err)
	}
	return s.CoolifyInstance(id)
}

// RecordCoolifyTest stores the outcome of a Test connection click.
func (s *Store) RecordCoolifyTest(id int64, teamID, teamName, errMsg string) error {
	var seen any
	if errMsg == "" {
		t := now()
		seen = formatTime(t)
	}
	_, err := s.db.Exec(`UPDATE coolify_instances SET team_id = ?, team_name = ?,
		last_seen_at = COALESCE(?, last_seen_at), last_error = ?, updated_at = ? WHERE id = ?`,
		teamID, teamName, seen, errMsg, formatTime(now()), id)
	if err != nil {
		return fmt.Errorf("store: record coolify test: %w", err)
	}
	return nil
}

// DeleteCoolifyInstance removes a row; bindings keep working with a null source.
func (s *Store) DeleteCoolifyInstance(id int64) error {
	res, err := s.db.Exec(`DELETE FROM coolify_instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete coolify: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
