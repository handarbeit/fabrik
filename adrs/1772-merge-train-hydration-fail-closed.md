# ADR 1772: Merge-Train Base Grouping Fails Closed on an Unhydrated Cache Entry

**Date**: 2026-09-18
**Status**: Accepted
**Issue**: #1772 — merge-train pins the default branch for an unhydrated member: fail closed
instead of assuming default base

## Context

`groupQueuedByRepoAndBase` (ADR-1648) partitions each repo's Queued members by resolved base
branch. An item with no `base:` label is bucketed under the `defaultPartitionBase` sentinel without
ever touching git — the zero-cost path for the overwhelming common case. An item carrying a
`base:<branch>` label instead resolves its real branch via `baseBranchForItem`, behind two existing
fail-closed guards (WorktreeManager not yet registered; `baseBranchForItem` error) that exclude the
item from batching rather than guess.

Both of those guards live *inside* the `itemHasBaseLabel(item)` branch. `itemHasBaseLabel` is a pure
scan of `item.Labels` — it has no way to distinguish "this item genuinely has no `base:` label" from
"this item's `Labels` slice hasn't loaded yet." A Queued member whose board-cache entry is a fresh,
never-deep-fetched `Store` snapshot has an empty `Labels` slice for the latter reason, not the
former. `itemHasBaseLabel` reports `false` either way, so the item falls straight through to
`defaultPartitionBase` — neither existing guard is ever reached, because neither lives on the path
an unhydrated item takes.

This reached production on v0.0.81 (reported in #1688 by @bdueck): an integration PR was opened
against a protected default branch and self-merged nine minutes later, bypassing a human promotion
gate whose entire purpose was to guard that branch. The reporter's board carries `base:<branch>`
members; one of them was queued while its cache entry was still cold.

The reporter's first framing was a post-start/`SIGHUP` bootstrap window. He retracted it: the
confirmed occurrence had roughly two hours of uptime, no restart, no `SIGHUP`, and many successful
default-base landings both before and after. A per-poll probe-driven catch-up path
(`runProbeAndDeepFetch`) can leave an individual item's `BoardProbeItem`-sourced snapshot without a
deep fetch on any given poll, long after startup — the cache can be cold for one specific item at
any time, not just in a narrow boot window.

## Decision

Add a third fail-closed guard to `groupQueuedByRepoAndBase`, running unconditionally as the first
statement of the per-item loop — before `base := defaultPartitionBase` is set from anything and
before `itemHasBaseLabel` is even called:

```go
if c := e.cache(); c != nil && !c.IsItemDeepFetched(rg.repoKey, item.Number) {
    e.logf(item.Number, "merge-train", "cache entry for #%d not yet hydrated (no deep-fetch recorded) — excluding from batching this poll, will retry\n", item.Number)
    continue
}
```

### Per-item, never a global flag

`CacheImpl.IsItemDeepFetched(repo, number)` is inherently keyed on `(repo, number)`. This is
deliberate and load-bearing: a global readiness flag such as `CacheImpl.IsBootstrapped()` (in
practice, "does the store hold any item at all") would have passed every test written against the
retracted post-start/`SIGHUP` framing and still failed in production, because the confirmed
occurrence had a cache that was bootstrapped and serving other items correctly — only this one
member's entry was cold. A global flag answers "has the cache loaded *something*"; the question that
matters here is "has the cache loaded *this* item."

### Structural, not just behavioral

Placing the check ahead of `itemHasBaseLabel`, rather than folding it into that branch or into one
of the two existing guards, makes "never pin from absent data" a property of control flow rather
than a behavior that has to be independently verified: the function that manufactures the false
"no label" signal is never invoked at all for an unhydrated item, so there is no path left by which
label-based logic can run on absent data.

### Fails open when no cache is wired

`e.cache() == nil` — a `GitHubAdapter`-backed engine (cache disabled, and every pre-existing
merge-train test built via `NewWithDeps`) — skips the check entirely. That mode always reads live,
full data on every call; there is no partial/unhydrated state it can be in, so treating "no
`CacheImpl`" as "unhydrated" would incorrectly exclude every item in every non-cache deployment and
break every existing test in this area.

### Accepted over-exclusion

`IsItemDeepFetched` is true only once `FetchItemDetails`'s deep fetch has run for that item; a
full shallow board reconcile can populate real `Labels` without ever touching
`LastDeepFetchAt`. An item that has gone through a shallow reconcile (accurate labels) but has never
individually been deep-fetched will still test as not-hydrated here and be excluded for one extra
poll. This is a conservative superset of the confirmed defect, not a new failure mode: exclusion is
self-healing (the item is picked up warm once deep-fetched, no operator action required), and the
window is rare. No supplementary "also treat a non-empty `Title` as hydrated" check was added — it
would add complexity the spec did not ask for, in exchange for shrinking an already-tolerable
window.

### Scope

Both existing consumers of `groupQueuedByRepoAndBase` — `handleMergeTrainBatch` (dispatch) and
`settleQueuedReviewFindings` (ADR-1208's pending-eject signal) — get this guard automatically,
matching ADR-1648's "one source of truth" design intent; neither needed a separate change.
`mergeTrainKeyForItem` (`runaway_alert_settle.go`) independently reconstructs the same bucketing
rule, but only for a member `fireRunawayGuard` has already alerted — which requires the member to
have already passed through this (now-fixed) grouping once, so it cannot observe an unhydrated
item and needs no equivalent guard.

## Consequences

- An unhydrated Queued member is excluded from batching and remains in `Queued` for that poll,
  logged under the `merge-train` tag naming the item and reason, exactly mirroring the two existing
  fail-closed guards' shape. It is picked up on a later poll once the ordinary deep-fetch catch-up
  path hydrates it — self-healing, no operator action required.
- A hydrated item with no `base:` label is unaffected: it still partitions to
  `defaultPartitionBase` exactly as before this issue (R5).
- A hydrated item with a `base:` label is unaffected: it still resolves to its real branch via
  `baseBranchForItem` exactly as before (no regression to #1648).
- No new git or GraphQL call is introduced. `IsItemDeepFetched` reads only the in-memory `Store`,
  preserving ADR-1648's zero-cost-for-the-common-case constraint.

## Rejected Alternatives

- **A global `bootstrapped` flag** (e.g., gating on `CacheImpl.IsBootstrapped()` or an equivalent
  "has the cache ever loaded anything" signal). Rejected per the Decision section above: it matches
  the retracted bootstrap-window framing, not the confirmed per-item cold-cache occurrence, and
  would not have caught the actual production defect.
- **Treating non-empty `Title` as a hydration signal**, to shrink the accepted over-exclusion
  window described above. Rejected as unnecessary complexity for a rare, already self-healing edge
  case that the issue's acceptance criteria do not require closing.

## Prior Art

- [ADR-1648](1648-merge-train-per-base-partitioning.md) — the (repo, base) partitioning scheme this
  defect lives inside, and the two existing fail-closed guards this one is structurally modeled on.
- [ADR-1647](1647-merge-train-non-default-base-exclusion.md) — the superseded exclusion-based
  precursor to ADR-1648; establishes the "exclude the one item, not the whole repo group" fail-closed
  posture this issue's guard also follows.
