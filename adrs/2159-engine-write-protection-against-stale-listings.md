# ADR 2159: Protect the engine's own recent writes from stale reconcile listings

**Date**: 2026-10-11
**Status**: Accepted
**Issue**: #2159 — reconcile: a light reconcile reverts the engine's own label writes with a pre-write board listing

## Context

`reconcileTick` calls `CacheImpl.LightReconcile`, which fetches a board listing, and on any drift `Reconcile(freshBoard)` overwrites the cached shallow state with it. GitHub's board listing lags writes by seconds (the #1974 board-lag probe measures this), and a write can also land while the fetch is in flight. A reconcile that ran 1–2 s after an engine label write therefore restored the pre-write value, the cache stayed wrong until the next tick (~3 min), every gate reading the cache saw the wrong state, and the channel stream (#1968) emitted a false label event with actor `unknown`. Observed on the dev daemon on 2026-10-11 for #2091, #2097 and #2116.

## Decision

The decision is made **inside the item store**, under the Store mutex, atomically with `applyShallowItem` — not in `boardcache.Reconcile` by reading the store first, which would race with a concurrent engine write.

1. **Record** (`ItemState.EngineWriteAt`): per item, keys `label:<name>`, `status`, `closed` → time of the engine's last write through the cache write-through (`LocalLabelAdded/Removed`, `LocalStatusUpdated`, new `LocalIssueClosed`). The write-through runs only after the REST call succeeded, so a failed write is never recorded. In memory only.
2. **Listing fetch time** (`ProjectBoard.FetchedAt`, `ShallowBoardItemUpdated.FetchedAt`): stamped by `LightReconcile` just before the fetch; zero (all other callers) disables protection, preserving today's behaviour.
3. **Rule** (`itemstate.protectShallow`): a field with a record at W keeps its *current cached value* while `FetchedAt < W + grace` (`DefaultEngineWriteGrace`, 30 s). Records are dropped once a listing fetched at or after W agrees with the cache, or when they expire. A listing fetched after W + grace wins.
4. **One helper for both sides**: `Store.EffectiveShallow` runs the same rule read-only for `LightReconcile`'s drift comparison, and `LightReconcile` snapshots the cache *after* the fetch. An item whose only difference is a recent engine write is not drift, so it neither flips the stream unhealthy nor triggers a reconcile. While a record shields the item the `UpdatedAt` comparison is skipped (engine writes never bump the cached `UpdatedAt`).
5. **No false events**: a kept value is never written to the item, so no `LabelDeltas` exist. The bookkeeping map is excluded from the store's no-op detection and `EngineWriteRecordedChanged` is not a wake flag, so a redundant write or a dropped record is not a `Change`.
6. **Shutdown** (`cleanupLockedIssues`): skip the REST removal only when the engine itself recorded removing the lock label (`EngineWriteAt[label:<lock>]`) and the cache confirms it is gone; treat `gh.ErrNotFound` as success; always release the lock in the store. A cached set that merely lacks the label is not proof (the board query truncates labels; the deep-fetch write-back is unprotected), so without the record the removal is still attempted.

## Alternatives considered

- **Monotonic sequence number instead of wall-clock + grace.** Deterministic, but cannot express GitHub-side listing lag (a listing requested *after* the write that still misses it), which is the observed 1–2 s case.
- **Protect the engine's intended value.** Would override a later webhook change (a human unlabelling after the engine labelled). Protecting the cache's *current* value respects it with no webhook-side recording.
- **Reuse `LabelAppliedAt`.** Adds-only, keep-first and read by gates; unsuitable.
- **Gate `applyProjectItem` (deep fetch) too.** Out of scope per the issue and ADR-957; the helper makes it a small later extension.

## Consequences

- A genuine external change that lands inside the grace window is corrected by the next tick, not the one that saw it.
- A listing lagging by more than the grace still wins; the constant has a test setter and can be tuned.
- Writes that bypass the cache write-through, and the deep-fetch label write-back, remain unprotected.
- **Follow-up (not fixed here):** the shutdown 404s have a second cause independent of reconcile — `engine/reinvoke.go` marks `Lock.HeldByThis` without applying a lock label and nothing releases it, so every reinvoke leaves a held lock that cleanup then tries to remove. The shutdown-side changes above stop the 404s; releasing the lock when the reinvoke ends is a separate issue.
