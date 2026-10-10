package engine

import (
	"fmt"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// trainPhase is one named step of a merge-train episode (#2050, ADR 2050). It is
// the single "train phase changed" value: noteTrainPhase fans it out to BOTH the
// board status line (board, the existing #2048 wording, unchanged) and the TUI
// row (label), so the two projections cannot drift apart.
type trainPhase struct {
	label     string // TUI wording: "trial CI #4012", "bisecting (step 2)", …
	board     string // board status-line text; "" = this phase writes no board line
	activeSet bool   // the members passed to the hook are the train's held set
}

// The constructors below are the only place phase wording lives.

// phaseAdmitted: the batch is formed and about to be assembled.
func phaseAdmitted(batchSize int) trainPhase {
	return trainPhase{label: "assembling", board: statusLineQueued(batchSize), activeSet: true}
}

// phaseAssembling: building a trial branch. No board line (none existed).
func phaseAssembling() trainPhase { return trainPhase{label: "assembling"} }

// phaseWaitingForSlot: the train is queued for a worker slot for conflict
// resolution. No board line.
func phaseWaitingForSlot() trainPhase { return trainPhase{label: "waiting for slot"} }

// phaseResolvingConflicts: Claude is resolving a conflict on member num.
func phaseResolvingConflicts(num int) trainPhase {
	return trainPhase{
		label: fmt.Sprintf("resolving conflicts on #%d", num),
		board: statusLineTrial(0, "resolving conflicts"),
	}
}

// phaseTrialCI: waiting on the trial's CI; prNum is the draft CI PR (0 before
// one exists, or under the test seam).
func phaseTrialCI(prNum int) trainPhase {
	label := "trial CI"
	if prNum > 0 {
		label = fmt.Sprintf("trial CI #%d", prNum)
	}
	return trainPhase{label: label, board: statusLineTrial(prNum, "CI running"), activeSet: true}
}

// phaseBisecting: bisection validation step of at most ceiling.
func phaseBisecting(step, ceiling int) trainPhase {
	return trainPhase{
		label:     fmt.Sprintf("bisecting (step %d)", step),
		board:     statusLineBisect(step, ceiling),
		activeSet: true,
	}
}

func phaseLanding() trainPhase {
	return trainPhase{label: "landing", board: statusLineLanding, activeSet: true}
}

// phaseOneAtATime: the fallback processing members as singleton batches. No
// board line (each singleton's own trial writes its own).
func phaseOneAtATime() trainPhase { return trainPhase{label: "one-at-a-time"} }

// phaseCatchingUp: the singleton catch-up is waiting on prNum's CI.
func phaseCatchingUp(prNum int) trainPhase {
	return trainPhase{label: fmt.Sprintf("catching up #%d", prNum), board: statusLineCatchUp(prNum)}
}

// phaseReleased: the train gives its members back to the Queued column.
func phaseReleased() trainPhase {
	return trainPhase{label: "released", board: statusLineQueuedWaiting}
}

// trainPhaseRecord is what the test observer sees for every hook call: the TUI
// label and the board text (""= none) the call produced.
type trainPhaseRecord struct {
	Label string
	Board string
}

// noteTrainPhase is the single "train phase changed" hook (R4/FR-008). In order:
//
//  1. the board status line — via the existing setMembersStatusLine, wording
//     untouched, skipped when the phase has no board text (FR-014: the board's
//     lines, timing and count are unchanged);
//  2. the episode: phase + start time (and the held set for a phase that has
//     one);
//  3. one structural tui.TrainRowEvent — never the droppable emit, because a
//     dropped event leaves a stale phase on the row, and never while holding
//     the episode lock.
//
// members is the set the board line is written for; a nil episode (callers with
// no worker context) still gets the board line, so behaviour without a TUI is
// exactly the pre-#2050 one.
func (e *Engine) noteTrainPhase(ep *trainEpisode, repoKey string, members []trainMember, ph trainPhase) {
	if ph.board != "" && len(members) > 0 {
		e.setMembersStatusLine(members, ph.board)
	}
	if e.trainPhaseObserverForTest != nil {
		e.trainPhaseObserverForTest(trainPhaseRecord{Label: ph.label, Board: ph.board})
	}
	if ep == nil {
		return
	}
	now := time.Now()
	ep.mu.Lock()
	if ph.activeSet && members != nil {
		nums := make([]int, len(members))
		for i, m := range members {
			nums[i] = m.item.Number
		}
		ep.active = nums
	}
	ep.phase = ph.label
	ep.phaseStartedAt = now
	title := ep.titleLocked()
	ep.mu.Unlock()
	e.emitStructural(tui.TrainRowEvent{Repo: repoKey, Title: title, Phase: ph.label, PhaseStartedAt: now})
}

// emitTrainRow re-emits the row after a membership change, keeping the current
// phase and its start time (the elapsed does not restart on a title-only change).
func (e *Engine) emitTrainRow(ep *trainEpisode, repoKey string) {
	if ep == nil {
		return
	}
	ep.mu.Lock()
	title, phase, started := ep.titleLocked(), ep.phase, ep.phaseStartedAt
	ep.mu.Unlock()
	e.emitStructural(tui.TrainRowEvent{Repo: repoKey, Title: title, Phase: phase, PhaseStartedAt: started})
}

// noteTrainEjected records that member num left the train for a reason (short,
// title-friendly) and refreshes the row. atAssembly marks an ejection made
// while forming/assembling the trial.
func (e *Engine) noteTrainEjected(ep *trainEpisode, repoKey string, num int, reason string, atAssembly bool) {
	if ep.recordEjected(num, reason, atAssembly) {
		e.emitTrainRow(ep, repoKey)
	}
}

// noteTrainDeferred records that member num was held out of the batch (it stays
// Queued) and refreshes the row.
func (e *Engine) noteTrainDeferred(ep *trainEpisode, repoKey string, num int, reason string) {
	if ep.recordDeferred(num, reason) {
		e.emitTrainRow(ep, repoKey)
	}
}

// droppedNumbers returns the numbers present in before but not in after.
func droppedNumbers(before, after []trainMember) []int {
	kept := make(map[int]bool, len(after))
	for _, m := range after {
		kept[m.item.Number] = true
	}
	var out []int
	for _, m := range before {
		if !kept[m.item.Number] {
			out = append(out, m.item.Number)
		}
	}
	return out
}

// SetTrainOutcomeNeutralisedForTest restores the pre-#2050 behaviour of the
// train's completion event — a blanket Skipped: true with no outcome — so the
// History-outcome tests can show they fail without the real emission (FR-013).
func (e *Engine) SetTrainOutcomeNeutralisedForTest(neutralised bool) {
	e.trainOutcomeNeutralisedForTest = neutralised
}
