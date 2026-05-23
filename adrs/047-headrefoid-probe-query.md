# ADR 047: Add headRefOid to Probe and Deep-Fetch GraphQL Queries

**Date**: 2026-05-23  
**Status**: Accepted

## Context

ADR 027 explicitly rejected extending the GraphQL query for `headRefOid` when implementing the CI gate:

> Adding `pullRequest { headRefOid }` to the board GraphQL query would avoid a separate REST call for the head SHA. Rejected because the existing query is already large. The head SHA is only needed on CI-gate paths (stages with `wait_for_ci: true`, which is a subset of all items). A targeted REST call via `FetchLinkedPR` has lower cost for the common case (no CI gate).

ADR 027's preferred approach was to fetch `HeadSHA` via the `FetchLinkedPR` REST fallback in `boardcache/boardcache.go`. In production, this path was found to be broken in three colluding ways (tracked as issue #779):

1. **Bug 1** (`PRDetailsUpdated` drops `HeadSHA`): The `PRDetailsUpdated` mutation struct had no `HeadSHA` field, so the REST fallback result's SHA was silently discarded.

2. **Bug 2** (`FetchLinkedPR` cache write fires only once with stale data): The `PRHeadSHAUpdated` write was gated on `linkedPRNum == 0` (only fires at link-establishment), and used the pre-fetch snapshot's SHA instead of the fresh REST result.

3. **Bug 3** (Cache-hit eligibility accepts empty `HeadSHA`): The cache-hit guard for `FetchLinkedPR` required only `lpr.Title != ""`, serving stale records with `HeadSHA == ""` as valid cache hits on every subsequent poll.

The combined effect: `checkCIGate` received `HeadSHA == ""` from the cache after the first poll, treated it as "no PR to gate," and silently cleared the CI gate — adding `stage:Validate:complete` and removing `fabrik:awaiting-ci` even while CI was actively failing. This made the failure mode unrecoverable without restarting Fabrik.

## Decision

Fix all three bugs and extend the GraphQL queries to make `headRefOid` available from the polling paths independently of the REST fallback.

### Fix A: Add `headRefOid` to the Probe and Deep-Fetch Queries

ADR 027's concern was the cost of the **full board query** (`FetchProjectBoard`), which fetches all items with labels, assignees, and linked PRs. This query is already large.

The probe query (`ProbeProjectBoard`) is architecturally distinct: it fetches only scalar identity fields per item (no labels, no comments, no full PR data) and was specifically designed to be cheap. Adding `headRefOid` to the probe's single `closedByPullRequestsReferences(first: 1)` node is a small incremental cost — one additional string field per item with a linked PR — that is negligible relative to the query's total size.

The deep-fetch query (`FetchItemDetails`) is per-item and already fetches extensive linked-PR data (comments, reviews, review threads). Adding `headRefOid` there adds no meaningful cost.

**Scope of change**: `headRefOid` is added to all three `closedByPullRequestsReferences` query blocks in `github/project.go`:
- Block 2 (probe): populated into `BoardProbeItem.LinkedPRHeadSHA`; applied to the store as `PRHeadSHAUpdated` in `engine/poll.go:runProbeAndDeepFetch` on every poll cycle.
- Block 3 (deep fetch): populated into `ProjectItem.LinkedPRHeadSHA`; applied as a second `PRHeadSHAUpdated` after `ItemDeepFetched` in `boardcache/boardcache.go:FetchItemDetails`.
- Block 1 (shallow board / reconcile): populated into `ProjectItem.LinkedPRHeadSHA`; applied as `PRHeadSHAUpdated` in `boardcache/boardcache.go:Reconcile` (60-min backstop repair path).

The existing guard in `applyProjectItem` that prevents `LinkedPR.HeadSHA` from being overwritten by shallow data (`lpr.HeadSHA != ""` guard at `store.go:839`) is intentionally preserved. All SHA writes go through `PRHeadSHAUpdated`, not through `applyProjectItem`.

### Fix B: FetchLinkedPR Cache Write Uses Fresh SHA, Fires Unconditionally

The `FetchLinkedPR` REST fallback now writes `pr.HeadSHA` (the fresh result) instead of `existingSHA` (the stale pre-fetch snapshot), and removes the `linkedPRNum == 0` guard so `PRHeadSHAUpdated` fires on every fallback, not just at link-establishment. `PRHeadSHAUpdated` is idempotent; writing the authoritative REST SHA on every fallback is safe.

### Fix C: Cache-Hit Guard Requires Non-Empty HeadSHA

The `FetchLinkedPR` cache-hit eligibility check is tightened to require `lpr.HeadSHA != ""` in addition to `lpr.Title != ""`. A record with a populated Title but empty HeadSHA is treated as a cache miss, forcing a REST fallback. This prevents any future code path from accidentally writing an empty-SHA record into the cache and having it served as a valid hit.

### Fix E: CI Gate Treats Empty HeadSHA as Blocked

`checkCIGate` in `engine/ci.go` previously treated `pr.HeadSHA == ""` the same as `pr == nil` (clearing the gate). The conditions are now split: `pr == nil` still clears the gate (no PR to check); `pr.HeadSHA == ""` on a non-nil PR blocks the gate with `(true, false, false)` until the SHA is populated. This is a belt-and-suspenders defense — with Fixes A, B, C in place the cache should never serve an empty-SHA record for a valid PR, but if it does, the gate remains armed rather than silently disarming.

## Why Not Extend the Full Board Query

ADR 027's cost concern applies specifically to `FetchProjectBoard`, which runs every 60 minutes as a reconcile backstop. Adding `headRefOid` to that query (Block 1) is a lower-priority improvement included here purely for defense-in-depth (the reconcile path repairs state every 60 min). The critical path is Block 2 (probe), which runs every poll and is already cheap.

## Relationship to ADR 027

This ADR partially supersedes the "Extending the GraphQL Query for Head SHA" alternative rejected in ADR 027. The distinction:

- ADR 027 rejected adding `headRefOid` to the **full board query** (Block 1 = large query, all items).
- This ADR adds `headRefOid` to the **probe query** (Block 2 = minimal query, one string field per item) and **deep-fetch query** (Block 3 = per-item query that already fetches extensive PR data).

The REST fallback (`FetchLinkedPR`) remains in place as the authoritative source for `HeadSHA` when a direct `checkCIGate` call triggers a cache miss. The GraphQL paths now serve as the primary population mechanism so the REST fallback is rarely needed in practice.

## Consequences

- `headRefOid` is fetched on every probe cycle for items with a linked PR, regardless of whether those items are on CI-gated stages. This is a small cost increase (one extra string per linked PR per poll) accepted as the price of cache correctness.
- `BoardProbeItem` and `ProjectItem` gain a `LinkedPRHeadSHA string` field.
- The store's `HeadSHA` for any item with a linked PR is now refreshed at probe frequency rather than only on REST fallback or webhook events.
- `FetchLinkedPR` cache hits now require both `Title != ""` and `HeadSHA != ""`, meaning more cache misses for newly-linked PRs (they fall through to REST until both fields are populated). This is by design — the brief extra REST call eliminates the failure mode.
