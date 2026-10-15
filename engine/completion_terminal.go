package engine

import (
	"errors"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// Stage completion after the linked PR already reached a terminal state (#2299,
// ADR 2299). A Validate worker can return after its own PR merged (a second
// Validate run was in flight while the CI gate of the first cleared and the
// settle scan merged). Completion used to arm fabrik:awaiting-ci against the
// merged PR without looking at it.

// ErrPushSkippedPRMerged is returned by the post-stage push wrappers when the
// item's linked PR is already merged: pushing would at best fail the lease and at
// worst re-create the remote branch GitHub deleted on merge (#2299 R5).
var ErrPushSkippedPRMerged = errors.New("push skipped: linked PR already merged")

// SetCompletionMergedCheckDisabledForTest neutralises the live terminal check in
// handleStageComplete and runCatchUpPhase2 (the non-vacuity seam, #2299 R1).
func (e *Engine) SetCompletionMergedCheckDisabledForTest(disabled bool) {
	e.completionMergedCheckOff.Store(disabled)
}

// SetMergeInFlightGuardDisabledForTest neutralises the in-flight-worker merge
// deferral in runCatchUpPhase2 (the non-vacuity seam, #2299 R4).
func (e *Engine) SetMergeInFlightGuardDisabledForTest(disabled bool) {
	e.mergeInFlightGuardOff.Store(disabled)
}

// SetMergedPushGuardDisabledForTest neutralises the merged-PR pre-push check
// (the non-vacuity seam, #2299 R5).
func (e *Engine) SetMergedPushGuardDisabledForTest(disabled bool) {
	e.mergedPushGuardOff.Store(disabled)
}

// completionTerminalCheck is handleStageComplete's live look at the linked PR of
// a wait_for_ci stage that just completed. It returns true when it handled the
// item (merged → terminal path; closed unmerged → pause) and the caller must
// return without arming fabrik:awaiting-ci. Validate only: the terminal path is
// Validate-specific, and any other wait_for_ci stage keeps today's behaviour
// (the settle scan's PRMergeTerminal handling covers it). A read failure is
// logged and returns false, so the label is armed as before (R3).
func (e *Engine) completionTerminalCheck(board *gh.ProjectBoard, item gh.ProjectItem, stage *stages.Stage) bool {
	if e.completionMergedCheckOff.Load() || stage.Name != "Validate" || stage.CleanupWorktree {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, state := e.classifyLinkedPRTerminal(owner, repo, item.Number, "awaiting-ci")
	switch state {
	case linkedPRReadErr:
		e.logf(item.Number, "awaiting-ci", "could not read linked PR at completion — arming awaiting-ci as before; the settle scan's terminal handling is the backstop\n")
		return false
	case linkedPRMerged:
		e.logf(item.Number, "awaiting-ci", "linked PR #%d already merged at completion — taking terminal path, not arming awaiting-ci\n", pr.Number)
		e.advanceMergedValidateItem(board, item, stage, pr, nil)
		return true
	case linkedPRClosedUnmerged:
		if hasLabel(item.Labels, "fabrik:paused") {
			e.logf(item.Number, "awaiting-ci", "linked PR #%d closed without merging at completion, item already paused — not arming awaiting-ci\n", pr.Number)
			return true
		}
		e.logf(item.Number, "awaiting-ci", "linked PR #%d closed without merging at completion — pausing, not arming awaiting-ci\n", pr.Number)
		e.pauseForPRClosedNotMerged(board, item, stage, pr.Number)
		return true
	}
	return false
}

// mergeDeferredForWorker is runCatchUpPhase2's R4 guard: while a worker for the
// item is in flight the merge decision waits. It records a merge-unsettled
// admission cooldown (ADR 2096) because nothing else re-admits the item once the
// worker exits (WorkerExited is not a cycle-set trigger), and returns true when
// the caller must not merge.
func (e *Engine) mergeDeferredForWorker(item gh.ProjectItem) bool {
	if e.mergeInFlightGuardOff.Load() {
		return false
	}
	repo := itemOwnerRepoString(item, e.defaultRepo())
	stageName, inFlight := e.ciBackstopWorkerInFlight(repo, item.Number)
	if !inFlight {
		return false
	}
	e.logf(item.Number, "awaiting-ci", "merge deferred — worker in flight (stage=%s); re-evaluating on a later poll\n", stageName)
	e.store.Apply(itemstate.CooldownRecorded{
		Repo:   repo,
		Number: item.Number,
		Reason: mergeUnsettledCooldownReason,
		Until:  e.now().Add(e.mergeGateRecheckInterval()),
	})
	return true
}

// mergedPRPushRefused reports whether the item's linked PR is already merged, in
// which case the post-stage push must not be made (#2299 R5). One live
// FetchLinkedPR per push; every unreadable input fails open (push as before).
func (e *Engine) mergedPRPushRefused(item gh.ProjectItem, branch string) bool {
	if e.mergedPushGuardOff.Load() || item.IsPR {
		return false
	}
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil || pr == nil || pr.Number == 0 || !pr.Merged {
		return false
	}
	e.logf(item.Number, "push", "linked PR #%d already merged — not pushing %s (would re-create the deleted remote branch)\n", pr.Number, branch)
	return true
}

// clearMergedOrphanAwaitingCI is settleAwaitingCIScan's R6 handling for a
// fabrik:awaiting-ci item at a column that is not a wait_for_ci stage: when its
// linked PR is merged the label is a leftover, not a CI wait. It clears the label
// and makes sure the issue is closed where GitHub will not do it. A withheld
// stage:<X>:complete label (the completion was deferred behind the CI gate that
// never ran) is back-filled first, so the item is not hidden from the operator's
// merged-but-open sweeps; the full advance and the landing-verification marker
// are not repeated here — the item already left the CI-waiting column, and those
// belong to the transition that moved it. A failed back-fill leaves the label in
// place so the next poll retries. Returns true when handled (the caller skips the
// orphan retry/escalation).
func (e *Engine) clearMergedOrphanAwaitingCI(item gh.ProjectItem) bool {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, state := e.classifyLinkedPRTerminal(owner, repo, item.Number, "awaiting-ci-settle")
	if state != linkedPRMerged {
		return false
	}
	e.logf(item.Number, "awaiting-ci-settle", "fabrik:awaiting-ci at column %q but linked PR #%d is already merged — clearing the label, not escalating\n", item.Status, pr.Number)
	if e.fillGateCheckedCompleteLabels(owner, repo, item) {
		return true // retry the whole clear on the next poll; still never a pause
	}
	e.removeAwaitingCILabel(owner, repo, item)
	e.closeIssueIfNonDefaultBase(item, pr.Number)
	return true
}
