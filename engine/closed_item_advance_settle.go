package engine

import (
	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// settleClosedItemsToDone is the per-poll settle scan that generalizes
// runValidatePRTerminalAdvance's "closed item → advance to Done" transition
// from Validate-only to any non-Done, non-cleanup, non-Validate column —
// including any other gate-checked stage (e.g. a Review stage configured with
// wait_for_reviews: true, the shipped default) and holding stages (e.g.
// Queued), which are excluded from admission dispatch but not from this scan
// (see below). A closed issue sitting at Specify/Plan/Implement/Review/Backlog
// never passes itemMayNeedWork/itemNeedsWork's admission guard (engine/item.go),
// so it never reaches deepFetchCandidates and is never dispatched again — its
// worktree is never reaped and it never gets archived. Sourced directly from
// board.Items, not deepFetchCandidates, for the same reason as the
// child-placement and merge-train-member-close settle scans: the item this
// scan targets never reaches deepFetchCandidates in the first place.
//
// Deliberately not conditioned on any label (fabrik:paused, fabrik:awaiting-input,
// fabrik:blocked, etc.) — a closed issue at a non-terminal column is itself the
// complete, sufficient, and durable trigger; there is no marker to lose or leak,
// and no in-flight gate/lock label survives a closed issue meaningfully (no
// further pipeline work can occur on it regardless). See ADR-064.
//
// Excluded by stage.Name == "Validate", not stageIsGateChecked(stage) (ADR-1387
// follow-up, caught in review on PR #1388): the exclusion exists solely to avoid
// racing/double-advancing against runValidatePRTerminalAdvance/
// settleClosedValidateAdvance, which own Validate and only Validate — matching
// their scope by name, not by category, is what actually prevents that race.
// stageIsGateChecked(stage) is a broader category (any stage with wait_for_ci OR
// wait_for_reviews) that also matches a Review stage configured with
// wait_for_reviews: true — the shipped default (stages/examples/review.yaml) —
// and using it here left every such stage with zero remaining owners: dispatch
// admission refuses it (R1), no settle-owner processes anything but Validate,
// and this scan (under the old, broader exclusion) skipped it too. A closed
// item stranded at Review was silently un-healed forever. The PR-merge-vs-pause
// nuance that Validate's settle-owner pair implements (advanceValidateTerminalItem
// checks whether the linked PR merged or closed unmerged, filling completion
// labels or pausing accordingly) is specific to Validate: Fabrik's own PR merge
// action (attemptMergeOnValidate) only ever runs as part of Validate completing,
// so no other stage's closed item needs that check — advanceClosedItemToDone's
// plain "move the board column" is exactly as sufficient for a closed Review
// item as it already is for Specify/Plan/Implement.
//
// Holding stages (e.g. Queued) are a universal backstop for issue #1072: a
// closed item stranded there — whatever the cause (a merge-train batch member
// closed by a human merge outside the train, or a pre-#1072 stray item from the
// old holding-stage-blind NextStage) — is rescued exactly like any other stage,
// UNLESS a merge-train worker is currently in flight for that item's repo
// (mergeTrainWorkerActive). That guard is the one real race here: an item can
// be closed-without-merging while it is still a live batch member mid-assembly
// or mid-bisection, and this scan must not yank it out from under the worker.
// Once the worker exits, finishTrain clears the marker and the item is safe to
// sweep unconditionally — same as any other stage, no PR-merge re-confirmation
// needed (see ADR-1072).
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
		// stage == nil covers columns with no matching stage config (a custom/
		// extra column, or Backlog on installs predating backlog.yaml) — such an
		// item has no stage bookkeeping to do and, by construction, no worktree,
		// so it advances unconditionally. A resolved Unmanaged stage (e.g. the
		// declared Backlog stage, issue #973) is deliberately NOT added to the
		// skip condition below for the same reason: it has no worktree either,
		// so a closed item there must still be advanced rather than stranded.
		// Only a *resolved* stage that is itself Cleanup/Validate, or a
		// Holding stage with a live train worker for its repo, is grounds to skip.
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage != nil {
			if stage.CleanupWorktree || stage.Name == "Validate" {
				continue
			}
			if stage.HoldingStage && e.mergeTrainWorkerActive(itemOwnerRepoString(item, e.defaultRepo())) {
				continue
			}
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
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	if c := e.cache(); c != nil {
		c.UpdateItemStatus(boardcache.ItemKey(owner+"/"+repo, item.Number), cleanupName)
	}
	// Advances the probe staleness baseline (#1090) — the status move above just
	// bumped the item's real GitHub updatedAt via the project-item mutation.
	e.store.Apply(itemstate.SelfWriteObserved{Repo: owner + "/" + repo, Number: item.Number})
	if e.webhookMgr != nil {
		e.webhookMgr.RegisterEchoIfSubscribed("projects_v2_item", "edited", item.ItemID)
	}
}
