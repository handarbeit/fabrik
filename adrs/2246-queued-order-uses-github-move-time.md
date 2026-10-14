# ADR 2246: Queued batch order uses GitHub's move time, not observation time

**Date**: 2026-10-14
**Status**: Accepted
**Issue**: #2246 — merge-train: Queued order follows engine observation time, not GitHub move time, for moves seen together
**Amends**: ADR-1833

## Context

ADR-1833 orders each (repo, base) partition by `(StatusEnteredAt, Number)`, where `StatusEnteredAt` is `time.Now()` at the moment the engine *observes* a Status change. Moves picked up in the same light reconcile or probe pass are applied in a tight loop in listing (board-position) order, so they are stamped in that order and the batch order among them is arbitrary relative to the order the moves happened. In the v0.0.84 gate, four members moved to Queued back to back as A, B, C, P formed the batch `[A, P, B, C]`. The order also decides `capBatch`'s truncation and bisection's halves, so it is not purely cosmetic.

ADR-1833 rejected a GitHub source because no cheap one existed (no REST event for Projects v2; a per-item field-history query is too costly). That analysis missed one cheap fact: the Status single-select value has its own `updatedAt`, selectable on the `fieldValueByName(name: "Status")` node both board queries already fetch.

## Decision

- **A second field, `ItemState.StatusAtGitHub` (and `gh.ProjectItem.StatusAtGitHub`, `gh.BoardProbeItem.StatusAtGitHub`), carries GitHub's own time for the move. `StatusEnteredAt` keeps its meaning ("first seen by this process").** The attention classifier (`SinceFirstSeen`), the local API's `status_entered_at` and `StatusEnteredBasis` deliberately want observation time; redefining the field would make newly seeded items look old and ripple into ADR-2120's anchor-grace logic. Zero means unknown.
- **Sources.**
  - Shallow board listing and probe: `... on ProjectV2ItemFieldSingleSelectValue { name updatedAt }`. This is one scalar on a node already fetched: no new node, connection or round trip. The value's own `updatedAt` is used rather than the project item's, which every field write bumps (the display fields are written constantly; `projectItemUpdatedAt` has to discount them).
  - Webhook: `projects_v2_item.updated_at`, already parsed into `ProjectV2ItemEdited.At`.
  - The engine's own write (`LocalStatusUpdated`) clears it: the engine is the writer, so daemon time is the move time.
  - A nil value, missing or unparseable time, and the direct-GraphQL `GitHubAdapter` path leave it zero. Zero is never read as the epoch.
- **Stamped once per transition.** It is assigned inside the same `Status != new` guard as `StatusEnteredAt` at the five existing sites, so a no-op re-apply, a stale listing or a replayed webhook for the same Status never moves it, and a formed batch stays stable. ADR-2159/2178 protection is safe by construction: when it keeps the local Status the guard does not fire; when it fires, the Status and its time come from the same `view`/`pi`.
- **Ordering.** `queueOrderTime(atGitHub, observed)` returns the GitHub time when known and plausible, else the observation time. A GitHub time more than 30 s (`queueOrderSkewTolerance`) after the observation is treated as clock skew and ignored, so a future timestamp cannot sort ahead of genuinely later moves. The batch sort and the local API's train position both use it, so they cannot drift. The sort key is `(queueOrderTime, Number)`.
- **Restart.** `Store.Reset` and the probe bootstrap go through `applyProjectItem` with an empty prior Status, so they seed true entry times from GitHub. That removes ADR-1833's "restart reshuffles the batch once" limitation for items whose time is known.

## Accepted limits

- **Second resolution.** GitHub's `DateTime` has no sub-second part. Moves within one second tie and fall to `Number`, which is deterministic but is not the true order. Observation order within the pass is just listing order (the defect), so it is not used as a tie-break. Scripted back-to-back moves, like the gate's four, may well tie; this fix helps only where GitHub's times differ. That is the irreducible limit of the data (issue R2: "as close to GitHub's order as the available data allows").
- **Mixed clock domains.** The engine's own move into Queued (the dominant production path) keeps daemon time; operator and UI moves use GitHub's. Skew between the two is typically a second or two and the previous behaviour was strictly less accurate. If it ever matters, record the mutation response's `updatedAt` for engine writes.
- **Persisted runs unaffected.** Only fresh batch formation reads the sort (ADR-2051).
- **The live test keeps its workaround.** `queueInObservedOrder` (#2243) waits for the engine to read each move and sleeps 5 s. Same-second ties still depend on observation, so it is not retired here; a follow-up can measure whether it can be relaxed.
- **The sim bed cannot see this.** It runs on `GitHubAdapter` and has no per-field `updatedAt`. Proof lives in `internal/itemstate`, `github`, `boardcache` and `engine` unit tests, as in ADR-1833; `TestQueuedOrder_FollowsGitHubMoveTimeNotListingOrder` fails when the helper ignores the GitHub time.

## Consequences

- Batch order, `capBatch` truncation and bisection halves follow the order GitHub recorded the moves, to the second.
- One extra scalar per item on the shallow and probe queries (no new nodes). Recorded wire-contract fixtures without the key parse as unknown.
