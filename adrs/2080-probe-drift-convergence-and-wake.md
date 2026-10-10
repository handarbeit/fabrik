# ADR 2080: Probe linkage drift converges, terminal items are exempt, probe writes do not wake the poll

**Status:** Accepted
**Issue:** #2080
**Date:** 2026-10-10
**Related:** ADR 044 (probe-driven poll loop), ADR 1716 (wake path rate-limit backoff gate), ADR 036/039 (single-owner store, wake flags)

## Context

`runProbeAndDeepFetch` compares each item's probe-reported linked PR with the cached `LinkedPR.Number` and, on a warm cache, invalidates the deep cache when they differ (ADR 044). The deep fetch that follows never writes the probe's value back: `applyProjectItem` only syncs `LinkedPR` when the incoming number is non-zero. When the two read paths keep disagreeing — the probe reports `0` for a PR that `closedByPullRequestsReferences` omits (a closed, unmerged PR; the REST lookup finds it and writes it to the cache) — the drift fires on every poll. On handarbeit/fabrik, 2026-10-09 to 10-10, one closed item fired it 8,767 times in ~25 h, ~350 forced polls an hour, draining the GraphQL budget until rate-limit backoff doubled the poll interval for every other item. The terminal short-circuit ran *after* the drift check, so a closed Done item reached the invalidation first.

## Decision

1. **Convergence by a per-item ledger, not by changing the deep-fetch apply.** An engine-local `probeDriftLedger` remembers the `(cached, probe)` pair last invalidated per item. A repeat of the pair is not invalidated again and is logged once; a different pair invalidates once; agreement forgets the pair. We did *not* make a deep-fetch `0` clear a cached `LinkedPR`: `base:<branch>` items read `0` from both GraphQL paths while a real PR is linked, PRs found through REST would be wiped, and `prToKey`/review/CI state hang off `LinkedPR`. The ledger meets the requirement's "recorded as the reconciled value for that probe reading". It lives on the Engine, not the Store, because it is probe-loop bookkeeping rather than item state (precedent: `flakeReruns`, `ciBackstopLiveEvaluated`); a restart loses it and costs one extra invalidation per affected item.
2. **Terminal items skip the drift check.** An item flagged `Terminal` in the same cleanup stage, or closed in a cleanup stage with no worktree (`probeOnlyTerminal`, the quiet form of `isProbeOnlyTerminal`), is exempt — it covers a closed item that never received `stage:Done:complete`, so `isTerminalPredicate` never flagged it.
3. **Probe-origin writes do not wake the poll.** The probe runs at the start of the poll that dispatches, so its own writes are already seen by that poll. The probe loop wraps its Store mutations in `itemstate.FromProbe`, which tags the `Change` with `OriginProbe`; `newWakeChObserver` ignores that origin. This is the requirement's "invalidation must not by itself trigger an immediate re-poll" form; a per-item early-wake limiter was rejected because it would also throttle legitimate label-driven wakes at stage boundaries, which is the latency this issue is about. `OriginProbe` is an *origin*, not a `ChangeFlags` bit: flags say what changed, the origin says who changed it.
4. **A loop is reported.** More than `probeDriftLoopThreshold` (10) invalidations of one item within `probeDriftLoopWindow` (1 h, `e.now()`) logs one warning naming the item and both values, so an unforeseen variant is a warning rather than a quiet budget drain.

## Consequences

- A new probe-loop Store write must be wrapped in `FromProbe` or it will wake the poll again.
- The cached `LinkedPR.Number` may stay stale for an item whose PR was closed; every other reader already uses the REST or webhook paths.
- The sim gained an opt-in board cache (`EnvOptions.BoardCache`, `Engine.UseBoardCacheForTest`) and an opt-in simgh fidelity option (`WithClosedPRsOmittedFromBoard`); the previous sim never ran `runProbeAndDeepFetch`.
- Neutralisation seam `Engine.SetProbeDriftNeutralisationForTest` shows the pre-fix loop in the unit tests and the sim.
