//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strings"
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
// declared-waits, composition) cannot pass until that follow-up merges and
// the two label objects exist on the bed repo; the regression guard
// (undeclared/nil) has no such dependency and ships green in this PR alone.
// See adrs/1298-e2e-expected-reviewers-coverage.md.
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
//
// Determinism (#1312): four scenarios below (FastAdvance,
// DeclaredWaitsAndReprompts, UndeclaredRegressionGuard,
// FastAdvanceComposesWithAuthoritative) seed via seedReviewGateItemDraft
// instead of seedReviewGateItem — their properties under test ("nothing was
// requested", "declared but unrequested") would be falsified by
// RequestPRReviewer, so unlike the review_authority_test.go fix they can't
// use a genuinely outstanding reviewer to make the wait deterministic.
// Opening the member PR as a draft keeps the bed's claude-review.yml bot
// from ever reviewing it (its job is guarded by
// `if: github.event.pull_request.draft == false`, triggering only on
// opened/ready_for_review), removing the incidental review entirely rather
// than racing it.
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
// Uses seedReviewGateItemDraft (#1312): this assertion can't currently fail
// against an incidental bot review (a race there falls through to the
// ordinary outer clearing branch, which also never applies the label), but a
// race would silently stop this test from exercising the FR-2 fast-advance
// code path it's named for. The draft-PR construction is coverage integrity,
// not a correctness fix.
//
// Wall-clock: ~2-5 min (a short window well under FABRIK_REVIEW_WAIT_TIMEOUT).
func TestExpectedReviewersFastAdvance(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItemDraft(t, env, env.RepoAlpha, "main", "Review", "expected-none-fast-advance", expectedReviewersNoneLabel)
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
// Uses seedReviewGateItemDraft (#1312): this scenario's entire mechanism
// depends on nothing being reviewed until the Phase 1/2 timeouts fire, but
// expected_reviewers is not consulted by checkReviewGate's outer clearing
// branch — an incidental bot COMMENT review would clear the gate in advisory
// mode before Phase 1 can ever be reached, silently preventing the
// re-prompt ladder (this test's actual subject) from engaging at all.
// RequestPRReviewer isn't an option here either: a genuinely outstanding
// *requested* reviewer routes to the mixed/human pause path instead of the
// bot ladder (reviewGateAllBots). The draft PR is never marked ready for
// the duration of this test, so the bed's claude-review.yml never reviews
// it and there is no incidental review to race.
//
// Wall-clock (worst case): ~2xFABRIK_REVIEW_WAIT_TIMEOUT + buffer — see
// README for a recommended bed value.
func TestExpectedReviewersDeclaredWaitsAndReprompts(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)

	num, prNum, _ := seedReviewGateItemDraft(t, env, env.RepoAlpha, "main", "Review", "expected-declared-reprompt", expectedReviewersDeclaredLabel)
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

// TestExpectedReviewersUndeclaredRegressionGuard covers requirements
// scenario 4: undeclared (nil) expected_reviewers still never fast-advances
// — proves the shipped default (FR-5) is unchanged. Seeds with no extra
// label at all, against the bed's existing, untouched Review stage — this
// pins the `expected != nil` half of reviewGateFastAdvance specifically:
// it fails if fast-advance were ever changed to treat nil the same as an
// empty slice. Zero dependency on the follow-up engine issue; ships green
// in this PR alone.
//
// Uses seedReviewGateItemDraft (#1312): the property under test is "nothing
// requested, nothing reviewed yet" — RequestPRReviewer would falsify that
// premise, so it can't be used to make the wait deterministic the way the
// TestReviewAuthority* scenarios do. A draft PR means the bed's
// claude-review.yml bot never reviews it, so there is no incidental review
// to race against the engine's first gate evaluation.
//
// Wall-clock: ~2-5 min.
func TestExpectedReviewersUndeclaredRegressionGuard(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItemDraft(t, env, env.RepoAlpha, "main", "Review", "expected-nil-regression")
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
// Uses seedReviewGateItemDraft (#1312): this is the opposite-direction risk
// of the WaitForIssueLabel-precondition race — an incidental bot review
// landing before fast-advance's own check would make hasReviews true, and in
// authoritative mode with no branch-protection review requirement configured
// that falls back to "satisfied unless CHANGES_REQUESTED", which a bare
// COMMENT is not, so the gate would still apply fabrik:awaiting-review via
// the authoritative-verdict branch rather than fast-advancing — failing this
// bounded "must never appear" assertion in the opposite direction from a
// timeout. A draft PR removes the incidental review entirely.
//
// Wall-clock: ~2-5 min.
func TestExpectedReviewersFastAdvanceComposesWithAuthoritative(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	num, prNum, _ := seedReviewGateItemDraft(t, env, env.RepoAlpha, "main", "Review", "expected-none-authoritative",
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

// TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation covers AC2
// (Finding 2/R2, #1375): a stage-declared expected_reviewers bot must never
// defer an outstanding human reviewer's authoritative escalation behind its
// own re-prompt ladder. Before the fix, reviewGateAllBots's declared-reviewer
// branch (len(outstanding)==0) saw only the declared bot once the human
// responded and returned true — routing the item into the bot re-prompt
// ladder (Phase 1: an @mention comment to the synthetic declared name,
// fabrik:bot-reprompted applied) instead of the human's authoritative
// escalation, deferring a CHANGES_REQUESTED verdict the declared bot cannot
// possibly resolve. Per the Verification Note, this must fail against
// current main.
//
// Unlike this file's other scenarios, this test seeds via seedReviewGateItem
// (not …Draft) and calls RequestPRReviewer: the property under test requires
// a genuinely outstanding *human* reviewer, mirroring
// tests/e2e/review_authority_test.go's determinism technique (#1312) rather
// than this file's usual "nothing requested" construction.
//
// Human requested (real outstanding reviewer) + bot declared via
// expected-reviewers:declared + review-authority:authoritative. The human
// submits REQUEST_CHANGES; the human path must win. Per R1's reinvoke model
// (#1375), "the human path wins" concretely means the review-reinvoke fires
// to address the feedback (Finding 1) — this test asserts that AND that the
// declared bot's ladder never engages, waiting out a full
// FABRIK_REVIEW_WAIT_TIMEOUT window (the window Phase 1 would fire in, if
// buggy, timed from the original fabrik:awaiting-review application — not
// from the review submission) to make the absence conclusive rather than a
// race.
//
// Wall-clock (worst case): ~FABRIK_REVIEW_WAIT_TIMEOUT + ~15 min.
func TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation(t *testing.T) {
	t.Parallel()
	env := LoadEnv(t)
	AssertFabrikRunning(t, env)

	reviewerToken := readEnvFileReviewerToken(t, env)
	if reviewerToken == "" {
		t.Skip("FABRIK_REVIEWER_TOKEN not set in test bed .env — required for deterministic verdict scenarios")
	}
	reviewWaitTimeout := readEnvFileReviewWaitTimeout(t, env)

	num, prNum, _ := seedReviewGateItem(t, env, env.RepoAlpha, "main", "Review", "declared-bot-no-defer",
		"review-authority:authoritative", expectedReviewersDeclaredLabel)

	AssertPRAuthorIsExpectedIdentity(t, env, env.RepoAlpha, prNum)
	reviewerLogin := TokenLogin(t, reviewerToken)
	if engineLogin := TokenLogin(t, env.GHToken); engineLogin == reviewerLogin {
		t.Fatalf("FABRIK_REVIEWER_TOKEN resolves to %q, the same identity as the engine/PR author — "+
			"set FABRIK_REVIEWER_TOKEN to a distinct GitHub account's PAT", reviewerLogin)
	}

	// Request the reviewer so outstanding is genuinely non-empty by
	// construction, before confirming the gate's pre-verdict block — same
	// determinism technique as review_authority_test.go (#1312).
	RequestPRReviewer(t, env, env.RepoAlpha, prNum, reviewerLogin)
	t.Logf("requested reviewer %q on %s PR #%d — outstanding is non-empty by construction", reviewerLogin, env.RepoAlpha, prNum)

	WaitForIssueLabel(t, env, env.RepoAlpha, num, "fabrik:awaiting-review", 10*time.Minute)
	AssertLabelWasApplied(t, env, env.RepoAlpha, num, "fabrik:awaiting-review")
	t.Logf("fabrik:awaiting-review confirmed on %s#%d before any review submitted — human requested, bot %q declared",
		env.RepoAlpha, num, expectedReviewersSyntheticName)

	logOffset := LogOffset(t, env)
	SubmitPRReview(t, env, reviewerToken, env.RepoAlpha, prNum, "REQUEST_CHANGES")
	t.Logf("submitted REQUEST_CHANGES review on %s PR #%d — outstanding now empty, declared bot still unresponsive", env.RepoAlpha, prNum)

	// The human path must win: the reinvoke fires to address the feedback
	// (Finding 1, #1375) — this alone would already fail against current
	// main, where the authoritative reinvoke path is unreachable while
	// blocked.
	WaitForLogLine(t, env, fmt.Sprintf("[#%d review-reinvoke] re-invoking stage", num), logOffset, 10*time.Minute)
	t.Logf("review-reinvoke dispatched for %s#%d on the human's CHANGES_REQUESTED verdict", env.RepoAlpha, num)

	// Wait out a full FABRIK_REVIEW_WAIT_TIMEOUT window from the original
	// fabrik:awaiting-review application — the window Phase 1's bot
	// re-prompt would fire in, if reviewGateAllBots incorrectly read
	// allBots=true for the still-outstanding declared bot once the human's
	// verdict dropped outstanding to empty. Poll throughout rather than a
	// single sleep, failing immediately (not just at the end) if the wrong
	// escalation fires.
	deadline := time.Now().Add(time.Duration(reviewWaitTimeout+10) * time.Minute)
	for time.Now().Before(deadline) {
		if labels, err := tryIssueLabels(env, env.RepoAlpha, num); err == nil && slices.Contains(labels, "fabrik:bot-reprompted") {
			t.Fatalf("fabrik:bot-reprompted applied to %s#%d — the declared bot's re-prompt ladder fired instead of "+
				"deferring to the human's authoritative CHANGES_REQUESTED escalation (Finding 2 regression)", env.RepoAlpha, num)
		}
		pollSleep(pollBase())
	}
	AssertLabelWasNeverApplied(t, env, env.RepoAlpha, num, "fabrik:bot-reprompted")

	bodies, err := tryPRComments(env, env.RepoAlpha, prNum)
	if err != nil {
		t.Fatalf("read PR comments on %s PR #%d: %v", env.RepoAlpha, prNum, err)
	}
	for _, b := range bodies {
		if strings.Contains(b, "@"+expectedReviewersSyntheticName) {
			t.Fatalf("PR #%d on %s carries an @mention of the declared bot %q — the bot ladder re-prompted it "+
				"instead of deferring to the human's authoritative escalation (Finding 2 regression): %q",
				prNum, env.RepoAlpha, expectedReviewersSyntheticName, b)
		}
	}
	t.Logf("AC2 verified: %s#%d — declared bot %q never re-prompted; human's authoritative CHANGES_REQUESTED escalation won",
		env.RepoAlpha, num, expectedReviewersSyntheticName)
}
