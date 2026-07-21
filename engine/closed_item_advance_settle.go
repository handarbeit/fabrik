package engine

import (
	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// settleClosedItemsToDone is the per-poll settle scan that generalizes
// runValidatePRTerminalAdvance's "closed item → advance to Done" transition
// from Validate-only to any non-Done, non-Holding, non-cleanup, non-gate-checked
// column. A closed issue sitting at Specify/Plan/Implement/Review/Backlog never
// passes itemMayNeedWork/itemNeedsWork's admission guard (engine/item.go), so it
// never reaches deepFetchCandidates and is never dispatched again — its worktree
// is never reaped and it never gets archived. Sourced directly from board.Items,
// not deepFetchCandidates, for the same reason as the child-placement and
// merge-train-member-close settle scans: the item this scan targets never
// reaches deepFetchCandidates in the first place.
//
// Deliberately not conditioned on any label (fabrik:paused, fabrik:awaiting-input,
// fabrik:blocked, etc.) — a closed issue at a non-terminal column is itself the
// complete, sufficient, and durable trigger; there is no marker to lose or leak,
// and no in-flight gate/lock label survives a closed issue meaningfully (no
// further pipeline work can occur on it regardless). See ADR-064.
//
// Gate-checked stages (currently only Validate) are excluded so this scan never
// races or double-advances against runValidatePRTerminalAdvance, which remains
// the exclusive owner of closed items at gate-checked stages.
func (e *Engine) settleClosedItemsToDone(board *gh.ProjectBoard) {
	cleanup := cleanupStage(e.cfg)
	if cleanup == nil {
		return
	}
	// Hoisted out of the per-item loop: both checks are independent of the
	// individual item, so evaluating them once avoids O(N) redundant map
	// lookups and repeated warning-log spam when the status field or option
	// is unavailable across a poll with many stranded closed items.
	if e.statusField == nil {
		e.logf(0, "warn", "closed-item advance: status field metadata not available; cannot move items to %s — will retry next poll\n", cleanup.Name)
		return
	}
	optionID, ok := e.statusField.Options[cleanup.Name]
	if !ok {
		e.logf(0, "warn", "closed-item advance: no status option %q found on project board (available: %v) — will retry next poll\n",
			cleanup.Name, mapKeys(e.statusField.Options))
		return
	}
	for _, item := range board.Items {
		if !item.IsClosed {
			continue
		}
		// stage == nil covers columns with no matching stage config (Backlog,
		// or any custom/extra column) — such an item has no stage bookkeeping
		// to do and, by construction, no worktree, so it advances unconditionally.
		// Only a *resolved* stage that is itself Cleanup/Holding/gate-checked
		// is grounds to skip.
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage != nil && (stage.CleanupWorktree || stage.HoldingStage || stageIsGateChecked(stage)) {
			continue
		}
		e.advanceClosedItemToDone(board, item, optionID, cleanup.Name)
	}
}

// advanceClosedItemToDone moves a single closed item's board Status directly
// to the cleanup stage, mirroring advanceToQueued's shape (status-field lookup,
// API call, cache write-through, webhook echo). No completion label is added —
// unlike advanceToQueued, this scan has no stage:X:complete bookkeeping to do;
// landing at the cleanup column is sufficient to let the existing
// CleanupWorktree dispatch path (engine/item.go) take over on the next poll.
func (e *Engine) advanceClosedItemToDone(board *gh.ProjectBoard, item gh.ProjectItem, optionID, cleanupName string) {
	e.logf(item.Number, "closed-advance", "closed issue stranded outside %s — advancing\n", cleanupName)
	if err := e.client.UpdateProjectItemStatus(board.ProjectID, item.ItemID, e.statusField.FieldID, optionID); err != nil {
		e.logf(item.Number, "warn", "closed-item advance: could not move to %s: %v — will retry next poll\n", cleanupName, err)
		return
	}
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), cleanupName)
	}
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}
}
