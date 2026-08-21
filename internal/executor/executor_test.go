package executor

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

type buffer struct{ b strings.Builder }

func (w *buffer) Write(p []byte) (int, error) { return w.b.Write(p) }

func TestRunCapturesBothStreams(t *testing.T) {
	var out buffer
	res := Process{}.Run(context.Background(), Step{
		Name: "test", Command: "echo to-stdout; echo to-stderr 1>&2", Dir: t.TempDir(),
		Env: BaseEnv(t.TempDir(), nil),
	}, &out)
	if !res.OK() {
		t.Fatalf("result: %+v", res)
	}
	body := out.b.String()
	if !strings.Contains(body, "to-stdout") || !strings.Contains(body, "to-stderr") {
		t.Fatalf("output: %q", body)
	}
}

func TestRunReportsExitCode(t *testing.T) {
	res := Process{}.Run(context.Background(), Step{
		Name: "test", Command: "exit 7", Dir: t.TempDir(), Env: BaseEnv(t.TempDir(), nil),
	}, &buffer{})
	if res.OK() || res.ExitCode != 7 {
		t.Fatalf("result: %+v", res)
	}
}

func TestRunKillsProcessTreeOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	res := Process{}.Run(ctx, Step{
		Name: "test", Command: "sleep 30 & sleep 30", Dir: t.TempDir(), Env: BaseEnv(t.TempDir(), nil),
	}, &buffer{})
	if !res.TimedOut {
		t.Fatalf("expected a timeout: %+v", res)
	}
	// The kill must not wait for the child to finish on its own.
	if time.Since(start) > 15*time.Second {
		t.Fatalf("timeout took %s", time.Since(start))
	}
}

func TestRunReportsCancellationSeparatelyFromTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	res := Process{}.Run(ctx, Step{
		Name: "test", Command: "sleep 30", Dir: t.TempDir(), Env: BaseEnv(t.TempDir(), nil),
	}, &buffer{})
	if res.TimedOut {
		t.Fatal("a cancelled job is not a timeout")
	}
	if res.Err != "cancelled" {
		t.Fatalf("result: %+v", res)
	}
}

func TestBaseEnvExcludesProcessSecrets(t *testing.T) {
	// Anything sensitive this process holds must not reach a build step.
	t.Setenv("CI_SECRET_KEY", "the-master-key")
	t.Setenv("CI_BOOTSTRAP_ADMIN_PASSWORD", "admin-pw")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")

	env := BaseEnv("/workspace/job-1", map[string]string{"GITHUB_SHA": "abc"})
	joined := strings.Join(env, "\n")
	for _, forbidden := range []string{"the-master-key", "admin-pw", "aws-secret",
		"CI_SECRET_KEY", "CI_BOOTSTRAP_ADMIN_PASSWORD", "AWS_SECRET_ACCESS_KEY"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("job env leaks %q:\n%s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "GITHUB_SHA=abc") {
		t.Error("explicit values should be present")
	}
	if !strings.Contains(joined, "CI=true") {
		t.Error("CI=true should be set")
	}
	if !strings.Contains(joined, "HOME=/workspace/job-1") {
		t.Error("HOME should point at the job workspace, not the server's")
	}
	if !strings.Contains(joined, "PATH=") {
		t.Error("PATH is required for anything to run")
	}
}

func TestBaseEnvKeepsCertificateLocations(t *testing.T) {
	// TLS trust has to survive, or every https request in a build fails.
	t.Setenv("SSL_CERT_FILE", "/etc/ssl/cert.pem")
	env := strings.Join(BaseEnv(t.TempDir(), nil), "\n")
	if !strings.Contains(env, "SSL_CERT_FILE=/etc/ssl/cert.pem") {
		t.Fatalf("env: %s", env)
	}
}

func TestStepRunsInItsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/marker", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out buffer
	res := Process{}.Run(context.Background(), Step{
		Name: "test", Command: "ls", Dir: dir, Env: BaseEnv(dir, nil),
	}, &out)
	if !res.OK() || !strings.Contains(out.b.String(), "marker") {
		t.Fatalf("res=%+v out=%q", res, out.b.String())
	}
}
