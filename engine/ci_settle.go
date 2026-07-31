package engine

import (
	"context"
	"fmt"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/itemstate"
	"github.com/handarbeit/fabrik/stages"
)

// awaitingCIOrphanRetryStage is a dedicated, non-real stage name used to key the
// existing StageRetryIncremented/StageRetryCleared/Attempts counter for retries of
// a fabrik:awaiting-ci item stranded on a board column with no wait_for_ci stage
// (the "gate genuinely cannot be evaluated" case). The double-underscore wrapping
// makes it unrepresentable as a real YAML stage `name:`, mirroring
// nonDefaultBaseCloseRetryStage and mergeTrainMemberCloseRetryStage.
const awaitingCIOrphanRetryStage = "__awaiting_ci_orphan__"

// settleAwaitingCIScan is the sole per-poll evaluator of the CI gate for open,
// not-yet-complete fabrik:awaiting-ci items (#1270). Sourced directly from
// board.Items — independent of itemMayNeedWork, selectDeepFetchCandidates's
// cooldown pre-filter, and Phase 1's per-item admission gate. The field evidence
// for #1270 (an item stranded 80+ minutes with zero "settle" log lines, despite
// sibling items processing normally) narrowed the bug to that shared, three-layer
// admission pipeline silently excluding an awaiting-ci item without ever logging
// why — the same failure shape already fixed four times in this codebase for
// other durable markers (fabrik:awaiting-done, spawned-child placement,
// merge-train member-close, non-default-base close), each via a dedicated
// board.Items-sourced settle scan. This is the fifth.
//
// The scan does not reimplement any gate logic — it builds the same phase1Ctx and
// runs the identical catchUpPhase1Handlers chain Phase 1 uses, so it can never
// become a second, divergent owner of stage:X:complete under wait_for_ci (the
// "Clearing-owner invariant" in docs/state-machine.md §6.5.1). Phase 1's own
// admission gate is narrowed to hasComplete-only (see poll.go): since
// fabrik:awaiting-ci and stage:X:complete are mutually exclusive in steady state,
// every awaiting-ci item is !hasComplete and therefore always routed here, never
// to Phase 1, until the gate clears.
//
// This partition is exhaustive only in steady state, not atomically:
// addCompleteLabelAndRemoveCI (ci.go) adds stage:X:complete and removes
// fabrik:awaiting-ci via two separate GitHub API calls, so a transient failure
// of the second call can leave both labels present for one poll, routing the
// item to both Phase 1 (hasComplete=true) and this scan (fabrik:awaiting-ci
// present) in the same pass. The CI-fix-reinvoke dispatch itself cannot
// double-fire in that window — dispatchWithCycleLimit's snap.Worker() != nil
// guard is set synchronously before the reinvoke goroutine starts (reinvoke.go),
// confirmed by TestSettleAwaitingCIScan_NoDoubleDispatch. The pause-at-
// cycle-limit and CI-wait-timeout branches have no equivalent Worker() guard,
// but pauseForCITimeout/pauseForCIFixCycleLimit (ci.go) check
// hasCIGatePauseComment before posting — since the second pass's
// FetchItemDetails call is a genuinely fresh fetch that repopulates
// item.Comments, it observes the first pass's already-completed AddComment
// call and skips the duplicate
// (TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment
// pins this). This mirrors hasSkippedComment's precedent in
// no_work_needed_settle.go rather than adding a new idempotency primitive.
//
// Closed items are skipped: closed-issue recovery for a merged PR under
// fabrik:awaiting-ci is already owned by runValidatePRTerminalAdvance (ADR-056
// D2) — duplicating that here would be a second owner for no benefit. Paused
// items are skipped: either escalateAwaitingCIOrphanFailure already handled them
// (marker removed) or an operator is investigating for an unrelated reason and
// this scan must not fight them.
//
// FetchItemDetails runs here with no cooldown, once per fabrik:awaiting-ci item
// per poll — raised in PR review as a potential new GraphQL cost. It is not new:
// fabrik:awaiting-ci was already in selectDeepFetchCandidates's cooldown-bypass
// label list on main before this change (poll.go's hasAwaitingLabel), so these
// items were already deep-fetched every poll via the main loop. This scan
// relocates that existing per-poll fetch to its own admission path; it does not
// add a fetch that wasn't already happening.
func (e *Engine) settleAwaitingCIScan(ctx context.Context, board *gh.ProjectBoard, advancedItems map[string]bool) {
	var processed int
	for _, item := range board.Items {
		if !hasLabel(item.Labels, "fabrik:awaiting-ci") || hasLabel(item.Labels, "fabrik:paused") || item.IsClosed {
			continue
		}

		stage := stages.FindStage(e.cfg.Stages, item.Status)
		isWaitForCI := stage != nil && stage.WaitForCI != nil && *stage.WaitForCI
		if stage == nil || !isWaitForCI || stage.HoldingStage || stage.Unmanaged || stage.CleanupWorktree {
			e.logf(item.Number, "awaiting-ci-settle", "fabrik:awaiting-ci item stranded at column %q — not a wait_for_ci stage, cannot evaluate CI gate here; will retry\n", item.Status)
			e.recordAwaitingCIOrphanRetry(item)
			continue
		}

		repo := itemOwnerRepoString(item, e.defaultRepo())
		if err := e.readClient.FetchItemDetails(&item); err != nil {
			e.logf(item.Number, "awaiting-ci-settle", "could not deep-fetch item details: %v — will retry next poll\n", err)
			e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: item.Number, At: time.Now()})
			// A repeatedly failing deep-fetch is itself a "gate genuinely cannot be
			// evaluated" case (the issue's own example is PR-linkage loss, but any
			// persistent fetch failure — permissions, a deleted issue node, sustained
			// API errors — has the same shape: this scan can never reach checkCIGate
			// for the item). Route it through the same retry/escalate counter as the
			// orphan-column case so it surfaces as a pause + comment after MaxRetries
			// rather than retrying silently forever.
			e.recordAwaitingCIOrphanRetry(item)
			continue
		}
		// Capture prior-poll merge-queue membership BEFORE ItemDeepFetched
		// overwrites the store (ADR-058 D4 OQ-3) — mirrors selectDeepFetchCandidates
		// in poll.go exactly. checkAutoMergeConvergence (reached via
		// handleAutoMergeConvergence in the shared catchUpPhase1Handlers chain
		// below) uses this to detect the poll-native "left the merge queue" edge;
		// reading it from the store after ItemDeepFetched would always see the
		// just-overwritten current value and silently lose the edge.
		priorSnap, priorErr := e.store.Get(repo, item.Number)
		priorInQueue := priorErr == nil && priorSnap.LinkedPR() != nil && priorSnap.LinkedPR().IsInMergeQueue
		e.store.Apply(itemstate.ItemDeepFetched{Repo: repo, Number: item.Number, FreshState: item})

		// A concurrent clear between the shallow board read above and this deep-fetch
		// must not run stale state through the handler chain.
		if !hasLabel(item.Labels, "fabrik:awaiting-ci") || hasLabel(item.Labels, "fabrik:paused") {
			continue
		}

		// The item resolved to a real wait_for_ci stage this pass — clear any
		// orphan-retry count so a transient bounce through a stray column doesn't
		// accumulate toward escalation.
		e.store.Apply(itemstate.StageRetryCleared{Repo: repo, Number: item.Number, StageName: awaitingCIOrphanRetryStage})

		completeLabel := fmt.Sprintf("stage:%s:complete", stage.Name)
		pctx := &phase1Ctx{
			ctx:           ctx,
			board:         board,
			item:          item,
			stage:         stage,
			hasComplete:   hasLabel(item.Labels, completeLabel),
			advancedItems: advancedItems,
			priorInQueue:  priorInQueue,
		}
		claimed := false
		for _, h := range catchUpPhase1Handlers {
			if h.run(e, pctx) {
				claimed = true
				break
			}
		}
		if !claimed {
			// ADR-1216 same-poll joint-clearing handoff: when the CI gate clears on
			// this exact pass (Phase 1 ran but did not claim), the landing decision
			// must be reached immediately — deferring it to the next poll is exactly
			// the "review gate arms a poll late" race #1216 fixed. Phase 2 no longer
			// runs for these items in the main catch-up loop (they're excluded by its
			// hasComplete-only admission gate above), so this scan is now the only
			// place that reaches it for them.
			e.runCatchUpPhase2(ctx, board, item, stage, advancedItems)
		}
		processed++
	}
	if processed > 0 {
		e.logf(0, "awaiting-ci-settle", "processed %d fabrik:awaiting-ci item(s)\n", processed)
	}
}

// recordAwaitingCIOrphanRetry increments the in-memory retry counter for a
// fabrik:awaiting-ci item the scan could not settle this pass — either it sits
// on a board column with no wait_for_ci stage, or a repeated FetchItemDetails
// failure prevented the scan from ever reaching checkCIGate for it. Both are
// "the gate genuinely cannot be evaluated" cases from the item's own
// perspective, so they share one counter and one escalation path, mirroring
// escalateNoWorkNeededFailure's single generic-message counter for its own
// multiple failure causes (board-move failure vs. issue-close failure).
// Escalates via escalateAwaitingCIOrphanFailure once e.cfg.MaxRetries is reached.
func (e *Engine) recordAwaitingCIOrphanRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, awaitingCIOrphanRetryStage, e.escalateAwaitingCIOrphanFailure)
}

// escalateAwaitingCIOrphanFailure is called when a fabrik:awaiting-ci item has
// gone MaxRetries settle passes without the scan reaching a checkCIGate
// evaluation for it — either it never resolved to a wait_for_ci stage, or
// FetchItemDetails kept failing. Pauses the issue, removes the
// fabrik:awaiting-ci marker (retry suppression is no longer needed once
// fabrik:paused takes over), and posts an explanatory comment describing
// whichever cause is current at escalation time and the matching manual
// recovery step — mirroring escalateNonDefaultBaseCloseFailure/
// escalateMergeTrainMemberCloseFailure.
func (e *Engine) escalateAwaitingCIOrphanFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "fabrik:awaiting-ci item could not be settled %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	e.escalateSettle(item, "fabrik:awaiting-ci", awaitingCIOrphanRetryStage, func(item gh.ProjectItem) {
		stage := stages.FindStage(e.cfg.Stages, item.Status)
		isWaitForCI := stage != nil && stage.WaitForCI != nil && *stage.WaitForCI
		var problem, fix string
		if stage == nil || !isWaitForCI || stage.HoldingStage || stage.Unmanaged || stage.CleanupWorktree {
			problem = fmt.Sprintf("sits at board column %q, which has no `wait_for_ci` stage — the CI gate cannot be evaluated here", item.Status)
			fix = "move the issue back to the `wait_for_ci` stage it was awaiting CI for (e.g. `Validate`)"
		} else {
			problem = "could not be fetched from GitHub on repeated settle attempts, so its CI status could not be checked (see the engine log for the specific fetch error)"
			fix = "resolve the underlying GitHub API access issue for this repository/issue"
		}
		comment := fmt.Sprintf(
			"🏭 **Fabrik — awaiting-ci settle failed**\n\nThis issue carries `fabrik:awaiting-ci` but %s. This has persisted for %d settle attempt(s). The issue has been paused.\n\nManual fix: %s, then remove the `fabrik:paused` label.",
			problem, e.cfg.MaxRetries, fix,
		)
		e.postItemComment(item, comment, true)
	})
}
