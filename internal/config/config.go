// Package config loads process configuration from the environment.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the whole process configuration. Everything else (check names,
// timeouts, bindings) lives in the database and is edited in the configurator.
type Config struct {
	ListenAddr string
	DataDir    string
	// SecretKey is the raw material for the AES-GCM key protecting secret
	// columns. Required: losing it makes stored PEMs and tokens unreadable.
	SecretKey string
	// BootstrapAdminPassword, when set, seeds the admin user on first boot so
	// a fresh container can be driven by API without the browser wizard.
	BootstrapAdminPassword string
	// PublicBaseURL seeds the settings row on first boot only.
	PublicBaseURL string
}

// ErrNoSecretKey is returned when CI_SECRET_KEY is absent or too weak.
var ErrNoSecretKey = errors.New("CI_SECRET_KEY is required (32+ bytes of entropy)")

// minSecretKeyLen is the shortest CI_SECRET_KEY we accept. The key is hashed
// before use, so this is an entropy floor, not a cipher constraint.
const minSecretKeyLen = 32

// Load reads the environment and validates it.
func Load() (Config, error) {
	c := Config{
		ListenAddr:             env("LISTEN_ADDR", ":8080"),
		DataDir:                env("DATA_DIR", "/data"),
		SecretKey:              os.Getenv("CI_SECRET_KEY"),
		BootstrapAdminPassword: os.Getenv("CI_BOOTSTRAP_ADMIN_PASSWORD"),
		PublicBaseURL:          strings.TrimRight(os.Getenv("CI_PUBLIC_BASE_URL"), "/"),
	}
	if len(strings.TrimSpace(c.SecretKey)) < minSecretKeyLen {
		return Config{}, ErrNoSecretKey
	}
	if c.DataDir == "" {
		return Config{}, errors.New("DATA_DIR must not be empty")
	}
	return c, nil
}

// DBPath is the SQLite file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "ci.db") }

// LogDir holds one file per job.
func (c Config) LogDir() string { return filepath.Join(c.DataDir, "logs") }

// WorkspaceDir is the parent of the per-job checkout directories. It is
// deliberately outside DataDir so a Coolify volume can be mounted per purpose.
func (c Config) WorkspaceDir() string { return env("WORKSPACE_DIR", "/workspace") }

// EnsureDirs creates the directories the process writes to.
func (c Config) EnsureDirs() error {
	for _, d := range []string{c.DataDir, c.LogDir(), c.WorkspaceDir()} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
