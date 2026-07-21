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
	// priorInQueue is the item's previous-poll merge-queue membership, captured
	// in poll.go before ItemDeepFetched overwrites the store. checkAutoMergeConvergence
	// uses it to detect the poll-native "left the queue" edge (ADR-058 D4 OQ-3).
	priorInQueue bool
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
		// Guard: if a goroutine from a previous poll cycle is still running
		// dispatchReviewReinvoke for this item, skip the entire reinvoke path —
		// including cycle-limit checks — to avoid pausing an item while valid
		// work is still in progress. The store Worker field is the semantic
		// source of truth for in-flight state. (Enforced by
		// dispatchWithCycleLimit's in-flight bail.)
		return e.dispatchWithCycleLimit(
			pctx,
			"review-reinvoke",
			func(snap itemstate.Snapshot) int { return snap.ReviewCycles(pctx.stage.Name) },
			e.cfg.MaxReviewCycles,
			nil,
			func(repoStr string) {
				e.store.Apply(itemstate.ReviewCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			},
			func() { e.dispatchReviewReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage) },
			func(cycleCount int) {
				e.pauseForReviewCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, e.cfg.MaxReviewCycles)
			},
		)
	}
	return false
}

// dispatchWithCycleLimit implements the cycle-limit dispatch pattern shared
// by the review, rebase, and CI-fix reinvoke paths: read the item's store
// snapshot, bail if a reinvoke is already in-flight, read the current cycle
// count, apply an optional short-circuit (CI-fix's no-op-SHA debounce; nil
// for review/rebase, which have no such check), then either pause at the
// cycle limit or increment-and-dispatch. shortCircuit and the snapshot read
// are skipped together on a store-read error, mirroring each original
// block's behavior (cycleCount defaults to 0, and a short-circuit keyed off
// snapshot state can never fire without a snapshot). Always returns true —
// callers only reach this after already deciding a reinvoke condition holds
// (blocked review, merge conflict, CI failure).
func (e *Engine) dispatchWithCycleLimit(
	pctx *phase1Ctx,
	tag string,
	cycles func(itemstate.Snapshot) int,
	maxCycles int,
	shortCircuit func(itemstate.Snapshot) bool,
	increment func(repoStr string),
	dispatch func(),
	pause func(cycleCount int),
) bool {
	iKey := issueKey(pctx.item, e.defaultRepo())
	repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())

	var cycleCount int
	if snap, snapErr := e.store.Get(repoStr, pctx.item.Number); snapErr == nil {
		if snap.Worker() != nil {
			e.logf(pctx.item.Number, tag, "skipping dispatch — reinvoke already in-flight\n")
			return true
		}
		cycleCount = cycles(snap)
		if shortCircuit != nil && shortCircuit(snap) {
			return true
		}
	}

	if cycleCount >= maxCycles {
		pause(cycleCount)
	} else {
		increment(repoStr)
		dispatch()
		pctx.advancedItems[iKey] = true
	}
	return true
}

// handleAutoMergeConvergence claims items with fabrik:auto-merge-enabled,
// delegating all PR state monitoring to checkAutoMergeConvergence.
// settlePRMergeState is called so the convergence path shares a single settle
// read per poll cycle with no independent mergeable_state interpretation; the
// merge/CI gates remain bypassed — GitHub owns the merge decision for these items.
func (e *Engine) handleAutoMergeConvergence(pctx *phase1Ctx) bool {
	if !hasLabel(pctx.item.Labels, "fabrik:auto-merge-enabled") {
		return false
	}
	settle := e.settlePRMergeState(pctx.item, pctx.stage)
	e.checkAutoMergeConvergence(pctx.ctx, pctx.board, pctx.item, pctx.stage, settle, pctx.priorInQueue)
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
		// function body) so D4 can still invoke it. Source the signal from BOTH the
		// poll-native ProjectItem field (always GraphQL-populated, reliable on every
		// cycle) and settle.PR: settle.PR carries the flag only on a fully-populated
		// boardcache hit and reports false on a cache miss (REST fallback), so the
		// ProjectItem field is the authoritative source and settle.PR is a fresher
		// supplement. Non-queue repos are unchanged (FR-3, both signals false-by-default).
		if prInMergeQueue(pctx.item) || (settle.PR != nil && settle.PR.IsInMergeQueue) {
			e.logf(pctx.item.Number, "merge-queue", "PR in merge queue — deferring rebase to queue\n")
			return true
		}
		return e.dispatchWithCycleLimit(
			pctx,
			"rebase-reinvoke",
			func(snap itemstate.Snapshot) int { return snap.RebaseCycles(pctx.stage.Name) },
			e.cfg.MaxRebaseCycles,
			nil,
			func(repoStr string) {
				e.store.Apply(itemstate.RebaseCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			},
			func() { e.dispatchRebaseReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage) },
			func(cycleCount int) {
				e.pauseForRebaseCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, e.cfg.MaxRebaseCycles)
			},
		)
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
		return e.dispatchWithCycleLimit(
			pctx,
			"ci-fix-reinvoke",
			func(snap itemstate.Snapshot) int { return snap.CIFixCycles(pctx.stage.Name) },
			e.cfg.MaxCiFixCycles,
			func(snap itemstate.Snapshot) bool {
				lastNoOpSHA := snap.LastCIFixNoOpSHA()
				if settle.PR == nil || lastNoOpSHA == "" || lastNoOpSHA != settle.PR.HeadSHA {
					return false
				}
				// The last CI-fix reinvoke for this exact head SHA pushed no new
				// commit — dispatching again would just repeat the same no-op
				// and burn cycle budget for nothing. Wait for the SHA to advance
				// (a genuine fix) or for CIWaitTimeout to fire (#958 leg 2).
				e.logf(pctx.item.Number, "ci-fix-reinvoke", "skipping dispatch — no-op already recorded for head %s\n",
					lastNoOpSHA[:min(8, len(lastNoOpSHA))])
				return true
			},
			func(repoStr string) {
				e.store.Apply(itemstate.CIFixCycleIncremented{Repo: repoStr, Number: pctx.item.Number, StageName: pctx.stage.Name})
			},
			func() { e.dispatchCIFixReinvoke(pctx.ctx, pctx.board, pctx.item, pctx.stage, settle) },
			func(cycleCount int) {
				e.pauseForCIFixCycleLimit(pctx.board, pctx.item, pctx.stage, cycleCount, e.cfg.MaxCiFixCycles)
			},
		)
	}
	if ciBlocked {
		return true // CI still pending; re-evaluate on next poll
	}

	return false
}
