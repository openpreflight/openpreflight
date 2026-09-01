// SPDX-License-Identifier: Apache-2.0

package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testKey generates a small RSA key: tests must not depend on a real App PEM.
func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	return key, string(encoded)
}

func TestParsePrivateKeyAcceptsBothEncodings(t *testing.T) {
	key, pkcs1 := testKey(t)
	if _, err := ParsePrivateKey(pkcs1); err != nil {
		t.Fatalf("PKCS#1: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := ParsePrivateKey(pkcs8); err != nil {
		t.Fatalf("PKCS#8: %v", err)
	}
	for _, bad := range []string{"", "not a key", "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----"} {
		if _, err := ParsePrivateKey(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestAppJWTIsVerifiableAndBackdated(t *testing.T) {
	key, _ := testKey(t)
	issued := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	token, err := AppJWT(4242, key, issued)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}
	// Verify the signature the way GitHub would.
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "4242" {
		t.Fatalf("iss must be the App id as a string, got %q", claims.Iss)
	}
	// GitHub rejects a future iat outright, so it must be backdated.
	if claims.Iat != issued.Add(-60*time.Second).Unix() {
		t.Fatalf("iat not backdated: %d", claims.Iat)
	}
	if claims.Exp-claims.Iat > 600 {
		t.Fatalf("JWT lifetime exceeds GitHub's 10 minute maximum: %ds", claims.Exp-claims.Iat)
	}
}

// fakeGitHub implements the endpoints we use.
type fakeGitHub struct {
	srv          *httptest.Server
	tokenCalls   int
	createdRuns  []map[string]any
	patchedRuns  []map[string]any
	lastAuthz    string
	installCount int
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{installCount: 2}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		f.lastAuthz = r.Header.Get("Authorization")
		if !strings.HasPrefix(f.lastAuthz, "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"id": 101, "account": map[string]any{"login": "acme", "type": "Organization"},
				"repository_selection": "selected"},
			{"id": 102, "account": map[string]any{"login": "vatsal", "type": "User"},
				"repository_selection": "all"},
		})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls++
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs-installation-token",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ghs-installation-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"repositories": []map[string]any{
				{"id": 1, "full_name": "acme/api", "name": "api", "private": true},
			},
		})
	})
	mux.HandleFunc("POST /repos/{owner}/{repo}/check-runs", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.createdRuns = append(f.createdRuns, body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 555, "name": body["name"], "status": body["status"]})
	})
	mux.HandleFunc("PATCH /repos/{owner}/{repo}/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.patchedRuns = append(f.patchedRuns, body)
		json.NewEncoder(w).Encode(map[string]any{"id": 555})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTestClient(t *testing.T, apiURL string) *Client {
	t.Helper()
	_, pemData := testKey(t)
	c, err := New(4242, pemData, apiURL)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInstallationsAndRepositories(t *testing.T) {
	f := newFakeGitHub(t)
	c := newTestClient(t, f.srv.URL)

	installs, err := c.Installations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(installs) != 2 || installs[0].Account.Login != "acme" {
		t.Fatalf("installations: %+v", installs)
	}
	repos, err := c.InstallationRepositories(context.Background(), 101)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/api" {
		t.Fatalf("repos: %+v", repos)
	}
}

func TestInstallationTokenIsCached(t *testing.T) {
	f := newFakeGitHub(t)
	c := newTestClient(t, f.srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.InstallationToken(context.Background(), 101); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokenCalls != 1 {
		t.Fatalf("expected one token mint, got %d", f.tokenCalls)
	}
	// A different installation needs its own token.
	if _, err := c.InstallationToken(context.Background(), 102); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("token cache is keyed wrongly: %d calls", f.tokenCalls)
	}
}

func TestExpiredTokenIsRefreshed(t *testing.T) {
	f := newFakeGitHub(t)
	c := newTestClient(t, f.srv.URL)
	if _, err := c.InstallationToken(context.Background(), 101); err != nil {
		t.Fatal(err)
	}
	// Jump past the cached token's expiry.
	c.nowFunc = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := c.InstallationToken(context.Background(), 101); err != nil {
		t.Fatal(err)
	}
	if f.tokenCalls != 2 {
		t.Fatalf("an expired token was reused: %d calls", f.tokenCalls)
	}
}

func TestCreateAndCompleteCheckRun(t *testing.T) {
	f := newFakeGitHub(t)
	c := newTestClient(t, f.srv.URL)

	run, err := c.CreateCheckRun(context.Background(), 101, CreateCheckRunInput{
		Repo: "acme/api", Name: "openpreflight", HeadSHA: "abc123",
		DetailsURL: "https://ci.example.com/runs/job-1",
		Output:     &CheckOutput{Title: "Running", Summary: "starting"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != 555 {
		t.Fatalf("check run id: %d", run.ID)
	}
	created := f.createdRuns[0]
	if created["status"] != "in_progress" {
		t.Fatalf("status: %v", created["status"])
	}
	if created["details_url"] != "https://ci.example.com/runs/job-1" {
		t.Fatalf("details_url not sent: %v", created["details_url"])
	}
	if created["started_at"] == nil {
		t.Fatal("an in-progress run should carry started_at")
	}

	if err := c.CompleteCheckRun(context.Background(), 101, CompleteCheckRunInput{
		Repo: "acme/api", CheckRunID: run.ID, Conclusion: "success",
		Output: &CheckOutput{Title: "Passed", Summary: "all good"},
	}); err != nil {
		t.Fatal(err)
	}
	patched := f.patchedRuns[0]
	if patched["status"] != "completed" || patched["conclusion"] != "success" {
		t.Fatalf("completion: %+v", patched)
	}
	if patched["completed_at"] == nil {
		t.Fatal("a completed run needs completed_at")
	}
}

func TestCheckOutputIsTruncated(t *testing.T) {
	long := strings.Repeat("a\n", maxOutputBytes)
	out := truncateOutput(CheckOutput{Title: strings.Repeat("t", 400), Summary: long, Text: long})
	if len(out["summary"].(string)) > maxOutputBytes {
		t.Fatalf("summary not truncated: %d bytes", len(out["summary"].(string)))
	}
	if len(out["title"].(string)) > 255 {
		t.Fatalf("title not truncated: %d bytes", len(out["title"].(string)))
	}
	if !strings.Contains(out["text"].(string), "truncated") {
		t.Fatal("truncation should say so")
	}
	// The tail is what matters in a build log, so it must be the part kept.
	tail := "the-last-line-of-the-log"
	got := truncateUTF8(strings.Repeat("x", maxOutputBytes)+"\n"+tail, maxOutputBytes)
	if !strings.HasSuffix(got, tail) {
		t.Fatal("truncation dropped the end of the log instead of the start")
	}
}

func TestTruncateKeepsValidUTF8(t *testing.T) {
	body := strings.Repeat("héllo wörld ✓\n", 8000)
	got := truncateUTF8(body, 1000)
	if len(got) > 1000 {
		t.Fatalf("over limit: %d", len(got))
	}
	for i, r := range got {
		if r == '�' {
			t.Fatalf("split a multi-byte rune at %d", i)
		}
	}
}

func TestAPIErrorMessages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	_, err := c.Installations(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	// The operator's actual mistake is usually a mismatched App ID and key.
	if !strings.Contains(err.Error(), "App ID") {
		t.Fatalf("unhelpful 401 message: %v", err)
	}
}
