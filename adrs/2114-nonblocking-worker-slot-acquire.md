# ADR 2114: Never block the poll goroutine on a worker slot

**Status:** Accepted
**Issue:** #2114
**Amends:** ADR-039 (Consequence 4)

## Context

`dispatchCandidates` took a worker slot with a blocking `select { case e.sem <- struct{}{}: case <-ctx.Done(): }` on the poll goroutine. With all `max_concurrent` slots busy and a dispatch candidate present, the poll stopped there until some worker exited. Everything `poll()` runs after dispatch — `settleTrainRuns`/`handleMergeTrainBatch`, the queued-review, member-close, runaway-alert, non-default-base, PR-ready and landing-verification settle scans — waited with it, and so did the next poll. Reconcile and channel pushes run on their own loops, so the daemon looked alive while nothing was being evaluated. Seen live on the dev daemon on 2026-10-11: over ten minutes against `poll: 180`.

The reinvoke dispatchers acquire inside their own goroutine, and the merge train's `acquireTrainSlot` (ADR-2046) is on a worker goroutine. `dispatchCandidates` was the only blocking acquire on the poll path.

## Decision

1. The acquire is non-blocking: `select { case e.sem <- struct{}{}: default: <defer> }`. A candidate that finds every slot busy is **deferred** — `WorkerEntered`, the issue context, `StageAttempted`, retry counts and labels are all untouched — and the loop carries on. `ctx.Err()` is checked at the top of each iteration, since the `default` branch hides cancellation. The `snap.Worker() != nil` guard, the R5 sentinel check (#1779), `WorkerEntered`-before-goroutine and the release/`WorkerExited` defer order are unchanged.
2. `dispatchCandidates` returns `(dispatched, deferredKeys)`. When anything was deferred it logs **one** poll-level line: `deferred dispatch of N candidate(s) #A, #B: all worker slots busy (in_use/max in use)`, using `len(e.sem)/cap(e.sem)` (true occupancy, including reinvoke and merge-train conflict-resolution holders).
3. `poll()` keeps deferred items eligible for the next poll:
   - its end-of-poll `periodic-re-eval` cooldown stamp skips them (R1: nothing recorded against the item);
   - `requeueDeferredDispatch` writes their keys back into `e.mayNeedWork` (directly, under `mayNeedWorkMu`, not through `store.Apply`, so it causes no wake) so the next poll admits them through `cycleSet`.

## Why the re-queue is needed (correction to ADR-039 Consequence 4)

ADR-039 said Deferred-Dispatch items are unaffected by excluding `WorkerLifecycleChanged` from `cycleSetFlags`. That holds for an item deferred by the `snap.Worker() != nil` guard, whose own earlier admission is not what blocked it. It does not hold for a slot-deferred item:

1. The item reached the poll through `cycleSet` (e.g. `StatusChanged` on a freshly released item) and the poll's drain consumed that entry.
2. The end-of-poll stamp would record `periodic-re-eval` for up to `PollSeconds*10`.
3. A worker exit fires `WorkerExited` and wakes the poll, but `cycleSetFlags` excludes `WorkerLifecycleChanged`, so the woken poll finds the item behind an active cooldown and skips it.

The wake alone therefore does not re-admit it. Skipping the stamp alone is not enough either: an item with no prior cooldown falls through `selectDeepFetchCandidates` to `continue` when it is not in `cycleSet`. Hence both the stamp skip and the re-queue.

## Re-admission survives a failed deep-fetch

The poll that re-admits a deferred item does so through `cycleSet` alone (no expired cooldown consumed), and the deferral skipped the `periodic-re-eval` stamp. If that poll's `FetchItemDetails` fails, or `itemMayNeedWork` refuses the item inside the deep-fetch failure backoff, the item used to be dropped with no cooldown and no `cycleSet` entry — stranded until an unrelated change. `selectDeepFetchCandidates` therefore re-arms `periodic-re-eval` at both drop sites for any `cycleSet`-admitted item (previously only when an expiry was consumed). The re-arm interval is the same `githubRecheckInterval` that governs the failure backoff, so a persistent failure stays one fetch per interval rather than one per poll.

## Reinvokes can take a freed slot first

A reinvoke goroutine blocked on `e.sem` takes a freed slot before the poll's non-blocking attempt. A deferred dispatch can be delayed by reinvokes, but is never stranded: its key stays queued and the next poll tries again.

## Alternatives rejected

- **Skip the stamp only.** Starves a cycleSet-admitted item with no prior cooldown, as above.
- **Add `WorkerLifecycleChanged` to `cycleSetFlags`.** Re-opens the #576 wake-loop ADR-039 removed (≈750–1500 wasted GraphQL points per five minutes).
- **A cheap `len==cap` pre-check ahead of the sentinel probe.** Saves one `ps` per saturated poll but changes where R4's sentinel check sits; not worth it.
- **Throttling the log line.** A changing in-use count is a real signal and the volume is one line per poll.

## Consequences

- The poll never waits on a worker; the settle scans and train evaluation run every poll regardless of saturation.
- While slots stay full, each poll re-deep-fetches each deferred item (cache-served when the board cache is fresh). Bounded by the number of deferred items and visible in the log line.
- A wake dropped under GraphQL/REST backoff (#1716) leaves the item to the backed-off ticker, which still re-admits it because its key is queued.
- A merge-train slot release does not fire `WorkerExited` and so does not wake the poll; the next ticker poll covers it.
- Consistent with ADR-2046 (train slots stay on worker goroutines) and ADR-1270 (settle scans are independent of dispatch admission).
