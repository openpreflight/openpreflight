// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const coolifyWorkspaceMountinfo = `
741 387 0:59 / / rw,relatime - overlay overlay rw,lowerdir=/var/lib/containerd/x
750 741 8:1 /var/lib/docker/volumes/openpreflight_ci-data/_data /data rw,relatime master:1 - ext4 /dev/sda1 rw
751 741 8:1 /var/lib/docker/volumes/openpreflight_ci-workspace/_data /workspace rw,relatime master:1 - ext4 /dev/sda1 rw
`

func TestHostPathRewritesWorkspaceVolume(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(f, []byte(coolifyWorkspaceMountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountinfoFile
	mountinfoFile = f
	t.Cleanup(func() { mountinfoFile = old })

	got := hostPath("/workspace/job-1/repo")
	want := "/var/lib/docker/volumes/openpreflight_ci-workspace/_data/job-1/repo"
	if got != want {
		t.Fatalf("hostPath = %q, want %q", got, want)
	}
}

func TestHostPathPrefersLongestMount(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mountinfo")
	body := `
1 0 8:1 / / rw - ext4 /dev/sda1 rw
2 1 8:1 /var/lib/docker/volumes/ws/_data /workspace rw - ext4 /dev/sda1 rw
`
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountinfoFile
	mountinfoFile = f
	t.Cleanup(func() { mountinfoFile = old })

	got := hostPath("/workspace/abc")
	if got != "/var/lib/docker/volumes/ws/_data/abc" {
		t.Fatalf("got %q", got)
	}
}

func TestHostPathCIWorkspaceHostOverridesMountinfo(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(f, []byte(coolifyWorkspaceMountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountinfoFile
	mountinfoFile = f
	t.Cleanup(func() { mountinfoFile = old })
	t.Setenv("CI_WORKSPACE_HOST", "/host/ws")
	t.Setenv("WORKSPACE_DIR", "/workspace")

	got := hostPath("/workspace/job/repo")
	if got != "/host/ws/job/repo" {
		t.Fatalf("got %q", got)
	}
}

func TestUnescapeMountOctal(t *testing.T) {
	got := unescapeMount(`/path\040with\040spaces`)
	if got != "/path with spaces" {
		t.Fatalf("got %q", got)
	}
}

func TestParseMountinfoLine(t *testing.T) {
	line := "751 741 8:1 /var/lib/docker/volumes/openpreflight_ci-workspace/_data /workspace rw,relatime master:1 - ext4 /dev/sda1 rw"
	m, ok := parseMountinfoLine(line)
	if !ok {
		t.Fatal("expected parse")
	}
	if m.root != "/var/lib/docker/volumes/openpreflight_ci-workspace/_data" {
		t.Fatalf("root %q", m.root)
	}
	if m.point != "/workspace" {
		t.Fatalf("point %q", m.point)
	}
}

func TestDockerRunVolumeUsesHostPath(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(f, []byte(coolifyWorkspaceMountinfo), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountinfoFile
	mountinfoFile = f
	t.Cleanup(func() { mountinfoFile = old })

	bin := writeFakeDocker(t, `
vol=""
while [ $# -gt 0 ]; do
  if [ "$1" = "--volume" ]; then
    vol=$2
  fi
  if [ "$1" = "-c" ]; then
    shift
    printf '%s\n' "$vol"
    eval "$1"
    exit $?
  fi
  shift
done
exit 1
`)
	dir := "/workspace/job-9/repo"
	var out strings.Builder
	res := Docker{Bin: bin, Image: "node:24"}.Run(context.Background(), Step{
		Name: "test", Command: "true", Dir: dir,
	}, &out)
	if !res.OK() {
		t.Fatalf("result: %+v out=%q", res, out.String())
	}
	if !strings.Contains(out.String(), "/var/lib/docker/volumes/openpreflight_ci-workspace/_data/job-9/repo:/work") {
		t.Fatalf("volume arg missing from output: %q", out.String())
	}
}
