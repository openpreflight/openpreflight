// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"action":"requested"}`)
	if err := VerifySignature("s3cret", body, sign("s3cret", body)); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if err := VerifySignature("s3cret", body, sign("wrong", body)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong secret: got %v", err)
	}
	// One changed byte must invalidate it: the HMAC covers the exact payload.
	if err := VerifySignature("s3cret", []byte(`{"action":"requested" }`), sign("s3cret", body)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("mutated body accepted: %v", err)
	}
	for _, header := range []string{"", "sha1=abcdef", "sha256=zzzz", "sha256="} {
		if err := VerifySignature("s3cret", body, header); err == nil {
			t.Fatalf("header %q was accepted", header)
		}
	}
	if err := VerifySignature("", body, sign("", body)); err == nil {
		t.Fatal("an App with no configured secret must not verify anything")
	}
}

const checkSuitePush = `{
  "action": "requested",
  "check_suite": {
    "id": 5150,
    "head_sha": "1111111111111111111111111111111111111111",
    "head_branch": "main",
    "pull_requests": [],
    "app": {"id": 4242}
  },
  "repository": {"id": 10, "full_name": "winpra/api", "private": true},
  "installation": {"id": 777},
  "sender": {"login": "someone"}
}`

func TestParseCheckSuite(t *testing.T) {
	ev, skip, err := Parse(EventCheckSuite, []byte(checkSuitePush))
	if err != nil || skip != "" {
		t.Fatalf("parse: err=%v skip=%q", err, skip)
	}
	if ev.Repo != "winpra/api" || ev.Branch != "main" || ev.InstallationID != 777 {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.SHA != "1111111111111111111111111111111111111111" {
		t.Fatalf("sha: %q", ev.SHA)
	}
	if ev.IsFork {
		t.Fatal("a same-repo branch push is not a fork")
	}
	if ev.CheckSuiteID != 5150 {
		t.Fatalf("check suite id not carried on the check_suite path: %d", ev.CheckSuiteID)
	}
}

func TestParseIgnoresNonActionableActions(t *testing.T) {
	body := []byte(`{"action":"completed","check_suite":{"head_sha":"abc","head_branch":"main"},
		"repository":{"full_name":"o/r"},"installation":{"id":1}}`)
	_, skip, err := Parse(EventCheckSuite, body)
	if err != nil {
		t.Fatal(err)
	}
	if skip == "" {
		t.Fatal("check_suite.completed should be skipped, not run")
	}
}

func TestParseIgnoresUnhandledEvents(t *testing.T) {
	body := []byte(`{"repository":{"full_name":"o/r"},"installation":{"id":1}}`)
	if _, skip, err := Parse("push", body); err != nil || skip == "" {
		t.Fatalf("push should be skipped: err=%v skip=%q", err, skip)
	}
}

func TestForkDetectionByHeadRepo(t *testing.T) {
	// head_branch is present but the PR head is a different repo: still a fork.
	body := []byte(`{
	  "action": "requested",
	  "check_suite": {
	    "head_sha": "abc1234",
	    "head_branch": "patch-1",
	    "pull_requests": [{"number": 3, "head": {"ref":"patch-1","repo": {"id": 99, "full_name": "outsider/api"}},
	                       "base": {"ref":"main","repo": {"id": 10, "full_name": "winpra/api"}}}]
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 5}
	}`)
	ev, skip, err := Parse(EventCheckSuite, body)
	if err != nil || skip != "" {
		t.Fatalf("parse: %v %q", err, skip)
	}
	if !ev.IsFork {
		t.Fatal("a PR whose head repo differs from the base repo is a fork")
	}
}

func TestForkDetectionByNullHeadBranch(t *testing.T) {
	body := []byte(`{
	  "action": "requested",
	  "check_suite": {"head_sha": "abc1234", "head_branch": null, "pull_requests": []},
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 5}
	}`)
	ev, skip, err := Parse(EventCheckSuite, body)
	if err != nil || skip != "" {
		t.Fatalf("parse: %v %q", err, skip)
	}
	if !ev.IsFork {
		t.Fatal("a null head_branch is the fork signal when there is no PR list")
	}
	if ev.Branch != "" {
		t.Fatalf("branch should be empty, got %q", ev.Branch)
	}
}

func TestSameRepoPullRequestIsNotFork(t *testing.T) {
	body := []byte(`{
	  "action": "requested",
	  "check_suite": {
	    "head_sha": "abc1234", "head_branch": "feature/x",
	    "pull_requests": [{"head": {"repo": {"id": 10, "full_name": "winpra/api"}},
	                       "base": {"repo": {"id": 10, "full_name": "winpra/api"}}}]
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 5}
	}`)
	ev, _, err := Parse(EventCheckSuite, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.IsFork {
		t.Fatal("a branch PR inside the same repo is not a fork")
	}
}

func TestParseCheckRunRerequestedCarriesAppID(t *testing.T) {
	body := []byte(`{
	  "action": "rerequested",
	  "check_run": {
	    "id": 900, "head_sha": "cafe1234", "name": "openpreflight",
	    "app": {"id": 4242},
	    "check_suite": {"id": 6161, "head_sha":"cafe1234","head_branch": "main", "pull_requests": []}
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 777}
	}`)
	ev, skip, err := Parse(EventCheckRun, body)
	if err != nil || skip != "" {
		t.Fatalf("parse: %v %q", err, skip)
	}
	if ev.CheckRunAppID != 4242 {
		t.Fatalf("app id not carried: %d", ev.CheckRunAppID)
	}
	if ev.Branch != "main" || ev.SHA != "cafe1234" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	// The suite id comes from the nested check_run.check_suite on this path.
	if ev.CheckSuiteID != 6161 {
		t.Fatalf("check suite id not carried on the check_run path: %d", ev.CheckSuiteID)
	}
}

// A payload with no suite id must parse and stay actionable: the id is recorded
// for traceability, never load-bearing (ADR 005).
func TestParseMissingCheckSuiteIDIsStillActionable(t *testing.T) {
	body := []byte(`{
	  "action": "rerequested",
	  "check_run": {
	    "id": 900, "head_sha": "cafe1234", "name": "openpreflight",
	    "app": {"id": 4242},
	    "check_suite": {"head_sha":"cafe1234","head_branch": "main", "pull_requests": []}
	  },
	  "repository": {"id": 10, "full_name": "winpra/api"},
	  "installation": {"id": 777}
	}`)
	ev, skip, err := Parse(EventCheckRun, body)
	if err != nil || skip != "" {
		t.Fatalf("parse: %v %q", err, skip)
	}
	if ev.CheckSuiteID != 0 {
		t.Fatalf("expected a zero suite id, got %d", ev.CheckSuiteID)
	}
	if ev.SHA != "cafe1234" {
		t.Fatalf("a missing suite id must not affect the rest of the event: %+v", ev)
	}
}

func TestParseRejectsMalformedPayloads(t *testing.T) {
	cases := map[string]string{
		"not json":        `{`,
		"no repository":   `{"action":"requested","check_suite":{"head_sha":"a","head_branch":"m"}}`,
		"no sha":          `{"action":"requested","check_suite":{"head_branch":"m"},"repository":{"full_name":"o/r"},"installation":{"id":1}}`,
		"no installation": `{"action":"requested","check_suite":{"head_sha":"a","head_branch":"m"},"repository":{"full_name":"o/r"}}`,
	}
	for name, body := range cases {
		if _, _, err := Parse(EventCheckSuite, []byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
