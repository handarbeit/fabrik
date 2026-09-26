package engine

import (
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
)

// commentGateBlocksLanding is the comment gate on attemptMergeOnValidate's
// landing decision (#1862, ADR-1862): an item with an unprocessed comment must
// not be merged, have auto-merge enabled, or be advanced to Queued. Without it
// Phase 2 merges from a snapshot that already contains the comment and dispatch
// then processes it against the just-merged branch, orphaning the rework (#1832).
//
// "Unprocessed" is exactly findNewComments — the same predicate the non-Validate
// advance guard in runCatchUpPhase2 uses — so the two can never disagree. It
// therefore excludes the engine's own 🏭 comments, 🚀'd comments, comments
// watermarked in the store, and bot service notices, and it counts exactly what
// dispatch will go on to process. A held item is never held by something dispatch
// will not act on: an unprocessable comment ends in the comment breakers' pause
// (fabrik:paused, which the catch-up loop skips), never a silent block.
//
// liveRead reports whether item.Comments came from a successful live
// FetchItemDetails; when it did not, the snapshot may be tens of minutes old
// (handleStageComplete) and the gate holds conservatively — a false hold is
// transient, a false merge is the bug being fixed (mirrors ADR-1216).
//
// On a hold it records a "comment-pending" cooldown, mirroring handleReviewGate's
// "review-blocked", so itemMayNeedWork's expiry path re-evaluates the item every
// githubRecheckInterval even when nothing bumps updatedAt. Dispatch of the
// pending comment itself is unaffected (the plain new-comment path has no
// cooldown gate).
func (e *Engine) commentGateBlocksLanding(item gh.ProjectItem, liveRead bool) bool {
	pending := e.findNewComments(item)
	if len(pending) == 0 && liveRead {
		return false
	}
	if len(pending) > 0 {
		e.logf(item.Number, "comment-gate", "holding landing decision: %d unprocessed comment(s) pending — will re-evaluate once processed\n", len(pending))
	} else {
		e.logf(item.Number, "comment-gate", "holding landing decision: live comment re-read failed, comment state unknown\n")
	}
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   itemOwnerRepoString(item, e.defaultRepo()),
		Number: item.Number,
		Reason: "comment-pending",
		Until:  e.now().Add(e.githubRecheckInterval()),
	})
	return true
}
