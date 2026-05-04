# ADR 035: Four-Layer Status Reconciliation for User Mode

**Status**: Accepted  
**Date**: 2026-05-03  
**Supersedes (partially)**: ADR 032 (§Board-column changes and §`projects_v2_item` Event Support)  
**Supplements**: ADR 034 (Event-Sourced Board Cache via Webhook Deltas)

## Context

ADR 032 established that board-column changes in user mode are caught by the safety-net poll within `WebhookIdleCap` (60 minutes). ADR 034 added an in-memory board cache that is reconciled by a full `FetchProjectBoard` call every 60 minutes.

This 60-minute window produces unacceptable latency for users who manually move issues between board columns to trigger stage transitions. Issue #467 demonstrated a concrete 76-minute gap: a column move at ~16:20Z was not detected until ~17:36Z. GitHub does **not** deliver `projects_v2_item` events in user mode (PAT-based, repo-level webhooks via `gh webhook forward`), making the webhook stream blind to column moves permanently in this configuration.

Running the full board reconcile more frequently is not a viable solution: `FetchProjectBoard` is Fabrik's most expensive GraphQL query (full nested fields, comments, labels, PRs). Running it every 10 minutes would drain the GraphQL budget without proportional benefit.

## Decision

Replace the single 60-minute full-board reconcile loop with a four-layer strategy:

### Layer 0 — Write-through on self-driven transitions

When Fabrik itself calls `UpdateProjectItemStatus` to advance an issue, the cache is updated immediately at the call site with zero GraphQL cost.

**Implementation**: Every call site that invokes `UpdateProjectItemStatus` on success must also call `cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), newStatus)` via the safe type-assertion pattern:

```go
if cacheImpl, ok := e.readClient.(*boardcache.CacheImpl); ok {
    cacheImpl.UpdateItemStatus(boardcache.ItemKey(item.Repo, item.Number), newStatus)
}
```

Current call sites: `engine/stages.go` `advanceToNextStage` and `moveToNextDone`. Future call sites must follow the same pattern — this is a load-bearing invariant.

### Layer 1 — Opportunistic per-event Status refresh

After the board cache applies a webhook delta for an `issues` or `issue_comment` event on a known item, a single-item `FetchProjectItemStatus` query is issued and the result is applied to the cache. Cost: ~1–5 GraphQL points per event.

**Scope**: Restricted to `issues` and `issue_comment` event types. `pull_request`, `pull_request_review`, and `pull_request_review_comment` events require an O(N) cache scan to find the linked issue — the cost-benefit doesn't justify it for Layer 1. `check_run` events carry no item ID. Layer 2 covers these gaps within its cadence.

**Failure mode**: Best-effort. Errors are logged as warnings; the delta pipeline is never blocked.

**Paused cache**: Skipped when `IsPaused() == true` (cache is being fully reconciled on stream recovery).

### Layer 2 — Periodic status-field-only sweep

A `runReconciliationLoop` goroutine ticks at `ProjectStatusPollSeconds` (default 600 s, configurable via `--status-poll` / `FABRIK_STATUS_POLL` / `config.yaml status_poll`). Each tick calls `FetchProjectItemStatusBatch(projectID)` — a lightweight GraphQL query returning only `itemNodeID → statusName` for all board items — and applies drift via `ApplyStatusBatch`.

**Cost**: O(⌈N/100⌉) requests per tick (pagination at 100 items per page). A 200-item board uses 2 requests per 10 minutes = 12 per hour, well within the 5,000-point/hour budget.

**Empty project ID guard**: If `cacheImpl.ProjectID() == ""` (bootstrap not yet complete), the tick is skipped with a log warning.

### Bootstrap and stream-recovery (unchanged)

`FetchProjectBoard` (full board fetch) runs on startup (bootstrap) and on webhook stream recovery. These paths must handle all field types and remain unchanged.

## Consequences

### Residual latency

| Change type | Detection mechanism | Worst-case latency |
|-------------|--------------------|--------------------|
| Fabrik-driven stage advance | Layer 0 write-through | Zero |
| Column move + coincident issue activity | Layer 1 per-event refresh | Seconds |
| Column move with no other activity | Layer 2 periodic sweep | `ProjectStatusPollSeconds` (default 10 min) |
| Stream recovery (gap in webhook delivery) | Full reconcile | Until stream recovers |

The 60-minute worst-case latency from ADR 032 is reduced to the Layer 2 cadence (default 10 minutes) at significantly lower GraphQL cost than running the full reconcile more frequently.

### New exports from `boardcache`

- `ItemKey(repo string, number int) string` — exported to prevent format duplication between packages. This format (`owner/repo#number`) is now the public API for constructing cache keys.
- `CacheImpl.UpdateItemStatus(key, newStatus string)` — write lock, idempotent, no-op if key absent.
- `CacheImpl.ApplyStatusBatch(updates map[string]string)` — batch drift application under one write lock.
- `CacheImpl.GetItemID(key string) (string, bool)` — returns the PVTI_ node ID for Layer 1.
- `CacheImpl.ProjectID() string` — returns the project node ID for Layer 2.

### New `GitHubClient` methods

- `FetchProjectItemStatus(itemID string) (string, error)` — single-item Status query.
- `FetchProjectItemStatusBatch(projectID string) (map[string]string, error)` — paginated batch Status query.

These methods belong on `GitHubClient` only, not on `boardcache.ReadClient`. They are called directly from engine code (`e.client.*`), not through the cache read path.

### Write-through convention (load-bearing)

**Every future call site that calls `UpdateProjectItemStatus` to mutate a project item's Status must also call `cacheImpl.UpdateItemStatus` on the success path.** Failure to do so means the cache stales on self-driven transitions and the issue will not advance until the next Layer 2 sweep. This invariant is documented in `docs/state-machine.md §D.7`.

### `boardcache.ReadClient` unchanged

The `ReadClient` interface (used by `GitHubAdapter` and `boardcache` tests) is not modified. The new methods are engine-facing only.

## Alternatives Considered

**Run full reconcile at 10-minute cadence**: Rejected. Full board fetch is Fabrik's most expensive query. Running it 6× more often provides no additional benefit over the lightweight status sweep and wastes GraphQL budget.

**Include PR events in Layer 1**: Rejected. Mapping a PR number to a cache key requires an O(N) scan of all cached items under read lock. The marginal coverage improvement (catching column moves that coincide only with PR events, not issue events) doesn't justify the complexity and latency. Layer 2's 10-minute sweep covers the gap.

**Export `ItemKey` vs. inline format string**: Exporting chosen. The format `repo + "#" + strconv.Itoa(number)` is simple but must be consistent across packages. An exported helper prevents a silent divergence if the format ever changes and signals to callers that the format is stable and intentional.
