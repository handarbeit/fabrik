# ADR-044: Probe-driven poll loop (replace per-poll shallow Reconcile)

**Status:** Accepted  
**Issue:** #685  
**Date:** 2026-05-10

## Context

The per-poll shallow `FetchProjectBoard` call was the dominant idle-burn cost on the GraphQL budget. On every poll cycle (~15 s), the engine fetched the full shallow board — including `labels(first:30)` and `closedByPullRequestsReferences(first:5)` per item — regardless of whether anything had changed. At 100 items per page, these nested connections cost ~3 500 nodes per poll just to discover that the board was idle.

Observed in production: a 166-item idle project board was the largest per-minute GraphQL consumer across four running Fabrik instances over a 15.5-hour window with no Claude work — pure baseline polling cost.

## Decision

**Replace the unconditional per-poll `FetchProjectBoard` + `Reconcile` call with a cheap `ProbeProjectBoard` probe that fires `FetchItemDetails` only for items whose `effectiveUpdatedAt` has advanced.**

### Probe query

`ProbeProjectBoard` fetches per item:

- `id`, `updatedAt` (project-item node)
- `content.__typename`, `content.id`, `content.number`, `content.state`, `content.updatedAt`, `content.repository.nameWithOwner`
- `content.closedByPullRequestsReferences(first:1) { nodes { number, updatedAt } }`

No `labels` connection at any nesting level.

### Staleness signal

Per item: `effectiveUpdatedAt = max(issue.updatedAt, projectItem.updatedAt, linkedPR.updatedAt)`.

This single value is compared against the store's `LastSeenSourceUpdatedAt` (written by `ItemDeepFetched`). If `effectiveUpdatedAt > LastSeenSourceUpdatedAt`, the item is stale and `FetchItemDetails` is called. This is functionally equivalent to tracking separate `lastSeenIssueUpdatedAt` and `lastSeenLinkedPRUpdatedAt` fields — the max collapses both signals into one — and requires no new `ItemState` field.

### Linkage drift detection

`closedByPullRequestsReferences(first:1)` serves two purposes:

1. **PR-side drift signal**: PR reviews, comments, and state changes bump `linkedPR.updatedAt`. The probe detects this via `effectiveUpdatedAt` without needing `fabrik:awaiting-review` to force a per-poll deep-fetch.

2. **Linkage drift detection**: if `probe.linkedPRNumber ≠ cached LinkedPR.Number`, `DeepFetchInvalidated` is applied (resetting `LastSeenSourceUpdatedAt`), forcing an immediate deep-fetch. This preserves the linkage-drift behavior previously handled by `Reconcile`'s `LinkedPRNumberShallow` branch.

### `ProbeBoardItemUpdated` mutation constraint

The probe constructs a `ProjectItem` with an empty `Labels` field (the probe query fetches no labels). `ShallowBoardItemUpdated` calls `applyShallowItem`, which sets `item.Labels = pi.Labels` — silently wiping the cached label set if called with probe data.

A new mutation type `ProbeBoardItemUpdated` is required. Its handler (`applyProbeItem`) updates `IsClosed`, `State`, `IsPR`, `Status`, and `UpdatedAt`, and **explicitly skips `Labels`**. This is a hard constraint: any future code that applies probe data to the store must use `ProbeBoardItemUpdated` (or an equivalent that skips Labels), never `ShallowBoardItemUpdated`.

### `Reconcile` scope

`Reconcile` is now Bootstrap-path only:

- **Bootstrap** (first poll, unbootstrapped cache): `FetchProjectBoard` → `Bootstrap` (which calls `Reconcile`).
- **Drift recovery** (webhook mode): `LightReconcile` → `Reconcile(freshBoard)` on detected drift.
- **Per-poll**: replaced by `runProbeAndDeepFetch`. `Reconcile` is not called.

The `LinkedPRNumberShallow` linkage-drift branch in `Reconcile` is removed (linkage drift is now in the probe loop). The `LinkedPRNumberShallow` field in `ProjectItem` is retained with an updated comment.

### `LightReconcile` unchanged

`LightReconcile` (webhook mode, `reconcileTicker` goroutine, every 3 minutes) continues to call `FetchProjectBoard` (full shallow board with labels). Updating it to use `ProbeProjectBoard` would require adding `ProbeProjectBoard` to the `ReadClient` interface and updating `GitHubAdapter` and all mocks. The savings from `LightReconcile` (≤ 20 calls/hour at 3-minute cadence) are negligible compared to the main-loop savings (≤ 240 probe calls/hour). `LightReconcile` is left as-is.

### Startup transient-label scan

A one-shot `runStartupTransientLabelScan` runs after the first successful poll. It scans `store.All()` for closed items carrying transient lifecycle labels and calls `cleanupClosedIssueLocks` / `cleanupClosedIssueTransientLabels`. No GitHub API call is needed — Bootstrap already populated the store with full label data from `FetchProjectBoard`. This handles the restart-recovery case: an issue closed mid-stage during a prior crash may have stale transient labels that the probe-only flow can't surface (probe carries no labels).

## Consequences

- **Per-poll GraphQL cost**: ~5–10× reduction on idle boards by eliminating `labels(first:30)` (the single largest driver).
- **PR-side change detection latency**: unchanged — `linkedPR.updatedAt` in the probe provides the same signal as the previous `closedByPullRequestsReferences(first:5)`.
- **Label visibility during steady-state**: unchanged — labels come from `FetchItemDetails` (full label set), now written on first access and refreshed on any `updatedAt` drift.
- **Constraint for future contributors**: mutations applied from probe data MUST use `ProbeBoardItemUpdated`, not `ShallowBoardItemUpdated`. Adding labels or other deep fields to the probe query without a corresponding mutation-type update would silently corrupt the cache.
- **`LightReconcile` residual cost**: ≤ 20 full shallow board fetches/hour in webhook mode. Acceptable; a follow-up could add `ProbeProjectBoard` to `ReadClient` to eliminate this cost.

---

## Addendum — Probe bootstrap (issue #710, 2026-05-11)

The Bootstrap section above stated: *"Bootstrap (first poll, unbootstrapped cache): `FetchProjectBoard` → `Bootstrap`."* This has been superseded.

### New cold-start path

The virgin-cache branch now uses `ProbeProjectBoard → BootstrapFromProbe` instead of `FetchProjectBoard → Bootstrap`.

**Why**: `FetchProjectBoard` carries `labels(first:30)` and `closedByPullRequestsReferences(first:5)` per item, costing ~2 350 nodes on a 47-item board just to warm a cold cache. `ProbeProjectBoard` costs ~250 nodes for the same board. An overnight test revealed the old path was causing Fabrik to go deaf for ~1 hour after restart when the shared GraphQL budget was already depleted.

### `BootstrapFromProbe(items []BoardProbeItem, projectID string)`

New method on `boardcache.CacheImpl`. Constructs synthetic `[]gh.ProjectItem` from probe items and calls `store.Reset`, which populates `LinkedPR.Number` from `LinkedPRNumber` via `applyProjectItem`. This prevents the subsequent probe cycle from seeing spurious linkage-drift on items that already have a PR.

After `store.Reset`, the engine calls `seedTerminalFromProbeItems` (see Addendum 3) to apply `TerminalFlagSet` for items that are both closed, in a cleanup stage, and have no on-disk worktree. The simplified predicate (no label check, but with worktree check) means these items are never deep-fetched — not by the first probe cycle, and not by subsequent cycles unless their status changes.

### Label absence and its consequences

Probe results carry no labels. After `BootstrapFromProbe`:

- `runStartupTransientLabelScan` is a no-op (empty label sets; stale transient labels on closed terminal items will not be detected at startup).
- `runStartupTerminalScan` is a no-op (label-aware predicate requires labels; cold-start seeding was done by `BootstrapFromProbe` instead).

The accepted gap: an item with stale transient labels that closed mid-Done-stage in a prior crash will not be cleaned up at startup. Probability is very low. The steady-state deep-fetch path cleans up non-terminal items naturally on the first probe cycle.

### Linkage-drift gate

`runProbeAndDeepFetch` now gates the linkage-drift check on `s.LastDeepFetchAt.IsZero()`:

- **Never deep-fetched** (`LastDeepFetchAt == zero`): probe's `LinkedPRNumber` is written authoritatively via `PRDetailsUpdated` (updates `LinkedPR.Number` and `prToKey` reverse index) without firing `DeepFetchInvalidated`. There is no prior deep cache to invalidate.
- **Warm cache** (`LastDeepFetchAt != zero`): existing `DeepFetchInvalidated` path unchanged — real linkage drift forces an immediate re-deep-fetch.

This is a correctness fix as well as an optimization: the old code fired `DeepFetchInvalidated` on every cold start for every item with a PR (because the old `Bootstrap` from `FetchProjectBoard` wrote `LinkedPR.Number=0`, and the first probe found the actual PR number — interpreted as drift). With `BootstrapFromProbe` correctly seeding `LinkedPR.Number`, this spurious case no longer arises; the gate also eliminates it for any remaining edge cases.

### Bootstrap path summary (updated)

| Trigger | Path | Labels in store? | Terminal seeding |
|---|---|---|---|
| Virgin cache (default) | `ProbeProjectBoard → BootstrapFromProbe` | No | `IsClosed + CleanupWorktree + worktree absent` (engine-side, see Addendum 3) |
| Webhook startup (before `wm.Start()`) | `ProbeProjectBoard → BootstrapFromProbe` | No | `IsClosed + CleanupWorktree + worktree absent` (engine-side, see Addendum 3) |
| Drift recovery / reconcile | `LightReconcile → Reconcile(freshBoard)` | Yes (from `FetchProjectBoard`) | `runStartupTerminalScan` |

---

## Addendum 2 — Webhook startup path converged (issue #751, 2026-05-19)

### Problem

The webhook-mode startup block at `engine/poll.go` called `FetchProjectBoard` (~2 350 GraphQL nodes) and fed the result to `CacheImpl.Bootstrap()`. `Bootstrap()` populated the store via `store.Reset()` but never set `LastDeepFetchAt`, `LastSeenSourceUpdatedAt`, or the `Terminal` flag. On the next poll, `runProbeAndDeepFetch` found `s.Terminal == false` for every closed Done item (the terminal-skip short-circuit did not fire) and `IsItemCacheFresh() == false` because `LastDeepFetchAt.IsZero()` — triggering `FetchItemDetails` for every item on the board. On a 47-item board this burned ~2 350 nodes at restart with no useful result.

The polling-only startup path was not affected: it fell into the virgin-cache branch that already used `ProbeProjectBoard → BootstrapFromProbe`. The asymmetry was the bug.

### Fix

The webhook startup block was replaced with the same `ProbeProjectBoard → BootstrapFromProbe` pattern used by the virgin-cache branch:

1. `ProbeProjectBoard` (~250 nodes) fetches the board probe.
2. If the probe returns zero items (transient indexer hiccup), the cache is left virgin and the first poll retries.
3. Otherwise, `BootstrapFromProbe` seeds the store with synthetic items; the engine's `seedTerminalFromProbeItems` then seeds `Terminal=true` for closed cleanup-stage items whose worktrees are absent on disk — these are skipped by `runProbeAndDeepFetch` on the first poll. See Addendum 3 for the worktree-presence check.

The replacement block runs synchronously before `wm.Start()`, preserving the no-delta-in-empty-cache ordering guarantee.

`CacheImpl.Bootstrap()` was removed — it had no remaining callers after this change. `FetchProjectBoard()` itself is retained; the light-reconcile loop still uses it legitimately.

### Cost impact

| Scenario | Before | After |
|---|---|---|
| Webhook restart, N=47 items, k=5 active | ~2 350 nodes (probe) + 47 × deep-fetch | ~250 nodes (probe) + 5 × deep-fetch |
| Virgin-cache (polling-only) | Already using BootstrapFromProbe (no change) | Same |

---

## Addendum 3 — Terminal seeding moved to engine with worktree-presence check (issue #858, 2026-06-16)

### Problem

The probe-only terminal predicate (`IsClosed + CleanupWorktree`) in `BootstrapFromProbe` and `isProbeOnlyTerminal` both seeded `Terminal=true` without verifying that the on-disk worktree was absent. This meant items that were closed and in the Done stage — but whose worktrees had never been cleaned up — were incorrectly marked terminal at bootstrap, permanently preventing `processItem` from running the `cleanup_worktree` action.

The common trigger was an auto-upgrade restart between a PR merge (which closes the issue and advances the board to Done) and the next `processItem` invocation. The engine restarted, observed the closed Done item in the probe, seeded `Terminal=true`, and never touched the issue again. Worktrees accumulated silently on disk.

### Fix

**Terminal seeding responsibility moved from `boardcache` to `engine`** (consistent with ADR-036's directive that lifecycle decisions belong in the engine).

`BootstrapFromProbe` signature changed: `stagesCfg []*stages.Stage` parameter removed. The method now only populates the store from probe data; it no longer applies `TerminalFlagSet`.

Two new engine methods handle terminal seeding:

- **`(e *Engine) isProbeOnlyTerminal(item gh.ProjectItem) bool`** — replaces the package-level function. Checks `IsClosed`, `CleanupWorktree`, and (new) `e.worktreeExistsForItem(item)`. Returns true only when all three conditions hold: closed, cleanup stage, AND worktree absent on disk. Logs the outcome at the decision point (FR-6).

- **`(e *Engine) seedTerminalFromProbeItems(items []gh.BoardProbeItem)`** — called by the engine immediately after each `BootstrapFromProbe` invocation (both the webhook startup path at `poll.go:451` and the virgin-cache path at `poll.go:929`). Iterates probe items, calls `isProbeOnlyTerminal`, and applies `TerminalFlagSet` only for items whose worktrees are confirmed absent.

The single production call to the old `isProbeOnlyTerminal` in `runProbeAndDeepFetch`'s new-item branch was updated to call the new `*Engine` method with the in-scope `minimal` `gh.ProjectItem`.

### Consequence

Items that are closed and in a Done/cleanup stage but whose worktrees still exist on disk proceed through the normal dispatch path: deep-fetch → `processItem` → `cleanup_worktree` → worktree removed → `isTerminalPredicate` (label-aware) confirms terminal on the next poll. The existing cost-saving short-circuit is preserved for items whose worktrees have already been cleaned up.
