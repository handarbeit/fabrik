//go:build e2e

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const lateCheckWorkflowFile = "late-check-suite-gate.yml"

// assertLateCheckWorkflowInstalled skips the test unless the bed repo has
// testdata/late-check-suite-gate.yml installed and active. The check is
// deliberately NOT a required status check (a path-filtered required check would
// block every unrelated PR), so the assert*Required branch-protection idiom does
// not apply — workflow presence is the prerequisite instead.
func assertLateCheckWorkflowInstalled(t *testing.T, env *Env, repo string) {
	t.Helper()
	out, err := ghOutput(env, "api",
		fmt.Sprintf("repos/%s/actions/workflows/%s", repo, lateCheckWorkflowFile), "--jq", ".state")
	if err != nil {
		if strings.Contains(out, "404") || strings.Contains(out, "Not Found") {
			t.Skipf("%s not installed on %s — install it per tests/e2e/README.md (Additional prerequisites for TestLateCheckRunSuiteGate)", lateCheckWorkflowFile, repo)
		}
		// Any other error is not evidence the bed is unprovisioned; fail rather
		// than let an API blip masquerade as a skip.
		t.Fatalf("could not read workflow %s on %s: %v\n%s", lateCheckWorkflowFile, repo, err, out)
	}
	if state := strings.TrimSpace(out); state != "active" {
		t.Skipf("%s on %s is in state %q, not active — enable it (see tests/e2e/README.md, Additional prerequisites for TestLateCheckRunSuiteGate)", lateCheckWorkflowFile, repo, state)
	}
}

// TestLateCheckRunSuiteGate is the live e2e proof of #1822/#1829 (suite-aware CI
// gate): with wait_for_ci on, the CI gate must NOT clear while a check run that
// does not exist yet — a job queued behind a `needs:` dependency — is still to
// come, even though every check run that DOES exist is green.
//
// Construction (testdata/late-check-suite-gate.yml): late-check-fast turns green
// once the engine has applied fabrik:awaiting-ci (so the gate is active); only
// then does its `needs:` dependent, late-check-slow, get a check run at all, and
// it sleeps ~4 min (several 60s engine polls) before succeeding. Pre-#1822 the
// gate reads "all existing runs green" the moment the fast job finishes and
// clears — before the late run exists.
//
// One yolo issue is taken to Validate directly (member PR on fabrik/issue-N via
// CreateMemberPR, then SetIssueStatus — no full Specify→Implement pipeline), so
// exactly one real Validate Claude invocation runs. Assertions, all on GitHub's
// own timestamps (see checkLateCheckOrdering):
//
//	A3 (vacuity guard) fast.completed_at >= fabrik:awaiting-ci applied-at
//	A1                 late.started_at   >= fast.completed_at
//	A2                 stage:Validate:complete applied-at >= late.completed_at
//
// stage:Validate:complete is applied at CI-gate clearance in both merge_train
// modes (addCompleteLabelAndRemoveCI); merge_train only changes what happens
// after (auto-merge vs. Queued), which this test never waits for — so it passes
// identically under "off" and "on". Log lines are informational only.
//
// Skips cleanly when the workflow is not installed on the bed, or (mode "on")
// when the board has no Queued column.
//
// Known limit: the empty window between the fast job finishing and the late job's
// check run existing is normally only seconds, so a regressed engine (60s polls)
// would clear inside it only some of the time. Configuring a wait timer on the
// late-check-gate environment widens it to minutes (README "Additional prerequisites for TestLateCheckRunSuiteGate").
//
// Wall-clock: ~20–35 min (Validate's Claude run + the ~4 min sleep + polls).
// Cost: one Validate Claude invocation, one CI cycle (~5 runner-minutes).
func TestLateCheckRunSuiteGate(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	assertLateCheckWorkflowInstalled(t, env, env.RepoAlpha)
	if resolveTrainMode(t, env) == "on" {
		// requireTrainBed skips unless a Queued column exists (mode is "on" here).
		requireTrainBed(t, env)
	}

	logStart := LogOffset(t, env)
	stamp := time.Now().UTC().Format("150405.000")
	title := fmt.Sprintf("e2e late check-run suite gate (%s)", stamp)
	num := FileIssue(t, env, env.RepoAlpha, title,
		"e2e scenario for the suite-aware CI gate. The only requirement is that the file "+
			"under e2e/late-check/entries/ exists on the branch; there is nothing to implement. "+
			"Validate should confirm that and complete.",
		"fabrik:yolo")
	itemID := AddIssueToProject(t, env, env.RepoAlpha, num)

	branch := fmt.Sprintf("fabrik/issue-%d", num)
	path := uniqueMemberPath("e2e/late-check/entries/late-check.txt", num)
	prNum := CreateMemberPR(t, env, env.RepoAlpha, "main", branch, path,
		"late check-run suite-gate scenario member\n", title, num)
	LinkedPRNumber(t, env, env.RepoAlpha, num)

	// Seed the prior stage as complete and place the card at Validate, so the
	// engine dispatches one real Validate invocation (same seeding as
	// seedReviewGateItem).
	AddLabel(t, env, env.RepoAlpha, num, "stage:Review:complete")
	SetIssueStatus(t, env, itemID, "Validate")
	t.Logf("seeded %s#%d (PR #%d, path %s) at Status=Validate; awaiting the CI gate", env.RepoAlpha, num, prNum, path)

	// The gate is active once Validate completes and the engine applies
	// fabrik:awaiting-ci. Read the durable events log rather than current labels:
	// the label is removed again at gate clearance.
	awaitingCIAt := waitForLabelFirstApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-ci", 45*time.Minute)
	t.Logf("fabrik:awaiting-ci first applied at %s", awaitingCIAt.Format(time.RFC3339))

	// The gate clears (stage:Validate:complete) only once CI is complete — after
	// the ~4 min late job plus engine polls.
	validateCompleteAt := waitForLabelFirstApplied(t, env, env.RepoAlpha, num, "stage:Validate:complete", 20*time.Minute)
	t.Logf("stage:Validate:complete first applied at %s", validateCompleteAt.Format(time.RFC3339))

	// The head SHA now (Validate may have pushed a rebase; CI then re-fired on the
	// new head, and the persisted fabrik:awaiting-ci lets its fast job finish at
	// once, so the assertions hold on whichever SHA is head).
	sha, err := prHeadSHA(env, env.RepoAlpha, prNum)
	if err != nil {
		t.Fatal(err)
	}
	runs := FetchCheckRunTimings(t, env, env.RepoAlpha, sha)
	for _, r := range runs {
		t.Logf("check run %-18s status=%-11s conclusion=%-8s started=%s completed=%s",
			r.Name, r.Status, r.Conclusion, r.StartedAt.Format(time.RFC3339), r.CompletedAt.Format(time.RFC3339))
	}

	// Informational only: the engine's hold line fires only if a gate poll happened
	// to land in the empty window, so it is never asserted.
	held := CountLogLines(t, env, "check suite(s) still running", logStart)
	t.Logf("informational: %d 'check suite(s) still running ... holding the CI gate' settle log line(s) since test start", held)

	if err := checkLateCheckOrdering(runs, awaitingCIAt, validateCompleteAt); err != nil {
		t.Fatal(err)
	}
	t.Logf("late check-run ordering verified: gate active -> fast green -> late run started -> late run completed -> stage:Validate:complete")
}
