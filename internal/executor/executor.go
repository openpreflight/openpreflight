// Package executor runs pipeline steps as local processes or as `docker run`.
package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Step is one shell command to run in the checkout.
type Step struct {
	Name    string
	Command string
	Dir     string
	// Env is the complete environment for the step. Callers build it from
	// scratch; nothing from the server process leaks in except what BaseEnv
	// deliberately keeps.
	Env []string
}

// Result is the outcome of one step.
type Result struct {
	Name     string        `json:"name"`
	Command  string        `json:"command,omitempty"`
	Skipped  bool          `json:"skipped,omitempty"`
	ExitCode int           `json:"exit_code"`
	Duration time.Duration `json:"duration_ns"`
	Err      string        `json:"error,omitempty"`
	TimedOut bool          `json:"timed_out,omitempty"`
}

// OK reports whether the step passed (or was skipped, which is not a failure).
func (r Result) OK() bool { return r.Skipped || (r.ExitCode == 0 && r.Err == "") }

// Executor runs steps. Process is the default; Docker is used when the plan
// names a runtime image or the job is a fork PR.
type Executor interface {
	Run(ctx context.Context, step Step, out io.Writer) Result
}

// Process runs steps as child processes of this server.
type Process struct{}

// Run executes one step, killing the whole process group on timeout or cancel so
// a detached `npm test` child cannot outlive the job.
func (Process) Run(ctx context.Context, step Step, out io.Writer) Result {
	res := Result{Name: step.Name, Command: step.Command}
	start := time.Now()

	cmd := exec.Command("/bin/sh", "-c", step.Command)
	cmd.Dir = step.Dir
	cmd.Env = step.Env
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
	// Own process group: killing the negated pid takes the shell and everything
	// it spawned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		res.Err = err.Error()
		res.ExitCode = -1
		res.Duration = time.Since(start)
		return res
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		res.Duration = time.Since(start)
		res.ExitCode = exitCode(err)
		if err != nil && res.ExitCode == -1 {
			res.Err = err.Error()
		}
		return res
	case <-ctx.Done():
		killGroup(cmd)
		// Give the tree a moment to die before we stop waiting on it.
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		res.Duration = time.Since(start)
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		res.ExitCode = -1
		if res.TimedOut {
			res.Err = "timed out"
		} else {
			res.Err = "cancelled"
		}
		return res
	}
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	syscall.Kill(pgid, syscall.SIGTERM)
	time.Sleep(2 * time.Second)
	syscall.Kill(pgid, syscall.SIGKILL)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// BaseEnv builds a job environment from scratch. Secrets held by this process
// (CI_SECRET_KEY, PEMs, webhook secrets, Coolify tokens, installation tokens)
// are never passed to a step, so only an explicit allow-list of host variables
// survives.
func BaseEnv(workdir string, extra map[string]string) []string {
	keep := map[string]bool{"PATH": true, "LANG": true, "LC_ALL": true, "TZ": true, "SSL_CERT_FILE": true, "SSL_CERT_DIR": true}
	env := map[string]string{
		"HOME":                   workdir,
		"TMPDIR":                 workdir + "/.tmp",
		"CI":                     "true",
		"CONTINUOUS_INTEGRATION": "true",
		// npm writes here; without it npm falls back to the server's HOME.
		"npm_config_cache": workdir + "/.npm",
		"PATH":             "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && keep[k] {
			env[k] = v
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}
