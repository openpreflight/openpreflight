// Package pipeline decides what commands a job runs: the repo's pipeline file,
// else the binding's command overrides, else Node defaults inferred from
// package.json.
package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is the repo's pipeline file. `runtime` is parsed and reported but ignored
// until there is a Docker executor (PLAN.md).
type Spec struct {
	Runtime string `yaml:"runtime"`
	Install string `yaml:"install"`
	Test    string `yaml:"test"`
	Build   string `yaml:"build"`
	Timeout string `yaml:"timeout"`
}

// Step is one named command in the plan.
type Step struct {
	Name    string
	Command string
}

// Plan is the resolved sequence for one job.
type Plan struct {
	Steps   []Step
	Timeout time.Duration
	// Source says where the plan came from, for the log header and the Check
	// Run summary.
	Source string
	// Runtime is the declared runtime, recorded so the log can say it was
	// ignored rather than silently dropping it.
	Runtime string
}

// Overrides are the binding's optional commands.
type Overrides struct {
	Install string
	Test    string
	Build   string
}

// Any reports whether the binding supplies any command.
func (o Overrides) Any() bool { return o.Install != "" || o.Test != "" || o.Build != "" }

// ErrNothingToRun means no pipeline file, no overrides and no recognisable
// project: the Check Run should be skipped, not failed.
var ErrNothingToRun = errors.New("pipeline: nothing to run")

// LoadSpec reads the pipeline file from a checkout. Missing file is not an
// error: ok is false and the caller falls back.
func LoadSpec(repoDir, filename string) (Spec, bool, error) {
	if filename == "" {
		filename = ".ci.yml"
	}
	// The filename comes from configuration; keep it inside the checkout.
	clean := filepath.Clean(filename)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return Spec{}, false, fmt.Errorf("pipeline: %q is outside the repository", filename)
	}
	raw, err := os.ReadFile(filepath.Join(repoDir, clean))
	if os.IsNotExist(err) {
		return Spec{}, false, nil
	}
	if err != nil {
		return Spec{}, false, fmt.Errorf("pipeline: read %s: %w", clean, err)
	}
	var spec Spec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return Spec{}, false, fmt.Errorf("pipeline: %s is not valid YAML: %w", clean, err)
	}
	return spec, true, nil
}

// Resolve builds the plan. Precedence: pipeline file, then binding overrides,
// then Node defaults.
func Resolve(repoDir, pipelineFile string, ov Overrides, defaultTimeout time.Duration) (Plan, error) {
	spec, found, err := LoadSpec(repoDir, pipelineFile)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Timeout: defaultTimeout}

	if found && (spec.Install != "" || spec.Test != "" || spec.Build != "") {
		plan.Source = pipelineFileName(pipelineFile)
		plan.Runtime = spec.Runtime
		plan.Steps = named(spec.Install, spec.Test, spec.Build)
		if spec.Timeout != "" {
			d, err := ParseDuration(spec.Timeout)
			if err != nil {
				return Plan{}, fmt.Errorf("pipeline: %s: %w", plan.Source, err)
			}
			plan.Timeout = d
		}
		return plan, nil
	}

	if ov.Any() {
		plan.Source = "binding commands"
		plan.Steps = named(ov.Install, ov.Test, ov.Build)
		return plan, nil
	}

	steps, err := nodeDefaults(repoDir)
	if err != nil {
		return Plan{}, err
	}
	if len(steps) == 0 {
		return Plan{}, ErrNothingToRun
	}
	plan.Source = "Node defaults from package.json"
	plan.Steps = steps
	return plan, nil
}

func pipelineFileName(f string) string {
	if f == "" {
		return ".ci.yml"
	}
	return f
}

// named drops empty commands: a pipeline file that sets only `test` runs only
// the test step rather than failing on an empty install.
func named(install, test, build string) []Step {
	var out []Step
	for _, s := range []Step{{"install", install}, {"test", test}, {"build", build}} {
		if strings.TrimSpace(s.Command) != "" {
			out = append(out, s)
		}
	}
	return out
}

// packageJSON is the part of package.json we read.
type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
}

// nodeDefaults infers steps from package.json. A missing test or build script is
// simply not a step — missing scripts are skipped, not failed (PLAN.md).
func nodeDefaults(repoDir string) ([]Step, error) {
	raw, err := os.ReadFile(filepath.Join(repoDir, "package.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pipeline: read package.json: %w", err)
	}
	var pkg packageJSON
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("pipeline: package.json is not valid JSON: %w", err)
	}
	install, runner := nodeCommands(repoDir, pkg.PackageManager)
	steps := []Step{{Name: "install", Command: install}}
	if _, ok := pkg.Scripts["test"]; ok {
		steps = append(steps, Step{Name: "test", Command: runner + " test"})
	}
	if _, ok := pkg.Scripts["build"]; ok {
		steps = append(steps, Step{Name: "build", Command: runner + " run build"})
	}
	return steps, nil
}

// nodeCommands picks the install command and script runner from the lockfile
// present in the checkout, falling back to npm.
func nodeCommands(repoDir, declared string) (install, runner string) {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(repoDir, name))
		return err == nil
	}
	switch {
	case strings.HasPrefix(declared, "pnpm") || exists("pnpm-lock.yaml"):
		return "pnpm install --frozen-lockfile", "pnpm"
	case strings.HasPrefix(declared, "yarn") || exists("yarn.lock"):
		return "yarn install --immutable", "yarn"
	case exists("package-lock.json"), exists("npm-shrinkwrap.json"):
		return "npm ci", "npm"
	default:
		// No lockfile: npm ci would fail outright, so install is the honest
		// choice even though it is not reproducible.
		return "npm install --no-audit --no-fund", "npm"
	}
}

// ParseDuration accepts Go durations plus the bare-minutes shorthand people
// write in CI files ("15m", "90s", "1h30m").
func ParseDuration(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, errors.New("empty duration")
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration like 15m or 1h30m", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout %q must be positive", v)
	}
	return d, nil
}
