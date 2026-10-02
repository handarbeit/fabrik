//go:build e2e

package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCommentLandingGateHolds is the live-e2e proof of the #1862 landing gate
// (ADR-1862, engine/comment_landing_gate.go): an unprocessed human comment
// blocks the landing decision until it has been processed, and only then does
// the item land. Runs in both train modes and both auth legs; the gate sits in
// attemptMergeOnValidate ahead of the merge-train fork, so the hold is the same
// `comment-gate` line either way.
//
// NOTE — deviation from the issue text. The issue names the `advance` line
// ("skipping stage %q — %d unprocessed comment(s) pending") as the blocked
// transition under merge_train: on. That line (engine/poll.go) fires only for
// NON-Validate stages. At Validate the hold is `comment-gate` under BOTH modes,
// and advanceToQueued is only reached after the gate clears. So this scenario
// asserts `comment-gate` in both modes, and under merge_train: on additionally
// asserts the item is never Queued while the comment is unprocessed.
//
//   - A1: the `[#N comment-gate] holding landing decision: …` line appears; at
//     that point, and at every poll until the comment is processed, the issue is
//     open, the PR is unmerged, fabrik:auto-merge-enabled is absent and the
//     board Status is not Queued.
//   - A2: the comment gets 👀 then 🚀 (eyes.created_at <= rocket.created_at) and
//     only afterwards does the item land (issue closed_at >= rocket.created_at;
//     merged_at too when the member PR itself merged). fabrik:paused is never
//     applied and no "comment not applied" reply is posted (the work had not
//     landed when the comment was processed).
//
// # Deterministic placement (no timing luck)
//
// The item is seeded with fabrik:yolo, stage:Validate:complete, a non-draft
// member PR on fabrik/issue-<N> and NO board Status, so the engine cannot act
// on it. The scenario then waits for the PR's slow-gate to go green and submits
// a reviewer-token APPROVE (so neither the CI gate, the review gate nor
// reviewGateBlocksLanding can claim the item first), posts the human comment,
// verifies it through REST, waits for GitHub to finish computing the PR's
// mergeability (so the merge gate cannot claim it on an "unknown"
// mergeable_state either), and only THEN moves the item into Validate. The
// engine's first landing evaluation therefore always sees the comment.
//
// # Identity
//
// The comment is posted by FABRIK_TOKEN's account (arbeithand): a human to Fabrik
// in both legs (see postHumanIssueComment). FABRIK_REVIEWER_TOKEN is used only for
// the approval, never to classify. The scenario skips cleanly without it, since a
// bed relying on Pruefer's review with a long timeout could apply fabrik:paused
// and corrupt A1/A2.
//
// Wall-clock: ~20–35 min per train mode and auth leg (one slow-gate CI wait ~10
// min, one comment-review Claude invocation, then the landing — train mode adds
// a trial CI cycle). Cost: ~$0.10–0.50 per run.
func TestCommentLandingGateHolds(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)
	repo := env.RepoAlpha
	assertSlowGateRequired(t, env, repo)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — the scenario needs a deterministic non-author APPROVE so the review gate " +
			"cannot decide when the item lands (a review-wait timeout would apply fabrik:paused and corrupt the assertions)")
	}
	trainMode := resolveTrainMode(t, env)
	t.Logf("bed train mode: %s", trainMode)

	issue, pr, itemID := seedLandingCandidate(t, env, repo, "main", "gate", "e2e/comment-landing/gate.md", "fabrik:yolo")

	// Clear the CI and review gates before the item is visible to the engine.
	WaitForCheckConclusion(t, env, repo, pr, "slow-gate", "success", 30*time.Minute)
	SubmitPRReview(t, env, reviewerToken, repo, pr, "APPROVE")

	tipBefore, err := restPRField(env, repo, pr, ".head.sha")
	if err != nil || tipBefore == "" {
		t.Fatalf("read PR #%d head sha: %v", pr, err)
	}

	// Post the unprocessed comment while the item has no Status, and confirm it
	// is readable before exposing the item.
	commentID := postHumanIssueComment(t, env, repo, issue,
		"Reviewer note: please double-check the wording of this change. No code change is required — acknowledge only, do not modify or push anything.")
	if !commentExistsViaREST(env, repo, commentID) {
		t.Fatalf("comment %d on %s#%d is not yet readable via REST — refusing to expose the item to the engine", commentID, repo, issue)
	}

	// GitHub recomputes mergeability lazily (after the approval, or whenever the
	// base moves under a concurrent test's merge). Until it is computed, the merge
	// gate claims the item and the comment is processed before any landing
	// decision runs, so the scenario would observe nothing.
	AwaitPRMergeableSettled(t, env, repo, pr, 10*time.Minute)

	offset := LogOffset(t, env)
	SetIssueStatus(t, env, itemID, "Validate")
	AwaitStatusVisible(t, env, repo, issue, "Validate", awaitSeedTimeout)
	t.Logf("comment %d posted, item moved into Validate (train mode %s); scanning bed log from offset %d", commentID, trainMode, offset)

	// assertHeld fails if the item shows any sign of having landed (or been queued)
	// while the comment has no 🚀. A violation is re-checked against the reactions
	// first: reacting and landing can straddle one read, which is legitimate.
	assertHeld := func(when string) {
		t.Helper()
		var problems []string
		if state, err := tryIssueState(env, repo, issue); err == nil && state != "OPEN" {
			problems = append(problems, "issue state "+state)
		}
		if merged, err := restPRField(env, repo, pr, ".merged"); err == nil && merged == "true" {
			problems = append(problems, "PR merged")
		}
		if labels, err := tryIssueLabels(env, repo, issue); err == nil && slices.Contains(labels, "fabrik:auto-merge-enabled") {
			problems = append(problems, "fabrik:auto-merge-enabled applied")
		}
		if s := projectStatus(t, env, repo, issue); strings.TrimSpace(s) == "Queued" {
			problems = append(problems, "board Status Queued")
		}
		if len(problems) == 0 {
			return
		}
		if _, r, err := commentReactionTimes(env, repo, commentID); err == nil && !r.IsZero() {
			return
		}
		t.Fatalf("A1 (%s): the item moved toward landing (%s) while comment %d on %s#%d was unprocessed (no 🚀) — the #1862 gate did not hold",
			when, strings.Join(problems, ", "), commentID, repo, issue)
	}

	// --- A1: the gate engages. ---
	// A precondition guard (#1973): if the engine never reached the landing decision
	// with the comment pending (a Phase 1 CI/review gate claimed the item first) the
	// gate under test was never exercised. Only this TIMEOUT is inconclusive;
	// assertHeld and every later check stay Fatalf.
	holdLine := waitForLogMatchInconclusive(t, env, offset, 15*time.Minute,
		"the comment-gate hold line for #"+strconv.Itoa(issue)+" — the engine never reached the landing decision with the comment pending "+
			"(a Phase 1 CI/review gate may have claimed the item first)",
		func(l string) bool { return countCommentGateHolds([]string{l}, issue) > 0 })
	t.Logf("A1: gate engaged: %s", strings.TrimSpace(holdLine))
	assertHeld("at the hold line")

	// --- A2: processed (👀 then 🚀), and only then landed. ---
	eyes, rocket := waitForCommentRocket(t, env, repo, commentID, 40*time.Minute, func() { assertHeld("while awaiting processing") })
	if !reactionsOrdered(eyes, rocket) {
		t.Fatalf("A2: comment %d reactions out of order or 👀 missing: eyes=%v rocket=%v", commentID, eyes, rocket)
	}
	t.Logf("A2: comment processed: 👀 %s, 🚀 %s", eyes.Format(time.RFC3339), rocket.Format(time.RFC3339))

	tipAfter, err := restPRField(env, repo, pr, ".head.sha")
	if err != nil || tipAfter != tipBefore {
		t.Fatalf("PR #%d head moved during comment processing (%s -> %s, err=%v) — the comment worker pushed, "+
			"which would re-run Validate and invalidate the landing-order assertion", pr, tipBefore, tipAfter, err)
	}

	WaitForMemberLanded(t, env, repo, issue, 60*time.Minute)

	closedAt, err := issueClosedAt(env, repo, issue)
	if err != nil {
		t.Fatalf("read closed_at of %s#%d: %v", repo, issue, err)
	}
	if !landedAtOrAfter(rocket, closedAt) {
		t.Fatalf("A2: %s#%d closed at %v, before the comment's 🚀 at %v — the item landed before the comment was processed",
			repo, issue, closedAt, rocket)
	}
	if mergedAt, err := prMergedAt(env, repo, pr); err == nil && !mergedAt.IsZero() && !landedAtOrAfter(rocket, mergedAt) {
		t.Fatalf("A2: PR #%d merged at %v, before the comment's 🚀 at %v", pr, mergedAt, rocket)
	}

	bodies, err := tryPRComments(env, repo, issue)
	if err != nil {
		t.Fatalf("read comments on %s#%d: %v", repo, issue, err)
	}
	if n := countMatching(bodies, isPostMergeReply); n != 0 {
		t.Fatalf("%d \"comment not applied\" reply(ies) on %s#%d — the comment was processed AFTER the work landed, not before", n, repo, issue)
	}
	AssertLabelWasNeverApplied(t, env, repo, issue, "fabrik:paused")
	t.Logf("TestCommentLandingGateHolds passed (train mode %s): held until 🚀, then landed (closed %s)", trainMode, closedAt.Format(time.RFC3339))
}
