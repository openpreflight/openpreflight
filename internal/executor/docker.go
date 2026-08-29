// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Docker runs steps with `docker run --rm` against Host (DOCKER_HOST).
// The job container never receives the engine socket.
type Docker struct {
	Host  string
	Image string
	// Bin is the docker CLI. Empty means "docker". Tests inject a fake.
	Bin string
}

func (d Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

// Available reports whether the docker CLI can reach an engine.
func (d Docker) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin(), "info")
	cmd.Env = d.cmdEnv()
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func (d Docker) cmdEnv() []string {
	env := os.Environ()
	if d.Host != "" {
		env = append(env, "DOCKER_HOST="+d.Host)
	}
	return env
}

// Run executes one step inside Image. The checkout is mounted at /work.
func (d Docker) Run(ctx context.Context, step Step, out io.Writer) Result {
	res := Result{Name: step.Name, Command: step.Command}
	start := time.Now()
	if err := ValidImage(d.Image); err != nil {
		res.Err = err.Error()
		res.ExitCode = -1
		res.Duration = time.Since(start)
		return res
	}
	dir, err := filepath.Abs(step.Dir)
	if err != nil {
		res.Err = err.Error()
		res.ExitCode = -1
		res.Duration = time.Since(start)
		return res
	}
	// The engine sees host paths. hostPath rewrites this process's checkout
	// directory when we are a container using the host docker.sock.

	args := []string{
		"run", "--rm",
		"--network", "bridge",
		"--workdir", "/work",
		"--volume", hostPath(dir) + ":/work",
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--security-opt", "no-new-privileges",
		"--cap-drop", "ALL",
		"--env", "HOME=/work",
		"--env", "CI=true",
		"--env", "CONTINUOUS_INTEGRATION=true",
	}
	for _, kv := range step.Env {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			continue
		}
		switch key {
		case "PATH", "HOME", "TMPDIR", "npm_config_cache":
			continue
		}
		args = append(args, "--env", key+"="+val)
	}
	args = append(args, d.Image, "/bin/sh", "-c", step.Command)

	cmd := exec.CommandContext(ctx, d.bin(), args...)
	cmd.Env = d.cmdEnv()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil

	err = cmd.Run()
	res.Duration = time.Since(start)
	res.ExitCode = exitCode(err)
	if ctx.Err() != nil {
		res.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		if res.TimedOut {
			res.Err = "timed out"
		} else {
			res.Err = "cancelled"
		}
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		return res
	}
	if err != nil && res.ExitCode == -1 {
		res.Err = err.Error()
	}
	return res
}

// Ping is Available with a background context, for callers that have none.
func (d Docker) Ping() bool {
	return d.Available(context.Background())
}
