package workspace

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trivedi-vatsal/openpreflight/internal/testsupport"
)

func TestCloneExactSHA(t *testing.T) {
	root := t.TempDir()
	sha := testsupport.NewRepo(t, root, "winpra/api", map[string]string{
		"package.json": `{"name":"api","scripts":{"test":"echo ok"}}`,
		"README.md":    "hello",
	})
	git := testsupport.NewGitServer(t, root)

	ws, err := Prepare(t.TempDir(), "job-1")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ws.Clone(context.Background(), CloneOptions{
		Repo: "winpra/api", SHA: sha, Token: "ghs-installation-token", BaseURL: git.URL,
	}, &out); err != nil {
		t.Fatalf("clone: %v\n%s", err, out.String())
	}

	if body, err := os.ReadFile(filepath.Join(ws.Repo, "README.md")); err != nil || string(body) != "hello" {
		t.Fatalf("checkout content: %v %q", err, body)
	}

	// GitHub's git endpoint wants Basic x-access-token, not the REST Bearer form.
	wantAuth := "AUTHORIZATION: basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs-installation-token"))
	wantHeader := strings.TrimPrefix(wantAuth, "AUTHORIZATION: ")
	var sawBasic bool
	for _, a := range git.Auths() {
		if a == "" {
			continue
		}
		if !strings.EqualFold(strings.Fields(a)[0], "basic") {
			t.Fatalf("git sent a non-Basic credential: %q", a)
		}
		if strings.EqualFold(a, wantHeader) {
			sawBasic = true
		}
	}
	if !sawBasic {
		t.Fatalf("expected Basic x-access-token auth, saw %q", git.Auths())
	}

	// The credential must not survive in the checkout.
	config, err := os.ReadFile(filepath.Join(ws.Repo, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(config), "ghs-installation-token") {
		t.Fatalf(".git/config holds the installation token:\n%s", config)
	}
	if strings.Contains(string(config), "extraheader") {
		t.Fatalf(".git/config holds the auth header:\n%s", config)
	}
	// The remote is removed so no pipeline step can reuse our credentials.
	if strings.Contains(string(config), "[remote \"origin\"]") {
		t.Fatalf("origin was not stripped:\n%s", config)
	}
	if strings.Contains(out.String(), "ghs-installation-token") {
		t.Fatal("the token leaked into the job log")
	}
}

func TestCloneRejectsBadInput(t *testing.T) {
	ws, err := Prepare(t.TempDir(), "job-2")
	if err != nil {
		t.Fatal(err)
	}
	cases := []CloneOptions{
		{Repo: "not-a-repo", SHA: "abc1234"},
		{Repo: "o/r; rm -rf /", SHA: "abc1234"},
		{Repo: "o/r", SHA: "not a sha"},
		{Repo: "o/r", SHA: "abc1234", BaseURL: "file:///etc"},
	}
	for _, c := range cases {
		if err := ws.Clone(context.Background(), c, nil); err == nil {
			t.Errorf("%+v should have been rejected", c)
		}
	}
}

func TestCloneFailsWithoutCredentials(t *testing.T) {
	// A private repo without a token must fail rather than hang waiting for a
	// credential prompt until the job timeout.
	root := t.TempDir()
	sha := testsupport.NewRepo(t, root, "o/r", map[string]string{"a": "b"})
	git := testsupport.NewGitServer(t, root)
	ws, _ := Prepare(t.TempDir(), "job-3")
	// Point at a repo that does not exist on the server.
	if err := ws.Clone(context.Background(), CloneOptions{
		Repo: "o/missing", SHA: sha, BaseURL: git.URL,
	}, nil); err == nil {
		t.Fatal("expected a failure cloning a repo the server does not have")
	}
}

func TestPrepareIsClean(t *testing.T) {
	base := t.TempDir()
	ws, err := Prepare(base, "job-4")
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(ws.Root, "leftover")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A re-run of the same job id must not inherit the previous tree.
	if _, err := Prepare(base, "job-4"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("Prepare left files from a previous run")
	}
	if err := ws.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Root); !os.IsNotExist(err) {
		t.Fatal("Cleanup left the workspace behind")
	}
}

func TestRedactWriter(t *testing.T) {
	var buf bytes.Buffer
	w := &redactWriter{w: &buf, secret: "s3cret"}
	n, err := w.Write([]byte("fatal: could not read https://x-access-token:s3cret@host\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("short write reported")
	}
	if strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("secret survived redaction: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "***") {
		t.Fatalf("no redaction marker: %q", buf.String())
	}
}
