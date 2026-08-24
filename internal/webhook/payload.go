package webhook

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event names we handle. Everything else is acknowledged and dropped.
const (
	EventCheckSuite = "check_suite"
	EventCheckRun   = "check_run"
)

// repoRef is the repository shape shared by the payloads.
type repoRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

type pullRequestRef struct {
	Number int `json:"number"`
	Head   struct {
		Ref  string  `json:"ref"`
		SHA  string  `json:"sha"`
		Repo repoRef `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string  `json:"ref"`
		Repo repoRef `json:"repo"`
	} `json:"base"`
}

type checkSuitePayload struct {
	HeadSHA      string           `json:"head_sha"`
	HeadBranch   *string          `json:"head_branch"`
	PullRequests []pullRequestRef `json:"pull_requests"`
	App          struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

type rawPayload struct {
	Action     string             `json:"action"`
	CheckSuite *checkSuitePayload `json:"check_suite"`
	CheckRun   *struct {
		ID         int64              `json:"id"`
		HeadSHA    string             `json:"head_sha"`
		Name       string             `json:"name"`
		CheckSuite *checkSuitePayload `json:"check_suite"`
		App        struct {
			ID int64 `json:"id"`
		} `json:"app"`
		PullRequests []pullRequestRef `json:"pull_requests"`
	} `json:"check_run"`
	Repository   repoRef `json:"repository"`
	Installation struct {
		ID int64 `json:"id"`
	} `json:"installation"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

// Event is the normalised form of an actionable delivery.
type Event struct {
	Name           string // check_suite | check_run
	Action         string
	Repo           string // owner/name
	SHA            string
	Branch         string // "" when GitHub gave us none (fork PR)
	InstallationID int64
	// CheckRunAppID is the App that owns the re-requested Check Run. We only act
	// on our own; another App's rerequest is not ours to answer.
	CheckRunAppID int64
	IsFork        bool
	PullNumber    int
	Sender        string
}

// SkipReason explains why an otherwise valid delivery produces no job. Empty
// means actionable.
type SkipReason string

// Parse decodes a delivery. err is for malformed payloads; a valid payload we
// choose not to act on comes back with a non-empty skip reason.
func Parse(eventName string, body []byte) (Event, SkipReason, error) {
	var raw rawPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		return Event{}, "", fmt.Errorf("webhook: decode %s payload: %w", eventName, err)
	}
	ev := Event{
		Name:           eventName,
		Action:         raw.Action,
		Repo:           raw.Repository.FullName,
		InstallationID: raw.Installation.ID,
		Sender:         raw.Sender.Login,
	}
	if ev.Repo == "" {
		return ev, "", fmt.Errorf("webhook: %s payload has no repository", eventName)
	}

	var suite *checkSuitePayload
	var prs []pullRequestRef

	switch eventName {
	case EventCheckSuite:
		// GitHub sends completed too; there is nothing for us to do with it.
		if raw.Action != "requested" && raw.Action != "rerequested" {
			return ev, SkipReason("check_suite action " + raw.Action + " is not actionable"), nil
		}
		if raw.CheckSuite == nil {
			return ev, "", fmt.Errorf("webhook: check_suite payload has no check_suite")
		}
		suite = raw.CheckSuite
		prs = raw.CheckSuite.PullRequests
		ev.SHA = raw.CheckSuite.HeadSHA
	case EventCheckRun:
		if raw.Action != "rerequested" {
			return ev, SkipReason("check_run action " + raw.Action + " is not actionable"), nil
		}
		if raw.CheckRun == nil {
			return ev, "", fmt.Errorf("webhook: check_run payload has no check_run")
		}
		ev.SHA = raw.CheckRun.HeadSHA
		ev.CheckRunAppID = raw.CheckRun.App.ID
		suite = raw.CheckRun.CheckSuite
		prs = raw.CheckRun.PullRequests
		if len(prs) == 0 && suite != nil {
			prs = suite.PullRequests
		}
	default:
		return ev, SkipReason("event " + eventName + " is not handled"), nil
	}

	if suite != nil && suite.HeadBranch != nil {
		ev.Branch = strings.TrimPrefix(*suite.HeadBranch, "refs/heads/")
	}
	if ev.SHA == "" && suite != nil {
		ev.SHA = suite.HeadSHA
	}
	if ev.SHA == "" {
		return ev, "", fmt.Errorf("webhook: %s payload has no head sha", eventName)
	}
	ev.IsFork = isFork(raw.Repository, suite, prs)
	if len(prs) > 0 {
		ev.PullNumber = prs[0].Number
		if ev.Branch == "" && prs[0].Head.Ref != "" {
			ev.Branch = strings.TrimPrefix(prs[0].Head.Ref, "refs/heads/")
		}
	}
	if ev.InstallationID == 0 {
		return ev, "", fmt.Errorf("webhook: %s payload has no installation id", eventName)
	}
	return ev, "", nil
}

// isFork detects a fork PR. There is no dedicated flag: head_branch is null on
// fork PRs, and any PR whose head repo differs from the base repository is a
// fork (AUDIT.md).
func isFork(base repoRef, suite *checkSuitePayload, prs []pullRequestRef) bool {
	for _, pr := range prs {
		head := pr.Head.Repo
		if head.ID != 0 && base.ID != 0 && head.ID != base.ID {
			return true
		}
		if head.FullName != "" && base.FullName != "" && !strings.EqualFold(head.FullName, base.FullName) {
			return true
		}
	}
	// A null head_branch with no PR list is the other fork signal. Treating an
	// unattributable event as a fork fails closed: we skip instead of running
	// untrusted code.
	if suite != nil && suite.HeadBranch == nil {
		return true
	}
	return false
}
