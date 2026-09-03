// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: 15 * time.Minute, Overrides: Overrides{Install: "ignored"}})
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
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
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

func TestRuntimeOnlyFileAppliesToFallbackCommands(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "runtime: node:24\ntimeout: 5m\n")
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)
	writeFile(t, dir, "package-lock.json", `{}`)

	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Runtime != "node:24" {
		t.Fatalf("runtime from a commands-less file must still apply: %q", plan.Runtime)
	}
	if plan.Timeout != 5*time.Minute {
		t.Fatalf("timeout from that file must still apply: %s", plan.Timeout)
	}
	if plan.Source != "Node defaults from package.json" {
		t.Fatalf("source: %q", plan.Source)
	}
	if len(plan.Steps) != 2 {
		t.Fatalf("steps: %+v", plan.Steps)
	}
}

func TestBindingOverridesUsedWhenNoFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute, Overrides: Overrides{Test: "make check"}})
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
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
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
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
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
		plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
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
	if _, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute}); !errors.Is(err, ErrNothingToRun) {
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
	if _, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute}); err == nil {
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

func sourceOf(t *testing.T, plan Plan, field string) string {
	t.Helper()
	src := plan.OriginOf(field)
	if src == "" {
		t.Fatalf("no origin recorded for %q; origins: %+v", field, plan.Origins)
	}
	return src
}

// TestOriginsNamePerValueLayers is item 15's point: two values in one plan can
// come from two different layers, and a single plan-wide Source cannot say so.
func TestOriginsNamePerValueLayers(t *testing.T) {
	dir := t.TempDir()
	// runtime from the file; commands from the binding; timeout from settings.
	writeFile(t, dir, ".ci.yml", "runtime: node:24\n")
	plan, err := Resolve(dir, Inputs{
		PipelineFile:         ".ci.yml",
		PipelineFileSource:   SourceBinding,
		Overrides:            Overrides{Test: "make check"},
		DefaultTimeout:       9 * time.Minute,
		DefaultTimeoutSource: SourceSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceOf(t, plan, "runtime"); got != ".ci.yml" {
		t.Errorf("runtime origin: %q", got)
	}
	if got := sourceOf(t, plan, "test"); got != SourceBindingCommands {
		t.Errorf("test origin: %q", got)
	}
	if got := sourceOf(t, plan, "timeout"); got != SourceSettings {
		t.Errorf("timeout origin: %q", got)
	}
	if got := sourceOf(t, plan, "pipeline_file"); got != SourceBinding {
		t.Errorf("pipeline_file origin: %q", got)
	}
}

func TestTimeoutOriginIsTheFileWhenTheFileSetsIt(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "test: go test ./...\ntimeout: 3m\n")
	plan, err := Resolve(dir, Inputs{
		PipelineFile: ".ci.yml", DefaultTimeout: time.Minute, DefaultTimeoutSource: SourceSettings,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Timeout != 3*time.Minute {
		t.Fatalf("timeout: %s", plan.Timeout)
	}
	if got := sourceOf(t, plan, "timeout"); got != ".ci.yml" {
		t.Fatalf("timeout origin: %q", got)
	}
}

// TestForkRuntimeOriginNamesSettings covers the value with security
// consequences. Before this it was applied in the runner and the log still
// credited the pipeline file.
func TestForkRuntimeOriginNamesSettings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "test: npm test\n")
	plan, err := Resolve(dir, Inputs{
		PipelineFile: ".ci.yml", DefaultTimeout: time.Minute,
		DefaultRuntime: "node:22", IsFork: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Runtime != "node:22" {
		t.Fatalf("a fork must inherit default_runtime: %q", plan.Runtime)
	}
	if got := sourceOf(t, plan, "runtime"); !strings.Contains(got, "default_runtime") {
		t.Fatalf("runtime origin should name the setting: %q", got)
	}
}

func TestNonForkDoesNotInheritDefaultRuntime(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".ci.yml", "test: npm test\n")
	plan, err := Resolve(dir, Inputs{
		PipelineFile: ".ci.yml", DefaultTimeout: time.Minute, DefaultRuntime: "node:22",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Runtime != "" {
		t.Fatalf("default_runtime is a fork policy, not a global executor: %q", plan.Runtime)
	}
	if got := sourceOf(t, plan, "runtime"); got != SourceDefault {
		t.Fatalf("runtime origin: %q", got)
	}
}

func TestGoInference(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "go.mod", "module example.com/x\n\ngo 1.24\n")
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != "Go defaults from go.mod" {
		t.Fatalf("source: %q", plan.Source)
	}
	want := []string{"go mod download", "go test ./...", "go build ./..."}
	if len(plan.Steps) != 3 {
		t.Fatalf("steps: %+v", plan.Steps)
	}
	for i, w := range want {
		if plan.Steps[i].Command != w {
			t.Errorf("step %d: %q want %q", i, plan.Steps[i].Command, w)
		}
	}
}

func TestRustInference(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Cargo.toml", "[package]\nname = \"x\"\n")
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// No Cargo.lock, so --locked would fail outright.
	if plan.Steps[0].Command != "cargo fetch" {
		t.Fatalf("install: %q", plan.Steps[0].Command)
	}
	writeFile(t, dir, "Cargo.lock", "")
	plan, err = Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Steps[0].Command != "cargo fetch --locked" {
		t.Fatalf("install with a lockfile: %q", plan.Steps[0].Command)
	}
}

// TestPythonInferenceOmitsTestWithoutEvidence is AUDIT Q4: pytest exits 5 on
// "no tests collected", so an unconditional test step would fail a check for a
// repository that simply has no tests.
func TestPythonInferenceOmitsTestWithoutEvidence(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "requirements.txt", "requests\n")
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Command != "pip install -r requirements.txt" {
		t.Fatalf("steps: %+v", plan.Steps)
	}

	if err := os.Mkdir(filepath.Join(dir, "tests"), 0o750); err != nil {
		t.Fatal(err)
	}
	plan, err = Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 2 || plan.Steps[1].Command != "pytest" {
		t.Fatalf("a tests/ directory is the evidence pytest needs: %+v", plan.Steps)
	}
}

func TestPythonInstallPrefersLockfiles(t *testing.T) {
	cases := []struct{ lockfile, want string }{
		{"uv.lock", "uv sync --frozen"},
		{"poetry.lock", "poetry install --no-interaction"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		writeFile(t, dir, "pyproject.toml", "[project]\nname = \"x\"\n")
		writeFile(t, dir, c.lockfile, "")
		plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Steps[0].Command != c.want {
			t.Errorf("%s: %q want %q", c.lockfile, plan.Steps[0].Command, c.want)
		}
	}
}

// TestAmbiguousProjectWarnsAndKeepsNode covers the Go service with a JS front
// end. Node stays first so no repository that works today changes plan, but a
// silent pick is the failure mode this wave exists to remove.
func TestAmbiguousProjectWarnsAndKeepsNode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"test":"jest"}}`)
	writeFile(t, dir, "go.mod", "module example.com/x\n\ngo 1.24\n")
	plan, err := Resolve(dir, Inputs{PipelineFile: ".ci.yml", DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Source != "Node defaults from package.json" {
		t.Fatalf("Node must stay first: %q", plan.Source)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "Go (go.mod)") {
		t.Fatalf("warnings: %+v", plan.Warnings)
	}
}
