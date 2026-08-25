// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"fmt"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
)

// checkTitle is the one-line title GitHub shows next to the check name.
func checkTitle(conclusion string) string {
	switch conclusion {
	case "success":
		return "Passed"
	case "failure":
		return "Failed"
	case "timed_out":
		return "Timed out"
	case "cancelled":
		return "Cancelled"
	case "skipped":
		return "Skipped"
	default:
		return strings.ToUpper(conclusion[:1]) + conclusion[1:]
	}
}

// summarise renders the step table shown in the README:
//
//	✓ install   8s
//	✓ test     21s
//	✓ build    13s
//
//	Passed in 42s
func summarise(conclusion string, results []executor.Result, note, detailsURL string) string {
	var b strings.Builder
	if len(results) > 0 {
		width := 0
		for _, r := range results {
			if len(r.Name) > width {
				width = len(r.Name)
			}
		}
		b.WriteString("```\n")
		var total time.Duration
		for _, r := range results {
			total += r.Duration
			b.WriteString(fmt.Sprintf("%s %-*s %8s\n", mark(r), width, r.Name, human(r)))
		}
		b.WriteString("```\n\n")
		b.WriteString(fmt.Sprintf("**%s in %s**\n", checkTitle(conclusion), total.Round(time.Second)))
	} else {
		b.WriteString(fmt.Sprintf("**%s**\n", checkTitle(conclusion)))
	}
	if note != "" {
		b.WriteString("\n" + note + "\n")
	}
	if detailsURL != "" {
		b.WriteString("\n[View full logs](" + detailsURL + ")\n")
	}
	return b.String()
}

func mark(r executor.Result) string {
	switch {
	case r.Skipped:
		return "–"
	case r.OK():
		return "✓"
	default:
		return "✗"
	}
}

func human(r executor.Result) string {
	if r.Skipped {
		return "skipped"
	}
	d := r.Duration.Round(time.Second)
	if d < time.Second {
		d = time.Second
	}
	return d.String()
}
