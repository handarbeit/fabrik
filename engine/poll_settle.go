package engine

import (
	"errors"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// settleNoWorkNeededScan retries the outstanding Done-move/close for any
// item carrying fabrik:awaiting-done, independent of item.Status (the board
// move may still be sitting at whichever column the emitting stage ran in —
// see itemMayNeedWork/itemNeedsWork, which suppress normal dispatch for
// these items). Paused items are skipped: either escalateNoWorkNeededFailure
// already handled them (marker removed) or an operator is investigating a
// paused item for an unrelated reason and this scan must not fight them.
func (e *Engine) settleNoWorkNeededScan(board *gh.ProjectBoard, candidates []gh.ProjectItem) {
	for _, item := range candidates {
		if !hasLabel(item, "fabrik:awaiting-done") || hasLabel(item, "fabrik:paused") {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil {
			e.logf(item.Number, "warn", "no-work-needed settle: no stage matches board column %q — will retry next poll\n", item.Status)
			e.recordNoWorkNeededRetry(item)
			continue
		}
		e.settleNoWorkNeeded(board, item, stage)
	}
}

// settleRevalidateScan handles operator-facing fabrik:revalidate label
// re-entry. Runs on ALL candidates unconditionally (paused items included —
// FR-5). Uses next-poll dispatch: does not mutate candidates in place.
func (e *Engine) settleRevalidateScan(candidates []gh.ProjectItem) {
	for _, item := range candidates {
		if !hasLabel(item, "fabrik:revalidate") {
			continue
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil || stage.Name != "Validate" {
			stageName := item.Status
			if stage != nil {
				stageName = stage.Name
			}
			e.logf(item.Number, "warn", "fabrik:revalidate applied to non-Validate stage %q — removing label, no action\n", stageName)
			if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, "fabrik:revalidate"); rerr != nil {
				if errors.Is(rerr, gh.ErrNotFound) {
					if c := e.cache(); c != nil {
						c.ApplyLabelRemoved(boardcache.ItemKey(repoStr, item.Number), "fabrik:revalidate")
					}
				} else {
					e.logf(item.Number, "warn", "revalidate: could not remove label from non-Validate issue: %v\n", rerr)
				}
			} else {
				if c := e.cache(); c != nil {
					c.ApplyLabelRemoved(boardcache.ItemKey(repoStr, item.Number), "fabrik:revalidate")
				}
				if e.webhookMgr != nil {
					e.webhookMgr.RegisterEcho("issues", "unlabeled", boardcache.ItemKey(repoStr, item.Number)+"+"+"fabrik:revalidate")
				}
			}
			continue
		}
		// In-flight guard: do not interrupt an active Validate worker (FR-4).
		if snap, snapErr := e.store.Get(repoStr, item.Number); snapErr == nil && snap.Worker() != nil {
			e.logf(item.Number, "revalidate", "Validate worker in-flight — deferring revalidate to next poll\n")
			continue
		}
		e.handleRevalidateLabel(item, owner, repo)
	}
}

// settleSHAInvalidationScan detects force-pushes or external commits that
// change the linked PR's HEAD SHA after stage:Validate:complete was
// recorded. Runs on ALL candidates where stage:Validate:complete is present.
func (e *Engine) settleSHAInvalidationScan(candidates []gh.ProjectItem) {
	for _, item := range candidates {
		if !hasLabel(item, "stage:Validate:complete") {
			continue
		}
		owner, repo := itemOwnerRepo(item, e.defaultRepo())
		repoStr := itemOwnerRepoString(item, e.defaultRepo())
		snap, snapErr := e.store.Get(repoStr, item.Number)
		if snapErr != nil {
			continue
		}
		completedSHA := snap.ValidateCompletedSHA()
		if completedSHA == "" {
			// FR-5: no recorded SHA (pre-feature item or worktree HEAD unavailable) — do nothing.
			continue
		}
		lpr := snap.LinkedPR()
		if lpr == nil || lpr.HeadSHA == "" || lpr.HeadSHA == completedSHA {
			continue
		}
		// FR-6: in-flight guard — let the active Validate worker finish.
		if snap.Worker() != nil {
			e.logf(item.Number, "validate-sha", "Validate worker in-flight — deferring SHA invalidation to next poll\n")
			continue
		}
		e.handleValidateSHAInvalidation(item, owner, repo)
	}
}

// settleChildPlacements retries the outstanding project Status placement for
// any spawned child carrying fabrik:awaiting-placement, independent of stage
// dispatch. Sourced from board.Items directly, NOT deepFetchCandidates — a
// stranded child sits in a column (typically Backlog) with no matching
// configured stage, so it never passes itemMayNeedWork's stage == nil guard
// and never reaches deepFetchCandidates (see engine/spawn_settle.go). Paused
// items are skipped: either escalateChildPlacementFailure already handled
// them (marker removed) or an operator is investigating for an unrelated
// reason and this scan must not fight them.
func (e *Engine) settleChildPlacements(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item, childPlacementLabel) || hasLabel(item, "fabrik:paused") {
			continue
		}
		if item.IsClosed {
			// A closed child needs no further board dispatch — the only purpose of
			// correct placement was to let the pipeline process it, which no longer
			// applies. Clear the marker without attempting placement or escalation.
			owner, repo := itemOwnerRepo(item, e.defaultRepo())
			e.clearChildPlacementMarker(item, owner, repo)
			continue
		}
		e.settleChildPlacement(board, item)
	}
}
