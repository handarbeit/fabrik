# ADR 1557: Bounded Lifetime of Pending-Eject Signals

**Date**: 2026-10-15
**Status**: Accepted
**Issue**: #1557 — merge-train: unconsumed pending-review-eject signals survive the worker's early-return paths into later batches
**Amends**: ADR-1208 §6 (the pending-eject signal); also covers the #1863 comment sibling (ADR-1863)

## Context

ADR-1208 lets `settleQueuedReviewFindings` (poll goroutine) hand an eject decision to a live merge-train
run through a mutex-guarded signal (`markPendingReviewEject`), which the run consumes at its own
checkpoints (`applyPendingReviewEjects`). #1863 added a parallel signal for unprocessed human comments
(`markPendingCommentEject`).

Signals were only ever *consumed* at checkpoints. A run that ends without passing a checkpoint after
the signal was marked — a red singleton rerouted by `ejectRedSingleton`, a green landing, the runaway
guard, an assembly failure, a cancelled step, a member ejected for another cause but left Queued —
left the entry in `queuedReviewEjects` / `queuedCommentEjects`. A later run containing the same issue
number consumed it at its first checkpoint and ejected a member for a finding that no longer
existed: an unexplained member disappearance.

Since #2051 the "worker" is a *run* (persisted, spanning many polls, ended by `trainRun.finish()` /
`finishTrain`), but the defect is the same. Research also found a race that a plain drain-on-exit does
not close: the scan reads the live run's batch (`mergeTrainBatchMembers`), deep-fetches each member
over the network, and only then marks. A step goroutine can finish the run, and drain, in that
window; the late mark then lands in an empty map and leaks.

## Decision

1. **Stamp on mark, check on take (the correctness rule).** Each signal records the run it was raised
   for: the `*mergeTrainWorkerState` stored in `mergeTrainInFlight[trainKey]`. That pointer is the
   run's identity — created at dispatch or restart adoption, shared by every step goroutine on both
   drivers, gone when the claim is released. `settleQueuedReviewFindings` reads it once per partition
   group (`mergeTrainRunState`) and passes it to both mark functions. Every checkpoint passes the calling
   run's state to `applyPendingReviewEjects`; `takePendingReviewEjectFor` / `takePendingCommentEjectFor`
   act only on a matching stamp and otherwise discard the entry and log it. Because the check is at the
   consumer, it holds for every exit path, including ones added later (R3), and a late mark carrying a
   finished run's stamp is inert (the race above).
2. **Drain on release (hygiene).** `releaseTrainClaim` — called by `finishTrain` and by the two
   adoption-failure paths that used to `mergeTrainInFlight.Delete` directly — first purges every signal
   stamped with the releasing run, under `queuedReviewEjectsMu`, then deletes the claim. Signals of
   another partition's run (ADR-1648: the maps stay keyed by bare `owner/repo`) are untouched.
3. **Both signals.** The comment signal shares the mutex, the lifetime and the consumers, so it gets the
   same stamp and drain.
4. **Dropped, not re-evaluated.** A stale signal is discarded rather than re-applied to the new run.
   This is safe — R2 — because the scan is level-triggered: every poll it re-evaluates each Queued
   member and re-raises the signal against whichever run is live then (or ejects the member directly
   when no run claims it). A still-unresolved finding therefore costs at most one poll of latency and
   is never lost; ADR-1208's within-one-batch-cycle guarantee stands. A discarded signal causes no
   ejection, no `MaxMergeTrainEjections` count, no pause and no `ReviewCycles` change (FR-004).
5. **Sites that just rerouted the member** (`deferMember`, `rerouteInvalidatedMember`) keep the
   stamp-agnostic `takePendingReviewEject` / `takePendingCommentEject`: the member has left Queued, so
   any signal for it, whatever its owner, is moot.

## Alternatives considered

- **Drain on exit only (the issue's option 1).** Leaves the scan-vs-finish race: the late mark survives
  the drain. Kept as the hygiene half of the decision.
- **Stamp only.** Correct, but leaves dead entries in the maps until overwritten; the drain tidies them.
- **Re-derive findings at each checkpoint (option 3).** Adds a network read to the run's checkpoints and
  removes the poll/run separation ADR-1208 deliberately built.

## Consequences

- `markPending*Eject` and `applyPendingReviewEjects*` gained an owner/state parameter; tests that
  exercise a run register the state first and stamp with it.
- A signal raised for a run is acted on only by that run. After a restart the maps are empty anyway
  (in-memory only), and detection re-raises from live review state.
- Not changed (deferred): the scan still decides pending-vs-direct from the run's immutable
  `batchNumbers`, so a member ejected from a run but left Queued waits for the next run's re-raise —
  now bounded to that run's duration by the drain. The scan also still clears only the comment signal,
  not the review signal, for an in-batch member with no findings; a symmetric clear must not fire when
  `queuedReviewFindings` degraded on a failed review read, and R1 does not need it.
- Tests: `engine/merge_train_pending_eject_lifetime_test.go` — the stale-signal cases fail against the
  pre-change code (AC1/AC3), paired with an unresolved-finding case that is still ejected by the next
  run (AC2).

See `docs/state-machine.md` §6.16 "Signal lifetime".
