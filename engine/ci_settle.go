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
// to Phase 1, until the gate clears — eliminating any double-dispatch risk.
//
// Closed items are skipped: closed-issue recovery for a merged PR under
// fabrik:awaiting-ci is already owned by runValidatePRTerminalAdvance (ADR-056
// D2) — duplicating that here would be a second owner for no benefit. Paused
// items are skipped: either escalateAwaitingCIOrphanFailure already handled them
// (marker removed) or an operator is investigating for an unrelated reason and
// this scan must not fight them.
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
			continue
		}
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
// fabrik:awaiting-ci item stranded on a board column with no wait_for_ci stage.
// Escalates via escalateAwaitingCIOrphanFailure once e.cfg.MaxRetries is reached.
func (e *Engine) recordAwaitingCIOrphanRetry(item gh.ProjectItem) {
	e.recordSettleRetry(item, awaitingCIOrphanRetryStage, e.escalateAwaitingCIOrphanFailure)
}

// escalateAwaitingCIOrphanFailure is called when a fabrik:awaiting-ci item has
// sat on a non-wait_for_ci column for MaxRetries settle passes without moving
// back to a stage where the CI gate can be evaluated. Pauses the issue, removes
// the fabrik:awaiting-ci marker (retry suppression is no longer needed once
// fabrik:paused takes over), and posts an explanatory comment naming the stray
// column and the manual recovery step — mirroring
// escalateNonDefaultBaseCloseFailure/escalateMergeTrainMemberCloseFailure.
func (e *Engine) escalateAwaitingCIOrphanFailure(item gh.ProjectItem) {
	e.logf(item.Number, "escalate", "fabrik:awaiting-ci item stranded off a wait_for_ci stage %d time(s) — pausing issue\n", e.cfg.MaxRetries)

	e.escalateSettle(item, "fabrik:awaiting-ci", awaitingCIOrphanRetryStage, func(item gh.ProjectItem) {
		comment := fmt.Sprintf(
			"🏭 **Fabrik — awaiting-ci settle failed**\n\nThis issue carries `fabrik:awaiting-ci` but sits at board column %q, which has no `wait_for_ci` stage — the CI gate cannot be evaluated here. This has persisted for %d settle attempt(s). The issue has been paused.\n\nManual fix: move the issue back to the `wait_for_ci` stage it was awaiting CI for (e.g. `Validate`), then remove the `fabrik:paused` label.",
			item.Status, e.cfg.MaxRetries,
		)
		e.postItemComment(item, comment, true)
	})
}
