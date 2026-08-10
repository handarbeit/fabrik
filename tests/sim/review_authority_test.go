package sim

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/engine"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// This file ports tests/e2e/review_authority_test.go's five scenarios
// (ADR-1250's review_authority: authoritative mode, #1258, revised by #1375
// to reinvoke rather than pause immediately on CHANGES_REQUESTED). All five
// exercise checkReviewGate/reviewGateAuthorityVerdict via the shared
// seedReviewGateItem helper (review_gate_helpers.go) placing an item
// directly on the Review column with stage:Review:complete already set —
// the same zero-cost construction precedent as the live suite's
// seedReviewGateItem, at zero Claude-call cost instead of zero
// GitHub-API-round-trip cost.
//
// Divergence from the live suite (R2, recorded once here): the live tests
// call RequestPRReviewer before their own deliberate verdict so "outstanding"
// is non-empty by construction, working around #1312 — the bed's real
// reviewer (Pruefer) landing an incidental review before the engine's first
// gate evaluation. simgh never synthesizes an incidental bot review
// (FIDELITY.md), so that workaround has no sim analogue and is not ported:
// checkReviewGate's outer hasReviews==false condition already blocks
// unconditionally before any review is submitted, deterministically, with no
// extra seeding needed.
//
// Reinvoke-dispatch observability (Research Constraint 8): the live suite
// asserts a reinvoke fired via WaitForLogLine against the engine's log. sim
// has no log-line-waiting primitive; this port uses the GitHub-observable
// substitute env.Claude.CommentCallCount("Review") incrementing —
// dispatchReviewReinvoke re-invokes via InvokeForComments, so this count is
// the direct, race-free signal that a review-reinvoke was dispatched.
//
// Scope limitation (unchanged from the live suite, ADR-1258): these
// scenarios only exercise the advance gate (checkReviewGate), never the
// landing gate (reviewGateBlocksLanding), which is reachable only through a
// stage literally named "Validate". reviewAuthorityStages below has no
// Validate stage.
//
// reviewCycleLimitCommentPrefix is the literal prefix of
// pauseForReviewCycleLimit's own comment (engine/reviews.go) — copied here
// the same way ciFixCycleLimitCommentPrefix is copied in
// ci_fix_reinvoke_test.go, since the engine's own const is unexported.
// TestReviewAuthorityCycleLimitPauses uses it to confirm the pause is
// provably pauseForReviewCycleLimit's own, not merely a label combination
// another gate could in principle also produce (comment content is
// observable via projectItem's Comments field — see
// expected_reviewers_test.go's corrected file doc comment).
const reviewCycleLimitCommentPrefix = "🏭 **Fabrik — review cycle limit reached**"

// reviewDatabaseIDSeq allocates unique PRReview.DatabaseID values —
// buildReviewBodyCommentsFromReviews (engine/reviews.go) skips any review
// with DatabaseID == 0, so a review meant to be picked up as actionable
// reinvoke feedback needs a real, unique one; a review only meant to satisfy
// the gate's clearing/verdict check (never intended to trigger a reinvoke)
// deliberately leaves this unset.
var reviewDatabaseIDSeq atomic.Int64

// seedActionableReview seeds a review with a non-empty body and a unique
// DatabaseID — the exact shape buildReviewBodyCommentsFromReviews treats as
// actionable reinvoke feedback (see reviewDatabaseIDSeq's doc comment).
func seedActionableReview(t *testing.T, env *Env, prNum int, author, state, body string) {
	t.Helper()
	id := int(reviewDatabaseIDSeq.Add(1))
	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{
		Author:     author,
		State:      state,
		Body:       body,
		DatabaseID: id,
	})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("seedActionableReview: %v", err)
	}
}

// reviewAuthorityStages: Review carries wait_for_reviews (opt-in gate);
// review_authority itself is applied per-issue via the
// review-authority:<mode> label (extractReviewAuthorityOverride), not a
// stage-config field, matching what these scenarios are actually porting —
// see review_gate_helpers.go and CLAUDE.md's review-authority:<mode> entry.
func reviewAuthorityStages() []*stages.Stage {
	tr := true
	return []*stages.Stage{
		{Name: "Implement", Order: 1, CreateDraftPR: true, MarkPRReadyOnComplete: true},
		{Name: "Review", Order: 2, WaitForReviews: &tr},
		{Name: "Done", Order: 3, CleanupWorktree: true},
	}
}

// newReviewAuthorityEnv is the shared setup for all five scenarios below.
// reviewWaitTimeout/maxReviewCycles are passed explicitly for the same
// reason as conjunctive_ci_review_gate_test.go's newConjunctiveGateEnv:
// engine.Config has no built-in fallback for either when NewEnv's Config
// literal bypasses cmd/root.go's flag-resolution defaults — left unset, both
// read as an immediate, false pause/timeout.
func newReviewAuthorityEnv(t *testing.T, reviewWaitTimeout time.Duration, maxReviewCycles int) *Env {
	t.Helper()
	return NewEnv(t, EnvOptions{
		Stages: reviewAuthorityStages(),
		// Real-time-anchored gate branches (checkAwaitingReviewTimeout) need
		// StartTime near time.Now(), not the package default (2026-01-01) —
		// see ci_fix_reinvoke_test.go's StartTime comment for the identical
		// reasoning.
		StartTime: time.Now(),
		ConfigureCfg: func(cfg *engine.Config) {
			cfg.ReviewWaitTimeout = reviewWaitTimeout
			cfg.MaxReviewCycles = maxReviewCycles
		},
	})
}

// TestReviewAuthorityReinvokesOnChangesRequested ports the live test of the
// same name (AC1/AC6, #1375, ADR-1375): an authoritative CHANGES_REQUESTED
// review body triggers a bounded reinvoke — the primary response — not an
// immediate pause. AC7's "the same review is never reprocessed" is asserted
// via env.Claude.CommentCallCount staying at exactly 1 through a settle
// window of further polls, since a second dispatch could only be caused by
// re-dispatching on the same, already-processed review (no other review ever
// arrives in this scenario).
func TestReviewAuthorityReinvokesOnChangesRequested(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5) // large enough to never fire in this test

	num, prNum := seedReviewGateItem(t, env, "Review", "reinvoke-changes", "review-authority:authoritative")

	// Confirm the gate's trivial, pre-verdict block (hasReviews==false blocks
	// unconditionally) before any review is submitted — proves the gate is
	// genuinely engaged, not a poll racing ahead of the review.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("fabrik:paused applied before any review was ever submitted")
	}
	t.Logf("fabrik:awaiting-review confirmed on #%d before any review submitted — gate is genuinely engaged", num)

	seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", "please address this before merge")
	t.Logf("submitted REQUEST_CHANGES review on PR #%d (body-only, zero inline comments — AC6's target shape)", prNum)

	// AC1/AC6: the reinvoke must fire — CommentCallCount incrementing is the
	// direct evidence dispatchReviewReinvoke's InvokeForComments ran.
	AdvanceUntil(t, env, func(env *Env) bool {
		return env.Claude.CommentCallCount("Review") >= 1
	}, 80)
	if env.Claude.CommentCallCount("Review") < 1 {
		t.Fatal("expected a review-reinvoke dispatch (Claude.CommentCallCount(\"Review\") >= 1)")
	}
	// The reinvoke, not a pause, is the primary response.
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("fabrik:paused applied — the reinvoke should be the primary response to a single CHANGES_REQUESTED body")
	}
	t.Logf("AC1/AC6 verified: #%d reinvoked on an authoritative CHANGES_REQUESTED body, never paused", num)

	// AC7: drive a further settle window and confirm no second dispatch
	// fires for the same, already-processed review (the dedup keyed on
	// reviewBodyCommentID in buildReviewBodyCommentsFromReviews).
	RunPolls(t, env, 20)
	if got := env.Claude.CommentCallCount("Review"); got != 1 {
		t.Fatalf("AC7 violated: expected exactly 1 review-reinvoke dispatch after the settle window (dedup on the same processed review), got %d", got)
	}
	t.Logf("AC7 verified: #%d's single CHANGES_REQUESTED review was not reprocessed across a further settle window", num)
}

// TestReviewAuthorityCycleLimitPauses ports the live test of the same name
// (AC4): maxCycles distinct CHANGES_REQUESTED reviews each trigger a bounded
// reinvoke; the (maxCycles+1)th routes to pauseForReviewCycleLimit instead of
// another reinvoke — the terminal fallback half of the reinvoke model, R5.
func TestReviewAuthorityCycleLimitPauses(t *testing.T) {
	t.Parallel()
	const maxCycles = 2
	env := newReviewAuthorityEnv(t, 15*time.Minute, maxCycles)

	num, prNum := seedReviewGateItem(t, env, "Review", "cycle-limit", "review-authority:authoritative")

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("fabrik:awaiting-review confirmed on #%d — gate is genuinely engaged", num)

	for i := 0; i < maxCycles; i++ {
		before := env.Claude.CommentCallCount("Review")
		seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", fmt.Sprintf("round %d: still not right", i+1))
		AdvanceUntil(t, env, func(env *Env) bool {
			return env.Claude.CommentCallCount("Review") > before
		}, 80)
		t.Logf("cycle %d/%d: reinvoke dispatched for #%d (Claude.CommentCallCount(\"Review\")=%d)", i+1, maxCycles, num, env.Claude.CommentCallCount("Review"))
	}

	// The (maxCycles+1)th distinct CHANGES_REQUESTED review must route to the
	// cycle-limit pause, not another reinvoke.
	seedActionableReview(t, env, prNum, "reviewer-human", "CHANGES_REQUESTED", "still not right, one more time")

	WaitForIssueLabel(t, env, num, "fabrik:paused", 80)
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-input", 20)

	item := projectItem(t, env, num)
	if item.IsClosed {
		t.Fatal("issue closed after cycle limit — expected OPEN")
	}
	// Non-vacuity: exactly maxCycles reinvoke dispatches happened — the
	// (maxCycles+1)th review triggered a pause, not a further dispatch.
	if got := env.Claude.CommentCallCount("Review"); got != maxCycles {
		t.Fatalf("expected exactly %d review-reinvoke dispatches before the cycle-limit pause, got %d", maxCycles, got)
	}
	// Pause provenance: the live suite confirms this pause specifically came
	// from pauseForReviewCycleLimit (WaitForPRCommentContainingAny) rather
	// than trusting the label pair alone — carried forward here the same way
	// ci_fix_reinvoke_test.go's cycle-limit test does for its own comment.
	if !hasCommentWithPrefix(item.Comments, reviewCycleLimitCommentPrefix) {
		t.Fatalf("no engine-authored review cycle-limit comment (prefix %q) found on #%d — the issue paused, but not provably via pauseForReviewCycleLimit",
			reviewCycleLimitCommentPrefix, num)
	}
	t.Logf("AC4 verified: #%d paused via the review cycle limit after %d genuine reinvoke cycle(s), not an unbounded loop", num, maxCycles)
}

// TestReviewAuthorityClearsOnApproval ports the live test of the same name
// (scenario 2): authoritative + APPROVED clears the gate. Descoped from
// "merges" to "gate clears" per the file-level scope-limitation note.
func TestReviewAuthorityClearsOnApproval(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5)

	num, prNum := seedReviewGateItem(t, env, "Review", "clears-approval", "review-authority:authoritative")

	// Confirm the gate genuinely engages before approving — see the file
	// doc comment's "no RequestPRReviewer needed" divergence note.
	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("fabrik:paused applied before any review was ever submitted")
	}
	t.Logf("fabrik:awaiting-review confirmed on #%d before approval — gate is genuinely engaged", num)

	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	t.Logf("submitted APPROVE review on PR #%d", prNum)

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-review", 80)
	if hasLabel(IssueLabels(t, env, num), "fabrik:paused") {
		t.Fatal("fabrik:paused applied — authoritative gate should have cleared on approval, not paused")
	}
	t.Logf("R2 verified: #%d — authoritative gate cleared on APPROVED, never paused", num)
}

// TestReviewAuthorityYoloDoesNotBypassBlock ports the live test of the same
// name (scenario 3) — the core composition guarantee from ADR-1250:
// fabrik:yolo does not bypass an authoritative gate. Descoped from "merges
// under yolo" to "gate clears" for the same reason as ClearsOnApproval.
func TestReviewAuthorityYoloDoesNotBypassBlock(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5)

	num, prNum := seedReviewGateItem(t, env, "Review", "yolo-no-bypass", "review-authority:authoritative", "fabrik:yolo")

	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{Author: "reviewer-human", State: "CHANGES_REQUESTED", Body: "please fix"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	t.Logf("submitted REQUEST_CHANGES review on PR #%d (fabrik:yolo present)", prNum)

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)

	// The gate must block even though yolo is set — drive further polls and
	// confirm the block is still genuinely holding, not vacuously.
	RunPolls(t, env, 20)
	item := projectItem(t, env, num)
	if item.IsClosed {
		t.Fatal("yolo bypassed the authoritative gate — issue closed while CHANGES_REQUESTED still stood")
	}
	if !hasLabel(item.Labels, "fabrik:awaiting-review") {
		t.Fatal("yolo bypassed the authoritative gate — fabrik:awaiting-review cleared while CHANGES_REQUESTED still stood")
	}
	t.Logf("yolo did not bypass the block: #%d still OPEN with fabrik:awaiting-review after CHANGES_REQUESTED", num)

	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{Author: "reviewer-human", State: "APPROVED"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	t.Logf("submitted APPROVE review on PR #%d", prNum)

	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("R3 verified: #%d — yolo composed correctly: blocked on CHANGES_REQUESTED, cleared on APPROVE", num)
}

// TestReviewAuthorityAdvisoryRegressionGuard ports the live test of the same
// name (scenario 4): proves the additive authoritative check inside
// checkReviewGate does not leak into advisory (default) mode — runs against
// the plain Review stage with no review-authority label at all. Would fail
// today if the authoritative check were made non-additive (e.g. replacing
// rather than extending the outer len(outstanding)==0 && hasReviews
// condition).
func TestReviewAuthorityAdvisoryRegressionGuard(t *testing.T) {
	t.Parallel()
	env := newReviewAuthorityEnv(t, 15*time.Minute, 5)

	num, prNum := seedReviewGateItem(t, env, "Review", "advisory-regression") // no review-authority label — default advisory

	WaitForIssueLabel(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("fabrik:awaiting-review confirmed on #%d before review submission — gate is genuinely engaged", num)

	env.Sim.Sim().SeedReview(env.OwnerRepo, prNum, gh.PRReview{Author: "reviewer-human", State: "CHANGES_REQUESTED", Body: "please fix"})
	if err := env.Sim.Sim().Err(); err != nil {
		t.Fatalf("SeedReview: %v", err)
	}
	t.Logf("submitted REQUEST_CHANGES review on PR #%d (advisory Review stage)", prNum)

	// Advisory mode: any submitted review (regardless of verdict) with no
	// outstanding requested reviewers clears the gate — pre-#1250 behavior,
	// must be unchanged.
	WaitForLabelAbsent(t, env, num, "fabrik:awaiting-review", 80)
	t.Logf("R4 verified: #%d — advisory gate cleared on CHANGES_REQUESTED (additive check did not narrow the default path)", num)
}
