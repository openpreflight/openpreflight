// SPDX-License-Identifier: Apache-2.0

// Package workspace prepares a per-job checkout and clones the exact SHA.
package workspace

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Workspace is one job's directory tree.
type Workspace struct {
	Root string // /workspace/<job-id>
	Repo string // <root>/repo, the checkout
}

// shaPattern guards the SHA before it reaches a git argument.
var shaPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// repoPattern guards owner/name before it becomes part of a URL.
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9-_.]+/[A-Za-z0-9-_.]+$`)

// Prepare creates a clean directory tree for a job.
func Prepare(base, jobID string) (*Workspace, error) {
	root := filepath.Join(base, jobID)
	if err := os.RemoveAll(root); err != nil {
		return nil, fmt.Errorf("workspace: clear %s: %w", root, err)
	}
	for _, d := range []string{root, filepath.Join(root, ".tmp"), filepath.Join(root, ".npm")} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("workspace: mkdir %s: %w", d, err)
		}
	}
	return &Workspace{Root: root, Repo: filepath.Join(root, "repo")}, nil
}

// ErrTooLarge means the checkout plus whatever the build wrote exceeded the
// operator's cap. Failing the job beats filling the server's disk, which takes
// the whole worker down rather than one build.
var ErrTooLarge = errors.New("workspace: over max_workspace_bytes")

// Usage returns the total apparent size of the tree in bytes. Symlinks are not
// followed, so a link into /nix or /usr counts as the link and not its target.
//
// This walks rather than accounting per write: a walk between steps is cheap
// next to a compile, and an accountant would have to sit in every writer the
// pipeline might use, which is not possible for arbitrary shell commands.
func (w *Workspace) Usage() (int64, error) {
	if w == nil || w.Root == "" {
		return 0, nil
	}
	var total int64
	err := filepath.WalkDir(w.Root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// A build can delete a file mid-walk; that is not a measurement
			// failure worth failing the job over.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("workspace: measure: %w", err)
	}
	return total, nil
}

// CheckSize reports ErrTooLarge when the tree is over limit. A limit of zero or
// less disables the check, for an operator who would rather fill the disk than
// fail a build.
func (w *Workspace) CheckSize(limit int64) error {
	if limit <= 0 {
		return nil
	}
	used, err := w.Usage()
	if err != nil {
		// Do not fail a build because the measurement failed.
		return nil
	}
	if used > limit {
		return fmt.Errorf("%w: %s used, limit %s",
			ErrTooLarge, humanBytes(used), humanBytes(limit))
	}
	return nil
}

// humanBytes formats a byte count the way an operator would read it back in the
// settings field that produced the limit.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Cleanup removes the whole tree.
func (w *Workspace) Cleanup() error {
	if w == nil || w.Root == "" {
		return nil
	}
	if err := os.RemoveAll(w.Root); err != nil {
		return fmt.Errorf("workspace: cleanup: %w", err)
	}
	return nil
}

// CloneOptions describes one checkout.
type CloneOptions struct {
	// Repo is owner/name.
	Repo string
	// SHA is the exact commit the Check Run is for.
	SHA string
	// Token is an installation access token. It is passed to git through the
	// environment and never written to disk or to a command line.
	Token string
	// BaseURL is the git origin root, e.g. https://github.com, or the
	// GitHub Enterprise host. Empty means github.com.
	BaseURL string
	// PullNumber, when set, is used as a fetch fallback for fork PRs
	// (`refs/pull/N/head` on the base repository).
	PullNumber int
}

// Clone fetches exactly one commit and detaches onto it, then removes the
// remote so no pipeline step can push or re-fetch with our credentials.
func (w *Workspace) Clone(ctx context.Context, opts CloneOptions, out io.Writer) error {
	if !repoPattern.MatchString(opts.Repo) {
		return fmt.Errorf("workspace: refusing to clone %q: expected owner/name", opts.Repo)
	}
	if !shaPattern.MatchString(opts.SHA) {
		return fmt.Errorf("workspace: refusing to check out %q: expected a commit sha", opts.SHA)
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://github.com"
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return fmt.Errorf("workspace: git base URL %q must start with http:// or https://", base)
	}
	if err := os.MkdirAll(w.Repo, 0o750); err != nil {
		return fmt.Errorf("workspace: mkdir repo: %w", err)
	}
	remote := fmt.Sprintf("%s/%s.git", base, opts.Repo)

	if err := w.git(ctx, opts.Token, base, []string{"init", "--quiet"}, out); err != nil {
		return err
	}
	if err := w.git(ctx, opts.Token, base, []string{"remote", "add", "origin", remote}, out); err != nil {
		return err
	}
	fetchSpec := opts.SHA
	if err := w.git(ctx, opts.Token, base, []string{"fetch", "--no-tags", "--depth", "1", "origin", fetchSpec}, out); err != nil {
		if opts.PullNumber <= 0 {
			return err
		}
		ref := fmt.Sprintf("refs/pull/%d/head", opts.PullNumber)
		if err2 := w.git(ctx, opts.Token, base, []string{"fetch", "--no-tags", "--depth", "1", "origin", ref}, out); err2 != nil {
			return err
		}
	}
	if err := w.git(ctx, opts.Token, base, []string{"checkout", "--detach", "--quiet", "FETCH_HEAD"}, out); err != nil {
		return err
	}
	return w.git(ctx, opts.Token, base, []string{"remote", "remove", "origin"}, out)
}

// git runs one git command in the checkout with the auth header supplied
// through GIT_CONFIG_* environment variables.
//
// The token must not go in the remote URL (it would land in .git/config and in
// `git remote -v`) and must not go in argv (visible in `ps`). GIT_CONFIG_COUNT
// keeps it in the process environment only, which is what actions/checkout
// achieves with a temporary config entry.
//
// GitHub's git smart-HTTP endpoint wants Basic x-access-token:TOKEN, not the
// REST API's Bearer form (AUDIT.md).
func (w *Workspace) git(ctx context.Context, token, base string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", w.Repo}, args...)...)
	env := []string{
		"PATH=" + envOr("PATH", "/usr/local/bin:/usr/bin:/bin"),
		"HOME=" + w.Root,
		// Never let git stop for credentials: fail the step instead of hanging
		// until the job timeout.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/echo",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}
	if token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			// The config key is scoped to this origin so the header is not sent
			// anywhere else the pipeline might reach.
			fmt.Sprintf("GIT_CONFIG_KEY_0=http.%s/.extraheader", base),
			"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic "+basic,
		)
	}
	cmd.Env = env
	// git writes progress to stderr; both streams belong in the job log.
	cmd.Stdout = out
	cmd.Stderr = &redactWriter{w: out, secret: token}
	start := time.Now()
	if out != nil {
		fmt.Fprintf(out, "$ git %s\n", strings.Join(args, " "))
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workspace: git %s failed after %s: %w", args[0], time.Since(start).Round(time.Millisecond), err)
	}
	return nil
}

// redactWriter blanks the installation token if git ever echoes it (a bad URL in
// an error message, for instance) so it cannot reach a shareable log page.
type redactWriter struct {
	w      io.Writer
	secret string
}

func (r *redactWriter) Write(p []byte) (int, error) {
	if r.w == nil {
		return len(p), nil
	}
	if r.secret == "" || !strings.Contains(string(p), r.secret) {
		n, err := r.w.Write(p)
		if err != nil {
			return n, err
		}
		return len(p), nil
	}
	cleaned := strings.ReplaceAll(string(p), r.secret, "***")
	if _, err := r.w.Write([]byte(cleaned)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
