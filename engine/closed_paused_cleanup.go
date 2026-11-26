package engine

import (
	"strings"

	"github.com/handarbeit/fabrik/internal/itemstate"
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

// SetClosedCleanupFillDisabledForTest turns off the landing-label fill of a
// closed item's Done cleanup (#2628) so a test can show its assertions bite
// (the non-vacuity seam). It is independent of SetClosedPausedCleanupDisabledForTest:
// the pause clearing (#2459) keeps working with only this flag set.
func (e *Engine) SetClosedCleanupFillDisabledForTest(disabled bool) {
	e.closedCleanupFillOff.Store(disabled)
}

// deferClosedCleanup records the periodic-re-eval cooldown that makes the whole
// closed-item cleanup retry next poll. stage:<Done>:complete is the only
// re-admission stop, so every failure before it must come back here (#2459,
// #2628).
func (e *Engine) deferClosedCleanup(repoStr string, item gh.ProjectItem) {
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   repoStr,
		Number: item.Number,
		Reason: reasonPeriodicReEval,
		Until:  e.now().Add(e.githubRecheckInterval()),
	})
}

// fillMergedLandingLabelsOnClosedCleanup records, on a closed item first seen at
// Done, that its linked PR landed (#2628, ADR 2628). A PR merged outside Fabrik
// closes the issue and the board's "Item closed -> Done" automation moves it to
// Done before the engine polls, so the Validate terminal path never runs; this
// fills the gate-checked completion labels with fillGateCheckedCompleteLabels,
// unchanged, before the caller applies stage:<Done>:complete.
//
// The linked PR is read live (classifyLinkedPRTerminal), never from cached
// state. It returns false when the read or the fill failed, so the caller leaves
// the completion label unapplied and retries the whole cleanup next poll. No PR,
// an open PR and a PR closed unmerged return true having written nothing. No
// comment, board move, landing-verification marker or pause is added.
func (e *Engine) fillMergedLandingLabelsOnClosedCleanup(item gh.ProjectItem) bool {
	if e.closedCleanupFillOff.Load() {
		return true
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, state := e.classifyLinkedPRTerminal(owner, repo, item.Number, "cleanup")
	switch state {
	case linkedPRReadErr:
		return false
	case linkedPRMerged:
	default:
		return true
	}
	missing := e.missingGateCheckedCompleteLabels(item)
	if e.fillGateCheckedCompleteLabels(owner, repo, item) {
		return false
	}
	if len(missing) > 0 {
		e.logf(item.Number, "cleanup", "linked PR #%d merged — filled %s before Done cleanup (#2628)\n", pr.Number, strings.Join(missing, ", "))
	}
	return true
}
