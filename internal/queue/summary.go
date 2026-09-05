// SPDX-License-Identifier: Apache-2.0

package queue

import (
	"fmt"
	"strings"
	"time"

	"github.com/openpreflight/openpreflight/internal/executor"
	"github.com/openpreflight/openpreflight/internal/store"
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

// titleFor is checkTitle plus the failing step, which is the one fact a reader
// wants from a pull request's collapsed check list — the only place the title is
// visible without opening the run.
func titleFor(conclusion string, results []executor.Result) string {
	if conclusion != "failure" {
		return checkTitle(conclusion)
	}
	for _, r := range results {
		if r.Skipped || r.OK() {
			continue
		}
		if r.TimedOut {
			return fmt.Sprintf("Failed: %s timed out", r.Name)
		}
		if r.ExitCode > 0 {
			return fmt.Sprintf("Failed: %s (exit %d)", r.Name, r.ExitCode)
		}
		return fmt.Sprintf("Failed: %s", r.Name)
	}
	return checkTitle(conclusion)
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

// PathFilterDiagnostic is the "why did this run, or not" block. It goes in the
// log on every outcome and in the Check Run summary on a skip, because a reader
// on GitHub cannot see the worker's log file.
//
// Exported because the dry-run endpoint has to answer the same question about a
// commit that has not run, and two implementations of "why" would drift.
func PathFilterDiagnostic(filter string, changed, matched int, allowed bool) string {
	result := "SKIP"
	if allowed {
		result = "RUN"
	}
	return fmt.Sprintf("Changed files: %d\nMatched files: %d\nFilter: %s\nResult: %s",
		changed, matched, strings.Join(strings.Fields(filter), ", "), result)
}

// skipExplanation turns a stored skip reason into a sentence for the Check Run.
// The fork cases are the ones an operator can act on, so each says what to change.
func skipExplanation(reason string) string {
	switch reason {
	case store.SkipReasonForkDisabled:
		return "Fork pull requests are not run. This repository's checks are disabled for forks " +
			"because a fork PR would run code from outside your organisation on your server. " +
			"An operator can allow them by turning off skip_fork_prs and setting default_runtime, " +
			"which makes fork jobs run in Docker."
	case store.SkipReasonForkNoDocker:
		return "Fork pull requests are enabled but no Docker engine is reachable. Fork jobs always " +
			"run in a container, never as a process on the host, so this one cannot run. " +
			"Set CI_DOCKER_HOST or mount the engine socket."
	case store.SkipReasonForkNoRuntime:
		return "Fork pull requests are enabled but settings.default_runtime is empty. Fork jobs need " +
			"an image to run in; set one so they have a container to use."
	case store.SkipReasonPathFilter:
		return "No changed path matched this binding's path filter."
	case store.SkipReasonNoPipeline:
		return "Nothing to run: no pipeline file, no binding commands, and no recognisable project."
	default:
		return "Skipped."
	}
}
