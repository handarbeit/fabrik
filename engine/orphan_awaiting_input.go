package engine

import (
	"errors"

	gh "github.com/handarbeit/fabrik/github"
)

// SetOrphanAwaitingInputClearDisabledForTest turns the orphan-label clearing
// off (#1970) so a test can show its assertions bite (the non-vacuity seam).
func (e *Engine) SetOrphanAwaitingInputClearDisabledForTest(disabled bool) {
	e.orphanAwaitingInputOff.Store(disabled)
}

// clearOrphanAwaitingInput removes a bare fabrik:awaiting-input — one with no
// fabrik:paused beside it (#1970). The engine always applies the pair together,
// but about 25 pause messages tell the operator to remove only fabrik:paused,
// and isAwaitingInput (both labels) plus the paused-only guards then treat the
// item as running normally while the stale label lingers, surfacing as
// needs-human. handleStageComplete clears it only when a run emits
// FABRIK_STAGE_COMPLETE, which the comment skills never do. A comment-path run
// that emits FABRIK_BLOCKED_ON_INPUT pauses the item properly (#2280); that pair
// carries fabrik:paused and is left alone here.
//
// The decision reads live labels (item.Labels is a snapshot that predates the
// resume gate's own removals) and strips only when fabrik:paused is absent: a
// real pause is lifted by a human comment alone (ADR-069, ADR-1813), never here.
// A fetch or removal failure is logged and left for the next pass.
func (e *Engine) clearOrphanAwaitingInput(item gh.ProjectItem) {
	if e.orphanAwaitingInputOff.Load() {
		return
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	labels, err := e.client.FetchLabels(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "warn", "clearOrphanAwaitingInput: could not fetch labels: %v\n", err)
		return
	}
	if !hasLabel(labels, "fabrik:awaiting-input") || hasLabel(labels, "fabrik:paused") {
		return
	}
	if err := e.removeLabelChecked(item, "fabrik:awaiting-input"); err != nil && !errors.Is(err, gh.ErrNotFound) {
		e.logf(item.Number, "warn", "could not remove orphaned awaiting-input label: %v\n", err)
		return
	}
	e.logf(item.Number, "unblock", "removed orphaned fabrik:awaiting-input (no fabrik:paused) (#1970)\n")
}
