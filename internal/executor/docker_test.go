// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFakeDocker(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "docker")
	body := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestValidImage(t *testing.T) {
	ok := []string{"node:24", "node:24-alpine", "ghcr.io/org/app:1.2", "repo@sha256:" + strings.Repeat("a", 64)}
	for _, img := range ok {
		if err := ValidImage(img); err != nil {
			t.Errorf("%s: %v", img, err)
		}
	}
	bad := []string{"", "node:24; rm -rf /", "--privileged", "node:24 $(id)", "node 24", "-node:24"}
	for _, img := range bad {
		if err := ValidImage(img); err == nil {
			t.Errorf("%s: expected rejection", img)
		}
	}
}

func TestDockerAvailableUsesCLI(t *testing.T) {
	bin := writeFakeDocker(t, `if [ "$1" = "info" ]; then exit 0; fi; exit 1`)
	if !(Docker{Bin: bin, Image: "node:24"}).Available(context.Background()) {
		t.Fatal("expected available")
	}
	bin = writeFakeDocker(t, `exit 1`)
	if (Docker{Bin: bin, Image: "node:24"}).Available(context.Background()) {
		t.Fatal("expected unavailable")
	}
}

// TestDockerAvailableTimesOutRatherThanHanging keeps the probe bounded. The
// budget has to stay generous — a probe that times out reports the engine as
// unreachable and fails a job that would have worked — so this asserts the
// bound exists without asserting a specific duration.
func TestDockerAvailableTimesOutRatherThanHanging(t *testing.T) {
	bin := writeFakeDocker(t, `sleep 30`)
	start := time.Now()
	if (Docker{Bin: bin, Image: "node:24", ProbeTimeout: 300 * time.Millisecond}).
		Available(context.Background()) {
		t.Fatal("a probe that never answers is not available")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe took %s: it is not bounded", elapsed)
	}
}

func TestDockerRunInvokesCLIAndCapturesOutput(t *testing.T) {
	bin := writeFakeDocker(t, `
# skip until -c, then eval the command (stand-in for the container)
while [ $# -gt 0 ]; do
  if [ "$1" = "-c" ]; then
    shift
    eval "$1"
    exit $?
  fi
  shift
done
exit 1
`)
	dir := t.TempDir()
	var out strings.Builder
	res := Docker{Bin: bin, Image: "node:24"}.Run(context.Background(), Step{
		Name: "test", Command: "echo hello-docker", Dir: dir, Env: []string{"GITHUB_SHA=abc"},
	}, &out)
	if !res.OK() {
		t.Fatalf("result: %+v out=%q", res, out.String())
	}
	if !strings.Contains(out.String(), "hello-docker") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestDockerRunRejectsBadImageWithoutCallingCLI(t *testing.T) {
	bin := writeFakeDocker(t, `echo called >> "$0.called"; exit 0`)
	res := Docker{Bin: bin, Image: "node:24; id"}.Run(context.Background(), Step{
		Name: "x", Command: "true", Dir: t.TempDir(),
	}, ioDiscard{})
	if res.OK() {
		t.Fatal("bad image must not run")
	}
	if _, err := os.Stat(bin + ".called"); err == nil {
		t.Fatal("docker CLI must not be invoked for a rejected image")
	}
}

func TestDockerRunHonoursCancel(t *testing.T) {
	bin := writeFakeDocker(t, `while [ "$1" != "-c" ]; do shift; done; shift; eval "$1"`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	res := Docker{Bin: bin, Image: "node:24"}.Run(ctx, Step{
		Name: "x", Command: "sleep 30", Dir: t.TempDir(),
	}, ioDiscard{})
	if !res.TimedOut {
		t.Fatalf("expected timeout: %+v", res)
	}
}

// waitForFile blocks until path exists, so a cancellation test can fire once the
// fake has actually started rather than racing process spawn. Spawning a shell
// from a temp dir can take several hundred milliseconds here, which is enough to
// make a fixed-delay cancel arrive before the child runs a single command.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared: the fake docker did not start", path)
}

// TestDockerRunReturnsPromptlyOnCancel is the test the bug needed. The old
// assertion above passes while Run sits for the full `sleep 30`, because
// res.TimedOut reads ctx.Err() and says nothing about whether anything died.
// Bound the wall clock, and cancel only once the child is confirmed running.
func TestDockerRunReturnsPromptlyOnCancel(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	bin := writeFakeDocker(t, `
while [ $# -gt 0 ]; do
  if [ "$1" = "-c" ]; then shift; touch "`+started+`"; eval "$1"; exit $?; fi
  shift
done
exit 1
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitForFile(t, started)
		cancel()
	}()

	start := time.Now()
	res := Docker{Bin: bin, Image: "node:24"}.Run(ctx, Step{
		Name: "x", Command: "sleep 30", Dir: t.TempDir(),
	}, ioDiscard{})
	elapsed := time.Since(start)

	if res.TimedOut {
		t.Fatalf("cancel is not a deadline: %+v", res)
	}
	if res.Err != "cancelled" {
		t.Fatalf("expected cancelled, got %+v", res)
	}
	// killGroup sleeps 2s between SIGTERM and SIGKILL, so ~2s is the floor.
	// Anything approaching `sleep 30` means the tree outlived cancellation.
	if elapsed > 10*time.Second {
		t.Fatalf("Run took %s: the process tree outlived cancellation", elapsed)
	}
}

// TestDockerRunRemovesContainerOnCancel proves the engine-side half. Killing the
// `docker run` client does not stop the container it started, so cancellation
// has to remove it explicitly or a cancelled job keeps building.
func TestDockerRunRemovesContainerOnCancel(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "rm.log")
	started := filepath.Join(dir, "started")
	// Two roles in one fake: `rm` records its arguments; `run` writes a cidfile
	// the way the real client does, then blocks.
	bin := writeFakeDocker(t, `
if [ "$1" = "rm" ]; then echo "$@" >> "`+log+`"; exit 0; fi
while [ $# -gt 0 ]; do
  if [ "$1" = "--cidfile" ]; then echo "deadbeefcafe" > "$2"; fi
  if [ "$1" = "-c" ]; then shift; touch "`+started+`"; eval "$1"; exit $?; fi
  shift
done
exit 1
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitForFile(t, started)
		cancel()
	}()

	Docker{Bin: bin, Image: "node:24"}.Run(ctx, Step{
		Name: "x", Command: "sleep 30", Dir: t.TempDir(),
	}, ioDiscard{})

	body, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("docker rm was never called: %v", err)
	}
	if !strings.Contains(string(body), "rm -f deadbeefcafe") {
		t.Fatalf("expected `rm -f deadbeefcafe`, got %q", string(body))
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
