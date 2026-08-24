package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trivedi-vatsal/openpreflight/internal/secret"
)

func TestRotateSecretsRebindsColumns(t *testing.T) {
	oldKey := strings.Repeat("o", 40)
	newKey := strings.Repeat("n", 40)
	oldBox, err := secret.New(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	newBox, err := secret.New(newKey)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "ci.db"), oldBox)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	pemEnc, _ := oldBox.Seal("-----BEGIN PRIVATE KEY-----\nold\n-----END PRIVATE KEY-----")
	whEnc, _ := oldBox.Seal("webhook-secret")
	tokEnc, _ := oldBox.Seal("coolify-token")
	if _, err := st.DB().Exec(`INSERT INTO github_apps
		(name, slug, app_id, pem_enc, webhook_secret_enc, created_at, updated_at)
		VALUES ('app', 'app', 1, ?, ?, ?, ?)`, pemEnc, whEnc, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO coolify_instances
		(name, base_url, api_token_enc, created_at, updated_at)
		VALUES ('cfy', 'https://coolify.example', ?, ?, ?)`, tokEnc, ts, ts); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(filepath.Join(dir, "ci.db"), newBox)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := st.RotateSecrets(oldBox)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("rotated %d rows, want 2", n)
	}
	app, err := st.GitHubAppBySlug("app")
	if err != nil {
		t.Fatal(err)
	}
	pem, err := st.DecryptPEM(app)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pem, "old") {
		t.Fatalf("pem: %q", pem)
	}
	inst, err := st.CoolifyInstance(1)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := st.DecryptCoolifyToken(inst)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "coolify-token" {
		t.Fatalf("token: %q", tok)
	}

	n, err = st.RotateSecrets(oldBox)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second rotate should be a no-op, got %d", n)
	}
}

func TestRotateSecretsRejectsUnreadableRow(t *testing.T) {
	oldBox, err := secret.New(strings.Repeat("o", 40))
	if err != nil {
		t.Fatal(err)
	}
	newBox, err := secret.New(strings.Repeat("n", 40))
	if err != nil {
		t.Fatal(err)
	}
	other, err := secret.New(strings.Repeat("x", 40))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "ci.db"), newBox)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	enc, err := other.Seal("orphan")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`INSERT INTO coolify_instances
		(name, base_url, api_token_enc, created_at, updated_at)
		VALUES ('cfy', 'https://coolify.example', ?, ?, ?)`, enc, ts, ts); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RotateSecrets(oldBox); err == nil {
		t.Fatal("a column sealed under neither key must fail the rotate")
	}
}
