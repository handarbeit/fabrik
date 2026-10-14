package engine

import (
	"fmt"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// SetStageFirstRoutingDisabledForTest restores the pre-#2220 routing (a pending
// comment always goes to the comment path, whether or not the stage has run) so
// a test can show its stage-first assertions bite (the non-vacuity seam).
func (e *Engine) SetStageFirstRoutingDisabledForTest(disabled bool) {
	e.stageFirstRoutingOff.Store(disabled)
}

// stageRanOnEntry reports whether the item's current stage has already produced
// its output, so that a pending comment belongs to the comment path (#2220,
// ADR 2220). When it has not, the stage itself runs first and the comments stay
// unprocessed for the comment pass that follows it.
//
// The decision reads labels and the store's engine-pause record only: both
// survive a restart, and neither lags the way the deep Comments field can (a
// stale read of a stage comment would re-run a finished stage). A stage comment
// is deliberately not a signal — an incomplete run posts its partial output as
// one, and handing a pending comment to the comment skill then could complete
// the stage from that partial output. Anything undeterminable counts as "not
// run", so the stage runs.
func (e *Engine) stageRanOnEntry(item gh.ProjectItem, stage *stages.Stage, enginePaused, resumedFromPause bool) bool {
	if e.stageFirstRoutingOff.Load() {
		return true
	}
	if resumedFromPause || enginePaused {
		return true
	}
	for _, l := range []string{
		fmt.Sprintf("stage:%s:complete", stage.Name),
		fmt.Sprintf("stage:%s:failed", stage.Name),
		reworkingLabelName(stage.Name),
		"fabrik:awaiting-ci",
		"fabrik:awaiting-review",
		"fabrik:awaiting-done",
	} {
		if hasLabel(item.Labels, l) {
			return true
		}
	}
	return false
}
