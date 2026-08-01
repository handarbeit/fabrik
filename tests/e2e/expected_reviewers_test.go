//go:build e2e

package e2e

import (
	"slices"
	"testing"
	"time"
)

// This file covers ADR-1283's expected_reviewers (declared unrequested
// reviewers for the review gate) end-to-end (issue #1298), using
// deterministic harness-posted verdicts (SubmitPRReview +
// FABRIK_REVIEWER_TOKEN) — never the bed's COMMENT-only claude-review.yml
// bot — for every verdict assertion. This is the direct #1258 precedent
// applied to a second, list-shaped stage-config field.
//
// Mechanism: all scenarios seed a member PR directly via the GitHub API
// (seedReviewGateItem -> CreateMemberPR, zero Claude cost) against the bed's
// existing Review column/stage (default config — untouched). A declared
// expected_reviewers value is applied per issue via one of two labels passed
// as an extraLabel to seedReviewGateItem:
//
//	expectedReviewersNoneLabel     = "expected-reviewers:none"     -> []string{}
//	expectedReviewersDeclaredLabel = "expected-reviewers:declared" -> []string{expectedReviewersSyntheticName}
//
// Engine support for reading these labels is a separate, decoupled follow-up
// issue (not yet filed as of this PR — see the Implement-stage PR
// description for its exact spec), mirroring how #1261 was kept decoupled
// from #1258. Scenarios that rely on a declared value (fast-advance,
// declared-waits, precedence, composition) cannot pass until that follow-up
// merges and the two label objects exist on the bed repo; the regression
// guard (undeclared/nil) has no such dependency and ships green in this PR
// alone. See adrs/1298-e2e-expected-reviewers-coverage.md.
//
// Bed setup required: FABRIK_REVIEWER_TOKEN (existing prerequisite, reused
// from #1258), and the "expected-reviewers:none" / "expected-reviewers:declared"
// labels must already exist as label objects in the target repo (gh issue
// create --label fails hard, not gracefully, if either doesn't — see README
// prerequisite). Neither is a bed column or stage-YAML variant, and no
// scenario skips for lack of either — a missing label surfaces as a
// t.Fatalf from FileIssue, by design (mirroring #1258's rejection of silent
// skips).
//
// expectedReviewersSyntheticName is deliberately not a real GitHub account
// or an installed bot (e.g. NOT handarbeit-pruefer) — reviewGateAllBots'
// declared-reviewer branch doesn't check IsBot for declared names, so any
// structurally-valid (validateExpectedReviewers-passing) string works, and a
// synthetic name avoids racing a real actor's review against the
// deterministic re-prompt-ladder assertions in
// TestExpectedReviewersDeclaredWaitsAndReprompts.
//
// Scope limitation (see adrs/1298-e2e-expected-reviewers-coverage.md, and
// its #1258 precedent): the landing/auto-merge gate (reviewGateBlocksLanding,
// called only from attemptMergeOnValidate) is reachable ONLY through a stage
// literally named "Validate" (engine/stages.go, engine/poll.go,
// engine/pr_terminal_advance.go all hard-gate on stage.Name == "Validate").
// Seeding an item on Review cannot reach that stage-name-gated path, so this
// file only exercises checkReviewGate (the advance gate) — a documented,
// accepted gap, not a silently missing one.
//
// All scenarios only touch checkReviewGate, which has no FABRIK_MERGE_TRAIN
// branching — they pass identically under both legs of the suite's two-mode
// gate.
const (
	expectedReviewersNoneLabel     = "expected-reviewers:none"
	expectedReviewersDeclaredLabel = "expected-reviewers:declared"
	expectedReviewersSyntheticName = "e2e-synthetic-declared-reviewer"
)

// TestExpectedReviewersFastAdvance covers requirements scenario 1: with
// expected_reviewers: [] declared, no requested reviewer, and no submitted
// review, the gate fast-advances instead of waiting out
// FABRIK_REVIEW_WAIT_TIMEOUT — the #1080 stall this feature exists to
// eliminate. Requires the follow-up engine-side label-read issue to be
// merged; see the file-level doc comment.
//
// Wall-clock: ~2-5 min (a short window well under FABRIK_REVIEW_WAIT_TIMEOUT).
func TestExpectedReviewersFastAdvance(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "expected-none-fast-advance", expectedReviewersNoneLabel)
	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)

	// The gate must never apply fabrik:awaiting-review at all — a fast
	// advance clears before the label-apply branch is ever reached. Poll for
	// a bounded window comfortably shorter than FABRIK_REVIEW_WAIT_TIMEOUT so
	// a regressed (non-fast-advancing) gate is distinguishable from a
	// fast-advancing one within this test's own timeout, not just by the
	// absence of the label at the very end.
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if labels, err := tryIssueLabels(env, env.RepoAlpha, num); err == nil && slices.Contains(labels, "fabrik:awaiting-review") {
			t.Fatalf("fabrik:awaiting-review applied to %s#%d despite expected_reviewers: [] and nothing requested/reviewed — "+
				"gate did not fast-advance (reviewGateFastAdvance not firing, or the follow-up engine label-read issue not merged)",
				env.RepoAlpha, num)
		}
		pollSleep(pollBase())
	}
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("R1 verified: %s#%d fast-advanced — fabrik:awaiting-review never applied with expected_reviewers: [] declared", env.RepoAlpha, num)
}

// TestExpectedReviewersDeclaredWaitsAndReprompts covers requirements
// scenario 2: with expected_reviewers: [<name>] declared and no requested
// reviewer, the gate waits (does not fast-advance) and the bot re-prompt
// ladder engages — behavior undeclared config cannot reach at all (this is
// the first e2e coverage of the bot re-prompt ladder). Folds Phase 1 and
// Phase 2 into one continuation, mirroring how #1258 folded its scenario 5
// into scenario 1's test. Requires the follow-up engine-side label-read
// issue to be merged; see the file-level doc comment.
//
// Wall-clock (worst case): ~2xFABRIK_REVIEW_WAIT_TIMEOUT + buffer — see
// README for a recommended bed value.
func TestExpectedReviewersDeclaredWaitsAndReprompts(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "expected-declared-reprompt", expectedReviewersDeclaredLabel)
	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)

	// Contrast with TestExpectedReviewersFastAdvance: a declared-but-unmatched
	// reviewer must NOT fast-advance — fabrik:awaiting-review is applied
	// immediately, same as the undeclared default.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d — declared reviewer holds the gate open", env.RepoAlpha, num)

	// Phase 1: after one ReviewWaitTimeout window with no response, the
	// engine re-prompts the declared identity via a direct @mention comment
	// (no PR review-request mutation, since it was never formally requested)
	// and applies fabrik:bot-reprompted.
	phase1Wait := time.Duration(reviewWaitTimeout+10) * time.Minute
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:bot-reprompted", phase1Wait)
	t.Logf("fabrik:bot-reprompted appeared on %s#%d — Phase 1 re-prompt fired", env.RepoAlpha, num)

	WaitForPRCommentContainingAny(t, env, env.RepoAlpha, prNum,
		[]string{"@" + expectedReviewersSyntheticName}, 5*time.Minute)
	t.Logf("Phase 1 re-prompt comment on %s PR #%d mentions the declared reviewer %q", env.RepoAlpha, prNum, expectedReviewersSyntheticName)

	// Phase 2: the synthetic name never actually reviews, so after a second
	// full timeout window the engine gives up and pauses for a human,
	// removing both fabrik:bot-reprompted and fabrik:awaiting-review.
	phase2Wait := time.Duration(reviewWaitTimeout+10) * time.Minute
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:paused", phase2Wait)
	t.Logf("fabrik:paused appeared on %s#%d — Phase 2 timed out with no response from the declared reviewer", env.RepoAlpha, num)
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-input", 5*time.Minute)

	if state := IssueState(t, env, env.RepoAlpha, num); state != "OPEN" {
		t.Fatalf("expected issue OPEN after declared-reviewer Phase 2 timeout, got %s on %s#%d", state, env.RepoAlpha, num)
	}
	t.Logf("R2 verified: %s#%d — declared reviewer held the gate open, Phase 1 re-prompted, Phase 2 paused for human", env.RepoAlpha, num)
}

// TestExpectedReviewersPrecedenceGuard covers requirements scenario 3: a
// genuinely requested reviewer (non-empty outstanding) overrides
// expected_reviewers: [] — the declaration narrows waiting for *unrequested*
// reviewers only and must never bypass wait_for_reviews for a reviewer
// GitHub is actually tracking. Requires the follow-up engine-side
// label-read issue to be merged; see the file-level doc comment.
//
// (The other half of "precedence" — an already-submitted review overriding
// expected_reviewers: [] — is definitionally the same pre-#1283
// hasReviews-clearing path every existing review-gate scenario already
// exercises, since reviewGateFastAdvance short-circuits on hasReviews before
// the declaration is even consulted; it is not re-asserted here as it would
// be redundant, not a new assertion. See adrs/1298-e2e-expected-reviewers-coverage.md.)
//
// Wall-clock: ~5-10 min.
func TestExpectedReviewersPrecedenceGuard(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "expected-none-precedence", expectedReviewersNoneLabel)
	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)

	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); reviewerLogin == engineLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	t.Logf("requested reviewer %q on %s PR #%d despite expected_reviewers: [] being declared", reviewerLogin, env.RepoAlpha, prNum)

	// A genuinely outstanding requested reviewer must hold the gate open —
	// the empty declaration must not fast-advance past it.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d — requested reviewer overrides expected_reviewers: []", env.RepoAlpha, num)

	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "APPROVE")
	t.Logf("submitted APPROVE review on %s PR #%d", env.RepoAlpha, prNum)

	WaitForLabelAbsent(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	t.Logf("R3 verified: %s#%d — gate held for the requested reviewer despite expected_reviewers: [], cleared once they reviewed", env.RepoAlpha, num)
}

// TestExpectedReviewersUndeclaredRegressionGuard covers requirements
// scenario 4: undeclared (nil) expected_reviewers still never fast-advances
// — proves the shipped default (FR-5) is unchanged. Seeds with no extra
// label at all, against the bed's existing, untouched Review stage — this
// pins the `expected != nil` half of reviewGateFastAdvance specifically:
// it fails if fast-advance were ever changed to treat nil the same as an
// empty slice. Zero dependency on the follow-up engine issue; ships green
// in this PR alone.
//
// Wall-clock: ~2-5 min.
func TestExpectedReviewersUndeclaredRegressionGuard(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "expected-nil-regression")
	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)

	// Undeclared (nil) must behave exactly as it did before #1283: the gate
	// blocks unconditionally on "nothing requested, nothing reviewed yet",
	// applying fabrik:awaiting-review immediately rather than fast-advancing.
	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("R4 verified: %s#%d — undeclared (nil) expected_reviewers still applies fabrik:awaiting-review, never fast-advances", env.RepoAlpha, num)
}

// TestExpectedReviewersFastAdvanceComposesWithAuthoritative covers
// requirements scenario 5: expected_reviewers: [] fast-advance composes with
// review_authority: authoritative — per reviewGateFastAdvance's doc comment,
// this path is deliberately independent of authority because it only fires
// when hasReviews is false (before any verdict could be weighed). Worth
// pinning so a future change can't couple them and recreate #1080 for
// authoritative stages. Requires the follow-up engine-side label-read issue
// (for expected-reviewers:none) AND #1261 (for review-authority:authoritative,
// already merged) to be in effect; see the file-level doc comment.
//
// Wall-clock: ~2-5 min.
func TestExpectedReviewersFastAdvanceComposesWithAuthoritative(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "expected-none-authoritative",
		expectedReviewersNoneLabel, "review-authority:authoritative")
	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)

	// Same bounded-window assertion as TestExpectedReviewersFastAdvance:
	// fast-advance must fire before any authority-verdict branch is ever
	// reached (that branch only activates once hasReviews is true, which
	// never happens here — nothing is ever reviewed).
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if labels, err := tryIssueLabels(env, env.RepoAlpha, num); err == nil && slices.Contains(labels, "fabrik:awaiting-review") {
			t.Fatalf("fabrik:awaiting-review applied to %s#%d despite expected_reviewers: [] and review-authority:authoritative — "+
				"fast-advance did not fire ahead of the authority-verdict branch", env.RepoAlpha, num)
		}
		pollSleep(pollBase())
	}
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("R5 verified: %s#%d fast-advanced despite review-authority:authoritative — fast-advance composes independently of authority mode", env.RepoAlpha, num)
}
