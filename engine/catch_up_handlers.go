package engine

import (
	"context"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// phase1Ctx carries all state shared across Phase 1 handler calls for a single
// catch-up loop item. Using a struct avoids wide function signatures and makes
// it easy to add future handlers without changing call sites.
type phase1Ctx struct {
	ctx           context.Context
	board         *gh.ProjectBoard
	item          gh.ProjectItem
	stage         *stages.Stage
	hasComplete   bool
	advancedItems map[string]bool
}

// catchUpHandler is a named Phase 1 gate/recovery handler. The name field lets
// tests assert handler precedence by position — a future reorder is a test
// failure, not a silent behavioral change (ADR-056 D3).
type catchUpHandler struct {
	name string
	run  func(*Engine, *phase1Ctx) bool
}

// catchUpPhase1Handlers is the ordered list of Phase 1 catch-up handlers.
// Each handler returns true to claim the item (no further handlers run, Phase 2
// is skipped for this item) or false to pass through to the next handler.
//
// Ordering is structurally enforced by slice position:
//   - dependencies: blocked items bypass all gates
//   - reviewGate: review threads addressed before any merge/CI gate evaluation
//   - autoMergeConvergence: items in GitHub native auto-merge call settlePRMergeState
//     for merge/CI state (eliminating split-brain) but bypass the merge/CI gates
//     (GitHub owns the merge decision)
//   - mergeAndCIGates: merge-conflict gate runs before the CI gate (ADR-028)
var catchUpPhase1Handlers = []catchUpHandler{
	{name: "dependencies", run: (*Engine).handleDependencies},
	{name: "reviewGate", run: (*Engine).handleReviewGate},
	{name: "autoMergeConvergence", run: (*Engine).handleAutoMergeConvergence},
	{name: "mergeAndCIGates", run: (*Engine).handleMergeAndCIGates},
}

// handleDependencies claims the item when it has unresolved blocking
// dependencies. checkDependencies handles label mutation and comment posting.
func (e *Engine) handleDependencies(pctx *phase1Ctx) bool {
	return e.checkDependencies(pctx.board, pctx.item, pctx.stage)
}

// handleReviewGate runs the review gate and review reinvoke dispatch. Only
// active when the stage has genuinely completed (hasComplete == true); during
// the CI-await window (fabrik:awaiting-ci && !hasComplete) the gate is skipped
// to prevent spurious fabrik:awaiting-review re-application (#617).
func (e *Engine) handleReviewGate(pctx *phase1Ctx) bool {
	if !pctx.hasComplete {
		return false
	}
	blocked, timedOut := e.checkReviewGate(pctx.board, pctx.item, pctx.stage)
	if blocked {
		// Record CooldownAt["review-blocked"] so itemMayNeedWork's expiry path
		// re-evaluates this item every 10 × PollSeconds even when nothing bumps
		// updatedAt. This lets Phase 1/Phase 2 review-reprompt timers fire on a
		// non-responsive bot reviewer (issue #495).
		cooldown := time.Duration(e.cfg.PollSeconds*10) * time.Second
		e.store.Apply(itemstate.CooldownRecorded{
			Repo:   itemOwnerRepoString(pctx.item, e.defaultRepo()),
			Number: pctx.item.Number,
			Reason: "review-blocked",
			Until:  time.Now().Add(cooldown),
		})
		return true
	}
	if timedOut {
		e.pauseForReviewTimeout(pctx.board, pctx.item, pctx.stage)
		return true
	}
	// Gate cleared naturally — if reviews with actionable body text were
	// submitted, re-invoke the stage agent to address the feedback before
	// advancing. Reviews with empty bodies (e.g. APPROVED with no comment)
	// have nothing to address; fall through to Phase 2.
	if syntheticComments := e.buildReviewThreadComments(pctx.item); len(syntheticComments) > 0 {
		iKey := issueKey(pctx.item, e.defaultRepo())
		repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())
		// Guard: if a goroutine from a previous poll cycle is still running
		// dispatchReviewReinvoke for this item, skip the entire reinvoke path —
		// including cycle-limit checks — to avoid pausing an item while valid
		// work is still in progress. The store Worker field is the semantic
		// source of truth for in-flight state.
		var cycleCount int
		if snap, snapErr := e.store.Get(repoStr, pctx.item.Number); snapErr == nil {
			if snap.Worker() != nil {
				e.logf(pctx.item.Number, "review-reinvoke", "skipping dispatch — review reinvoke already in-flight\n")
				return true
			}
			cycleCount = snap.ReviewCycles(pctx.stage.Name)
		}
		maxCycles := e.cfg.MaxReviewCycles
		if cycleCount >= maxCycles {
			e.pauseForReviewCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, maxCycles)
		} else {
			e.store.Apply(itemstate.ReviewCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			e.dispatchReviewReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage)
			pctx.advancedItems[iKey] = true
		}
		return true
	}
	return false
}

// handleAutoMergeConvergence claims items with fabrik:auto-merge-enabled,
// delegating all PR state monitoring to checkAutoMergeConvergence.
// settlePRMergeState is called so the convergence path shares a single settle
// read per poll cycle with no independent mergeable_state interpretation; the
// merge/CI gates remain bypassed — GitHub owns the merge decision for these items.
func (e *Engine) handleAutoMergeConvergence(pctx *phase1Ctx) bool {
	if !hasLabel(pctx.item, "fabrik:auto-merge-enabled") {
		return false
	}
	settle := e.settlePRMergeState(pctx.item, pctx.stage)
	e.checkAutoMergeConvergence(pctx.ctx, pctx.board, pctx.item, pctx.stage, settle)
	return true
}

// handleMergeAndCIGates runs the merge-conflict gate followed by the CI gate,
// sharing a single settlePRMergeState call. Merge runs before CI per ADR-028:
// a PR made unmergeable by a base-branch advance must be rebased before the
// engine spins on CI-await polls while the underlying blocker is a conflict.
func (e *Engine) handleMergeAndCIGates(pctx *phase1Ctx) bool {
	// Fetch all PR merge/CI state in a single pass. Both gates below consume
	// this result, ensuring they see identical GitHub state within one poll
	// cycle and eliminating the mergeable vs mergeable_state split-brain that
	// separate REST calls could produce.
	settle := e.settlePRMergeState(pctx.item, pctx.stage)

	mergeBlocked, mergeConflict := e.checkMergeabilityGate(pctx.item, pctx.stage, settle)
	if mergeConflict {
		// Merge-queue awareness (ADR-058 D3 FR-1): never dispatch a rebase reinvoke
		// for a PR the queue currently owns — the synthetic rebase+force-push ejects
		// it. The conflict resolution path stays available for D4's ejection→resolve
		// composition once the PR has left the queue. Guard at dispatch (not in the
		// function body) so D4 can still invoke it. Signal from GraphQL via settle.PR;
		// non-queue repos are unchanged (FR-3).
		if settle.PR != nil && settle.PR.IsInMergeQueue {
			e.logf(pctx.item.Number, "merge-queue", "PR in merge queue — deferring rebase to queue\n")
			return true
		}
		iKey := issueKey(pctx.item, e.defaultRepo())
		repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())
		var cycleCount int
		if snap, snapErr := e.store.Get(repoStr, pctx.item.Number); snapErr == nil {
			if snap.Worker() != nil {
				e.logf(pctx.item.Number, "rebase-reinvoke", "skipping dispatch — rebase reinvoke already in-flight\n")
				return true
			}
			cycleCount = snap.RebaseCycles(pctx.stage.Name)
		}
		maxCycles := e.cfg.MaxRebaseCycles
		if cycleCount >= maxCycles {
			e.pauseForRebaseCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, maxCycles)
		} else {
			e.store.Apply(itemstate.RebaseCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			e.dispatchRebaseReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage)
			pctx.advancedItems[iKey] = true
		}
		return true
	}
	if mergeBlocked {
		return true // mergeability not yet computed; re-evaluate on next poll
	}

	// CI gate: evaluate CI status for stages configured with wait_for_ci: true.
	// Runs in Phase 1 (unconditional) so CI failures are fixed regardless of
	// auto-advance setting. checkCIGate returns (blocked, ciFailure, timedOut).
	ciBlocked, ciFailure, ciTimedOut := e.checkCIGate(pctx.board, pctx.item, pctx.stage, settle)
	if ciTimedOut {
		e.pauseForCITimeout(pctx.board, pctx.item, pctx.stage)
		return true
	}
	if ciFailure {
		iKey := issueKey(pctx.item, e.defaultRepo())
		repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())
		var cycleCount int
		if snap, snapErr := e.store.Get(repoStr, pctx.item.Number); snapErr == nil {
			if snap.Worker() != nil {
				e.logf(pctx.item.Number, "ci-fix-reinvoke", "skipping dispatch — CI-fix reinvoke already in-flight\n")
				return true
			}
			cycleCount = snap.CIFixCycles(pctx.stage.Name)
		}
		maxCycles := e.cfg.MaxCiFixCycles
		if cycleCount >= maxCycles {
			e.pauseForCIFixCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, maxCycles)
		} else {
			e.store.Apply(itemstate.CIFixCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			e.dispatchCIFixReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage, settle)
			pctx.advancedItems[iKey] = true
		}
		return true
	}
	if ciBlocked {
		return true // CI still pending; re-evaluate on next poll
	}

	return false
}
