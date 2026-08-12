//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"
)

// TestMergeTrainRedSingletonReroutesOffQueued is the live e2e proof of #1545 R1/R6:
// a member paused for its own standalone (non-interaction) combined-Validate failure
// must be rerouted off the Queued holding column to stageBeforeHolding (Validate)
// BEFORE being paused — not left stranded in Queued, unreachable by any stage, the
// way ejectRedSingleton behaved before this issue.
//
// Construction: a SINGLE poison member on RepoAlpha. With exactly one Queued member,
// the combined trial batch has nobody to combine with, so its own file (which trips
// the repo's required "train-poison-guard" check) makes the singleton's own combined
// Validate red. runMergeTrainWorker's TrainCIRed branch special-cases `len(survivors)
// == 1` and calls ejectRedSingleton directly — bisection is never reached (there is
// nothing to isolate in a batch of one) — so this exercises the exact top-level arity
// guard the issue's Worked Example (#1450) hit, not the multi-member bisection path
// TestMergeTrainBisectionEjectsPoisoner already covers.
//
// Why this MUST be a live test, not only a sim/unit one: the fix is
// rerouteQueuedMemberOffHolding -> a real UpdateProjectItemStatus GraphQL mutation.
// A sim test only proves the engine DECIDED to move the card; it can't prove the
// mutation GitHub actually receives is well-formed, or that the reroute target
// ("Validate") resolves against this bed's real project-board Status field options —
// both properties only a live run against the real API can establish (see
// tests/sim/README.md's "Catches wire bugs: No" row and FIDELITY.md).
//
// R2's reroute-before-side-effects ordering guarantee (a failed reroute posts no
// comment and applies no pause) is NOT re-asserted here — injecting a genuine,
// deterministic UpdateProjectItemStatus failure against the real GitHub API isn't
// practical in this bed. That guarantee is covered by
// TestEjectRedSingleton_RerouteFailure_NoCommentNoPause (engine/merge_train_test.go)
// and, per the issue's own sim/live division, by the equivalent sim-bed regression
// tracked under #1452 — mirroring how ejectQueuedMemberForReviewFindings's identical
// R2 guarantee (#1208) is unit-test-only, with no live e2e counterpart either.
//
// Prerequisites: see tests/e2e/README.md prerequisite #18 (Queued column,
// train-poison-guard required check on fabrik-test-alpha). Skips cleanly if
// either prerequisite is absent.
//
// NOT t.Parallel(): the train forms from every item in Queued on a repo
// (handleMergeTrainBatch snapshots the whole column), so a single-member batch run
// concurrently with TestMergeTrainHappyPathLanding/TestMergeTrainBisectionEjectsPoisoner
// (both t.Parallel() on the same RepoAlpha) risks this test's lone poison member being
// batched together with a sibling test's clean members — which would route through
// bisection/landOneAtATime instead of the top-level arity guard this test exists to
// exercise. Dropping t.Parallel() guarantees this test runs to completion (Queued,
// disposed, and cleaned up) before any parallel Alpha merge-train scenario begins
// queueing its own members — the same "non-parallel completes first" guarantee
// TestMergeTrainRunawayGuardPausesBatch's doc comment already documents and relies on.
//
// Wall-clock: ~10–20 min (one combined validation — no bisection, no landing CI).
// Cost: low (no Claude invocations).
func TestMergeTrainRedSingletonReroutesOffQueued(t *testing.T) {
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	requireTrainBed(t, env)
	assertTrainPoisonGuardRequired(t, env, env.RepoAlpha)

	const base = "main"
	logStart := LogOffset(t, env)

	issue, _ := QueueMember(t, env, env.RepoAlpha, base, "redsingleton",
		"e2e/train/entries/redsingleton.txt",
		"POISON — #1545 red-singleton e2e member\n",
	)
	t.Logf("queued single poison member (issue #%d); awaiting red-singleton disposition", issue)

	// The top-level arity guard's own log line — proves bisection was never reached.
	WaitForLogLine(t, env, fmt.Sprintf("combined Validate RED for %s with a single member (#%d)", env.RepoAlpha, issue), logStart, 25*time.Minute)
	t.Logf("red-singleton disposition confirmed (bisection skipped)")

	// R1: the board Status must have left Queued for the reroute target
	// (stageBeforeHolding — "Validate" in this bed's default stage set) rather than
	// staying in Queued, which is what the pre-#1545 bug did.
	WaitForLogLine(t, env, "rerouted off Queued to Validate", logStart, 5*time.Minute)
	deadline := time.Now().Add(5 * time.Minute)
	var lastStatus string
	for time.Now().Before(deadline) {
		lastStatus = projectStatus(t, env, env.RepoAlpha, issue)
		if lastStatus == "Validate" {
			break
		}
		time.Sleep(10 * time.Second)
	}
	if lastStatus != "Validate" {
		t.Fatalf("expected #%d to be rerouted off Queued to Validate, last observed status %q", issue, lastStatus)
	}
	t.Logf("member #%d confirmed off Queued, at Status=Validate", issue)

	// The member is paused there — reachable this time, unlike the pre-#1545 pause
	// inside Queued.
	WaitForIssueLabel(t, env, env.RepoAlpha, issue, "fabrik:paused", 5*time.Minute)
	WaitForIssueLabel(t, env, env.RepoAlpha, issue, "fabrik:awaiting-input", 5*time.Minute)
	t.Logf("member #%d has fabrik:paused + fabrik:awaiting-input", issue)

	// R4: the disposition comment names the reroute target and the working recovery
	// action (fabrik:revalidate), not the stale "remove fabrik:paused" instruction
	// that silently no-op'd against an already-stage:Validate:complete item.
	WaitForIssueComment(t, env, env.RepoAlpha, issue, "own combined Validate is failing", 5*time.Minute)
	WaitForIssueComment(t, env, env.RepoAlpha, issue, "has left the Queued column for Validate", 5*time.Minute)
	WaitForIssueComment(t, env, env.RepoAlpha, issue, "fabrik:revalidate", 5*time.Minute)
	t.Logf("disposition comment confirmed: names Validate as the reroute target and fabrik:revalidate as the recovery action")

	// Never landed.
	if st := projectStatus(t, env, env.RepoAlpha, issue); st == "Done" {
		t.Fatalf("member #%d reached Done — red-singleton disposition should never land", issue)
	}

	WaitForNoStaleTrainArtifacts(t, env, env.RepoAlpha, 2*time.Minute)
	t.Logf("red-singleton reroute contract verified: single-member red batch -> arity guard -> reroute off Queued to Validate -> paused, reachable, correct recovery instruction")
}
