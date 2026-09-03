// SPDX-License-Identifier: Apache-2.0

// Package pipeline decides what commands a job runs: the repo's pipeline file,
// else the binding's command overrides, else defaults inferred from the
// project's own files.
//
// Every resolved value records where it came from. A plan that cannot say which
// of the four layers won is a plan an operator has to guess at, and guessing is
// what this package exists to remove.
package pipeline

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is the repo's pipeline file. `runtime` is a Docker image; empty means
// the worker process (Node in this image).
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

// Source labels for the layers that are not a file in the checkout. A file
// contributes its own name (".ci.yml"), which is why there is no constant for
// it.
const (
	// SourceBindingCommands is the binding's install/test/build overrides.
	// The string is load-bearing: it is persisted in jobs.plan_source.
	SourceBindingCommands = "binding commands"
	// SourceBinding is a per-binding value that is not a command, such as the
	// pipeline filename or the timeout.
	SourceBinding = "binding"
	// SourceSettings is the global settings row.
	SourceSettings = "settings"
	// SourceDefault is a built-in fallback with no configured origin.
	SourceDefault = "built-in default"
)

// Origin records where one resolved value came from. Fields are the names an
// operator sees in the UI and in the resolve API, not Go identifiers.
type Origin struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Plan is the resolved sequence for one job.
type Plan struct {
	Steps   []Step
	Timeout time.Duration
	// Source says where the *steps* came from, for the log header and the Check
	// Run summary. It is a summary of Origins, kept because it is persisted on
	// the job row (jobs.plan_source) and rendered on the run page.
	Source string
	// Runtime is the resolved Docker image. Empty means the host process executor.
	Runtime string
	// Origins is the per-value provenance: one entry per resolved value.
	Origins []Origin
	// Warnings are things that are legal but probably not intended — an
	// ambiguous project layout, most often. They never stop a run; the resolve
	// endpoint surfaces them so an operator can see them before one happens.
	Warnings []string
}

// OriginOf returns the recorded source for a field, or "" if it was not
// resolved. Used by the run page and the resolve API rather than a map so the
// order values were decided in is preserved.
func (p Plan) OriginOf(field string) string {
	for _, o := range p.Origins {
		if o.Field == field {
			return o.Source
		}
	}
	return ""
}

func (p *Plan) record(field, value, source string) {
	p.Origins = append(p.Origins, Origin{Field: field, Value: value, Source: source})
}

func (p *Plan) warn(format string, args ...any) {
	p.Warnings = append(p.Warnings, fmt.Sprintf(format, args...))
}

// Overrides are the binding's optional commands.
type Overrides struct {
	Install string
	Test    string
	Build   string
}

// Any reports whether the binding supplies any command.
func (o Overrides) Any() bool { return o.Install != "" || o.Test != "" || o.Build != "" }

// Inputs are everything outside the checkout that can affect the plan: the
// three configuration layers above the repository, plus whether this commit
// comes from a fork.
//
// The *Source fields say which layer supplied the value next to them. The
// caller knows that and this package does not, and a resolved value whose
// origin is "somewhere above the repo" is not much better than no origin at all.
type Inputs struct {
	// PipelineFile is the filename to look for, already resolved from the
	// binding and the settings default.
	PipelineFile       string
	PipelineFileSource string

	// Overrides are the binding's commands.
	Overrides Overrides

	// DefaultTimeout is the timeout the job started with. A pipeline file may
	// still lower or raise it.
	DefaultTimeout       time.Duration
	DefaultTimeoutSource string

	// DefaultRuntime is settings.default_runtime. It applies to fork commits
	// only: fork code always runs in Docker, never as a process on this host.
	DefaultRuntime string

	// IsFork marks a commit from a fork pull request.
	IsFork bool
}

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

// Resolve builds the plan. Precedence for the commands: pipeline file, then
// binding overrides, then inference from the project's own files. `runtime` and
// `timeout` resolve independently — a file that sets only `runtime:` still
// applies it to inferred commands — which is exactly why provenance has to be
// per value and not per plan.
func Resolve(repoDir string, in Inputs) (Plan, error) {
	file := pipelineFileName(in.PipelineFile)
	spec, found, err := LoadSpec(repoDir, file)
	if err != nil {
		return Plan{}, err
	}

	plan := Plan{Timeout: in.DefaultTimeout}
	plan.record("pipeline_file", file, orDefault(in.PipelineFileSource, SourceSettings))

	timeoutSource := orDefault(in.DefaultTimeoutSource, SourceSettings)
	if found {
		plan.Runtime = strings.TrimSpace(spec.Runtime)
		if spec.Timeout != "" {
			d, err := ParseDuration(spec.Timeout)
			if err != nil {
				return Plan{}, fmt.Errorf("pipeline: %s: %w", file, err)
			}
			plan.Timeout = d
			timeoutSource = file
		}
	}
	plan.record("timeout", plan.Timeout.String(), timeoutSource)
	plan.recordRuntime(file, in)

	if err := plan.resolveSteps(repoDir, file, found, spec, in.Overrides); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// recordRuntime resolves the executor. The fork fallback lives here rather than
// in the runner so that the one value with security consequences carries an
// origin like everything else — before this it was applied silently and the log
// still credited the pipeline file.
func (p *Plan) recordRuntime(file string, in Inputs) {
	switch {
	case p.Runtime != "":
		p.record("runtime", p.Runtime, file)
	case in.IsFork && strings.TrimSpace(in.DefaultRuntime) != "":
		p.Runtime = strings.TrimSpace(in.DefaultRuntime)
		p.record("runtime", p.Runtime, SourceSettings+" (default_runtime, for a fork commit)")
	default:
		p.record("runtime", "worker process", SourceDefault)
	}
}

// resolveSteps applies the three-layer command precedence and records an origin
// per step.
func (p *Plan) resolveSteps(repoDir, file string, found bool, spec Spec, ov Overrides) error {
	if found && (spec.Install != "" || spec.Test != "" || spec.Build != "") {
		p.Source = file
		p.Steps = named(spec.Install, spec.Test, spec.Build)
		p.recordSteps(file)
		return nil
	}

	if ov.Any() {
		p.Source = SourceBindingCommands
		p.Steps = named(ov.Install, ov.Test, ov.Build)
		p.recordSteps(SourceBindingCommands)
		return nil
	}

	inferred, err := infer(repoDir)
	if err != nil {
		return err
	}
	if len(inferred.steps) == 0 {
		return ErrNothingToRun
	}
	p.Source = inferred.source
	p.Steps = inferred.steps
	p.Warnings = append(p.Warnings, inferred.warnings...)
	p.recordSteps(inferred.source)
	return nil
}

func (p *Plan) recordSteps(source string) {
	for _, s := range p.Steps {
		p.record(s.Name, s.Command, source)
	}
}

func pipelineFileName(f string) string {
	if f == "" {
		return ".ci.yml"
	}
	return f
}

// Layer names which configuration layer supplied a value, walking the same
// binding → settings → built-in chain the value itself walked. Callers pass
// whether each layer had a value set; the first that did is the origin.
func Layer(bindingSet, settingsSet bool) string {
	switch {
	case bindingSet:
		return SourceBinding
	case settingsSet:
		return SourceSettings
	default:
		return SourceDefault
	}
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
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

// Validate reports everything wrong with a pipeline file rather than the first
// thing, and hands back the spec it managed to read.
//
// Resolve stops at the first error because a job cannot run on a broken file.
// A dry run is the opposite case: the operator is about to fix all of them, so
// finding out about the bad timeout only after fixing the bad image wastes a
// round trip. Image validation is left to the caller — the allow-list lives
// with the executor that has to survive it.
//
// A file that is not valid YAML yields exactly one problem: nothing else about
// it is knowable.
func Validate(repoDir, filename string) (Spec, []string) {
	spec, found, err := LoadSpec(repoDir, filename)
	if err != nil {
		return Spec{}, []string{strings.TrimPrefix(err.Error(), "pipeline: ")}
	}
	if !found {
		return Spec{}, nil
	}
	var problems []string
	if spec.Timeout != "" {
		if _, err := ParseDuration(spec.Timeout); err != nil {
			problems = append(problems, fmt.Sprintf("%s: timeout: %v", pipelineFileName(filename), err))
		}
	}
	return spec, problems
}
