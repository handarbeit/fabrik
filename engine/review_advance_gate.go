package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// reviewGateHoldsAdvance is the review gate for runCatchUpPhase2's non-Validate
// advance (#2491, ADR 2491). Every automatic route out of a wait_for_reviews stage
// asks the same verdict — reviewGateHold, the read-only core the landing decision
// and the overseer's advance already share — so no route can disagree with another
// about whether the review is satisfied.
//
// The route that needed it: handleReviewGate runs checkReviewGate only when
// hasComplete was true before the handler chain, so on the poll where CI clears and
// checkCIGate adds stage:X:complete the verdict was never consulted and Phase 2
// advanced a stage with a standing change request. Validate was protected by
// attemptMergeOnValidate; this gives the other stages the same protection.
//
// It returns true when the advance must be held. A hold logs why (the advance
// wording, so the log names the decision taken on this poll), applies
// fabrik:awaiting-review idempotently (the anchor of the review wait clock, #2467)
// and records a "review-blocked" admission cooldown like handleReviewGate. It adds
// no pause, failure or counter: stage:X:complete is already present, so the next
// poll's checkReviewGate owns the timeout and the escalation ladder. A failed read
// holds (fail closed), exactly as the landing gate does.
//
// Stages without wait_for_reviews are untouched: reviewGateHold returns the zero
// verdict before any read, and nothing is logged for them.
func (e *Engine) reviewGateHoldsAdvance(item gh.ProjectItem, stage *stages.Stage) bool {
	if e.reviewAdvanceGateOff.Load() {
		return false
	}
	if stage == nil || stage.WaitForReviews == nil || !*stage.WaitForReviews {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	h := e.reviewGateHoldFor(item, stage, owner, repo, reviewRouteAdvance)
	if !h.Blocked {
		e.logf(item.Number, "awaiting-review", "review gate clear — advancing\n")
		return false
	}
	e.holdLandingForReview(item, "%s", h.Msg)
	e.noteGateHold(item, gateReview, "review gate blocks the advance", nil)
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: "review-blocked",
		Until:  e.now().Add(e.githubRecheckInterval()),
	})
	return true
}

// SetReviewGateAdvanceDisabledForTest neutralises the Phase 2 review gate,
// restoring the pre-#2491 behaviour where the non-Validate advance never asked the
// review verdict (non-vacuity seam; per-Engine).
func (e *Engine) SetReviewGateAdvanceDisabledForTest(off bool) {
	e.reviewAdvanceGateOff.Store(off)
}
