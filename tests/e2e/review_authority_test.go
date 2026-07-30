//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"
)

// This file covers ADR-1250's review_authority: authoritative mode
// end-to-end (issue #1258), using deterministic harness-posted verdicts
// (SubmitPRReview + FABRIK_REVIEWER_TOKEN) — never the bed's COMMENT-only
// claude-review.yml bot — for every verdict assertion.
//
// Mechanism: all four scenarios seed a member PR directly via the GitHub API
// (CreateMemberPR, zero Claude cost) against the bed's existing Review
// column/stage (default, advisory config — untouched). Authoritative mode is
// applied per issue via the "review-authority:authoritative" label passed as
// an extraLabel to seedReviewGateItem; engine support for that label is
// #1261 (filed, decoupled from this issue — these scenarios cannot pass
// until both #1261 and this issue are merged). This exercises checkReviewGate
// (the advance gate) and its shared pure predicate reviewGateAuthorityVerdict
// without introducing any bed-local board column or stage-YAML variant — an
// earlier design that did so was rejected because review_authority is a
// property of a stage's config, not a distinct kind of stage, and because a
// bed prerequisite the operator hadn't yet set up would silently skip 3 of
// the 4 scenarios, letting the suite go green having validated zero
// authoritative behavior. See adrs/1258-e2e-review-authority-coverage.md.
//
// No bed setup is required beyond FABRIK_REVIEWER_TOKEN (see README) — none
// of these scenarios skip for lack of bed prerequisites.
//
// Scope limitation (see adrs/1258-e2e-review-authority-coverage.md): the
// landing/auto-merge gate (reviewGateBlocksLanding, called only from
// attemptMergeOnValidate) is reachable ONLY through a stage literally named
// "Validate" (engine/stages.go, engine/poll.go, engine/pr_terminal_advance.go
// all hard-gate on stage.Name == "Validate"). Applying the authority label to
// an item sitting on Review cannot reach that stage-name-gated path, so
// scenarios 2/3 below assert the gate *clears* (fabrik:awaiting-review
// disappears, fabrik:paused never applied), not that the item merges —
// reviewGateBlocksLanding's wiring around the same shared predicate is a
// documented, accepted e2e gap, not a silently missing one.
//
// All four scenarios only touch checkReviewGate, which has no
// FABRIK_MERGE_TRAIN branching — they pass identically under both legs of
// the suite's two-mode gate.

// TestReviewAuthorityBlocksAndPausesOnChangesRequested covers scenarios 1 and
// 5 from issue #1258: authoritative + CHANGES_REQUESTED blocks advancement,
// and — folded in as a continuation, since authoritative mode cannot reach
// the separate MaxReviewCycles path (dispatchReviewReinvoke only fires after
// the gate has already cleared) — a verdict that never clears eventually
// pauses via checkAwaitingReviewTimeout, with the authoritative pause reason
// naming the verdict rather than the misleading "no reviews submitted yet".
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) to be merged.
//
// Wall-clock: ~FABRIK_REVIEW_WAIT_TIMEOUT + 10 min. Use a short bed value
// (e.g. 2) for a fast iteration run — see README.
func TestReviewAuthorityBlocksAndPausesOnChangesRequested(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}
	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "blocks-pauses", "review-authority:authoritative")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	if engineLogin, reviewerLogin := TokenLogin(t, env.GHToken), TokenLogin(t, reviewerToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d", env.RepoAlpha, prNum)

	// Scenario 1: the gate must block — fabrik:awaiting-review appears, and
	// the item must not advance past Review (the durable, directly-observable
	// signal is that the gate label is applied and fabrik:paused is not yet
	// present).
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("fabrik:awaiting-review confirmed on %s#%d — authoritative gate is blocking on CHANGES_REQUESTED", env.RepoAlpha, num)

	// Scenario 5: wait out the review timeout and confirm the pause fires
	// with the authoritative reason, not the generic "no reviews submitted
	// yet" message — this is the distinguishing assertion for AC1 and AC5.
	timeoutWait := time.Duration(reviewWaitTimeout+10) * time.Minute
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:paused", timeoutWait)
	t.Logf("fabrik:paused appeared on %s#%d (authoritative review gate timed out)", env.RepoAlpha, num)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)

	if state := IssueState(t, env, env.RepoAlpha, num); state != "OPEN" {
		t.Fatalf("expected issue OPEN after authoritative review timeout, got %s on %s#%d", state, env.RepoAlpha, num)
	}

	// WaitForPRCommentContaining queries the /issues/<n>/comments REST
	// endpoint, which is identical for issue and PR numbers on GitHub — the
	// pause comment is posted on the issue (postComment uses item.Number),
	// not the PR, so passing the issue number here is correct.
	WaitForPRCommentContaining(t, env, env.RepoAlpha, num, "requested changes", 5*time.Minute)
	t.Logf("pause comment names the authoritative verdict (\"requested changes\") on %s#%d", env.RepoAlpha, num)

	bodies, err := tryPRComments(env, env.RepoAlpha, num)
	if err != nil {
		t.Fatalf("read comments on %s#%d: %v", env.RepoAlpha, num, err)
	}
	for _, b := range bodies {
		if strings.Contains(strings.ToLower(b), "no reviews submitted yet") {
			t.Fatalf("pause comment on %s#%d used the generic \"no reviews submitted yet\" message "+
				"instead of the authoritative verdict reason — authorityReason did not propagate: %q",
				env.RepoAlpha, num, b)
		}
	}
	t.Logf("R5 verified: pause reason reflects the CHANGES_REQUESTED verdict, not the generic message")
}

// TestReviewAuthorityClearsOnApproval covers scenario 2: authoritative +
// APPROVED clears the gate. Descoped from "merges" to "gate clears" — see
// the file-level doc comment on the landing-gate scope limitation.
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) to be merged.
//
// Wall-clock: ~2-5 min.
func TestReviewAuthorityClearsOnApproval(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "clears-approval", "review-authority:authoritative")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	if engineLogin, reviewerLogin := TokenLogin(t, env.GHToken), TokenLogin(t, reviewerToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "APPROVE")
	t.Logf("submitted APPROVE review on %s PR #%d", env.RepoAlpha, prNum)

	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("fabrik:awaiting-review cleared on %s#%d — authoritative gate cleared on APPROVED", env.RepoAlpha, num)

	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:paused")
	t.Logf("R2 verified: %s#%d never paused; authoritative gate cleared on approval", env.RepoAlpha, num)
}

// TestReviewAuthorityYoloDoesNotBypassBlock covers scenario 3 — the core
// composition guarantee from ADR-1250: fabrik:yolo does not bypass an
// authoritative gate. The item stays blocked while CHANGES_REQUESTED stands,
// and only clears once approved. Descoped from "merges under yolo" to "gate
// clears" for the same reason as TestReviewAuthorityClearsOnApproval.
//
// Requires #1261 (engine support for the review-authority:authoritative
// label) to be merged.
//
// Wall-clock: ~5-10 min.
func TestReviewAuthorityYoloDoesNotBypassBlock(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "yolo-no-bypass", "review-authority:authoritative", "fabrik:yolo")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	if engineLogin, reviewerLogin := TokenLogin(t, env.GHToken), TokenLogin(t, reviewerToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d (fabrik:yolo present)", env.RepoAlpha, prNum)

	// The gate must block even though yolo is set — a bounded withheld window
	// is sufficient here (unlike the first test, we don't need to wait out
	// the full timeout; we only need to confirm yolo doesn't short-circuit
	// the block).
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	blockedDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(blockedDeadline) {
		if state, err := tryIssueState(env, env.RepoAlpha, num); err == nil && state == "CLOSED" {
			t.Fatalf("yolo bypassed the authoritative gate — %s#%d closed while CHANGES_REQUESTED still stood", env.RepoAlpha, num)
		}
		pollSleep(pollBase())
	}
	t.Logf("yolo did not bypass the block: %s#%d still OPEN with fabrik:awaiting-review after CHANGES_REQUESTED", env.RepoAlpha, num)

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "APPROVE")
	t.Logf("submitted APPROVE review on %s PR #%d", env.RepoAlpha, prNum)

	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("R3 verified: %s#%d — yolo composed correctly: blocked on CHANGES_REQUESTED, cleared on APPROVE", env.RepoAlpha, num)
}

// TestReviewAuthorityAdvisoryRegressionGuard covers scenario 4: the
// regression guard proving the additive authoritative check inside
// checkReviewGate does not leak into advisory (default) mode. Runs against
// the existing "Review" column/stage with no authority label at all
// (untouched default config), so it ships green independent of #1261.
//
// This test would fail today if the authoritative check were made
// non-additive (e.g. if it replaced rather than extended the
// len(outstanding)==0 && hasReviews condition): a CHANGES_REQUESTED review
// on an advisory-mode stage must still clear the gate, exactly as it did
// before ADR-1250.
//
// Wall-clock: ~2-5 min.
func TestReviewAuthorityAdvisoryRegressionGuard(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "advisory-regression")

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	if engineLogin, reviewerLogin := TokenLogin(t, env.GHToken), TokenLogin(t, reviewerToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d (advisory Review stage)", env.RepoAlpha, prNum)

	// Advisory mode: any submitted review (regardless of verdict) with no
	// outstanding requested reviewers clears the gate — this is the
	// pre-#1250 behavior and must be unchanged.
	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("R4 verified: %s#%d — advisory gate cleared on CHANGES_REQUESTED (additive check did not narrow the default path)", env.RepoAlpha, num)
}
