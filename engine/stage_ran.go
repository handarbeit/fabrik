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
// The decision reads durable state only: labels, the store's engine-pause record
// and the stage's own output comment (FR-002). The comment is what covers a
// stage that blocked on input and was resumed by a comment pass that did not
// complete it — that leaves no label. It is a weaker signal than the labels
// (Comments is a deep field and can lag; an incomplete run also posts partial
// output), but a stage that has posted output is one the comment skills are
// written for, and it matches FR-003 ("whether the stage completed, blocked on
// input or is still being reworked"). Anything undeterminable counts as "not
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
	return findStageComment(item.Comments, stage.Name) != nil
}
