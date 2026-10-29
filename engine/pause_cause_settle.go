package engine

import (
	"errors"

	gh "github.com/handarbeit/fabrik/github"
)

// settlePauseCauseScan removes a fabrik:pause:<cause> label that outlived its
// fabrik:paused (#2487, ADR 2487). The engine's own lift paths clear the cause
// with the pause (clearPauseCause), but the dominant way a pause ends is a human
// removing fabrik:paused in the GitHub UI, which no engine code sees. A cause
// label with no pause would mislead exactly where people look, so this
// board-sourced scan (the "dedicated settle scan" pattern, ADR-1270 — an item the
// engine never dispatches still gets cleaned) strips it.
//
// The board snapshot only nominates candidates. The decision reads live labels
// (liveLabels) and removes only when fabrik:paused is absent there: the pause
// write order is paused first, then the cause, so a pause being applied right now
// is never mistaken for a stale cause. An unreadable item is left for the next
// pass (fail-closed toward keeping the label). Nothing here touches resume.
func (e *Engine) settlePauseCauseScan(board *gh.ProjectBoard) {
	if e.pauseCauseSettleOff.Load() {
		return
	}
	for _, item := range board.Items {
		if item.IsPR || hasLabel(item.Labels, "fabrik:paused") || len(pauseCauseLabelsIn(item.Labels)) == 0 {
			continue
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		labels, err := e.liveLabels(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "warn", "pause-cause settle: could not fetch labels: %v\n", err)
			continue
		}
		if hasLabel(labels, "fabrik:paused") {
			continue
		}
		for _, l := range pauseCauseLabelsIn(labels) {
			if err := e.removeLabelChecked(item, l); err != nil && !errors.Is(err, gh.ErrNotFound) {
				e.logf(item.Number, "warn", "could not remove stale pause-cause label %s: %v\n", l, err)
				continue
			}
			e.logf(item.Number, "unpause", "removed stale %s (no fabrik:paused) (#2487)\n", l)
		}
	}
}

// pauseCauseLabelsIn returns the cause-family labels in labels.
func pauseCauseLabelsIn(labels []string) []string {
	var out []string
	for _, l := range labels {
		if isPauseCauseLabel(l) {
			out = append(out, l)
		}
	}
	return out
}

// SetPauseCauseSettleDisabledForTest turns the stale-cause scan off (the
// non-vacuity seam: a test shows its stale-label assertion bites without it).
func (e *Engine) SetPauseCauseSettleDisabledForTest(disabled bool) {
	e.pauseCauseSettleOff.Store(disabled)
}
