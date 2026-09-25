//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// Check-run / label timing helpers for TestLateCheckRunSuiteGate (#1849).
//
// The scenario asserts an ORDERING between GitHub's own timestamps, so these
// helpers return times rather than conclusions. The parsing and ordering logic
// is pure (unit-tested in checkrun_timing_test.go); only the Fetch* / wait
// wrappers touch the network.

// The two job/check names defined by testdata/late-check-suite-gate.yml.
const (
	lateCheckFastName = "late-check-fast"
	lateCheckSlowName = "late-check-slow"
)

// CheckRunTiming is one check run's identity and GitHub-reported timestamps.
// GitHub check runs have no created_at; started_at is the earliest timestamp a
// check run carries. A zero time means GitHub reported null (not yet
// started/completed).
type CheckRunTiming struct {
	Name        string
	Status      string
	Conclusion  string
	StartedAt   time.Time
	CompletedAt time.Time
}

type rawCheckRun struct {
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	Conclusion  *string `json:"conclusion"`
	StartedAt   *string `json:"started_at"`
	CompletedAt *string `json:"completed_at"`
}

func parseOptionalTime(s *string) (time.Time, error) {
	if s == nil || *s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, *s)
}

// parseCheckRunTimings parses a `commits/{sha}/check-runs` response body.
func parseCheckRunTimings(body []byte) ([]CheckRunTiming, error) {
	var resp struct {
		CheckRuns []rawCheckRun `json:"check_runs"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing check-runs response: %w", err)
	}
	out := make([]CheckRunTiming, 0, len(resp.CheckRuns))
	for _, r := range resp.CheckRuns {
		started, err := parseOptionalTime(r.StartedAt)
		if err != nil {
			return nil, fmt.Errorf("check run %q started_at: %w", r.Name, err)
		}
		completed, err := parseOptionalTime(r.CompletedAt)
		if err != nil {
			return nil, fmt.Errorf("check run %q completed_at: %w", r.Name, err)
		}
		conclusion := ""
		if r.Conclusion != nil {
			conclusion = *r.Conclusion
		}
		out = append(out, CheckRunTiming{
			Name: r.Name, Status: r.Status, Conclusion: conclusion,
			StartedAt: started, CompletedAt: completed,
		})
	}
	return out, nil
}

// earliestLabeledAt scans an issue-events payload (one or more concatenated JSON
// arrays, as `gh api --paginate` emits) and returns the earliest `labeled` event
// time for the named label. found is false when the label was never applied.
//
// Earliest, not latest: a later re-application must not mask a premature first
// application (the engine's own FetchLabelAppliedAt takes the latest, which is
// the wrong direction for proving "not applied too early").
func earliestLabeledAt(body []byte, label string) (at time.Time, found bool, err error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var page []struct {
			Event     string `json:"event"`
			CreatedAt string `json:"created_at"`
			Label     *struct {
				Name string `json:"name"`
			} `json:"label"`
		}
		if derr := dec.Decode(&page); derr == io.EOF {
			break
		} else if derr != nil {
			return time.Time{}, false, fmt.Errorf("parsing issue events: %w", derr)
		}
		for _, ev := range page {
			if ev.Event != "labeled" || ev.Label == nil || ev.Label.Name != label {
				continue
			}
			ts, perr := time.Parse(time.RFC3339, ev.CreatedAt)
			if perr != nil {
				return time.Time{}, false, fmt.Errorf("labeled event created_at %q: %w", ev.CreatedAt, perr)
			}
			if !found || ts.Before(at) {
				at, found = ts, true
			}
		}
	}
	return at, found, nil
}

// latestRunNamed returns the most recently started check run with the given
// name (a re-run of the same job on the same SHA yields several).
func latestRunNamed(runs []CheckRunTiming, name string) (CheckRunTiming, bool) {
	var best CheckRunTiming
	found := false
	for _, r := range runs {
		if r.Name != name {
			continue
		}
		if !found || r.StartedAt.After(best.StartedAt) {
			best, found = r, true
		}
	}
	return best, found
}

// checkLateCheckOrdering evaluates the scenario's timing assertions and returns
// a descriptive error for the first one that does not hold. All comparisons are
// non-strict on equality: GitHub timestamps are second-granular and a correct
// engine can only act after observing the event it acts on, while the failure
// modes being guarded against are minutes apart.
//
//	A3 (vacuity guard): the fast job completed at/after fabrik:awaiting-ci was
//	   applied — i.e. the CI gate was already active when the fast job went
//	   green. Otherwise the run is inconclusive (a pre-#1822 engine would pass
//	   it too).
//	A1: the late job started at/after the fast job completed — the window in
//	   which "all existing check runs are green" existed.
//	A2: stage:Validate:complete was applied at/after the late job completed
//	   (successfully) — the gate did not clear early.
func checkLateCheckOrdering(runs []CheckRunTiming, awaitingCIAt, validateCompleteAt time.Time) error {
	fast, ok := latestRunNamed(runs, lateCheckFastName)
	if !ok {
		return fmt.Errorf("no %q check run on the PR head — workflow not triggered (path filter / head-ref guard miss?); have: %s",
			lateCheckFastName, describeRunNames(runs))
	}
	slow, ok := latestRunNamed(runs, lateCheckSlowName)
	if !ok {
		return fmt.Errorf("no %q check run on the PR head — the late job never appeared; have: %s",
			lateCheckSlowName, describeRunNames(runs))
	}
	for _, r := range []CheckRunTiming{fast, slow} {
		if r.CompletedAt.IsZero() || r.Status != "completed" {
			return fmt.Errorf("check run %q not completed (status=%q) although stage:Validate:complete was applied — gate cleared with a check run still outstanding", r.Name, r.Status)
		}
		if r.Conclusion != "success" {
			return fmt.Errorf("check run %q concluded %q, want success", r.Name, r.Conclusion)
		}
	}
	if fast.CompletedAt.Before(awaitingCIAt) {
		return fmt.Errorf("INCONCLUSIVE (A3): %s completed at %s, before fabrik:awaiting-ci was applied at %s — the fast job was already green before the CI gate was active, so this run cannot distinguish a suite-aware gate from a run-only gate",
			lateCheckFastName, fast.CompletedAt.Format(time.RFC3339), awaitingCIAt.Format(time.RFC3339))
	}
	if slow.StartedAt.Before(fast.CompletedAt) {
		return fmt.Errorf("A1: %s started at %s, before %s completed at %s — no window existed in which the late check run was absent",
			lateCheckSlowName, slow.StartedAt.Format(time.RFC3339), lateCheckFastName, fast.CompletedAt.Format(time.RFC3339))
	}
	if validateCompleteAt.Before(slow.CompletedAt) {
		return fmt.Errorf("A2: stage:Validate:complete applied at %s, before %s completed at %s — the CI gate cleared while the late check run was still outstanding",
			validateCompleteAt.Format(time.RFC3339), lateCheckSlowName, slow.CompletedAt.Format(time.RFC3339))
	}
	return nil
}

func describeRunNames(runs []CheckRunTiming) string {
	if len(runs) == 0 {
		return "(no check runs)"
	}
	names := make([]string, 0, len(runs))
	for _, r := range runs {
		names = append(names, r.Name)
	}
	return strings.Join(names, ", ")
}

// prHeadSHA returns the PR's current head commit SHA.
func prHeadSHA(env *Env, repo string, prNumber int) (string, error) {
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/pulls/%d", repo, prNumber), "--jq", ".head.sha")
	if err != nil {
		return "", fmt.Errorf("resolve head SHA for %s#%d: %w\n%s", repo, prNumber, err, out)
	}
	sha := strings.TrimSpace(out)
	if sha == "" || sha == "null" {
		return "", fmt.Errorf("empty head SHA for %s#%d", repo, prNumber)
	}
	return sha, nil
}

// FetchCheckRunTimings reads every check run on the given commit with its
// timestamps.
func FetchCheckRunTimings(t *testing.T, env *Env, repo, sha string) []CheckRunTiming {
	t.Helper()
	out, err := ghOutput(env, "api", fmt.Sprintf("repos/%s/commits/%s/check-runs?per_page=100", repo, sha))
	if err != nil {
		t.Fatalf("read check runs for %s@%s: %v\n%s", repo, sha, err, out)
	}
	runs, err := parseCheckRunTimings([]byte(out))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return runs
}

// tryLabelFirstAppliedAt reads the earliest `labeled` event for label from the
// issue's events log. The events log is durable — it survives the label later
// being removed — so callers can wait on it without racing a label's removal.
func tryLabelFirstAppliedAt(env *Env, repo string, issueNumber int, label string) (time.Time, bool, error) {
	out, err := ghOutput(env, "api", "--paginate",
		fmt.Sprintf("repos/%s/issues/%d/events?per_page=100", repo, issueNumber))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("read events for %s#%d: %w\n%s", repo, issueNumber, err, out)
	}
	return earliestLabeledAt([]byte(out), label)
}

// waitForLabelFirstApplied polls the issue's events log until label has been
// applied at least once, returning the earliest application time. Unlike
// WaitForIssueLabel it also succeeds if the label has already been removed again.
func waitForLabelFirstApplied(t *testing.T, env *Env, repo string, issueNumber int, label string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		at, found, err := tryLabelFirstAppliedAt(env, repo, issueNumber, label)
		if err != nil {
			t.Logf("waitForLabelFirstApplied: transient error on %s#%d: %v (will retry)", repo, issueNumber, err)
		} else if found {
			return at
		}
		pollSleep(pollBase())
	}
	t.Fatalf("timed out after %s waiting for label %q to be applied on %s#%d", timeout, label, repo, issueNumber)
	return time.Time{}
}
