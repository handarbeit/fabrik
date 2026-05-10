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
