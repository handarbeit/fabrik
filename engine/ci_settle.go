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
// this scan must not fight them. The closed check is re-applied after
// FetchItemDetails too (not just at the top-of-loop shallow-board check): a
// board.Items snapshot can be stale by the time this item's own deep-fetch
// runs, and until FetchItemDetails queried GraphQL's `closed` field (added
// alongside this scan per PR review, github/project.go), IsClosed had no way
// to reflect a since-closed issue at that point — a stale false there would
// have let this scan run the full gate chain against a now-closed issue,
// racing runValidatePRTerminalAdvance's exclusive ownership.
//
// FetchItemDetails runs here with no cooldown, once per fabrik:awaiting-ci item
// per poll — raised in PR review as a potential new GraphQL cost. It is not new:
// fabrik:awaiting-ci was already in selectDeepFetchCandidates's cooldown-bypass
// label list on main before this change (poll.go's hasAwaitingLabel), so these
// items were already deep-fetched every poll via the main loop. This scan
// relocates that existing per-poll fetch to its own admission path; it does not
// add a fetch that wasn't already happening. It also still benefits from
// e.readClient's own internal cache-freshness logic (CacheImpl.FetchItemDetails,
// boardcache.go) exactly as the old code path did — same function, same
// cacheIsStale check, regardless of caller. A follow-up PR review comment noted
// that the old code path's cache-hit-vs-deep-fetch pre-check log line (poll.go)
// had no equivalent here; added below, matching poll.go's pattern, for the same
// diagnosability reason this whole scan exists.
func (e *Engine) settleAwaitingCIScan(ctx context.Context, board *gh.ProjectBoard, advancedItems map[string]bool) {
	// examined counts every fabrik:awaiting-ci item this scan looked at, whether
	// or not it reached the CI gate; gateReached counts only those for which
	// handleMergeAndCIGates actually called checkCIGate (pctx.reachedCIGate).
	// Kept separate (PR review, pruefer): a poll where every item is retrying in
	// the orphan-column or deep-fetch-failure branches would otherwise log
	// "processed 0 item(s)", which reads as "nothing is happening" on exactly
	// the kind of poll an operator diagnosing a stall (this issue's own
	// motivating scenario) most needs visibility into.
	//
	// #1303: before this fix, gateReached incremented unconditionally at the
	// end of every loop iteration regardless of which handler in
	// catchUpPhase1Handlers claimed the item (or whether any did) — so the log
	// line below could report "1 reached the CI gate" for an item whose
	// checkCIGate call never actually ran (e.g. checkMergeabilityGate claimed it
	// first). That false signal delayed triage of the incident this issue
	// documents. gateReached now only counts pctx.reachedCIGate == true.
	var examined, gateReached int
	for _, item := range board.Items {
		if !hasLabel(item.Labels, "fabrik:awaiting-ci") || hasLabel(item.Labels, "fabrik:paused") || item.IsClosed {
			continue
		}
		examined++

		stage := stages.FindStage(e.cfg.Stages, item.Status)
		isWaitForCI := stage != nil && stage.WaitForCI != nil && *stage.WaitForCI
		if stage == nil || !isWaitForCI || stage.HoldingStage || stage.Unmanaged || stage.CleanupWorktree {
			e.logf(item.Number, "awaiting-ci-settle", "fabrik:awaiting-ci item stranded at column %q — not a wait_for_ci stage, cannot evaluate CI gate here; will retry\n", item.Status)
			e.recordAwaitingCIOrphanRetry(item)
			continue
		}

		repo := itemOwnerRepoString(item, e.defaultRepo())
		// Mirrors selectDeepFetchCandidates's pre-check log line (poll.go) — purely
		// diagnostic, doesn't gate the call. FetchItemDetails below goes through the
		// same e.readClient (CacheImpl when caching is enabled), whose own internal
		// freshness check (boardcache.go's cacheIsStale) decides cache-hit vs. real
		// GraphQL fetch identically regardless of caller; this scan gets that for
		// free. Without this line, an operator diagnosing a stall (this issue's own
		// motivating scenario) would see "examined N item(s)" but no visibility
		// into whether each item's fetch was a cache hit or a live API call.
		//
		// item.UpdatedAt here is from the single board snapshot fetched once at the
		// top of poll() — not re-queried mid-poll. If something earlier in this same
		// poll (e.g. runValidatePRTerminalAdvance, which runs before this scan)
		// mutated this exact item on GitHub without this board snapshot reflecting
		// it, IsItemCacheFresh could read a now-stale cache as fresh (PR review,
		// pruefer). Confirmed pre-existing and identical, not new here:
		// selectDeepFetchCandidates's own equivalent check (poll.go) uses the exact
		// same single-snapshot board.Items[i].UpdatedAt, so this is a property of
		// poll()'s one-snapshot-per-cycle architecture as a whole, not something
		// introduced by this scan. Accepted as-is — closing it would mean re-fetching
		// UpdatedAt mid-poll, which no part of poll() currently does anywhere.
		if c := e.cache(); c != nil && !c.IsPaused() && c.IsItemCacheFresh(item.Repo, item.Number, item.UpdatedAt) {
			e.logf(item.Number, "awaiting-ci-settle", "reading details from cache\n")
		} else {
			e.logf(item.Number, "awaiting-ci-settle", "deep-fetching details from GitHub\n")
		}
		if err := e.readClient.FetchItemDetails(&item); err != nil {
			e.store.Apply(itemstate.DeepFetchFailed{Repo: repo, Number: item.Number, At: time.Now()})
			if isTransientAPIError(err) {
				// #1313: a transient/global failure (rate-limit or secondary-rate-limit
				// exhaustion, 5xx, timeout, connection reset) is not evidence this
				// particular item is broken — it affects every item on the board
				// simultaneously and resolves itself (rate limits reset hourly). Defer
				// without touching the escalation counter at all, however many
				// consecutive polls are affected, so a rate-limit episode can never
				// mass-pause every fabrik:awaiting-ci item on the board.
				e.logf(item.Number, "awaiting-ci-settle", "could not deep-fetch item details (transient/rate-limited — deferring without counting toward escalation): %v\n", err)
				continue
			}
			// A repeatedly failing deep-fetch with a non-transient error is itself a
			// "gate genuinely cannot be evaluated" case (e.g. PR-linkage loss,
			// permissions, a deleted issue node) — this scan can never reach
			// checkCIGate for the item. Route it through the same retry/escalate
			// counter as the orphan-column case so it surfaces as a pause + comment
			// after MaxRetries rather than retrying silently forever.
			e.logf(item.Number, "awaiting-ci-settle", "could not deep-fetch item details (non-transient — counts toward escalation): %v\n", err)
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

		// A concurrent clear/close/pause between the shallow board read above and
		// this deep-fetch must not run stale state through the handler chain.
		// item.IsClosed specifically needs this fresh FetchItemDetails result — the
		// shallow board.Items copy captured at the top of the loop can't see a
		// since-closed issue, and closed-item recovery is runValidatePRTerminalAdvance's
		// exclusive job (ADR-056 D2); this scan must not race it (PR review, pruefer).
		if !hasLabel(item.Labels, "fabrik:awaiting-ci") || hasLabel(item.Labels, "fabrik:paused") || item.IsClosed {
			continue
		}

		// #1303: unconditional CIWaitTimeout backstop, evaluated ahead of the
		// Phase 1 handler chain below and independent of what any gate would
		// classify or claim. checkCIGate's own identical CIWaitTimeout guard
		// (classifyCIFromCheckRuns etc., ci.go) only fires once checkCIGate is
		// actually reached — but any silent claim earlier in the chain
		// (confirmed possible via a stale cached CheckRunsPending read, closed
		// by RefreshCheckRunsLive below for that specific cause, but not
		// provably the only way it can happen) makes checkCIGate unreachable,
		// leaving that inner timeout dead. This check does not depend on the
		// handler chain at all, so it bounds the whole class rather than just
		// the one confirmed cause. Idempotent: pauseForCITimeout no-ops via
		// hasCIGatePauseComment if a pause comment was already posted this
		// poll (e.g. by the normal checkCIGate path reached for a different
		// item), and a FetchLabelAppliedAt error or zero timestamp leaves this
		// a no-op, falling through to the normal gate-driven path unchanged.
		{
			owner, repoName := itemOwnerRepo(item, e.defaultRepo())
			if appliedAt, err := e.client.FetchLabelAppliedAt(owner, repoName, item.Number, "fabrik:awaiting-ci"); err != nil {
				e.logf(item.Number, "awaiting-ci-settle", "could not fetch awaiting-ci label timestamp for CIWaitTimeout backstop: %v\n", err)
			} else if !appliedAt.IsZero() && time.Since(appliedAt) >= e.ciWaitTimeout() {
				e.logf(item.Number, "awaiting-ci-settle", "fabrik:awaiting-ci exceeded CIWaitTimeout (%s) while never reaching the CI gate — escalating\n", e.ciWaitTimeout())
				e.pauseForCITimeout(board, item, stage)
				continue
			}
		}

		// #1303: prime the store with a genuinely fresh check-run read before
		// the handler chain runs. FetchCheckRuns' general cache-trust contract
		// deliberately still serves a cached PENDING classification (see its
		// doc comment) — on a webhook-less deployment nothing else ever
		// supersedes that snapshot, which is exactly the confirmed mechanism
		// behind this item silently latching in checkMergeabilityGate's
		// PRMergeUnsettled branch forever. Scoped to this one caller (the
		// bounded fabrik:awaiting-ci item set already deep-fetched
		// unconditionally above) rather than changing FetchCheckRuns' cache
		// trust for its ~35 other call sites. item.LinkedPRHeadSHA here comes
		// from the just-completed FetchItemDetails GraphQL deep-fetch
		// (headRefOid), not any REST-cached record. Non-fatal on error — falls
		// through to whatever FetchCheckRuns' own cache-trust check decides,
		// matching FetchItemDetails' failure handling above.
		if item.LinkedPRHeadSHA != "" && item.LinkedPRNumber != 0 {
			if c := e.cache(); c != nil {
				shaShort := item.LinkedPRHeadSHA[:min(8, len(item.LinkedPRHeadSHA))]
				owner, repoName := itemOwnerRepo(item, e.defaultRepo())
				e.logf(item.Number, "awaiting-ci-settle", "refreshing check runs live for sha=%s\n", shaShort)
				if err := c.RefreshCheckRunsLive(owner, repoName, item.LinkedPRHeadSHA); err != nil {
					e.logf(item.Number, "awaiting-ci-settle", "could not refresh check runs live for sha=%s: %v — falling back to cache\n", shaShort, err)
				}
			}
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
		if pctx.reachedCIGate {
			gateReached++
		}
	}
	if examined > 0 {
		e.logf(0, "awaiting-ci-settle", "examined %d fabrik:awaiting-ci item(s), %d reached the CI gate\n", examined, gateReached)
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
//
// #1313: this is called for the orphan-column case and for a non-transient
// FetchItemDetails failure only. A transient/global failure (rate-limit
// exhaustion, 5xx, timeout, connection reset — see isTransientAPIError) never
// reaches this function at all: it is deferred to a future poll without
// touching the counter, so a single rate-limit episode can never mass-pause
// every fabrik:awaiting-ci item on the board.
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
			problem = "could not be fetched from GitHub on repeated settle attempts due to a non-transient error (rate-limiting and other transient conditions defer automatically and never reach this point), so its CI status could not be checked (see the engine log for the specific fetch error)"
			fix = "resolve the underlying GitHub API access issue for this repository/issue"
		}
		comment := fmt.Sprintf(
			"🏭 **Fabrik — awaiting-ci settle failed**\n\nThis issue carries `fabrik:awaiting-ci` but %s. This has persisted for %d settle attempt(s). The issue has been paused.\n\nManual fix: %s, then remove the `fabrik:paused` label.",
			problem, e.cfg.MaxRetries, fix,
		)
		e.postItemComment(item, comment, true)
	})
}
