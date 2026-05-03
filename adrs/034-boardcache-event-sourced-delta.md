# ADR 034: Event-Sourced Board Cache via Webhook Deltas

**Status**: Accepted  
**Date**: 2026-05-02  
**Supplements**: ADR 032 (Webhook-Driven Event Delivery)

## Context

ADR 032 added webhook-driven event delivery: each verified payload wakes the poll loop, which immediately fetches the full board from GitHub GraphQL. This eliminated the 30-second polling wait but did not reduce the GraphQL points consumed _per event_. On a busy board with dozens of events per minute (CI check runs, PR review comments, label mutations), the board-fetch cost is replicated on every webhook wake.

Two additional problems motivate this ADR:

1. **Redundant deep-fetches.** `FetchItemDetails` is called for every item that passes `itemMayNeedWork`. Even after a single `issues.labeled` webhook that adds one label to one issue, the poll loop deep-fetches all items that might need work, paying GraphQL points for data that has not changed.

2. **Read amplification in the CI gate.** `checkCIGate` and `checkMergeabilityGate` call `FetchCheckRuns`, `FetchLinkedPR`, and `FetchPRMergeableState` on every poll cycle while an issue is in CI await. With webhook-driven wakes every 30–60 seconds during an active CI run, this multiplies quickly.

## Decision

Add an optional in-memory board cache (`boardcache.CacheImpl`) that is:

1. **Populated at startup** via a full `FetchProjectBoard` call immediately after the webhook listener starts.
2. **Kept current by webhook deltas** — typed pure functions apply incoming payloads as state mutations to the cache before the poll loop wakes.
3. **Served to read callers** via the existing `ReadClient` interface, replacing direct GitHub API calls for board/item/PR/check-run reads.
4. **Reconciled periodically** (every 60 minutes) via a full board fetch, healing any deltas missed by the webhook stream.
5. **Failed over on stream degradation** — when `checkHealthTransitions` fires `WebhookStreamUnhealthy`, the cache is paused and all reads fall back to GitHub. On recovery, the cache reconciles and resumes.

The cache is enabled by default when `--webhooks` is active and disabled otherwise. The `--board-cache` flag (and `FABRIK_BOARD_CACHE` env var) allows explicit override.

## Architecture

### Interface boundary

A new `boardcache.ReadClient` interface captures the 9 read-only methods from `engine.GitHubClient` that access board/item/PR/check-run state. `Engine.readClient` holds a `boardcache.ReadClient` instead of calling `Engine.client` directly for reads.

Two implementations:

- `boardcache.GitHubAdapter` — pass-through wrapper around any `ReadClient`. Used when `--board-cache=none`.
- `boardcache.CacheImpl` — in-memory cache with delta application and fallback. Used when `--board-cache=in-memory`.

`Engine.client` (the full `GitHubClient`) is retained for all write operations (mutations, label adds, PR creation, etc.) and for the direct GitHub fetches used in bootstrap and reconciliation. This keeps the cache's fallback path clean and prevents write paths from going through the cache.

### Delta functions

Each supported webhook event type maps to a typed delta function that parses the minimal required fields from the JSON payload and mutates the cache under a write lock. Delta functions are idempotent: applying the same event twice produces the same result as applying it once (idempotency keys: NodeID for comments/review-comments, DatabaseID for reviews, CheckRun.ID for check runs).

### Interface satisfaction via Go structural typing

`engine.GitHubClient` is a strict superset of `boardcache.ReadClient`. The concrete `gh.Client` (and the test mock `mockGitHubClient`) satisfy `boardcache.ReadClient` via Go's structural (implicit) typing without any additional adapter code. This means `boardcache.NewGitHubAdapter(engine.client)` works directly — no intermediate conversion type is needed.

### Shallow vs. deep cache fields

`Bootstrap` and `Reconcile` only populate shallow fields (the data returned by the board GraphQL query). Deep fields (comments, linked PR data, check runs) are populated lazily on cache miss via `FetchItemDetails`. The `deepFetched` set tracks which items have been deep-fetched; once set, subsequent `FetchItemDetails` calls are served from cache.

`Reconcile` preserves deep fields when updating an item: it copies shallow fields from the fresh snapshot but does not overwrite deep fields. This prevents a 60-minute reconciliation from triggering a burst of FetchItemDetails calls.

## Trade-offs

**Benefits:**
- Webhook wakes that touch a single item no longer trigger full board deep-fetches for unchanged items.
- CI gate polls during `fabrik:awaiting-ci` serve check runs from cache rather than GitHub API.
- The `ReadClient` abstraction is testable in isolation (`boardcache_test.go` tests the cache against a `mockClient` with call counters).

**Costs:**
- Cache coherence depends on the webhook stream. Missed events produce stale reads until the next reconciliation (up to 60 min). This was an accepted trade-off in ADR 032 for the poll loop; the same reasoning applies here.
- A stream-healthy cache that reads stale data (e.g., a label change not yet delivered) may cause the poll loop to make a decision based on cached state. The reconciliation loop and the safety-net poll bound the staleness window.
- Bootstrap requires a full `FetchProjectBoard` call at startup, adding one extra GraphQL request. This is amortized immediately.
- Additional complexity: `CacheImpl`, delta functions, reconciliation loop, stream-health callbacks.

## Alternatives Rejected

**Per-event live fetch (status quo):** The existing behavior after ADR 032. Simple but does not reduce per-event GraphQL cost.

**Full replacement of the poll loop with event sourcing:** Would require perfectly reliable webhook delivery and complex replay-on-restart logic. The existing poll-as-safety-net architecture is a better fit for a local CLI tool.

**External cache (Redis, SQLite):** Adds infrastructure. The in-memory approach is sufficient for a single-process tool; the cache is rebuilt cheaply from a single API call on restart.

## Consequences

- Steady-state GraphQL usage for read paths drops proportionally to the fraction of reads that hit the cache. For a board in CI await (frequent check_run events), the reduction is significant.
- The `boardcache` package is a new dependency of the `engine` package. It has no external imports beyond the existing `github` package types.
- `engine.GitHubClient` remains the single write-path interface; `boardcache.ReadClient` is read-only. This separation is enforced by the type system.
- ADR 032's health model (three states, grace period, unhealthy fallback) is reused as the cache failover trigger. No new health state is introduced.
