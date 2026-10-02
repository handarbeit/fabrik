//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tests/e2e/seedspec"
)

// TestYoloAutoMergeLabel is the regression test for #829 — replacing Fabrik's
// poll-merge loop with GitHub's native auto-merge for yolo issues — and, per
// #980, for the ADR-059 merge-train's own yolo landing contract.
//
// The test resolves the suite's merge-train mode (resolveTrainMode —
// E2E_TRAIN_MODE if set, else the bed's .env file) and asserts whichever
// contract that mode implies, since the two are mutually exclusive at the
// engine level (attemptMergeOnValidate
// diverts every yolo Validate completion straight to advanceToQueued before
// the fabrik:auto-merge-enabled label site is ever reached when the train is
// on):
//
//   - Train mode "off" (including default/unset) — unchanged from #829:
//     after Validate completes, the engine calls enablePullRequestAutoMerge
//     once and tags the issue with fabrik:auto-merge-enabled. GitHub then
//     merges the PR atomically when CI passes and the PR is mergeable.
//     Verifies FR-004/FR-005/SC-001: fabrik:auto-merge-enabled is applied at
//     some point (via the issue timeline), the issue closes, and the label
//     is absent afterward (removed once the PR merges).
//
//   - Train mode "on" (ADR-059 D6/D7) — the yolo item advances Validate →
//     Queued and lands via the merge train's own batch (landGreenBatch →
//     landMergeTrainBatch) or singleton (landSingleton) landing path, both of
//     which land the change via a separate integration/singleton PR and then
//     attempt to close the member's own PR. That close attempt only
//     sometimes succeeds: when the member merged cleanly into the trial
//     branch, its commits are already ancestors of the base branch by the
//     time the integration/singleton PR lands, so GitHub has already flipped
//     the member PR to MERGED on its own — the engine's own close call then
//     404/422s harmlessly (see #1271). CLOSED only happens when the trial
//     needed conflict resolution. Both are legitimate terminal states; the
//     invariant this test actually verifies is that the member's own PR was
//     never itself the merge vehicle — i.e. a distinct integration/singleton
//     PR exists and is the one GitHub reports MERGED. Verifies: the issue
//     reaches Done (board Status "Done" or issue CLOSED, whichever is
//     observed first — the same race-tolerant pattern WaitForMemberLanded
//     uses elsewhere), the member's own linked PR reaches a terminal state
//     (CLOSED or MERGED) while a distinct integration/singleton PR is
//     confirmed MERGED, a "landing complete" bed-log line appears after the
//     issue was filed, and fabrik:auto-merge-enabled is never applied at any
//     point.
//
// Seeded at Validate (#1992, ADR-1992): the pipeline traversal before Validate is
// set-up for this test, not its subject, so the item is seeded directly at
// Validate-complete (seedAtStage) — the full yolo path to Done stays covered by
// TestSmokeSingleRepoFullPipeline. Non-vacuity: neutralise the landing contract in
// the engine (e.g. stop attemptMergeOnValidate enabling native auto-merge in train
// mode "off", or stop advanceToQueued in mode "on") and the item never closes /
// never reaches Done, failing the wait below. Wall-clock saved: the ~20-30 min
// Specify → Review traversal.
//
// Out of scope here (better suited to unit/integration tests because
// provoking them deterministically in e2e is hard):
//   - convergence budget exhaustion (Story 3 / SC-003)
//   - mid-flight conflict triggering a bounded rebase (Story 2 / SC-002)
//   - cruise preservation (Story 4 / SC-004 — covered by unit tests of the
//     yolo/cruise gating logic)
//
// Wall-clock: ~5-15 min (+~3 min under train mode "on" for the #1874 exactly-once
// settle wait). Cost: $0 (no Claude invocation — the seed is GitHub-only).
func TestYoloAutoMergeLabel(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	trainMode := resolveTrainMode(t, env)
	t.Logf("bed train mode: %s", trainMode)

	stamp := time.Now().UTC().Format("20060102-150405")
	marker := fmt.Sprintf("auto-merge-yolo-%s", stamp)

	// Seeded at Validate (#1992): the subject is the Validate-complete landing
	// contract, so the item arrives with stage:Validate:complete and a ready member PR
	// instead of being driven Specify → Review at real model, CI and quota cost. The
	// seed gives the member PR's number up front — no WaitForLinkedPR needed.
	logStart := LogOffset(t, env)
	num, prNum, _ := seedAtStage(t, env, env.RepoAlpha, seedspec.Spec{
		Column:      "Validate",
		Title:       fmt.Sprintf("e2e yolo auto-merge (%s)", stamp),
		IssueBody:   fmt.Sprintf("e2e verification of the yolo landing contract (#829, #980). marker=%s", marker),
		ExtraLabels: []string{"fabrik:yolo"},
		Path:        markerPath("TestYoloAutoMergeLabel"),
		PathMode:    seedspec.PathUnique,
		Content:     fmt.Sprintf("# e2e yolo auto-merge marker\n\nmarker=%s\n", marker),
	})
	t.Logf("seeded %s#%d at Validate (PR #%d), marker=%s", env.RepoAlpha, num, prNum, marker)

	if trainMode == "on" {
		t.Run("train-mode=on", func(t *testing.T) {
			WaitForMemberLanded(t, env, env.RepoAlpha, num, 45*time.Minute)
			t.Logf("%s#%d landed (Done or closed) — checking the merge-train path was taken", env.RepoAlpha, num)

			// GitHub auto-marks a PR MERGED the instant its head commits become
			// reachable from the base branch — even if Fabrik never called the
			// merge API on that PR. When the member merged cleanly into the
			// trial branch, that already happened by the time the
			// integration/singleton PR lands, so the member's own PR is MERGED
			// here, not CLOSED — and the engine's own close-the-member-PR call
			// harmlessly 404/422s (see #1271). CLOSED only happens when the
			// trial needed conflict resolution. Do NOT "fix" this back to a
			// CLOSED-only assertion: both are legitimate terminal states. The
			// invariant that actually matters — that the merge went through the
			// train and not around it — is verified below from the engine's own
			// landed comment, which names the path that merged it.
			//
			// Two landing shapes are legitimate. Under the trial paths the
			// landing PR is a DISTINCT integration/singleton PR, and the
			// member's own PR citing itself would be a direct-merge regression.
			// Under the singleton fast path (#1644) the member's own PR IS the
			// landing PR by design — it is landed directly once the pinned base
			// is already its ancestor, it is mergeable, and its own CI is green
			// and complete. The comment distinguishes the two, so this asserts
			// on the path rather than on distinctness alone: an unvalidated
			// direct merge posts no landed comment at all and still fails, at
			// waitForLandingPRDetail's own timeout.
			landingPRNum, viaFastPath := waitForLandingPRDetail(t, env, env.RepoAlpha, prNum, 5*time.Minute)
			if landingPRNum == prNum && !viaFastPath {
				t.Fatalf("member PR #%d cites itself as the landing PR on a non-fast-path landing — the member's own PR was the merge vehicle, not a separate integration/singleton PR (this would indicate a direct-merge regression)", prNum)
			}
			if landingPRNum != prNum && viaFastPath {
				t.Fatalf("singleton fast path cited PR #%d as the landing PR, but that path lands the member's own PR #%d — the comment and the mechanism disagree", landingPRNum, prNum)
			}
			assertPRMerged(t, env, env.RepoAlpha, landingPRNum)
			if viaFastPath {
				t.Logf("member PR #%d confirmed MERGED via the singleton fast path — train landing contract verified", landingPRNum)
			} else {
				t.Logf("distinct integration/singleton PR #%d confirmed MERGED — train landing contract verified", landingPRNum)
			}

			waitForPRClosed(t, env, env.RepoAlpha, prNum, 5*time.Minute)
			t.Logf("member PR #%d reached a terminal state (closed or merged-by-ancestry)", prNum)

			WaitForLogLine(t, env, "landing complete", logStart, 5*time.Minute)
			t.Logf("bed log shows a completed train landing")

			AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
			t.Logf("fabrik:auto-merge-enabled was never applied — train-on contract verified")

			// #1874 / ADR-1871: whichever landing path took it (fast path or trial),
			// the member landed exactly once. Wired ONLY into this subtest: under
			// merge_train: off the ordinary auto-merge path posts no landing comment,
			// so the comment count has no off-mode counterpart.
			AssertMembersLandedExactlyOnce(t, env, env.RepoAlpha,
				[]landedMember{{Name: "yolo-auto-merge", Issue: num, PR: prNum}}, logStart)
		})
		return
	}

	t.Run("train-mode=off", func(t *testing.T) {
		// Wait for the full pipeline to land the merge. On trivial PRs with no
		// branch protection or required reviews, GitHub may merge within seconds
		// of auto-merge being enabled — so we don't poll for the transient
		// fabrik:auto-merge-enabled label here. We let the issue close and then
		// audit the timeline.
		WaitForIssueClosedWithReviewCheck(t, env, env.RepoAlpha, num, 45*time.Minute)
		t.Logf("%s#%d closed — checking the auto-merge path was taken", env.RepoAlpha, num)

		// Race-free verification that auto-merge enablement happened: scan the
		// issue's timeline for a labeled-with-fabrik:auto-merge-enabled event.
		AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled")
		t.Logf("fabrik:auto-merge-enabled was applied at some point — GitHub native auto-merge path verified")

		// FR-005: the label must be removed when the PR merges. Fabrik needs
		// 1-2 poll cycles (~30-60s) after the merge to observe it and remove the
		// label — checking immediately races the cleanup poll.
		WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:auto-merge-enabled", 5*time.Minute)
		t.Logf("fabrik:auto-merge-enabled was cleaned up after merge — FR-005 verified")
	})
}
