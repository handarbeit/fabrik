package engine

import (
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// runPausedItemMergedPRRecovery scans for issues that are paused with
// fabrik:awaiting-ci or fabrik:awaiting-review but whose linked PR has since
// been merged externally. The main catch-up loop skips all paused items
// unconditionally, so without this scan a merged-while-paused issue would
// remain stranded in Fabrik's bookkeeping until the user manually intervenes.
//
// When a merged PR is detected, the function iterates all pipeline stages
// in ascending Order, starting from the stage after the highest already-complete
// stage and stopping before the cleanup-terminal stage. For each stage where
// WaitForCI or WaitForReviews is true and its :complete label is absent, the
// function adds the completion label. After all labels are added, the gate
// labels are cleared and the issue advances to the next board column.
//
// Uses e.client (direct GitHub API) — not e.readClient — because the boardcache
// may have stale Merged/State from before the PR was merged. Mirrors the same
// choice in checkAutoMergeConvergence (see ADR 053).
//
// Must NEVER dispatch workers or acquire e.sem. This function is read-only aside
// from label mutations and advanceToNextStage.
func (e *Engine) runPausedItemMergedPRRecovery(board *gh.ProjectBoard, items []gh.ProjectItem, advancedItems map[string]bool) {
	for _, item := range items {
		if !hasLabel(item, "fabrik:paused") {
			continue
		}
		if !hasLabel(item, "fabrik:awaiting-ci") && !hasLabel(item, "fabrik:awaiting-review") {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil {
			continue
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "paused-recovery", "could not fetch linked PR: %v — skipping\n", err)
			continue
		}
		if pr == nil || !pr.Merged {
			continue
		}
		e.logf(item.Number, "paused-recovery", "PR #%d merged while issue was paused — filling in gate-checked completion labels and advancing\n", pr.Number)

		// Find the highest-order stage that already has a :complete label so we
		// only fill in stages that haven't run yet (EC-3).
		highestCompleteOrder := -1
		for _, s := range e.cfg.Stages {
			if !s.CleanupWorktree && hasLabel(item, fmt.Sprintf("stage:%s:complete", s.Name)) {
				if s.Order > highestCompleteOrder {
					highestCompleteOrder = s.Order
				}
			}
		}

		// Iterate stages in ascending Order starting from the one after the
		// highest already-complete stage, stopping before the cleanup-terminal
		// stage (FR-1, FR-2). For each gate-checked stage missing its :complete
		// label, add it. Fail-fast on error to preserve idempotent retry (EC-2).
		//
		// Note: the Validate SHA-recording side-effect in addCompleteLabelAndRemoveCI
		// is intentionally skipped here — the PR is already merged when this code
		// path runs, so the SHA-invalidation guard has no work to do.
		fillFailed := false
		for _, s := range e.cfg.Stages {
			if s.CleanupWorktree {
				break
			}
			if s.Order <= highestCompleteOrder {
				continue
			}
			isGateChecked := (s.WaitForCI != nil && *s.WaitForCI) || (s.WaitForReviews != nil && *s.WaitForReviews)
			if !isGateChecked {
				continue
			}
			completeLabel := fmt.Sprintf("stage:%s:complete", s.Name)
			if hasLabel(item, completeLabel) {
				continue // already present — no-op (EC-2)
			}
			if addErr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); addErr != nil {
				e.logf(item.Number, "warn", "paused-recovery: could not add %s: %v — skipping item\n", completeLabel, addErr)
				fillFailed = true
				break
			} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
				cacheImpl.ApplyLabelAdded(boardcache.ItemKey(item.Repo, item.Number), completeLabel)
			}
			e.logf(item.Number, "paused-recovery", "added %s\n", completeLabel)
		}
		if fillFailed {
			continue
		}

		// Clear gate labels now that all completion labels have been added.
		if hasLabel(item, "fabrik:awaiting-ci") {
			e.removeAwaitingCILabel(owner, repo, item)
		}
		if hasLabel(item, "fabrik:awaiting-review") {
			e.removeAwaitingReviewLabel(owner, repo, item)
		}
		for _, lbl := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
			if hasLabel(item, lbl) {
				if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, lbl); rerr != nil {
					e.logf(item.Number, "warn", "paused-recovery: could not remove %s: %v\n", lbl, rerr)
				} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(item.Repo, item.Number), lbl)
				}
			}
		}
		if aerr := e.advanceToNextStage(board, item, stage); aerr != nil {
			e.logf(item.Number, "warn", "paused-recovery: could not advance: %v\n", aerr)
		}
		advancedItems[issueKey(item, e.defaultRepo())] = true
	}
}
