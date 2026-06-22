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
// Otherwise it delegates to wm.PushBranch unchanged.
func (e *Engine) pushBranchUnlessQueued(item gh.ProjectItem, wm *WorktreeManager) error {
	if prInMergeQueue(item) {
		e.logf(item.Number, "merge-queue", "PR in merge queue — skipping push (would eject from queue)\n")
		return nil
	}
	return wm.PushBranch(item.Number)
}
