package engine

import (
	"errors"
	"time"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// settleNoWorkNeededScan retries the outstanding Done-move/close for any
// item carrying fabrik:awaiting-done, independent of item.Status (the board
// move may still be sitting at whichever column the emitting stage ran in —
// see itemMayNeedWork/itemNeedsWork, which suppress normal dispatch for
// these items). Sourced from board.Items directly, NOT deepFetchCandidates —
// itemMayNeedWork returns false for exactly the items this scan exists to
// retry (retry eligibility and dispatch eligibility are different questions;
// see #1220), so a candidate list pre-filtered by it would drop them before
// this scan ever saw them. This matches the precedent already established by
// settleMergeTrainMemberCloses, settleNonDefaultBaseCloses, and
// settleChildPlacements. Paused items are skipped: either
// escalateNoWorkNeededFailure already handled them (marker removed) or an
// operator is investigating a paused item for an unrelated reason and this
// scan must not fight them.
func (e *Engine) settleNoWorkNeededScan(board *gh.ProjectBoard) {
	for _, item := range board.Items {
		if !hasLabel(item.Labels, "fabrik:awaiting-done") || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		if stage == nil {
			e.logf(item.Number, "warn", "no-work-needed settle: no stage matches board column %q — will retry next poll\n", item.Status)
			e.recordNoWorkNeededRetry(item)
			continue
		}
		// item.Comments is only populated by a deep fetch, which this item never
		// received (that's the whole reason it's stranded and reached here via
		// board.Items rather than deepFetchCandidates). Once the board has moved to
		// Done, settleNoWorkNeeded no longer consults item.Comments (its
		// hasSkippedComment idempotency check only runs while item.Status != "Done"),
		// so the fetch is skipped there to avoid spending a GraphQL call on the
		// already-working "Done move succeeded, only CloseIssue failed" sub-case.
		if item.Status != "Done" {
			repo := itemOwnerRepoString(item, e.defaultRepo())
			if err := e.readClient.FetchItemDetails(&item); err != nil {
				e.logf(item.Number, "warn", "no-work-needed settle: could not deep-fetch item details: %v — will retry next poll\n", err)
				e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: item.Number, At: time.Now()})
				e.recordNoWorkNeededRetry(item)
				continue
			}
			e.store.Apply(itemstate.ItemDeepFetched{Repo: repo, Number: item.Number, FreshState: item})
		}
		e.settleNoWorkNeeded(board, item, stage)
	}
}

// settleRevalidateScan handles operator-facing fabrik:revalidate label
// re-entry. Runs on ALL candidates unconditionally (paused items included —
// FR-5). Uses next-poll dispatch: does not mutate candidates in place.
func (e *Engine) settleRevalidateScan(candidates []gh.ProjectItem) {
	for _, item := range candidates {
		if !hasLabel(item.Labels, "fabrik:revalidate") {
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
		if !hasLabel(item.Labels, "stage:Validate:complete") {
			continue
		}
		e.checkValidateSHAInvalidation(item)
	}
}

// checkValidateSHAInvalidation evaluates one stage:Validate:complete item against its
// recorded completion SHA and strips the stale completion labels when the linked PR's
// head moved on. It returns true when it invalidated the item (the caller must then
// treat the item's label copy as stale). The #2158 exemptions, FR-5 empty-SHA skip and
// FR-6 worker guard all live here, so the scan and the pre-landing hydrate (#2343)
// apply exactly the same rule.
func (e *Engine) checkValidateSHAInvalidation(item gh.ProjectItem) bool {
	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	repoStr := itemOwnerRepoString(item, e.defaultRepo())
	snap, snapErr := e.store.Get(repoStr, item.Number)
	if snapErr != nil {
		return false
	}
	completedSHA := snap.ValidateCompletedSHA()
	if completedSHA == "" {
		// FR-5: no recorded SHA (pre-feature item or worktree HEAD unavailable) — do nothing.
		// After a restart the pre-landing hydrate (#2343) fills or handles this case.
		return false
	}
	lpr := snap.LinkedPR()
	if lpr != nil && lpr.HeadSHA == "" {
		// Right after a restart the engine-recorded head can still be empty while the
		// poll's PR facts already name the head (#2343): fall back to the observed head.
		withHead := *lpr
		withHead.HeadSHA = lpr.ObservedHead()
		lpr = &withHead
	}
	if lpr == nil || lpr.HeadSHA == "" || lpr.HeadSHA == completedSHA {
		return false
	}
	// FR-6: in-flight guard — let the active Validate worker finish.
	if snap.Worker() != nil {
		e.logf(item.Number, "validate-sha", "Validate worker in-flight — deferring SHA invalidation to next poll\n")
		return false
	}
	// #2158 R1: landed or terminal items keep their Validate completion.
	if reason := e.validateItemLanded(item, snap, lpr); reason != "" {
		e.logf(item.Number, "validate-sha", "head differs from Validate completion SHA but item is landed/terminal (%s) — leaving completion labels\n", reason)
		return false
	}
	// A catch-up pushes first and posts its marker after, so mid-catch-up the moved
	// head has no marker yet. Defer until the marker is out; an external push in that
	// window is detected one poll late.
	if lpr.Number != 0 && e.catchUpPushInFlight(owner, repo, lpr.Number) {
		e.logf(item.Number, "validate-sha", "merge-train catch-up of PR #%d in flight — deferring SHA invalidation to next poll\n", lpr.Number)
		return false
	}
	// #2158 R2: the engine's own pure catch-up merge is not an external push.
	// Re-record the head so a later real push still differs from the recorded SHA.
	pure, merr := e.headIsPureCatchUp(owner, repo, lpr)
	if merr != nil {
		e.logf(item.Number, "warn", "validate-sha: could not read PR #%d comments for catch-up marker: %v — deferring SHA invalidation to next poll\n", lpr.Number, merr)
		return false
	}
	if pure {
		e.store.Apply(itemstate.ValidateCompletedAtSHA{Repo: repoStr, Number: item.Number, SHA: lpr.HeadSHA})
		e.logf(item.Number, "validate-sha", "head %s is the engine's pure catch-up merge — keeping Validate completion, recording it as the completion SHA\n", lpr.HeadSHA)
		return false
	}
	e.handleValidateSHAInvalidation(item, owner, repo)
	return true
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
		if !hasLabel(item.Labels, childPlacementLabel) || hasLabel(item.Labels, "fabrik:paused") {
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
