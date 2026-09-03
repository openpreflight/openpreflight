// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// inference is what a detector produced: at most three steps, the source label
// that goes on them, and anything the operator should know about the guess.
type inference struct {
	steps    []Step
	source   string
	warnings []string
}

// detector recognises one kind of project. Order in detectors decides
// precedence, and Node is first so that no repository which works today changes
// its plan.
//
// A detector yields at most install/test/build — the same three steps a
// pipeline file can set. A detector that wants a fourth is a request for a
// pipeline DSL, which this project does not have and is not adding.
type detector struct {
	// language names the ecosystem for warnings and messages.
	language string
	// marker is the file whose presence identifies the project. It is also what
	// the source label cites, so an operator can go and look at it.
	marker string
	// alsoMarkedBy are additional filenames that identify the same ecosystem.
	alsoMarkedBy []string
	// steps builds the plan for a matched project.
	steps func(repoDir string) ([]Step, error)
}

func (d detector) matches(repoDir string) (string, bool) {
	for _, name := range append([]string{d.marker}, d.alsoMarkedBy...) {
		if exists(repoDir, name) {
			return name, true
		}
	}
	return "", false
}

// detectors is the ordered list. Keep Node first: see detector.
var detectors = []detector{
	{language: "Node", marker: "package.json", steps: nodeSteps},
	{language: "Go", marker: "go.mod", steps: goSteps},
	{language: "Rust", marker: "Cargo.toml", steps: rustSteps},
	{language: "Python", marker: "pyproject.toml", alsoMarkedBy: []string{"requirements.txt", "setup.py"}, steps: pythonSteps},
}

// infer runs the detectors in order and returns the first match. When more than
// one matches, the first still wins — but silently picking one of two plausible
// plans is the failure mode this whole wave exists to remove, so it warns.
func infer(repoDir string) (inference, error) {
	var matched []detector
	var markers []string
	for _, d := range detectors {
		if name, ok := d.matches(repoDir); ok {
			matched = append(matched, d)
			markers = append(markers, name)
		}
	}
	if len(matched) == 0 {
		return inference{}, nil
	}

	winner := matched[0]
	steps, err := winner.steps(repoDir)
	if err != nil {
		return inference{}, err
	}
	out := inference{
		steps:  steps,
		source: fmt.Sprintf("%s defaults from %s", winner.language, markers[0]),
	}
	if len(matched) > 1 {
		var others []string
		for i, d := range matched[1:] {
			others = append(others, fmt.Sprintf("%s (%s)", d.language, markers[i+1]))
		}
		out.warnings = append(out.warnings, fmt.Sprintf(
			"This repository also looks like %s. The %s defaults were used because they come first; "+
				"set install/test/build in the pipeline file or on the binding to choose explicitly.",
			strings.Join(others, " and "), winner.language))
	}
	if len(steps) == 0 {
		out.warnings = append(out.warnings, fmt.Sprintf(
			"%s was detected from %s but no runnable step could be inferred.",
			winner.language, markers[0]))
	}
	return out, nil
}

// packageJSON is the part of package.json we read.
type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
}

// nodeSteps infers steps from package.json. A missing test or build script is
// simply not a step — missing scripts are skipped, not failed.
func nodeSteps(repoDir string) ([]Step, error) {
	raw, err := os.ReadFile(filepath.Join(repoDir, "package.json"))
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
	switch {
	case strings.HasPrefix(declared, "pnpm") || exists(repoDir, "pnpm-lock.yaml"):
		return "pnpm install --frozen-lockfile", "pnpm"
	case strings.HasPrefix(declared, "yarn") || exists(repoDir, "yarn.lock"):
		return "yarn install --immutable", "yarn"
	case exists(repoDir, "package-lock.json"), exists(repoDir, "npm-shrinkwrap.json"):
		return "npm ci", "npm"
	default:
		// No lockfile: npm ci would fail outright, so install is the honest
		// choice even though it is not reproducible.
		return "npm install --no-audit --no-fund", "npm"
	}
}

// goSteps needs no evidence of tests: `go test ./...` on a module with no test
// files prints "[no test files]" and exits 0, so the step is safe to emit.
func goSteps(string) ([]Step, error) {
	return []Step{
		{Name: "install", Command: "go mod download"},
		{Name: "test", Command: "go test ./..."},
		{Name: "build", Command: "go build ./..."},
	}, nil
}

// rustSteps mirrors goSteps: cargo also passes a crate with no tests.
//
// `cargo fetch --locked` rather than plain fetch, so a stale Cargo.lock is a
// clear failure instead of a silently different dependency set. A workspace
// with no lockfile fails here and wants an explicit pipeline file.
func rustSteps(repoDir string) ([]Step, error) {
	install := "cargo fetch --locked"
	if !exists(repoDir, "Cargo.lock") {
		install = "cargo fetch"
	}
	return []Step{
		{Name: "install", Command: install},
		{Name: "test", Command: "cargo test"},
		{Name: "build", Command: "cargo build --release"},
	}, nil
}

// pythonSteps is the one asymmetric detector, and deliberately so: pytest exits
// 5 on "no tests collected", which would fail a check for a repository that
// simply has no tests. Go and Rust exit 0 in that case; Python does not, so the
// test step is emitted only when something in the checkout says tests exist.
//
// There is no build step. Python has no single correct one, and running
// `python -m build` against a repository that is not a package would fail a
// check for no reason.
func pythonSteps(repoDir string) ([]Step, error) {
	var install string
	switch {
	case exists(repoDir, "uv.lock"):
		install = "uv sync --frozen"
	case exists(repoDir, "poetry.lock"):
		install = "poetry install --no-interaction"
	case exists(repoDir, "requirements.txt"):
		install = "pip install -r requirements.txt"
	default:
		install = "pip install ."
	}
	steps := []Step{{Name: "install", Command: install}}
	if hasPythonTests(repoDir) {
		steps = append(steps, Step{Name: "test", Command: "pytest"})
	}
	return steps, nil
}

// hasPythonTests looks for the evidence pythonSteps requires: a tests
// directory, a top-level test file, or a pytest configuration table.
func hasPythonTests(repoDir string) bool {
	for _, dir := range []string{"tests", "test"} {
		if info, err := os.Stat(filepath.Join(repoDir, dir)); err == nil && info.IsDir() {
			return true
		}
	}
	if names, err := filepath.Glob(filepath.Join(repoDir, "test_*.py")); err == nil && len(names) > 0 {
		return true
	}
	if names, err := filepath.Glob(filepath.Join(repoDir, "*_test.py")); err == nil && len(names) > 0 {
		return true
	}
	for _, cfg := range []string{"pytest.ini", "tox.ini", "setup.cfg", "pyproject.toml"} {
		raw, err := os.ReadFile(filepath.Join(repoDir, cfg))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "[tool.pytest") || strings.Contains(string(raw), "[pytest]") {
			return true
		}
	}
	return false
}

func exists(repoDir, name string) bool {
	_, err := os.Stat(filepath.Join(repoDir, name))
	return err == nil
}
