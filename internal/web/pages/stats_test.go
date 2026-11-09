// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"testing"
	"time"

	"github.com/openpreflight/openpreflight/internal/store"
)

func job(status string, secs int) store.Job {
	j := store.Job{Status: status, CreatedAt: time.Now()}
	if secs > 0 {
		start := time.Now().Add(-time.Duration(secs) * time.Second)
		end := start.Add(time.Duration(secs) * time.Second)
		j.StartedAt, j.FinishedAt = &start, &end
	}
	return j
}

func TestDashStats(t *testing.T) {
	tests := []struct {
		name      string
		recent    []store.Job
		rate, mid string
		rateCap   string
	}{
		{
			name:    "nothing has finished yet",
			recent:  []store.Job{job(store.JobQueued, 0), job(store.JobInProgress, 0)},
			rate:    "-",
			rateCap: "no finished runs yet",
			mid:     "-",
		},
		{
			// A skipped job is not a failure - counting it as one would show a
			// red pass rate for a repo that is behaving exactly as configured.
			name:    "skipped runs count towards nothing",
			recent:  []store.Job{job(store.JobSuccess, 10), job(store.JobSkipped, 0)},
			rate:    "100%",
			rateCap: "1 of 1 finished",
			mid:     "10s",
		},
		{
			name:    "failures and errors are results",
			recent:  []store.Job{job(store.JobSuccess, 4), job(store.JobFailure, 60), job(store.JobError, 2), job(store.JobSuccess, 8)},
			rate:    "50%",
			rateCap: "2 of 4 finished",
			mid:     "8s", // 2,4,8,60 -> upper median, not dragged by the 60s outlier
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashStats(tt.recent, 3, 2, 5)
			if got[0].Value != tt.rate || got[0].Caption != tt.rateCap {
				t.Errorf("pass rate = %q %q, want %q %q", got[0].Value, got[0].Caption, tt.rate, tt.rateCap)
			}
			if got[3].Value != tt.mid {
				t.Errorf("run time = %q, want %q", got[3].Value, tt.mid)
			}
			if got[1].Value != "3" || got[2].Value != "2" || got[2].Caption != "of 5 bound" {
				t.Errorf("counts = %+v %+v", got[1], got[2])
			}
		})
	}
}

func TestJobLabels(t *testing.T) {
	for _, c := range []struct {
		j          store.Job
		ref, event string
	}{
		{store.Job{Ref: "main", Event: "check_suite.requested"}, "main", "push"},
		{store.Job{Ref: "refs/heads/feat/x", Event: "check_suite.requested"}, "feat/x", "push"},
		{store.Job{Ref: "refs/pull/47/head", PullNumber: 47, Event: "check_suite.requested"}, "PR #47", "pull request"},
		{store.Job{PullNumber: 9, Event: "check_run.rerequested"}, "PR #9", "re-run from GitHub"},
		{store.Job{Ref: "v1.2", Event: "manual.rerun"}, "v1.2", "re-run"},
		{store.Job{}, "—", "—"},
	} {
		if got := jobRefLabel(c.j); got != c.ref {
			t.Errorf("jobRefLabel(%q) = %q, want %q", c.j.Ref, got, c.ref)
		}
		if got := eventLabel(c.j); got != c.event {
			t.Errorf("eventLabel(%q) = %q, want %q", c.j.Event, got, c.event)
		}
	}
}

func TestHumanUnits(t *testing.T) {
	for n, want := range map[int64]string{0: "0 bytes", 1023: "1023 bytes", 4096: "4 KiB", 10485760: "10 MiB", 1536: "1.5 KiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[int]string{1: "Kept for a day.", 14: "Kept for 14 days.", 365: "Kept for a year.", 3650: "Kept for 10 years."} {
		if got := retentionLabel(d); got != want {
			t.Errorf("retentionLabel(%d) = %q, want %q", d, got, want)
		}
	}
}
