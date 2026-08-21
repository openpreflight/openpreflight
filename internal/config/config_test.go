package config

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRequiresSecretKey(t *testing.T) {
	t.Setenv("CI_SECRET_KEY", "")
	if _, err := Load(); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("got %v want ErrNoSecretKey", err)
	}
	// A short key is refused too: it is the entropy behind every stored secret.
	t.Setenv("CI_SECRET_KEY", "tooshort")
	if _, err := Load(); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("short key accepted: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CI_SECRET_KEY", strings.Repeat("k", 40))
	t.Setenv("LISTEN_ADDR", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("CI_PUBLIC_BASE_URL", "https://ci.example.com/")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != ":8080" || cfg.DataDir != "/data" {
		t.Fatalf("defaults: %+v", cfg)
	}
	if cfg.PublicBaseURL != "https://ci.example.com" {
		t.Fatalf("trailing slash not trimmed: %q", cfg.PublicBaseURL)
	}
	if cfg.DBPath() != "/data/ci.db" || cfg.LogDir() != "/data/logs" {
		t.Fatalf("paths: %q %q", cfg.DBPath(), cfg.LogDir())
	}
}

func TestEnsureDirs(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CI_SECRET_KEY", strings.Repeat("k", 40))
	t.Setenv("DATA_DIR", filepath.Join(base, "data"))
	t.Setenv("WORKSPACE_DIR", filepath.Join(base, "workspace"))
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Running twice must be fine: the container restarts on every deploy.
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
}
