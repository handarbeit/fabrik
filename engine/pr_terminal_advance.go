package engine

import (
	"errors"
	"fmt"

	"github.com/handarbeit/fabrik/boardcache"
	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/stages"
)

// advanceValidateTerminalItem is the shared per-item "Validate-stage PR
// reached a terminal state → advance to Done" logic (ADR-056 D2). It runs
// regardless of which gate label (fabrik:awaiting-ci, fabrik:awaiting-review,
// fabrik:rebase-needed, or any future label) is present — no disjointness
// maintained by label negation anywhere.
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
//
// Shared by two callers, partitioned on item.IsClosed (ADR-1387): the open-item
// owner runValidatePRTerminalAdvance (fed from deepFetchCandidates) and the
// closed-item owner settleClosedValidateAdvance (fed from board.Items). Neither
// caller filters on IsClosed here — each already only ever passes items of its
// own kind — so this function itself is IsClosed-agnostic, matching its
// pre-split behavior byte-for-byte.
func (e *Engine) advanceValidateTerminalItem(board *gh.ProjectBoard, item gh.ProjectItem, advancedItems map[string]bool) {
	stage := stages.FindStage(e.cfg.Stages, item.Status)
	if stage == nil || stage.Name != "Validate" || stage.CleanupWorktree {
		return
	}
	// Items with fabrik:auto-merge-enabled are exclusively managed by
	// checkAutoMergeConvergence (Phase 1) — but only once stage:Validate:complete
	// is also present, which is the only state attemptMergeOnValidate's
	// auto-merge/enqueue/direct-merge branches are ever supposed to produce (they
	// only ever apply the label once the gate has cleared). Deferring
	// unconditionally on the label alone assumed that invariant always holds.
	//
	// If it is ever violated (a labeling race, or a future caller of the label),
	// deferring here is not the safe skip it looks like — checkAutoMergeConvergence
	// is gated on hasComplete in the Phase 1 catch-up loop (poll.go), so it never
	// even sees an item missing stage:Validate:complete, closed or open. Under the
	// prior skip-and-warn version of this check, healing was not permanently lost:
	// fabrik:auto-merge-enabled is not in gateSettleOwnedTransientLabels
	// (poll.go), so cleanupClosedIssueTransientLabels — which runs later in the
	// same poll — sweeps it off any closed item unconditionally, and the next
	// poll's call into this function then falls through normally. So the actual
	// prior cost was a one-poll-cycle delay for a closed item, not a permanent
	// strand — Pruefer correctly caught the original comment here overstating
	// that. Healing inline below is still strictly better (no dependency on sweep
	// ordering, no wasted poll cycle, and open items reach the same fix — for an
	// open item in this state there is no sweep to fall back on at all, since
	// cleanupClosedIssueTransientLabels only ever runs on closed issues), so the
	// fix is unchanged; only the rationale for why it was worth making is
	// corrected (Pruefer, PR #1388).
	if hasLabel(item.Labels, "fabrik:auto-merge-enabled") {
		if hasLabel(item.Labels, "stage:Validate:complete") {
			return
		}
		e.logf(item.Number, "warn", "fabrik:auto-merge-enabled present without stage:Validate:complete — this violates the assumed invariant; healing via the normal terminal-advance flow instead of deferring to checkAutoMergeConvergence, which is unreachable for this item in this state (ADR-1387)\n")
	}
	iKey := issueKey(item, e.defaultRepo())
	if advancedItems[iKey] {
		return // already advanced this poll cycle; prevent double-advance
	}

	owner, repo := itemOwnerRepo(item, e.defaultRepo())
	pr, err := e.client.FetchLinkedPR(owner, repo, item.Number)
	if err != nil {
		e.logf(item.Number, "pr-terminal", "could not fetch linked PR: %v — skipping\n", err)
		return
	}
	if pr == nil || pr.Number == 0 {
		return // no linked PR
	}
	if !pr.Merged && pr.State != "closed" {
		return // PR is still open; not terminal
	}
	// FetchLinkedPR reads the PR list endpoint, whose `merged` flag is unreliable
	// (false for seconds after a merge). For a closed PR, confirm against the
	// authoritative single-PR endpoint before deciding advance-vs-pause — otherwise
	// a PR the engine just merged (e.g. the direct-merge fallback) gets misread as
	// "closed without merging" and the issue is wrongly paused.
	if !pr.Merged && pr.State == "closed" {
		if merged, mErr := e.client.FetchPRMerged(owner, repo, pr.Number); mErr != nil {
			e.logf(item.Number, "pr-terminal", "PR #%d closed; could not confirm merged state: %v — skipping (re-check next poll)\n", pr.Number, mErr)
			return
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
			if !s.CleanupWorktree && hasLabel(item.Labels, fmt.Sprintf("stage:%s:complete", s.Name)) {
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
			// An Unmanaged stage never receives gate-checked bookkeeping here,
			// regardless of any other flag it carries — it was never actually
			// dispatched to or through. This must be a `continue`, not folded into
			// the CleanupWorktree break below: an Unmanaged stage combining
			// unmanaged: true with cleanup_worktree: true (e.g. a misconfigured
			// Backlog) is also never the real terminal/cleanup stage — see
			// cleanupStage() in engine/stages.go, which applies the same exclusion
			// when resolving "the" Done stage elsewhere — so it must be skipped
			// over, not treated as a break point that would stop the fill loop
			// before any real gate-checked stage is ever considered.
			if s.Unmanaged {
				continue
			}
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
			if hasLabel(item.Labels, completeLabel) {
				continue // already present — idempotent no-op
			}
			if addErr := e.client.AddLabelToIssue(owner, repo, item.Number, completeLabel); addErr != nil {
				e.logf(item.Number, "warn", "pr-terminal: could not add %s: %v — skipping item\n", completeLabel, addErr)
				fillFailed = true
				break
			} else if c := e.cache(); c != nil {
				c.ApplyLabelAdded(boardcache.ItemKey(owner+"/"+repo, item.Number), completeLabel)
			}
			e.logf(item.Number, "pr-terminal", "added %s\n", completeLabel)
		}
		if fillFailed {
			return
		}

		// Clear all gate labels now that all completion labels have been added.
		if hasLabel(item.Labels, "fabrik:awaiting-ci") {
			e.removeAwaitingCILabel(owner, repo, item)
		}
		if hasLabel(item.Labels, "fabrik:awaiting-review") {
			e.removeAwaitingReviewLabel(owner, repo, item)
		}
		if hasLabel(item.Labels, "fabrik:rebase-needed") {
			e.removeRebaseNeededLabel(owner, repo, item)
		}
		for _, lbl := range []string{"fabrik:paused", "fabrik:awaiting-input"} {
			if hasLabel(item.Labels, lbl) {
				if rerr := e.client.RemoveLabelFromIssue(owner, repo, item.Number, lbl); rerr != nil {
					if errors.Is(rerr, gh.ErrNotFound) {
						// Label already absent on GitHub — desired end state achieved; sync cache.
						if c := e.cache(); c != nil {
							c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), lbl)
						}
					} else {
						e.logf(item.Number, "warn", "pr-terminal: could not remove %s: %v\n", lbl, rerr)
					}
				} else if c := e.cache(); c != nil {
					c.ApplyLabelRemoved(boardcache.ItemKey(owner+"/"+repo, item.Number), lbl)
				}
			}
		}

		if aerr := e.advanceToNextStage(board, item, stage); aerr != nil {
			e.logf(item.Number, "warn", "pr-terminal: could not advance to Done: %v\n", aerr)
		}
		e.closeIssueIfNonDefaultBase(item, pr.Number)
		advancedItems[iKey] = true
		return
	}

	// PR is closed without merging.
	// Skip if already paused to avoid posting a duplicate comment on the next poll.
	if hasLabel(item.Labels, "fabrik:paused") {
		return
	}
	e.logf(item.Number, "pr-terminal", "PR #%d closed without merging — pausing\n", pr.Number)
	e.pauseForPRClosedNotMerged(board, item, stage, pr.Number)
}

// runValidatePRTerminalAdvance is the open-item owner of the "Validate-stage
// PR reached a terminal state → advance to Done" transition (ADR-056 D2,
// ADR-1387). Sourced from deepFetchCandidates, so it only ever sees items that
// already passed itemMayNeedWork/itemNeedsWork admission — which, per
// ADR-1387, no longer admits closed items at Validate. The item.IsClosed skip
// below is therefore a redundant-but-explicit ownership boundary, not a
// load-bearing filter: it makes "this function never touches a closed item"
// directly unit-testable rather than incidental. Closed items at Validate are
// the exclusive responsibility of the board-sourced sibling,
// settleClosedValidateAdvance, below.
//
// Delegates all per-item logic to advanceValidateTerminalItem.
func (e *Engine) runValidatePRTerminalAdvance(board *gh.ProjectBoard, items []gh.ProjectItem, advancedItems map[string]bool) {
	for _, item := range items {
		if item.IsClosed {
			continue
		}
		e.advanceValidateTerminalItem(board, item, advancedItems)
	}
}

// settleClosedValidateAdvance is the closed-item owner of the "Validate-stage
// PR reached a terminal state → advance to Done" transition (ADR-1387),
// mirroring settleAwaitingCIScan (ADR-1270) and settleClosedItemsToDone
// (ADR-064) as the sixth instance of the board-sourced settle-scan pattern.
//
// Before ADR-1387, the only way for a closed item at Validate to reach
// advanceValidateTerminalItem's healing logic was to first pass
// itemMayNeedWork/itemNeedsWork's dispatch admission gate — which meant the
// same admission that let the settle-owner observe a closed item also made
// that item eligible for a real Claude stage invocation, producing an
// unbounded post-close dispatch loop (see the issue this ADR/scan fixes).
// This scan gives the settle-owner a feed that is entirely independent of
// dispatch admission — sourced directly from board.Items, exactly like its
// settle-scan siblings — so closed items can be healed without ever being
// admitted to dispatch.
//
// Cost: O(closed items currently sitting at Validate) — bounded by WIP at one
// late-pipeline stage, not board size. Calls only the lightweight
// FetchLinkedPR per candidate (never the expensive FetchItemDetails deep
// fetch), and only for items already known-closed from the shallow board
// snapshot.
//
// Unresolved-PR polling cost (pre-existing, unchanged by ADR-1387): if a
// candidate's linked PR is neither merged nor closed (e.g. a human closed the
// issue without touching the PR — an unusual, out-of-band action, the same
// trigger class as the loop this scan exists to fix), advanceValidateTerminalItem
// returns immediately with no state change, and this scan re-evaluates the item
// every poll — one FetchLinkedPR call, indefinitely, until the PR resolves.
// There is no timeout/escalation here comparable to settleAwaitingCIScan's
// CIBackstopTimeout backstop (ADR-1270; repurposed from CIWaitTimeout by
// ADR-1410). This exact no-escalation behavior already
// existed in the pre-ADR-1387 runValidatePRTerminalAdvance; ADR-1387 does not
// introduce it, and only lowers its cost — pre-ADR-1387 the same item was also
// being fully re-dispatched to Claude every poll (the bug this scan fixes), so
// the steady-state cost here (one lightweight, read-only API call per poll) is
// strictly cheaper, not a new failure mode. A stuck-PR escalation path is
// unrelated to ADR-1387's scope.
//
// Ownership: exclusive owner of closed items at Validate specifically — not
// "any gate-checked stage" (advanceValidateTerminalItem hardcodes
// stage.Name == "Validate", not stageIsGateChecked; see its doc comment) —
// together with its open-item sibling runValidatePRTerminalAdvance. The two
// are IsClosed-partitioned and never process the same item, so there is no
// race or double-advance between them. settleClosedItemsToDone (ADR-064)
// excludes stage.Name == "Validate" specifically, for exactly this reason,
// deferring to this scan instead — but it does NOT exclude other gate-checked
// stages (e.g. a Review stage configured with wait_for_reviews: true, the
// shipped default): a closed item stranded there has no PR-merge nuance for
// this pair to add, so settleClosedItemsToDone's plain "move to Done" handles
// it directly (caught in review on PR #1388 — see ADR-1387's "R1 Follow-up:
// Closed Item Stranded at a Non-Validate Gate-Checked Stage").
func (e *Engine) settleClosedValidateAdvance(board *gh.ProjectBoard, advancedItems map[string]bool) {
	for _, item := range board.Items {
		if !item.IsClosed {
			continue
		}
		e.advanceValidateTerminalItem(board, item, advancedItems)
	}
}
