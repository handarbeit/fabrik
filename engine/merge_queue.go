package engine

import (
	gh "github.com/handarbeit/fabrik/github"
)

// Merge-queue awareness (ADR-058 D3).
//
// On a repo with GitHub's merge queue enabled, Fabrik's preemptive rebasing and
// branch mutations fight the queue: the queue already enforces "up-to-date at
// merge time," and *any* push/rebase/base-change to a PR that is currently in the
// queue ejects it. These two pure helpers gate every engine-initiated git/PR
// branch mutation so Fabrik never touches a queued PR and stops preemptive
// rebasing on queue-enabled repos.
//
// Both helpers source their signal exclusively from the GraphQL-populated
// ProjectItem fields (LinkedPRIsInMergeQueue, LinkedPRIsMergeQueueEnabled) — never
// from e.client.FetchLinkedPR, whose REST backing always returns false for these
// flags (the queue state is GraphQL-only; see github/prs.go FetchLinkedPR). Both
// are false-by-default, so behavior on non-queue repos is byte-for-byte unchanged
// (the ADR-058 D1 backward-compat guarantee, FR-3).

// prInMergeQueue reports whether the linked PR is currently in the merge queue
// (FR-1). When true, the queue owns the PR: any push, rebase, or base change
// ejects it, so every mutation site must skip. Fires on the in-queue signal
// alone, regardless of the merge_queue kill-switch — a PR physically in the queue
// is queued no matter what the config says (it may have been queued before the
// operator flipped the switch).
func prInMergeQueue(item gh.ProjectItem) bool {
	return item.LinkedPRIsInMergeQueue
}

// suppressPreemptiveRebase reports whether preemptive rebasing (behind-but-clean)
// should be skipped for this item (FR-2). On a queue-enabled repo the queue
// enforces up-to-date at merge time, so Fabrik stops preemptively rebasing
// cruise/manual PRs. Keyed on LinkedPRIsMergeQueueEnabled && cfg.MergeQueue != "off"
// so the "off" kill-switch restores legacy preemptive-rebase behavior (mirrors the
// D2 enqueue kill-switch). This governs only the preemptive rebase in
// updateWorktreeFromMain; genuine conflict resolution (dispatchRebaseReinvoke,
// gated on PRMergeConflicting) is unaffected.
func (e *Engine) suppressPreemptiveRebase(item gh.ProjectItem) bool {
	return item.LinkedPRIsMergeQueueEnabled && e.cfg.MergeQueue != "off"
}

// pushBranchUnlessQueued wraps WorktreeManager.PushBranch with the FR-1 in-queue
// guard: pushing a queued PR's branch ejects it from the merge queue, so when the
// linked PR is in the queue the push is skipped and nil is returned (a no-op, as
// if the push had succeeded — callers treat push errors as non-fatal anyway).
//
// After the queue skip it applies the zero-ahead guard (#2089): a branch with no
// commits ahead of its base while its linked PR is open is not pushed; the item
// is paused and ErrPushRefusedZeroAhead is returned. Otherwise it delegates to
// wm.PushBranch unchanged. PR-creation callers, which push precisely because no
// PR exists yet, use pushBranchForNewPR instead.
func (e *Engine) pushBranchUnlessQueued(item gh.ProjectItem, wm *WorktreeManager) error {
	if prInMergeQueue(item) {
		e.logf(item.Number, "merge-queue", "PR in merge queue — skipping push (would eject from queue)\n")
		return nil
	}
	if e.zeroAheadPushRefused(item, wm) {
		return ErrPushRefusedZeroAhead
	}
	return wm.PushBranch(item.Number)
}

// pushBranchForNewPR is the push for the PR-creation paths (ensureDraftPR,
// processPRCreateMarker). It keeps the FR-1 in-queue skip but deliberately omits
// the zero-ahead guard (#2089): those callers push because no open PR exists, so
// a zero-ahead branch (e.g. a coordinator parent) is legitimate and there is no
// PR for the overwrite to close.
func (e *Engine) pushBranchForNewPR(item gh.ProjectItem, wm *WorktreeManager) error {
	if prInMergeQueue(item) {
		e.logf(item.Number, "merge-queue", "PR in merge queue — skipping push (would eject from queue)\n")
		return nil
	}
	return wm.PushBranch(item.Number)
}

// pushBranchOnCancel is the WIP-preservation push for a cancelled worker (R8,
// #1393). It keeps the in-queue skip and the zero-ahead refusal (#2089) — the
// overwrite is just as destructive on this path — but never pauses: the
// interruption that cancelled the worker (daemon shutdown or TUI stop) already
// pauses the item under pauseIssueMu, and a second pause from here would race
// it. A refusal returns ErrPushRefusedZeroAhead with only a log line.
func (e *Engine) pushBranchOnCancel(item gh.ProjectItem, wm *WorktreeManager) error {
	if prInMergeQueue(item) {
		e.logf(item.Number, "merge-queue", "PR in merge queue — skipping push (would eject from queue)\n")
		return nil
	}
	if e.zeroAheadCheck(item, wm, false) {
		return ErrPushRefusedZeroAhead
	}
	return wm.PushBranch(item.Number)
}
