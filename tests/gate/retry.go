package gate

import (
	"fmt"
	"sort"
	"strings"
)

// The bounded in-leg retry of INCONCLUSIVE tests (#1973, ADR-1973).
//
// A test ends INCONCLUSIVE (tests/e2e/inconclusive) when the precondition it
// needs never arose — a harness race, not an engine verdict. Nothing is retried
// because it FAILED: only a test that declared itself inconclusive is re-run,
// at the end of its leg, in the same cell (same auth/train mode, same bed, no
// restart), at most Config.InconclusiveRetries times. What stays inconclusive is
// recorded as INCONCLUSIVE — uncovered, not failed — and the invocation ends
// ExitCoverageIncomplete rather than reading as success.

// retryLogPath names an attempt's log next to the leg's first log: retry 1 of
// ".../go-test.json" is ".../go-test.retry-1.json".
func retryLogPath(first string, n int) string {
	if strings.HasSuffix(first, ".json") {
		return fmt.Sprintf("%s.retry-%d.json", strings.TrimSuffix(first, ".json"), n)
	}
	return fmt.Sprintf("%s.retry-%d", first, n)
}

// topLevelName is the top-level test an event belongs to ("" for a package-level
// event): a subtest's events fold into its parent.
func topLevelName(e Event) string {
	if e.Test == "" {
		return ""
	}
	if i := strings.IndexByte(e.Test, '/'); i >= 0 {
		return e.Test[:i]
	}
	return e.Test
}

// mergeAttempts is the leg's effective event stream after a retry: base with
// every test the retry re-ran replaced, wholesale, by that test's events from
// the retry (the LAST attempt wins per test). The retry's package-level events
// are dropped — the first attempt's still describe the package.
func mergeAttempts(base, retry []Event) []Event {
	reran := map[string]bool{}
	for _, e := range retry {
		if n := topLevelName(e); n != "" {
			reran[n] = true
		}
	}
	out := make([]Event, 0, len(base)+len(retry))
	for _, e := range base {
		if reran[topLevelName(e)] {
			continue
		}
		out = append(out, e)
	}
	for _, e := range retry {
		if topLevelName(e) != "" {
			out = append(out, e)
		}
	}
	return out
}

// suiteIncomplete is true when the stream shows a run that did not finish
// cleanly — a test still executing or never given a slot (a timeout kill): the
// bed state is unknown, so no retry is attempted.
func suiteIncomplete(c Classification) bool {
	return len(c.Running) > 0 || len(c.NeverStarted) > 0
}

// retryOutcome is what the retries of one leg amounted to.
type retryOutcome struct {
	First         []string // inconclusive after the first attempt
	PassedOnRetry []string
	FailedOnRetry []string
	Still         []string // inconclusive after the last attempt: UNCOVERED, not failed
	Attempts      int      // retry attempts actually run
}

// summarizeRetries compares the first attempt's inconclusive set with the
// final classification.
func summarizeRetries(first []string, final Classification, attempts int) retryOutcome {
	o := retryOutcome{First: first, Attempts: attempts}
	in := func(s []string, n string) bool {
		for _, x := range s {
			if x == n {
				return true
			}
		}
		return false
	}
	for _, n := range first {
		switch {
		case in(final.Inconclusive, n):
			o.Still = append(o.Still, n)
		case in(final.Pass, n):
			o.PassedOnRetry = append(o.PassedOnRetry, n)
		case in(final.Fail, n):
			o.FailedOnRetry = append(o.FailedOnRetry, n)
		}
	}
	for _, s := range [][]string{o.PassedOnRetry, o.FailedOnRetry, o.Still} {
		sort.Strings(s)
	}
	return o
}

func listOrNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// Summary is the per-leg line (R4): the count and the NAMES — including those
// that passed on retry, so a leg that is quietly flaky is never invisible. When
// the count exceeds warnAbove, a warning pointing at #1974 (the underlying
// harness races) follows.
func (o retryOutcome) Summary(label string, warnAbove int) string {
	if len(o.First) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "== inconclusive (leg: %s): %d on the first attempt: %s ==\n", label, len(o.First), strings.Join(o.First, ", "))
	fmt.Fprintf(&b, "   after %d retry attempt(s): passed on retry: %s; failed on retry: %s; still inconclusive (UNCOVERED, not failed): %s\n",
		o.Attempts, listOrNone(o.PassedOnRetry), listOrNone(o.FailedOnRetry), listOrNone(o.Still))
	if len(o.First) > warnAbove {
		fmt.Fprintf(&b, "   WARNING (leg: %s): %d inconclusive tests exceeds E2E_INCONCLUSIVE_WARN=%d — the harness races behind them are tracked in #1974\n", label, len(o.First), warnAbove)
	}
	return b.String()
}
