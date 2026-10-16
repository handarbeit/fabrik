# ADR 2347: The Layer 1 status refresh applies a GitHub read, not an engine write

## Status

Accepted. Amends ADR 2178 (a third input path under the same protection rule) and
the Layer 1 description of ADR 035.

## Context

`applyLayer1StatusRefresh` runs on every `issues` and `issue_comment` webhook,
reads the item's Status live from GitHub and applied it through
`CacheImpl.UpdateItemStatus` → `LocalStatusUpdated`, the engine-write path. That
stamped `EngineWriteAt["status"]` even when nothing changed, cleared
`StatusAtGitHub`, marked the change `EngineStatusWrite`/`OriginEngine` (so channel
events were attributed to `fabrik`), and had no staleness check: a read started
before a newer move could overwrite it, and the 30 s engine-write protection then
shielded the stale value from the correcting listing. This was the bypass ADR 2178
closed for the probe, reintroduced through another door.

## Decision

1. The refresh is applied as `itemstate.StatusRefreshed{Status, FetchedAt}` through
   `CacheImpl.ApplyStatusRefresh`. `FetchedAt` is the wall clock taken before the
   GitHub call (the Store stamps with `time.Now()`; `e.now()`/`SetClock` are not
   comparable, as in ADR 2178). `GitHubClient` is unchanged: the live reads return
   no GitHub timestamp.
2. The Store decides under its mutex (`applyStatusRefresh`): the existing
   `protectStatusClosed` rule is reused unchanged, evaluated against the item's own
   closed state so the `closed` record is never confirmed or dropped by a
   status-only read.
3. One extra rule covers "older than the field's last write, by any source": for an
   item that already holds a Status, a differing read with
   `FetchedAt < StatusEnteredAt` is dropped. `StatusEnteredAt` is the Store's
   observation time of the last Status change (engine, webhook or listing). The
   rule is conservative — a dropped read is corrected by the next webhook, probe or
   reconcile — and never blocks populating an empty Status.
4. An equal read applies nothing. A real change sets `Status`, clears `Terminal`,
   stamps `StatusEnteredAt` and sets `StatusAtGitHub` to unknown (zero) rather than
   leave the previous column's time, which would mis-order the merge-train Queued
   sort (ADR 2246).
5. It never records an engine write and sets no `EngineStatusWrite`, `Origin` or
   `Requester`, so `labelAttribute` yields `unknown` with no change to it.
6. `Change.Path` is a new `PathRefresh` (`status-refresh`) so the cache-divergence
   detector sees the refresh as a cache refresh from GitHub rather than a
   non-refresh (`PathNone`).
7. The refresh does not bump `localDeltaAt`: it is a read, not a local write
   (ADR 2180).

## Consequences

- A stale refresh can no longer revert a newer move, and a refresh cannot extend
  the engine-write protection window.
- Refresh-driven `column-changed` events change from `fabrik` to `unknown`.
- The Layer 1 refresh no longer feeds the cache-divergence ledger as an engine
  write; a refresh that disagrees with a recorded engine column write after the
  grace window is reported as a divergence on path `status-refresh`.
- The pre-#2347 behaviour stays reproducible through the per-Engine seam
  `SetLayer1AsEngineWriteForTest`, used by non-vacuity tests.
