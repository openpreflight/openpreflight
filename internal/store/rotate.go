package store

import (
	"fmt"

	"github.com/trivedi-vatsal/coolify-github-ci/internal/secret"
)

// RotateSecrets re-seals every secret column from old into this store's box
// (the key currently in CI_SECRET_KEY). Rows that already open with the new
// box are left alone so a restart after a partial rotate is safe. A row that
// opens with neither key is an error and stops the loop.
//
// Reads finish before writes: the store uses a single SQLite connection, so a
// Query left open would deadlock an UPDATE.
func (s *Store) RotateSecrets(old *secret.Box) (int, error) {
	if old == nil {
		return 0, fmt.Errorf("store: rotate: old key is required")
	}

	type appRow struct {
		id                int64
		pemEnc, secretEnc string
	}
	apps, err := s.db.Query(`SELECT id, pem_enc, webhook_secret_enc FROM github_apps`)
	if err != nil {
		return 0, fmt.Errorf("store: rotate github apps: %w", err)
	}
	var appRows []appRow
	for apps.Next() {
		var r appRow
		if err := apps.Scan(&r.id, &r.pemEnc, &r.secretEnc); err != nil {
			apps.Close()
			return 0, fmt.Errorf("store: rotate scan github app: %w", err)
		}
		appRows = append(appRows, r)
	}
	if err := apps.Err(); err != nil {
		apps.Close()
		return 0, err
	}
	if err := apps.Close(); err != nil {
		return 0, err
	}

	n := 0
	for _, r := range appRows {
		pem, pemNew, err := reopen(old, s.box, r.pemEnc)
		if err != nil {
			return n, fmt.Errorf("store: rotate github app %d pem: %w", r.id, err)
		}
		wh, whNew, err := reopen(old, s.box, r.secretEnc)
		if err != nil {
			return n, fmt.Errorf("store: rotate github app %d webhook secret: %w", r.id, err)
		}
		if pemNew && whNew {
			continue
		}
		newPEM, newWH := r.pemEnc, r.secretEnc
		if !pemNew {
			newPEM, err = s.box.Seal(pem)
			if err != nil {
				return n, err
			}
		}
		if !whNew {
			newWH, err = s.box.Seal(wh)
			if err != nil {
				return n, err
			}
		}
		if _, err := s.db.Exec(`UPDATE github_apps SET pem_enc = ?, webhook_secret_enc = ? WHERE id = ?`,
			newPEM, newWH, r.id); err != nil {
			return n, fmt.Errorf("store: rotate github app %d: %w", r.id, err)
		}
		n++
	}

	type instRow struct {
		id  int64
		enc string
	}
	rows, err := s.db.Query(`SELECT id, api_token_enc FROM coolify_instances`)
	if err != nil {
		return n, fmt.Errorf("store: rotate coolify: %w", err)
	}
	var instRows []instRow
	for rows.Next() {
		var r instRow
		if err := rows.Scan(&r.id, &r.enc); err != nil {
			rows.Close()
			return n, fmt.Errorf("store: rotate scan coolify: %w", err)
		}
		instRows = append(instRows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return n, err
	}
	if err := rows.Close(); err != nil {
		return n, err
	}

	for _, r := range instRows {
		tok, already, err := reopen(old, s.box, r.enc)
		if err != nil {
			return n, fmt.Errorf("store: rotate coolify %d: %w", r.id, err)
		}
		if already {
			continue
		}
		next, err := s.box.Seal(tok)
		if err != nil {
			return n, err
		}
		if _, err := s.db.Exec(`UPDATE coolify_instances SET api_token_enc = ? WHERE id = ?`, next, r.id); err != nil {
			return n, fmt.Errorf("store: rotate coolify %d: %w", r.id, err)
		}
		n++
	}
	return n, nil
}

// reopen prefers the new box (already rotated) and falls back to old.
// already is true when the new box opened the value, so the caller must not
// re-seal: Seal is non-deterministic and a second boot would rewrite every row.
func reopen(old, next *secret.Box, sealed string) (plaintext string, already bool, err error) {
	if sealed == "" {
		return "", true, nil
	}
	if pt, err := next.Open(sealed); err == nil {
		return pt, true, nil
	}
	pt, err := old.Open(sealed)
	if err != nil {
		return "", false, fmt.Errorf("value opens with neither the old nor the new CI_SECRET_KEY")
	}
	return pt, false, nil
}
