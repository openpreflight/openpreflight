// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// mountinfoFile is the Linux mount table. Tests swap it for a fixture.
var mountinfoFile = "/proc/self/mountinfo"

type mount struct {
	root, point string
}

// hostPath maps a path as this process sees it to the path the Docker
// *engine* must receive in `docker run -v`.
//
// When this process is a container using the host's docker.sock, a volume
// source of `/workspace/job` is created on the host (empty). The kernel's
// mount table names the host directory behind `/workspace`.
//
// CI_WORKSPACE_HOST, if set, is that host directory for WORKSPACE_DIR and
// wins over the mount table (remote engines, odd volume drivers).
func hostPath(container string) string {
	container = filepath.Clean(container)
	if host := strings.TrimRight(os.Getenv("CI_WORKSPACE_HOST"), "/"); host != "" {
		ws := filepath.Clean(env("WORKSPACE_DIR", "/workspace"))
		if covers(ws, container) {
			rel, err := filepath.Rel(ws, container)
			if err == nil {
				return filepath.Join(host, rel)
			}
		}
	}
	mounts, err := readMountinfo(mountinfoFile)
	if err != nil || len(mounts) == 0 {
		return container
	}
	best := mount{}
	for _, m := range mounts {
		if !covers(m.point, container) {
			continue
		}
		if len(m.point) > len(best.point) {
			best = m
		}
	}
	if best.point == "" {
		return container
	}
	rel, err := filepath.Rel(best.point, container)
	if err != nil {
		return container
	}
	return filepath.Join(best.root, rel)
}

func covers(point, path string) bool {
	if point == "/" {
		return true
	}
	return path == point || strings.HasPrefix(path, point+string(os.PathSeparator))
}

func readMountinfo(path string) ([]mount, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []mount
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		m, ok := parseMountinfoLine(sc.Text())
		if ok {
			out = append(out, m)
		}
	}
	return out, sc.Err()
}

// parseMountinfoLine reads one /proc/self/mountinfo row.
// Fields: mount_id parent_id maj:min root mount_point options [optional...] - fstype source super
func parseMountinfoLine(line string) (mount, bool) {
	fields := strings.Fields(line)
	if len(fields) < 10 {
		return mount{}, false
	}
	dash := 6
	for dash < len(fields) && fields[dash] != "-" {
		dash++
	}
	if dash >= len(fields) {
		return mount{}, false
	}
	return mount{
		root:  unescapeMount(fields[3]),
		point: unescapeMount(fields[4]),
	}, true
}

// unescapeMount undoes the octal escapes mountinfo uses for space, tab, newline, backslash.
func unescapeMount(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			n, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
