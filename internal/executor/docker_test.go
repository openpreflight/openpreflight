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
	if ! (Docker{Bin: bin, Image: "node:24"}).Available(context.Background()) {
		t.Fatal("expected available")
	}
	bin = writeFakeDocker(t, `exit 1`)
	if (Docker{Bin: bin, Image: "node:24"}).Available(context.Background()) {
		t.Fatal("expected unavailable")
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

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
