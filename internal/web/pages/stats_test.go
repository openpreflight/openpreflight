// SPDX-License-Identifier: Apache-2.0

package pages

import (
	"fmt"
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

func TestDashTiles(t *testing.T) {
	tests := []struct {
		name            string
		recent          []store.Job
		rate, unit, mid string
	}{
		{
			name:   "nothing has finished yet",
			recent: []store.Job{job(store.JobQueued, 0), job(store.JobInProgress, 0)},
			rate:   "—",
			mid:    "—",
		},
		{
			// A skipped job is not a failure - counting it as one would show a
			// red pass rate for a repo that is behaving exactly as configured.
			name:   "skipped runs count towards nothing",
			recent: []store.Job{job(store.JobSuccess, 10), job(store.JobSkipped, 0)},
			rate:   "100", unit: "%",
			mid: "10s",
		},
		{
			name:   "failures and errors are results",
			recent: []store.Job{job(store.JobSuccess, 4), job(store.JobFailure, 60), job(store.JobError, 2), job(store.JobSuccess, 8)},
			rate:   "50", unit: "%",
			mid: "8s", // 2,4,8,60 -> upper median, not dragged by the 60s outlier
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dashTiles(tt.recent, nil, nil, 2, 5, 3)
			if got[0].Value != tt.rate || got[0].Unit != tt.unit {
				t.Errorf("pass rate = %q%q, want %q%q", got[0].Value, got[0].Unit, tt.rate, tt.unit)
			}
			if got[1].Value != tt.mid {
				t.Errorf("run time = %q, want %q", got[1].Value, tt.mid)
			}
			if got[2].Value != "2" || got[2].Lines[0][1] != "3" || got[2].Lines[1][1] != "3" {
				t.Errorf("repos tile = %+v", got[2])
			}
		})
	}
}

func TestWeekTrendAndChartScale(t *testing.T) {
	daily := make([]store.DayCount, 14)
	daily[0] = store.DayCount{Passed: 1, Failed: 1}            // prior week 50%
	daily[13] = store.DayCount{Passed: 3, Failed: 1, Other: 2} // this week 75%
	last, prev, ok := weekRates(daily)
	if !ok || last != 75 || prev != 50 {
		t.Fatalf("weekRates = %d %d %v, want 75 50 true", last, prev, ok)
	}
	bars, top, total, failed := activityChart(daily)
	if top != 10 || total != 8 || failed != 2 || bars[13].Height != "60.0%" {
		t.Fatalf("chart: top %d total %d failed %d height %s", top, total, failed, bars[13].Height)
	}
	for n, want := range map[int]int{0: 4, 3: 4, 5: 5, 6: 10, 11: 20, 21: 50, 99: 100} {
		if got := niceCeil(n); got != want {
			t.Errorf("niceCeil(%d) = %d, want %d", n, got, want)
		}
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

func TestJobPagesAndRange(t *testing.T) {
	for _, c := range []struct {
		total, limit, offset int
		want                 []int
	}{
		{0, 25, 0, []int{1}},
		{20, 25, 0, []int{1}},
		{87, 25, 0, []int{1, 2, 0, 4}},
		{87, 25, 25, []int{1, 2, 3, 4}},
		{300, 25, 125, []int{1, 0, 5, 6, 7, 0, 12}},
		{300, 25, 275, []int{1, 0, 11, 12}},
	} {
		got := jobPages(c.total, c.limit, c.offset)
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("jobPages(%d, %d, %d) = %v, want %v", c.total, c.limit, c.offset, got, c.want)
		}
	}
	if got := jobRange(87, 25, 75, 12); got != "76–87 of 87" {
		t.Errorf("jobRange = %q", got)
	}
	if got := jobRange(0, 25, 0, 0); got != "0 of 0" {
		t.Errorf("jobRange empty = %q", got)
	}
}

func TestSecondsLabel(t *testing.T) {
	for n, want := range map[int]string{900: "15 minutes", 60: "1 minute", 3600: "1 hour", 7200: "2 hours", 90: "90 seconds", 1: "1 second"} {
		if got := secondsLabel(n); got != want {
			t.Errorf("secondsLabel(%d) = %q, want %q", n, got, want)
		}
	}
}
