# ADR 072: Live Re-Read at the Decision Point for Cache Staleness

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #957 — audit: find correctness-critical logic gated behind webhooks

## Context

`boardcache.CacheImpl`'s deep-fetch path (`FetchItemDetails`, `boardcache.go:642-693`) refreshes a deep field (comments, `BlockedBy`, linked-PR details) only when the board's `updatedAt` for that item has advanced past what the cache last saw (`IsItemCacheFresh`/`cacheIsStale`, `boardcache.go:535-571`). This is a correct optimization for the common case — most GitHub mutations do bump `updatedAt` — but it silently breaks whenever a mutation to a deep field does **not** bump the parent issue's `updatedAt`. Three such gaps have now been found by direct, independent live reproduction on `handarbeit/fabrik`:

1. **Comments (#982, 2026-07-13).** `preImplement` read a stale `item.Comments` snapshot at Implement dispatch and missed a just-posted Plan comment's `FABRIK_SPAWN_CHILD` block, silently skipping a spawn.
2. **PR linkage (pre-existing, motivating `engine/prcreate.go`'s `verifyAndHealLinkage`).** A PR's `Closes #N` linkage can drift from what the cache believes without a corresponding `updatedAt` bump on the issue.
3. **`blockedBy` dependency edges (#977, 2026-07-13).** Removing a dependency via the REST dependencies API does not bump the blocked issue's `updatedAt`. The cached `BlockedBy` edge list stayed stale indefinitely; `fabrik:blocked` never cleared despite zero open blockers at the API, for 15+ minutes of active polling.

Each of these was fixed independently, and each fix converged on the same shape of remedy — this ADR names that shape so the next occurrence doesn't have to be re-derived from scratch, and records that ADR 016's assumption about `checkDependencies` making no API calls no longer holds unconditionally.

## Decision

**At the specific decision point where a correctness-critical judgment depends on a deep field that might be stale, bypass the cache and re-read live — throttled by whatever cooldown already gates admission to that decision point, writing the result back to the Store when doing so keeps other readers consistent.**

Concretely, three instances of this pattern now exist in the codebase:

| Instance | Decision point | Live-read call | Throttle | Writes back to Store? |
|---|---|---|---|---|
| `recoverMissingPlanComment` (`engine/spawn.go:227-267`) | `preImplement`'s Plan-comment check before Implement dispatch | `e.client.FetchItemDetails(&fresh)` | Dedicated cooldown, reason `"spawn-recovery-deferred"` (`engine/spawn.go:228,235-236`) | No — used locally to decide whether to spawn |
| `verifyAndHealLinkage`/`verifyAndHealLinkageByBody` (`engine/prcreate.go:225-337`) | PR-linkage healing before trusting `Closes #N` discovery | `e.client.FetchItemDetails` | "Heal already attempted" idempotency guard | No — used locally to decide whether linkage needs repair |
| `checkDependencies`'s `blockedBy` recheck (`engine/dependencies.go:116-160`) | Re-evaluating an already-`fabrik:blocked` item's open dependencies | `e.client.FetchItemDetails(&fresh)` | Reuses the existing `dep-blocked` `CooldownAt` gate in `itemMayNeedWork` (`engine/item.go:260-269`) — no new cooldown needed | **Yes** — `e.store.Apply(itemstate.ItemDeepFetched{...})`, so `PushUnblockObserver` and other Store readers see the correction too |

All three share the same load-bearing detail: **`e.client` (the raw `GitHubClient`) is used, never `e.readClient` (`boardcache.ReadClient`, cache-wrapped)**. Going through `e.readClient` would re-enter the exact `updatedAt`-keyed freshness gate being worked around and defeat the fix.

**When to add a fourth instance:** only when (a) a deep field's staleness has a *confirmed* correctness consequence at a specific decision point — not speculatively, per ADR 003's polling-first principle, which already guarantees eventual poll convergence for the common case — and (b) that decision point already has, or can cheaply be given, a cooldown/idempotency gate to throttle the live read. Do not add an unconditional live read to every deep-field consumption site; that would reintroduce the wake-loop/API-spend risk this pattern is designed to avoid (the #576 concern `itemMayNeedWork`'s cooldown gate already documents).

**When a reconcile-based fix is the better shape instead:** `LightReconcile` (`boardcache.go:1039-1050`) is architecturally shallow-field-only (status, `updatedAt`, the fabrik-managed label subset) by design — extending it to cover a deep field is a materially larger change (new deep-fetch-in-reconcile machinery) than this pattern, and was explicitly ruled out for the `blockedBy` fix on those grounds (see `docs/cache-refactor/05-webhook-correctness-audit.md` §2). Prefer this ADR's pattern when the gap is narrow (one decision point, one field); reach for a reconcile extension only if a future gap turns out to span many decision points or fields such that per-site live reads would proliferate.

## Rationale

### Why not fix this in `boardcache.FetchItemDetails` itself?

The freshness gate is correct for the overwhelming majority of deep-field consumers — most mutations do bump `updatedAt`, and always live-reading would defeat the entire point of caching (this codebase's `boardcache/` package exists specifically to cut GraphQL spend, per ADR 034). The three known gaps are narrow: dependency-edge removal via REST, comment posting racing dispatch, and PR-linkage discovery racing the deep-fetch — each a specific GitHub API behavior that doesn't bump `updatedAt`, not a general property of deep fields. Fixing narrowly, at the decision point that's actually broken, keeps the fix's blast radius matched to the confirmed problem.

### Why reuse an existing cooldown instead of adding a dedicated one every time?

`recoverMissingPlanComment` needed a new cooldown because its decision point (`preImplement`) has no pre-existing admission throttle of its own — every Implement dispatch reaches it. `checkDependencies`'s `blockedBy` recheck, by contrast, is only ever reached for an already-blocked item once its `dep-blocked` cooldown has expired — that gate already exists and already solves the identical wake-loop concern, so a second, redundant cooldown would add complexity without a corresponding safety gain. Choose per-instance: reuse when a throttle already gates the call site, add a dedicated one when it doesn't.

## Consequences

**Positive:**
- The failure mode ("deep field goes stale forever because the parent's `updatedAt` didn't move") now has a named, documented remedy instead of requiring re-derivation from first principles each time it's found.
- `docs/cache-refactor/05-webhook-correctness-audit.md` uses this pattern as the reference point when flagging a fourth candidate (merge-queue membership) as a same-shape, unconfirmed risk rather than speculatively fixing it.

**Negative / accepted trade-offs:**
- `checkDependencies` is no longer purely in-memory for the already-blocked recheck path — see the ADR 016 status note below. This is a deliberate, permanent break from that ADR's original "no API calls" characterization, scoped narrowly to the recheck path only.
- The pattern is per-instance, not centralized — each of the three call sites re-implements its own "bypass cache, throttle, maybe write back" logic rather than sharing a helper. No shared helper was extracted because the three instances differ enough in throttle mechanism and write-back behavior (see table above) that a forced abstraction would likely obscure more than it'd save; revisit if a fourth instance makes the duplication costlier than the abstraction.

## See Also

- ADR 003 (`003-polling-over-webhooks.md`) — the foundational principle this pattern serves: webhooks are latency/cost optimizations, poll must always converge on correctness alone.
- ADR 016 (`016-graphql-state-for-dependency-resolution.md`) — amended with a status note pointing here; its "no API calls" consequence no longer holds unconditionally for `checkDependencies`.
- ADR 034 (`034-boardcache-event-sourced-delta.md`) — the cache/freshness-gate architecture this pattern works around at specific decision points, not broadly.
- `docs/cache-refactor/05-webhook-correctness-audit.md` — the full classification audit that catalogued all three instances and the categories still open for future audit.
