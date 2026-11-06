# ADR 2550: Reconcile ambiguous lock-label writes and reclaim orphaned engine locks at runtime

## Status

Accepted. Amends ADR 2393 (its consequence that a failed lock write "leaves a stray
`fabrik:locked:<id>`; existing stale-lock sweeps remove it" does not hold for `fabrik:editing` in
a running daemon). Mentioned for context: #2533 (the report; this change does not close it),
#552, #1135.

## Context

ADR 2393 made a failed `fabrik:editing` or lock write refuse the dispatch, uncharged, with a short
`lock-write-failed` cooldown, on the premise that a failed write left no label. GitHub can answer a
label write with a 5xx or a timeout and still apply it. When that happens to `fabrik:editing`, the
label is on the issue with no worker behind it, `itemNeedsWorkRetryAt` refuses the item on every
later poll and the only remover is the startup scan. During GitHub's 2026-10-30 incident (0.0.90)
two comments went unprocessed for more than two hours while `fabrik_status` described the item as a
`gate-hold` wait.

## Decisions

1. **Reconcile at the write site (R1).** After a transport-class failure
   (`gh.IsTransportError`) of an engine lock/state label write, `reconcileLabelWrite` re-reads the
   labels once through `liveLabels` (so the read reaches the Store) and the site acts on the
   answer: applied, not applied, or unknown. A 4xx or rate limit is an answer and triggers no read,
   so a healthy GitHub never sees the extra call. Applies to the `fabrik:editing` add
   (`processCommentsClassified`), the stage lock and `in_progress` adds (`acquireLockAndVerify`) and
   the removals (`applyLabelRemove`, `removeEditingLabel` after its retries are exhausted). The
   removal retry loops keep `isTransientError`; the reconcile runs after they give up.
2. **An applied add means proceed as holder, not remove.** The pass continues immediately with the
   label it now holds. Removing it would add a write during an outage and defer the work to a later
   poll. The echo reservation is kept and the Store is written through, so the label is
   indistinguishable from a normally taken one and the existing cleanup removes it. A landed
   `in_progress` marker sets `inProgressAdded` so `release()` removes it. Not applied and unknown
   keep ADR 2393's behaviour exactly.
3. **Runtime orphan reclaim (R2) as the backstop.** An in-memory clock per (item, label) records
   the first poll that saw `fabrik:editing` or the daemon's own `fabrik:locked:<id>` on an item with
   a known label set and no `Worker()`. `settleOrphanedLocks` removes a label older than the grace,
   at most 10 per poll, after re-checking the worker with `Peek` and confirming the label with a
   live read. gh.ErrNotFound is success, as in the startup cleanup. It is skipped while the forge
   hold is engaged or the daemon is draining. A reclaim re-admits the item (R3) with
   `requeueDeferredDispatch` and an already-due `lock-write-failed` cooldown and writes nothing
   else (no counter, breaker, `StageAttempted`, failed or pause label).
4. **One clock for reclaim and visibility (R4).** `internal/attention` gains
   `Input.LockOrphan{Label, Since, Grace}`, filled by the engine from the same clock. A new row
   directly after the worker row classifies a worker-less lock label past the grace as `stalled`
   with code `lock-orphaned`, ahead of the wait labels, the retry row and the gate-hold row. It
   does not wait for `StallThreshold`: the engine defines the grace and the reclaim that follows
   clears the verdict. No new `Cause` is added.
5. **Grace is `max(3m, 3 × PollSeconds)`**, not an operator setting. It exceeds worker-handle
   registration races, a worker that has just exited and replica lag, and scales with a slow poll
   cadence.
6. **Scope.** Reclaim covers `fabrik:editing` and the own lock only. `stage:<name>:in_progress` and
   `fabrik:reworking:*` stay with the startup scans (#1135 context; the rework marker has its own
   mark-first-then-clear invariant); R1 still reconciles `in_progress` writes. No new label, config
   key or `FABRIK_*` variable.
7. **Sim fault.** `simgh.FaultTable.ApplyThenFailWhen` runs the call against the model and then
   returns the injected error; the acceptance twin cannot be written without it. Every other rule
   still fails before the call.

## Consequences

- **Accepted risk: another Fabrik instance's live `fabrik:editing`.** The label carries no owner, so
  a second instance's long comment pass looks orphaned to this daemon once the grace has elapsed.
  Mitigations: the grace, the no-local-worker check, a live confirmation and, for `fabrik:editing`
  only, `FetchLabelAppliedAt` (a label younger than the grace restarts the clock). The consequence
  is a stripped lock, not data loss. The own `fabrik:locked:<id>` is instance-scoped and safe.
- **Stale replica on the re-read** can make R1 answer "not applied" for a landed add; the reclaim
  removes it after the grace.
- **Echo.** If the label landed and the reconcile found it absent or unknown, the later `labeled`
  webhook arrives unreserved and is attributed like any missed echo.
- **Latency.** When the Store learns of the label only through a deep fetch, the worst case is the
  grace plus one periodic re-evaluation.
- The clock is in memory; a restart restarts it and the startup cleanup covers a restart.

## Alternatives rejected

- Recording `unknown` on `lock-write-failed` and having no scan: it cannot catch other causes of an
  orphaned lock.
- Putting the clock in `itemstate`: a persisted-looking field for an engine-local concern.
- Letting the classifier read GitHub: it must stay pure and cache-only.
