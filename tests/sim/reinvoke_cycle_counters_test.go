package sim

import (
	"fmt"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	"github.com/handarbeit/fabrik/tests/sim/simclaude"
)

// Gap 4 (#1592): the two reinvoke-cycle counters, and the invariant between
// them — the largest single piece of this issue.
//
//   - 4a — ReviewCycleDecremented (#1045, ADR-1518): a review-reinvoke that
//     lands no new commit refunds its own prior ReviewCycleIncremented.
//     TestReviewCycleDecremented_NoCommitRefundsIndefinitely (AC5).
//   - 4b — NoOpCommentCycles (engine/comment_noop_breaker.go, #1555): a
//     separate, never-refunded counter over the same processComments
//     funnel. TestNoOpCommentCycles_TripsBreakerAndResetsOnProgress (AC6).
//   - 4c — the interaction, and this gap's primary deliverable: the
//     invariant (maxNoOpCommentCycles > maxReviewCycles, "one refunds and
//     the other does not") stated only in effectiveMaxNoOpCommentCycles's
//     doc comment. TestReviewCycleVsNoOpCommentCycle_Invariant (AC7).
//
// All three use advisory review mode (no review-authority label): traced
// through checkReviewGate, advisory clears blocked=false immediately on any
// submitted review regardless of verdict — the "chatty bot reviewer" shape
// #1045/ADR-1518 exists for — which keeps ReviewBlockedCycles (the
// never-refunded ADR-1518 counterpart, folded into the same dispatch-and-
// limit-check via max(ReviewCycles, ReviewBlockedCycles)) from climbing and
// confounding the assertions below. Authoritative mode would leave
// blocked=true on an unresolved verdict, defeating that isolation.
//
// 4a's other direction — a commit-landing reinvoke does NOT refund — is
// already covered by TestReviewAuthorityCycleLimitPauses
// (review_authority_test.go): its CHANGES_REQUESTED reinvokes run
// DefaultCommentScript, which commits a real marker file every round, so
// headBefore != headAfter and ReviewCycleDecremented never fires — that
// test's exact-maxCycles-then-pause behavior is itself a structural proof
// that increments are never refunded when a commit lands. Not duplicated
// here.
//
// The exact round counts and script sequences below were derived by tracing
// handleReviewGate/dispatchWithCycleLimit/checkNoOpCommentCycle's actual
// order (Plan stage), not assumed.

// reinvokeCycleCountersEnv builds the shared Env for all three scenarios:
// reviewAuthorityStages() (review_authority_test.go) with a real,
// wait_for_reviews-gated Review stage, StartTime near time.Now() (the
// real-wall-clock-anchored checkAwaitingReviewTimeout gate needs this, same
// reasoning as newReviewAuthorityEnv/ci_fix_reinvoke_test.go), and both
// cycle limits configured explicitly — engine.Config has no built-in
// fallback for either when NewEnv's Config literal bypasses cmd/root.go's
// flag-resolution defaults.
func reinvokeCycleCountersEnv(t *testing.T, maxReviewCycles, maxNoOpCommentCycles int) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages:    reviewAuthorityStages(),
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = time.Hour // generous — never times out mid-test
			cfg.MaxReviewCycles = maxReviewCycles
			cfg.MaxNoOpCommentCycles = maxNoOpCommentCycles
		},
	})
}

// preCreateWorktree creates issueNum's worktree on disk (checking out the
// fabrik/issue-<N> branch seedReviewGateItem already created on the model)
// before any reinvoke round runs.
//
// This matters specifically for 4a: dispatchReviewReinvoke's build() hook
// captures headBefore via gitHeadSHA(workDir) BEFORE processComments itself
// calls EnsureWorktree — dispatchReinvoke only computes the worktree path
// (wm.WorktreeDir), it does not create it. Without a worktree already on
// disk, round 1's headBefore is "" (gitHeadSHA errors on a missing
// directory), and dispatchReviewReinvoke's after hook requires headBefore
// != "" before ever comparing SHAs — so round 1's own ReviewCycleIncremented
// would never get refunded regardless of whether a commit landed, purely as
// an artifact of seedReviewGateItem's zero-Claude-cost construction never
// having touched a real worktree. Pre-creating it here removes that
// confound so every round's refund behavior is judged on its own merits.
func preCreateWorktree(t *testing.T, env *Env, issueNum int) {
	t.Helper()
	base, err := env.WM.DefaultBaseBranch()
	if err != nil {
		t.Fatalf("DefaultBaseBranch: %v", err)
	}
	if _, err := env.WM.EnsureWorktree(issueNum, base, false); err != nil {
		t.Fatalf("EnsureWorktree: %v", err)
	}
}

// driveReinvokeRound seeds a fresh actionable review — a unique DatabaseID
// each round (buildReviewFeedbackCommentsFromReviews would otherwise dedup
// an already-processed one, engine/reviews.go) AND a unique author each
// round. The author must vary too: simgh's FetchPRReviews models GitHub's
// own latest-review-per-author reduction, and a COMMENTED follow-up never
// supersedes that same author's existing entry (tests/sim/simgh/reviews.go's
// latestReviewsByAuthor) — reusing one author across rounds would make every
// round after the first invisible to the engine, since the reduction
// collapses them all down to round 1's review before checkReviewGate ever
// sees them. Distinct authors also mirror the "chatty bot reviewer" shape
// #1045/ADR-1518 exist for more directly than one author resubmitting would.
//
// Drives polls until the Review stage's comment-invocation count advances
// past its pre-round value, proving a reinvoke was genuinely dispatched
// this round rather than merely that time passed.
func driveReinvokeRound(t *testing.T, env *Env, prNum, round int, maxPolls int) {
	t.Helper()
	before := env.Claude.CommentCallCount("Review")
	seedActionableReview(t, env, prNum, fmt.Sprintf("chatty-review-bot-%d", round), "COMMENTED",
		fmt.Sprintf("round %d: a non-actionable status overview.", round))
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.CommentCallCount("Review") > before
	}, maxPolls)
}

// TestReviewCycleDecremented_NoCommitRefundsIndefinitely is 4a (AC5): a
// no-commit review-reinvoke refunds its own prior ReviewCycleIncremented
// (#1045, ADR-1518), so a chatty bot reviewer's non-actionable overviews
// never accumulate toward MaxReviewCycles.
//
// MaxReviewCycles=1 makes this the discriminating configuration: with the
// refund working, every round's pre-dispatch cycleCount reads back 0 (the
// prior round's increment having been refunded), so dispatchWithCycleLimit
// (0 >= 1 is false) always takes the dispatch branch, never the pause
// branch, however many rounds run. NoOpCommentReview (tests/sim/simclaude)
// supplies the no-commit shape every round.
//
// Non-vacuity (R5): with the limit at 1, any implementation that
// incremented ReviewCycles without ever refunding it would compute
// cycleCount=1 on round 2 (1 >= 1) and pause — round 2's check is what an
// unrefunded implementation would fail first. Confirmed by temporarily
// neutralizing dispatchReviewReinvoke's after hook (removing the
// ReviewCycleDecremented Apply call) and observing round 2 pause.
func TestReviewCycleDecremented_NoCommitRefundsIndefinitely(t *testing.T) {
	t.Parallel()
	env := reinvokeCycleCountersEnv(t, 1, 20) // MaxNoOpCommentCycles kept well out of the way
	env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())

	issueNum, prNum := seedReviewGateItem(t, env, "Review", "4a")
	preCreateWorktree(t, env, issueNum)

	for round := 1; round <= 3; round++ {
		driveReinvokeRound(t, env, prNum, round, 20)
		if hasLabel(IssueLabels(t, env, issueNum), "fabrik:paused") {
			t.Fatalf("round %d: issue paused despite MaxReviewCycles=1 — the no-commit refund (ReviewCycleDecremented) must be keeping the pre-dispatch cycle count at 0 every round", round)
		}
	}
}

// noOpCommentBreakerCommentPrefix is the literal prefix of
// tripNoOpCommentCycleBreaker's own comment (engine/comment_noop_breaker.go)
// — copied here the same way reviewCycleLimitCommentPrefix
// (review_authority_test.go) copies pauseForReviewCycleLimit's, since the
// engine's own const is unexported. Used to confirm which breaker actually
// tripped, not merely that a pause occurred (AC6/AC7).
const noOpCommentBreakerCommentPrefix = "🏭 **Fabrik — no-op comment-processing circuit breaker tripped**"

// TestNoOpCommentCycles_TripsBreakerAndResetsOnProgress is 4b (AC6): a
// sustained sequence of no-progress comment-processing cycles increments
// NoOpCommentCycles and trips the breaker at effectiveMaxNoOpCommentCycles;
// a progressing cycle resets the counter to zero rather than merely
// decrementing it.
//
// MaxNoOpCommentCycles=3, MaxReviewCycles=20 (kept out of the way). Six
// rounds, scripted [NoOp, NoOp, Commits, NoOp, NoOp, NoOp]: rounds 1-2
// increment to 2; round 3 commits and resets to 0 (checkNoOpCommentCycle's
// progressed branch); rounds 4-6 increment 1, 2, 3 — tripping exactly on
// round 6.
//
// Non-vacuity (R5): round 5 (count would-be 2, not yet at the limit) is the
// discriminating assertion. A regression that decremented on progress
// instead of resetting to zero would have left the counter at 2 after round
// 3's commit (3 no-ops, minus 1) rather than 0, reaching the limit of 3 on
// round 5 instead of round 6 — this test's round-5-not-paused check catches
// that even though round 6 alone (a bare "eventually pauses" check) would
// not. Confirmed by temporarily changing checkNoOpCommentCycle's progressed
// branch from NoOpCommentCycleReset to a decrement-by-one mutation and
// observing round 5 pause instead of round 6.
func TestNoOpCommentCycles_TripsBreakerAndResetsOnProgress(t *testing.T) {
	t.Parallel()
	env := reinvokeCycleCountersEnv(t, 20, 3)
	env.Claude.ForStageComments("Review",
		simclaude.NoOpCommentReview(),
		simclaude.NoOpCommentReview(),
		simclaude.CommentReviewCompleted(), // commits — resets the no-op counter
		simclaude.NoOpCommentReview(),
		simclaude.NoOpCommentReview(),
		simclaude.NoOpCommentReview(),
	)

	issueNum, prNum := seedReviewGateItem(t, env, "Review", "4b")
	preCreateWorktree(t, env, issueNum)

	for round := 1; round <= 6; round++ {
		driveReinvokeRound(t, env, prNum, round, 20)
		paused := hasLabel(IssueLabels(t, env, issueNum), "fabrik:paused")
		switch {
		case round < 6 && paused:
			t.Fatalf("round %d: issue paused prematurely — NoOpCommentCycles should not reach the limit of 3 until round 6 (rounds 1-2 increment, round 3 resets on commit, rounds 4-6 increment again)", round)
		case round == 6 && !paused:
			t.Fatal("round 6: issue not paused — expected the no-op comment-processing breaker to trip")
		}
	}

	item := projectItem(t, env, issueNum)
	if !hasCommentWithPrefix(item.Comments, noOpCommentBreakerCommentPrefix) {
		t.Error("round 6: no-op comment-processing breaker comment not found")
	}
	if hasCommentWithPrefix(item.Comments, reviewCycleLimitCommentPrefix) {
		t.Error("round 6: review-cycle-limit comment present — the wrong breaker tripped (MaxReviewCycles=20 should never be reached)")
	}
}

// TestReviewCycleVsNoOpCommentCycle_Invariant is 4c (AC7) — gap 4's primary
// deliverable: the interaction invariant between the two counters, stated
// only in effectiveMaxNoOpCommentCycles's doc comment
// (engine/comment_noop_breaker.go): NoOpCommentCycles' default (10) is
// deliberately higher than ReviewCycles' default (5) specifically because
// ReviewCycles is refunded to ~0 on every no-op reinvoke (4a) while
// NoOpCommentCycles never refunds — collapsing the two thresholds to the
// same value would silently nullify 4a's forgive-forever guarantee at
// whatever count they shared, with no terminal-state test noticing (the
// issue would still pause, just for the wrong reason and at the wrong
// count — #1555 review discussion cites a healthy, mergeable PR that
// accumulated 4 consecutive no-op cycles under ordinary duplicate-bot
// delivery, one shy of tripping at the old MaxReviewCycles default of 5).
//
// MaxReviewCycles=3, MaxNoOpCommentCycles=6 — mirrors the real default
// ratio (5:10) at a scale this test can drive quickly. A single sustained
// no-op reinvoke sequence (NoOpCommentReview registered once — the
// package's "last script repeats" rule covers every further round)
// exercises both counters together each round, exactly as production does
// (dispatchReviewReinvoke is built on dispatchReinvoke -> processComments,
// so one review-reinvoke cycle increments ReviewCycles pre-dispatch and
// NoOpCommentCycles inside processComments in the same pass).
//
// Assertions, both against the SAME six-round sequence:
//   - after round 4 (> MaxReviewCycles=3): NOT paused, and no
//     reviewCycleLimitCommentPrefix comment exists — 4a's refund is holding
//     ReviewCycles at 0 every round, so the review-cycle limit is never
//     reached despite 4 rounds exceeding its raw threshold.
//   - after round 6 (== MaxNoOpCommentCycles=6): paused, with the no-op
//     breaker's comment prefix present and the review-cycle-limit prefix
//     absent — naming which limit tripped (AC7's own requirement), not
//     merely that a pause occurred.
//
// Non-vacuity (R5): this is the assertion the doc comment's own warning is
// about. Confirmed by temporarily setting effectiveMaxNoOpCommentCycles to
// return e.cfg.MaxReviewCycles's value instead of its own (collapsing the
// two thresholds to the same value, mirroring "someone tidying two
// similar-looking cycle constants into one shared default") and observing
// the no-op breaker trip on round 3 instead of round 6, with the round-4
// "not yet paused" assertion failing as a direct consequence.
func TestReviewCycleVsNoOpCommentCycle_Invariant(t *testing.T) {
	t.Parallel()
	env := reinvokeCycleCountersEnv(t, 3, 6)
	env.Claude.ForStageComments("Review", simclaude.NoOpCommentReview())

	issueNum, prNum := seedReviewGateItem(t, env, "Review", "4c")
	preCreateWorktree(t, env, issueNum)

	for round := 1; round <= 6; round++ {
		driveReinvokeRound(t, env, prNum, round, 20)

		switch round {
		case 4:
			// Past MaxReviewCycles (3), but 4a's refund keeps ReviewCycles
			// at 0 every round — must NOT have paused via the review-cycle
			// limit.
			if hasLabel(IssueLabels(t, env, issueNum), "fabrik:paused") {
				t.Fatal("round 4 (> MaxReviewCycles=3): issue paused — the review-cycle limit tripped despite every reinvoke being a no-commit no-op; 4a's forgive-forever refund is not holding")
			}
			item := projectItem(t, env, issueNum)
			if hasCommentWithPrefix(item.Comments, reviewCycleLimitCommentPrefix) {
				t.Fatal("round 4: a review-cycle-limit comment exists despite no pause — inconsistent state")
			}
		case 6:
			// At MaxNoOpCommentCycles (6): the no-op breaker must trip,
			// and it must be nameable as the no-op breaker specifically,
			// not the review-cycle limit.
			if !hasLabel(IssueLabels(t, env, issueNum), "fabrik:paused") {
				t.Fatal("round 6 (== MaxNoOpCommentCycles=6): issue not paused — expected the no-op comment-processing breaker to trip")
			}
			item := projectItem(t, env, issueNum)
			if !hasCommentWithPrefix(item.Comments, noOpCommentBreakerCommentPrefix) {
				t.Error("round 6: no-op comment-processing breaker comment not found")
			}
			if hasCommentWithPrefix(item.Comments, reviewCycleLimitCommentPrefix) {
				t.Error("round 6: review-cycle-limit comment also present — naming the wrong limit (or both) as having tripped")
			}
		}
	}
}
