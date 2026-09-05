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
