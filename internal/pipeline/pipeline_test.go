package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPipelineFileWins(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "runtime: node:24\ninstall: npm ci\ntest: npm test\nbuild: npm run build\ntimeout: 5m\n")
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)

	plan, err := Resolve(dir, ".ci.yml", Overrides{Install: "ignored"}, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != ".ci.yml" {
		t.Fatalf("source: %q", plan.Source)
	}
	if plan.Timeout != 5*time.Minute {
		t.Fatalf("the file's timeout should win: %s", plan.Timeout)
	}
	if plan.Runtime != "node:24" {
		t.Fatalf("runtime not recorded: %q", plan.Runtime)
	}
	if len(plan.Steps) != 3 || plan.Steps[0].Command != "npm ci" {
		t.Fatalf("steps: %+v", plan.Steps)
	}
}

func TestPipelineFilePartialStepsOnly(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "test: go test ./...\n")
	plan, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Name != "test" {
		t.Fatalf("a file with one command should give one step: %+v", plan.Steps)
	}
	if plan.Timeout != time.Minute {
		t.Fatalf("absent timeout should inherit: %s", plan.Timeout)
	}
}

func TestBindingOverridesUsedWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)
	plan, err := Resolve(dir, ".ci.yml", Overrides{Test: "make check"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != "binding commands" {
		t.Fatalf("source: %q", plan.Source)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Command != "make check" {
		t.Fatalf("steps: %+v", plan.Steps)
	}
}

func TestNodeDefaultsSkipMissingScripts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)
	writeFile(t, dir, "package-lock.json", `{}`)
	plan, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("expected install + test only, got %+v", plan.Steps)
	}
	if plan.Steps[0].Command != "npm ci" {
		t.Fatalf("a lockfile means npm ci: %q", plan.Steps[0].Command)
	}
	if plan.Steps[1].Command != "npm test" {
		t.Fatalf("test step: %q", plan.Steps[1].Command)
	}
}

func TestNodeDefaultsWithoutLockfile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"build":"tsc"}}`)
	plan, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// npm ci fails without a lockfile, so install is the only honest choice.
	if plan.Steps[0].Command != "npm install --no-audit --no-fund" {
		t.Fatalf("install: %q", plan.Steps[0].Command)
	}
	if len(plan.Steps) != 2 || plan.Steps[1].Command != "npm run build" {
		t.Fatalf("steps: %+v", plan.Steps)
	}
}

func TestPackageManagerDetection(t *testing.T) {
	cases := []struct{ lockfile, wantInstall, wantRunner string }{
		{"pnpm-lock.yaml", "pnpm install --frozen-lockfile", "pnpm"},
		{"yarn.lock", "yarn install --immutable", "yarn"},
		{"package-lock.json", "npm ci", "npm"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeFile(t, dir, "package.json", `{"scripts":{"test":"x"}}`)
		writeFile(t, dir, c.lockfile, "")
		plan, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Steps[0].Command != c.wantInstall {
			t.Errorf("%s install: %q want %q", c.lockfile, plan.Steps[0].Command, c.wantInstall)
		}
		if plan.Steps[1].Command != c.wantRunner+" test" {
			t.Errorf("%s test: %q", c.lockfile, plan.Steps[1].Command)
		}
	}
}

func TestNothingToRun(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute); !errors.Is(err, ErrNothingToRun) {
		t.Fatalf("an empty repo should be skipped, got %v", err)
	}
}

func TestPipelineFileEscapeRejected(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../secrets.yml", "/etc/passwd"} {
		if _, _, err := LoadSpec(dir, name); err == nil {
			t.Errorf("%q should be rejected as outside the repository", name)
		}
	}
}

func TestInvalidYAMLIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "install: [unclosed\n")
	if _, err := Resolve(dir, ".ci.yml", Overrides{}, time.Minute); err == nil {
		t.Fatal("invalid YAML should fail loudly, not silently fall back")
	}
}

func TestParseDuration(t *testing.T) {
	if d, err := ParseDuration("15m"); err != nil || d != 15*time.Minute {
		t.Fatalf("15m: %v %s", err, d)
	}
	if d, err := ParseDuration("1h30m"); err != nil || d != 90*time.Minute {
		t.Fatalf("1h30m: %v %s", err, d)
	}
	for _, bad := range []string{"", "soon", "-5m", "0s", "15"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
