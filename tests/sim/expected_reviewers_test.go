package sim

import (
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
)

// This file ports tests/e2e/expected_reviewers_test.go's five scenarios
// (ADR-1283's expected_reviewers — declared unrequested reviewers for the
// review gate, #1298), the direct #1258 precedent applied to a second,
// list-shaped stage-config field. All five exercise checkReviewGate via the
// review_gate_helpers.go seedReviewGateItem/seedReviewGateItemDraft helpers,
// applying a declared value per issue via one of two labels:
//
//	expected-reviewers:none     -> &[]string{}
//	expected-reviewers:declared -> &[]string{expectedReviewersSyntheticName}
//
// (extractExpectedReviewersOverride, engine/reviews.go — expectedReviewersSyntheticName
// there is the literal string "e2e-synthetic-declared-reviewer", matching
// the live suite's own const of the same name exactly.)
//
// Divergence from the live suite (R2, recorded once here, same rationale as
// review_authority_test.go's file doc comment): the live tests use
// seedReviewGateItemDraft for four of five scenarios specifically to dodge
// #1312 (the bed's real reviewer, Pruefer, landing an incidental review
// before the assertion runs). simgh never synthesizes an incidental bot
// review, so this port uses the plain (non-draft) seedReviewGateItem
// throughout — the draft variant exists in review_gate_helpers.go for
// structural parity but carries no sim-side determinism concern.
//
// Scope limitation (unchanged from the live suite, ADR-1298 following
// ADR-1258): these scenarios only exercise the advance gate
// (checkReviewGate), never the landing gate (reviewGateBlocksLanding),
// reachable only through a stage literally named "Validate".
// reviewAuthorityStages (review_authority_test.go) has no Validate stage,
// reused here directly — same pipeline shape, same "Review" gate stage.
//
// Comment-content observability gap (recorded once here, applies to
// TestExpectedReviewersDeclaredWaitsAndReprompts and
// TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation below): the live
// suite asserts the Phase 1 re-prompt comment's exact text (an @mention of
// the declared synthetic name) and its absence. simgh's Sim/Instrumented
// never exposes issue/PR comment bodies back to a scenario — comments are
// write-only from the scenario's side (see ci_fix_reinvoke_test.go's #1320
// discriminator note for the identical gap). This port substitutes the
// strongest available GitHub-observable signals instead: the
// fabrik:bot-reprompted / fabrik:paused label sequence for the re-prompt
// ladder's presence, and fabrik:bot-reprompted's absence plus
// env.Claude.CommentCallCount("Review") for the ladder's correct
// non-engagement. Recorded as a coverage-matrix partial-port reason (R4).

// newExpectedReviewersTimeoutEnv backdates StartTime 24 real hours into the
// past — the same technique timeout_test.go's
// TestReviewWaitTimeout_FiresWithZeroRealElapsedTime uses (R3: this gate is
// GitHub-anchored via FetchLabelAppliedAt, not engine-Clock-anchored) — so
// both halves of the bot-reprompt ladder (Phase 1's ReviewWaitTimeout wait,
// Phase 2's second ReviewWaitTimeout wait from fabrik:bot-reprompted) are
// already overdue against real wall-clock time the instant each label is
// applied, without this test waiting out any real minutes. Only
// TestExpectedReviewersDeclaredWaitsAndReprompts uses this; every other
// scenario in this file needs the ladder to definitely NOT fire and uses the
// near-real-time newReviewAuthorityEnv instead.
func newExpectedReviewersTimeoutEnv(t *testing.T) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages:    reviewAuthorityStages(),
		StartTime: time.Now().Add(-24 * time.Hour),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = 15 * time.Minute
			cfg.MaxReviewCycles = 5
		},
	})
}

// TestExpectedReviewersFastAdvance ports the live test of the same name
// (requirements scenario 1): with expected_reviewers: [] declared, no
// requested reviewer, and no submitted review, the gate fast-advances
// instead of waiting out ReviewWaitTimeout — the #1080 stall this feature
// exists to eliminate.
func TestExpectedReviewersFastAdvance(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5) // must never fire in this test

	num, _ := seedReviewGateItemDraft(t, env, "Review", "expected-none-fast-advance", "expected-reviewers:none")

	// The gate must never apply fabrik:awaiting-review at all — a fast
	// advance clears before the label-apply branch is ever reached.
	RunPolls(t, env, 30)
	if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-review") {
		t.Fatal("fabrik:awaiting-review applied despite expected_reviewers: [] and nothing requested/reviewed — gate did not fast-advance")
	}
	t.Logf("R1 verified: #%d fast-advanced — fabrik:awaiting-review never applied with expected_reviewers: [] declared", num)
}

// TestExpectedReviewersDeclaredWaitsAndReprompts ports the live test of the
// same name (requirements scenario 2): with expected_reviewers: [<name>]
// declared and no requested reviewer, the gate waits (does not fast-advance)
// and the bot re-prompt ladder engages — behavior undeclared config cannot
// reach at all. Uses the backdated-clock timeout env (see
// newExpectedReviewersTimeoutEnv's doc comment) so both ladder phases fire
// without this test waiting out real minutes.
func TestExpectedReviewersDeclaredWaitsAndReprompts(t *testing.T) {
	t.Parallel()
	env := newExpectedReviewersTimeoutEnv(t)

	num, _ := seedReviewGateItemDraft(t, env, "Review", "expected-declared-reprompt", "expected-reviewers:declared")

	// Contrast with TestExpectedReviewersFastAdvance: a declared-but-unmatched
	// reviewer must NOT fast-advance — fabrik:awaiting-review is applied
	// immediately, same as the undeclared default.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("fabrik:awaiting-review confirmed on #%d — declared reviewer holds the gate open", num)

	// Phase 1: after one ReviewWaitTimeout window with no response, the
	// engine re-prompts the declared identity and applies fabrik:bot-reprompted.
	WaitForIssueLabel(t, env, num, "fabrik:bot-reprompted", 80)
	t.Logf("fabrik:bot-reprompted appeared on #%d — Phase 1 re-prompt fired", num)

	// Phase 2: the synthetic name never actually reviews, so after a second
	// full timeout window the engine gives up and pauses for a human,
	// removing both fabrik:bot-reprompted and fabrik:awaiting-review.
	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 20)
	t.Logf("fabrik:paused appeared on #%d — Phase 2 timed out with no response from the declared reviewer", num)

	if item := projectItem(t, env, num); item.IsClosed {
		t.Fatal("expected issue OPEN after declared-reviewer Phase 2 timeout")
	}
	t.Logf("R2 verified: #%d — declared reviewer held the gate open, Phase 1 re-prompted, Phase 2 paused for human", num)
}

// TestExpectedReviewersUndeclaredRegressionGuard ports the live test of the
// same name (requirements scenario 4): undeclared (nil) expected_reviewers
// still never fast-advances — proves the shipped default (FR-5) is
// unchanged. Pins the `expected != nil` half of reviewGateFastAdvance
// specifically.
func TestExpectedReviewersUndeclaredRegressionGuard(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5)

	num, _ := seedReviewGateItemDraft(t, env, "Review", "expected-nil-regression") // no expected-reviewers label at all

	// Undeclared (nil) must behave exactly as it did before #1283: the gate
	// blocks unconditionally on "nothing requested, nothing reviewed yet",
	// applying fabrik:awaiting-review immediately rather than fast-advancing.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("R4 verified: #%d — undeclared (nil) expected_reviewers still applies fabrik:awaiting-review, never fast-advances", num)
}

// TestExpectedReviewersFastAdvanceComposesWithAuthoritative ports the live
// test of the same name (requirements scenario 5): expected_reviewers: []
// fast-advance composes with review_authority: authoritative — per
// reviewGateFastAdvance's doc comment, this path is deliberately independent
// of authority mode because it only fires when hasReviews is false, before
// any verdict could be weighed.
func TestExpectedReviewersFastAdvanceComposesWithAuthoritative(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5)

	num, _ := seedReviewGateItemDraft(t, env, "Review", "expected-none-authoritative",
		"expected-reviewers:none", "review-authority:authoritative")

	// Same bounded-window assertion as TestExpectedReviewersFastAdvance:
	// fast-advance must fire before any authority-verdict branch is ever
	// reached (that branch only activates once hasReviews is true, which
	// never happens here — nothing is ever reviewed).
	RunPolls(t, env, 30)
	if hasLabel(IssueLabels(t, env, num), "fabrik:awaiting-review") {
		t.Fatal("fabrik:awaiting-review applied despite expected_reviewers: [] and review-authority:authoritative — fast-advance did not fire ahead of the authority-verdict branch")
	}
	t.Logf("R5 verified: #%d fast-advanced despite review-authority:authoritative — fast-advance composes independently of authority mode", num)
}

// TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation ports the live
// test of the same name (AC2, Finding 2/R2, #1375): a stage-declared
// expected_reviewers bot must never defer an outstanding human reviewer's
// authoritative escalation behind its own re-prompt ladder.
//
// Unlike this file's other scenarios, this reaches the state "outstanding
// requested reviewers empty, one human review exists, a declared bot hasn't
// responded" without ever formally requesting the human — SeedReview alone
// (no SeedReviewRequest) leaves reviewGateOutstanding's outstanding slice
// empty from the start, which is the state under test; the live suite's
// RequestPRReviewer call exists purely for its own #1312 workaround, which,
// per this file's divergence note, has no sim analogue and is not ported.
func TestReviewAuthorityDeclaredBotDoesNotDeferHumanEscalation(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5) // must not fire in this test — the ladder must never engage

	num, prNum := seedReviewGateItem(t, env, "Review", "declared-bot-no-defer",
		"review-authority:authoritative", "expected-reviewers:declared")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("fabrik:awaiting-review confirmed on #%d before any review submitted — bot declared, nothing requested", num)

	seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", "please address this")
	t.Logf("submitted REQUEST_CHANGES review on PR #%d — outstanding now empty, declared bot still unresponsive", prNum)

	// The human path must win: the reinvoke fires to address the feedback
	// (Finding 1, #1375) — this alone would already fail against a
	// pre-#1375 engine, where the authoritative reinvoke path is
	// unreachable while blocked.
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.CommentCallCount("Review") >= 1
	}, 80)
	if env.Claude.CommentCallCount("Review") < 1 {
		t.Fatal("expected a review-reinvoke dispatch on the human's CHANGES_REQUESTED verdict")
	}
	t.Logf("review-reinvoke dispatched for #%d on the human's CHANGES_REQUESTED verdict", num)

	// Drive a further settle window and confirm the declared bot's ladder
	// never engages — if reviewGateAllBots incorrectly read allBots=true for
	// the still-outstanding declared bot once the human's verdict dropped
	// outstanding to empty, fabrik:bot-reprompted would appear here.
	RunPolls(t, env, 20)
	if hasLabel(IssueLabels(t, env, num), "fabrik:bot-reprompted") {
		t.Fatal("fabrik:bot-reprompted applied — the declared bot's re-prompt ladder fired instead of deferring to the human's authoritative CHANGES_REQUESTED escalation (Finding 2 regression)")
	}
	t.Logf("AC2 verified: #%d — declared bot never re-prompted; human's authoritative CHANGES_REQUESTED escalation won", num)
}
