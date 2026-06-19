package engine

import (
	"errors"
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// runValidatePRTerminalAdvance is the single authoritative owner for the
// "Validate-stage PR reached a terminal state → advance to Done" transition.
// It runs regardless of which gate label (fabrik:awaiting-ci,
// fabrik:awaiting-review, fabrik:rebase-needed, or any future label) is
// present — no disjointness maintained by label negation anywhere (ADR-056 D2).
//
// For a merged PR: fills any missing gate-checked stage:X:complete labels in
// ascending order (starting after the highest already-complete stage), clears
// all gate labels, and calls advanceToNextStage.
//
// For a closed-without-merge PR: applies pauseForPRClosedNotMerged if the
// item is not already paused.
//
// Uses e.client (direct GitHub API) — not e.readClient — because the boardcache
// may have stale Merged/State from before the PR reached its terminal state.
// Mirrors the same choice in checkAutoMergeConvergence (ADR-053 carried constraint).
//
// Must NEVER dispatch workers or acquire e.sem. Runs in the main poll goroutine.
func (e *Engine) runValidatePRTerminalAdvance(board *gh.ProjectBoard, items []gh.ProjectItem, advancedItems map[string]bool) {
	for _, item := range items {
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil || stage.Name != "Validate" || stage.CleanupWorktree {
			continue
		}
		// Items with fabrik:auto-merge-enabled are exclusively managed by
		// checkAutoMergeConvergence (Phase 1). Single owner does not touch them.
		if hasLabel(item, "fabrik:auto-merge-enabled") {
			continue
		}
		iKey := issueKey(item, e.defaultRepo())
		if advancedItems[iKey] {
			continue // already advanced this poll cycle; prevent double-advance
		}

		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
		if err != nil {
			e.logf(item.Number, "pr-terminal", "could not fetch linked PR: %v — skipping\n", err)
			continue
		}
		if pr == nil || pr.Number == 0 {
			continue // no linked PR
		}
		if !pr.Merged && pr.State != "closed" {
			continue // PR is still open; not terminal
		}
		// FetchLinkedPR reads the PR list endpoint, whose `merged` flag is unreliable
		// (false for seconds after a merge). For a closed PR, confirm against the
		// authoritative single-PR endpoint before deciding advance-vs-pause — otherwise
		// a PR the engine just merged (e.g. the direct-merge fallback) gets misread as
		// "closed without merging" and the issue is wrongly paused.
		if !pr.Merged && pr.State == "closed" {
			if merged, mErr := e.client.FetchPRMerged(owner, repo, pr.Number); mErr != nil {
				e.logf(item.Number, "pr-terminal", "PR #%d closed; could not confirm merged state: %v — skipping (re-check next poll)\n", pr.Number, mErr)
				continue
			} else if merged {
				pr.Merged = true
			}
		}

		if pr.Merged {
			e.logf(item.Number, "pr-terminal", "PR #%d merged — filling gate-checked completion labels and advancing to Done\n", pr.Number)

			// Find the highest-order stage that already has a :complete label so we
			// only fill in stages that are missing their completion label (EC-3).
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
			// stage. For each gate-checked stage missing its :complete label, add
			// it. Fail-fast on error to preserve idempotent retry (EC-2).
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
					continue // already present — idempotent no-op
				}
				if addErr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); addErr != nil {
					e.logf(item.Number, "warn", "pr-terminal: could not add %s: %v — skipping item\n", completeLabel, addErr)
					fillFailed = true
					break
				} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
					cacheImpl.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), completeLabel)
				}
				e.logf(item.Number, "pr-terminal", "added %s\n", completeLabel)
			}
			if fillFailed {
				continue
			}

			// Clear all gate labels now that all completion labels have been added.
			if hasLabel(item, "fabrik:awaiting-ci") {
				e.removeAwaitingCILabel(owner, repo, item)
			}
			if hasLabel(item, "fabrik:awaiting-review") {
				e.removeAwaitingReviewLabel(owner, repo, item)
			}
			if hasLabel(item, "fabrik:rebase-needed") {
				e.removeRebaseNeededLabel(owner, repo, item)
			}
			for _, lbl := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
				if hasLabel(item, lbl) {
					if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, lbl); rerr != nil {
						if errors.Is(rerr, gh.ErrNotFound) {
							// Label already absent on GitHub — desired end state achieved; sync cache.
							if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
								cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), lbl)
							}
						} else {
							e.logf(item.Number, "warn", "pr-terminal: could not remove %s: %v\n", lbl, rerr)
						}
					} else if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
						cacheImpl.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), lbl)
					}
				}
			}

			if aerr := e.advanceToNextStage(board, item, stage); aerr != nil {
				e.logf(item.Number, "warn", "pr-terminal: could not advance to Done: %v\n", aerr)
			}
			advancedItems[iKey] = true
			continue
		}

		// PR is closed without merging.
		// Skip if already paused to avoid posting a duplicate comment on the next poll.
		if hasLabel(item, "fabrik:paused") {
			continue
		}
		e.logf(item.Number, "pr-terminal", "PR #%d closed without merging — pausing\n", pr.Number)
		e.pauseForPRClosedNotMerged(board, item, stage, pr.Number)
	}
}
