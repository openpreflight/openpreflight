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
	"syscall"
	"time"
)

// Docker runs steps with `docker run --rm` against Host (DOCKER_HOST).
// The job container never receives the engine socket.
type Docker struct {
	Host  string
	Image string
	// Bin is the docker CLI. Empty means "docker". Tests inject a fake.
	Bin string
	// ProbeTimeout bounds Available. Empty means defaultProbeTimeout.
	ProbeTimeout time.Duration
}

// defaultProbeTimeout bounds `docker info`. It is deliberately generous: the
// probe's answer decides whether a job fails with "the engine is not
// reachable", so a false negative fails a build that would have worked. A busy
// engine — or a busy host, which is what a CI box is — can take seconds to
// answer, and this was originally 3s, which timed out against a *fake* docker
// under load.
const defaultProbeTimeout = 15 * time.Second

func (d Docker) probeTimeout() time.Duration {
	if d.ProbeTimeout > 0 {
		return d.ProbeTimeout
	}
	return defaultProbeTimeout
}

func (d Docker) bin() string {
	if d.Bin != "" {
		return d.Bin
	}
	return "docker"
}

// Available reports whether the docker CLI can reach an engine.
//
// Bounded the same way Run is, and for the same reason: exec.CommandContext
// kills the client but Wait still blocks on pipes a grandchild inherited, so a
// hung probe would ignore its own timeout. Stdout and Stderr are left nil so
// os/exec attaches /dev/null directly instead of creating those pipes.
func (d Docker) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, d.probeTimeout())
	defer cancel()
	cmd := exec.Command(d.bin(), "info")
	cmd.Env = d.cmdEnv()
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err == nil
	case <-ctx.Done():
		killGroup(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		return false
	}
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
	// The container id is written here so cancellation can remove the container
	// engine-side. Killing the `docker run` client does not stop the container
	// it started, so without this a cancelled job keeps building.
	cidDir, err := os.MkdirTemp("", "openpreflight-cid-")
	if err != nil {
		res.Err = err.Error()
		res.ExitCode = -1
		res.Duration = time.Since(start)
		return res
	}
	defer os.RemoveAll(cidDir)
	// docker refuses to start if the cidfile already exists, so name it inside
	// a directory we just made rather than creating the file.
	cidPath := filepath.Join(cidDir, "cid")
	args = append(args, "--cidfile", cidPath)

	args = append(args, d.Image, "/bin/sh", "-c", step.Command)

	// Deliberately not exec.CommandContext: it SIGKILLs the client only, and
	// Wait would still block on the stdout/stderr pipes that grandchildren
	// inherited. This mirrors Process.Run — own process group, explicit kill,
	// bounded wait — so the two executors cannot drift apart again.
	cmd := exec.Command(d.bin(), args...)
	cmd.Env = d.cmdEnv()
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.Stdin = nil
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
		// A step can finish at the same moment the deadline passes; report the
		// deadline, since that is what the operator sees on the Check Run.
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
	case <-ctx.Done():
		// Remove the container first: the client is what holds the pipes open,
		// so killing it before the container is gone can orphan the container.
		d.removeContainer(cidPath)
		killGroup(cmd)
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

// removeContainer force-removes the container named by the cidfile. Best effort:
// the file may not exist yet if cancellation beat `docker run` to writing it, in
// which case no container was started either.
func (d Docker) removeContainer(cidPath string) {
	cid, err := os.ReadFile(cidPath)
	if err != nil {
		return
	}
	id := strings.TrimSpace(string(cid))
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rm := exec.CommandContext(ctx, d.bin(), "rm", "-f", id)
	rm.Env = d.cmdEnv()
	rm.Stdout = io.Discard
	rm.Stderr = io.Discard
	_ = rm.Run()
}

// Ping is Available with a background context, for callers that have none.
func (d Docker) Ping() bool {
	return d.Available(context.Background())
}
