# ADR 2573: The deep-fetch apply does not write a webhook receipt time into the Store

**Status:** Accepted
**Date:** 2026-11-14
**Issue:** [#2573](https://github.com/handarbeit/fabrik/issues/2573)
**Amends:** ADR 2344 ("`UpdatedAt` only moves forward" on a deep-fetch apply). **Refines:** ADR 1945, ADR 2180.

## Context

An item at `fabrik:awaiting-ci` whose linked PR kept receiving check events produced a light-reconcile drift report on almost every tick (713 of 714 drifts in about 39 hours on one daemon), marking the webhook transport unhealthy and rewriting the cache each time. The engine caused it:

1. `CacheImpl.FetchProjectBoard` serves each item with `UpdatedAt = max(Store UpdatedAt, localDeltaAt)` and raises `IssueLayerUpdatedAt` the same way. `localDeltaAt` is the daemon's wall-clock time of the latest webhook for the item (`bumpLocalDeltaAt`), always a little later than GitHub's own value, and a check event does not move GitHub's issue or PR `updatedAt`.
2. `settleAwaitingCIScan` (and every other deep-fetch apply site: `selectDeepFetchCandidates`, `poll_settle`, `stage_outcome_publish`, `liveItem`) applies that served copy through `ItemDeepFetched`. `FetchItemDetails` never rewrites `UpdatedAt` or the layer evidence, so the copy still carries the receipt time. `applyDeepFetched` moved `item.UpdatedAt` forward to it and stored the layer pair.
3. `LightReconcile` compared the Store's `UpdatedAt` with GitHub's listing and reported drift; `Reconcile` wrote GitHub's value back, and the next poll pushed the receipt time in again.

## Decision

1. **`applyDeepFetched` writes `UpdatedAt` and the layer evidence (`IssueLayerUpdatedAt`, `LinkedPRUpdatedAt`) only as a first population**, when the Store holds no `UpdatedAt` / no layer pair. Afterwards they reach the Store from GitHub reads only: the probe at the start of every poll, the listing and reconcile applies, and webhook payloads. A deep-fetch copy is the served or poll-start copy and never carries a fresher GitHub time than those, so nothing is lost. This is ADR 2344's rule ("a deep fetch writes only deep-owned facts") applied to the last two listing facts it still wrote.
2. **`IssueCommentTotal` is still written unconditionally.** It is GitHub's own count, not derived from the stamp.
3. **The freshness baselines keep absorbing the served value.** `LastSeenSourceUpdatedAt` / `LastSeenPRUpdatedAt` still rise to the served copy's times (`store.go`, unchanged). `localDeltaAt` is never cleared, so if the baseline stopped absorbing it every item that ever received a webhook would read as stale and take a live deep fetch on every poll. FR-004 (a webhook stamp still counts as an issue-layer change in `cacheIsStale`) is therefore unchanged by construction: the served board, `EvidenceFromItem` and `cacheIsStale` are not touched.
4. **First population still writes.** Skipping it would leave a zero `UpdatedAt` that reads as drift. A first-population write from a served copy can hold a receipt time once; the next probe or reconcile corrects it. That is a bounded one-time effect, not a loop.
5. **Seam:** `Store.SetReceiptTimeApplyForTest` / `Engine.SetReceiptTimeApplyForTest` restore the unconditional write (per-Store, nothing package-level), so the sim twin can show the drift returns.
6. **Sim fidelity.** `simgh.FetchItemDetails` overwrote the caller's `UpdatedAt` and layer evidence with GitHub's values (`*item = *full`); the real client never touches them. It now keeps the caller's non-zero values, which is what lets the sim carry a served (receipt-inflated) copy through the deep fetch. Before this fix the sim could not reproduce the defect.

## Rejected alternatives

- **Serve GitHub's `UpdatedAt` and carry the receipt time in a new `ProjectItem` field.** It changes the served-board contract and adds a field across `github`, `boardcache`, `itemstate` and the sim, to protect a value no consumer reads from a deep-fetch write. The only readers of the Store's `UpdatedAt` are `LightReconcile`, `Reconcile`, `EffectiveShallow` and `snapshotToProjectItem`.
- **Consume or clear `localDeltaAt` once a later deep fetch is applied.** It would stop serving a permanent override, but it redesigns the receipt-time mechanism, which this issue puts out of scope.
- **Guard `LightReconcile` against an apply-time receipt.** It changes how real drift is detected. Real drift is unchanged here: `LightReconcile` still compares `UpdatedAt`, Status and the gate-label key with GitHub's listing.

## Consequences

- No light-reconcile drift for an item with no real GitHub change and continuous check events; a real missed change is still reported, with or without a check-event stream (`tests/sim/receipt_time_drift_test.go`).
- **Pre-existing, not widened:** the baseline holds a daemon-clock time, so a daemon clock ahead of GitHub's by δ can hide a real change stamped within δ after a check event on a webhook-less path.
- Unit tests: `internal/itemstate/deep_fetch_apply_test.go`. As-built text: `docs/state-machine.md` D.4.
