package engine

import (
	"github.com/handarbeit/fabrik/stages"

	gh "github.com/handarbeit/fabrik/github"
)

// SetClosedPausedCleanupDisabledForTest turns the closed-paused cleanup
// admission off (#2459) so a test can show its assertions bite (the
// non-vacuity seam). One flag covers all three points: itemNeedsWorkRetryAt's
// pause veto, processItem's hoist and handleCleanupStage's pause clearing.
func (e *Engine) SetClosedPausedCleanupDisabledForTest(disabled bool) {
	e.closedPausedCleanupOff.Store(disabled)
}

// closedCleanupItem reports whether item is a closed, non-PR item at a cleanup
// stage (#2459, ADR 2459). Such an item has nothing left to protect, so the
// pause that skips an open item's cleanup does not apply to it: it is admitted
// for cleanup, dispatched straight to handleCleanupStage (never to the
// comment-resume gates, ADR 1387) and has its pause pair cleared there.
func (e *Engine) closedCleanupItem(item gh.ProjectItem, stage *stages.Stage) bool {
	if e.closedPausedCleanupOff.Load() {
		return false
	}
	return stage != nil && stage.CleanupWorktree && item.IsClosed && !item.IsPR
}

// clearPauseOnClosedCleanup removes fabrik:paused and fabrik:awaiting-input from
// a closed item being cleaned up. It returns false when a removal failed with a
// real error, so the caller can leave the completion label unapplied and retry
// the whole cleanup next poll. No comment is posted — nobody waits on a closed
// issue — and one log line is written when the visible labels carried a pause
// label.
//
// Removal is unconditional (not guarded by hasLabel) because cleanup items come
// from the shallow board listing, whose label list can be incomplete, as for
// fabrik:extend-turns. removeLabelChecked treats ErrNotFound as success.
func (e *Engine) clearPauseOnClosedCleanup(item gh.ProjectItem) bool {
	carried := hasLabel(item.Labels, "fabrik:paused") || hasLabel(item.Labels, "fabrik:awaiting-input")
	ok := true
	for _, label := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
		if err := e.removeLabelChecked(item, label); err != nil {
			e.logf(item.Number, "warn", "could not remove %s at cleanup, retrying next poll: %v\n", label, err)
			ok = false
		}
	}
	if ok {
		// The cause label lives and dies with fabrik:paused (#2487).
		e.clearPauseCause(item)
	}
	if ok && carried {
		e.logf(item.Number, "cleanup", "pause cleared because the issue is closed (#2459)\n")
	}
	return ok
}
