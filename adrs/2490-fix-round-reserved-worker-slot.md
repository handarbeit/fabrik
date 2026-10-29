# ADR 2490: Optional reserved worker slots for review, CI-fix and rebase rounds

## Status

Accepted. Follows [ADR 2115](2115-merge-train-reserved-worker-slot.md) (the merge
train's reserved slot), whose shape it copies. ADR 2115 and its slot are unchanged: the
two reservations are separate pools and never borrow from each other. Builds on
[ADR 2121](2121-worker-queue-and-attention-rows.md)'s queued-worker record.

## Context

Review, CI-fix and rebase reinvokes (the "fix rounds", `dispatchReinvoke`) take a slot
from the same `max_concurrent` semaphore as stage runs. When long Implement runs hold
every slot, a short round on a PR that is one push from landing queues behind them.
Community report #2453 (`totalslacker/chronicle`, Oct 1–26) logged slots fully busy for
13.8 h in 4.5 days and a fix round that waited 12 minutes.

Data recorded on the issue, which drives decision 1:

- **Fix-round waits are small.** 3.6 h over 24 occurrences (Review 1.8 h, Validate
  1.8 h), logged only from Oct 22 (about 4.5 of 25 days); under about 20 h for the
  whole month even scaled up generously, over about 200 issues.
- **Most measured starvation is new stage work waiting to start** (26.9 h of logged
  stage-run slot waits, Specify 15.3 h, and an estimated further 33.2 h before Oct 22).
  A fix-round reservation does not touch it.
- **The costly case is a clock problem.** The review-wait timeout fired during the
  12-minute slot wait; #2467 stops that clock, after which a reservation buys minutes,
  not avoided pauses.
- **Our own fleet.** One of five instances (`max_concurrent` 5) ever filled its slots,
  all busy 8.9 h of 329 h (3%). Of its 62 fix rounds, 18 started during or just after a
  full-slot window, and their queueing is at most 2.9 h in total (12 under 4 min, 6 at
  21–27 min). This is an upper bound: the engine logged nothing when a fix round took
  its slot.
- **Headroom is large.** About 16 worker-hours a day used, against 48–72 slot-hours a
  day at `max_concurrent` 2–3 (23–34%), so more slots is the other remedy.

## Decision

1. **Ship the setting off: default `0`.** `fix_round_slots` (flag
   `--fix-round-slots`, env `FABRIK_FIX_ROUND_SLOTS`, YAML `fix_round_slots`; flag over
   env over YAML) defaults to `0`, which is today's behaviour exactly. The default is
   revisited, as a separate change, only if fleet logs show fix rounds queueing behind
   full slots after #2467 has shipped. Decisions 5 and 6 are what make that observable.
   A negative or non-integer value from any source is refused at startup (the
   `max_stage_dispatches` pattern), not defaulted.
2. **A separate channel, `Engine.fixSem`, capacity `fix_round_slots`**, on top of
   `max_concurrent`; nil when the count is 0 and on `NewWithDeps` engines unless a test
   calls `EnableFixRoundSlotsForTest` (the ADR 2115 test convention). Only
   `acquireFixRoundSlot` acquires from it and only `dispatchReinvoke` calls that, so
   stage runs (`dispatchCandidates`, non-blocking on `e.sem`) and comment-review passes
   can never take it. A source-scan test (`TestFixSemReferencedOnlyWhereAllowed`) pins
   this. Rejected: a counter on the shared pool, which cannot say that a stage run may
   not use the reserved capacity.
3. **Acquire order.** The reserved slot first (deterministic, non-blocking), then a
   free ordinary slot, else wait on either, with the existing `ctx.Done()` and SIGHUP
   drain-gate cancel paths and cycle refunds. Reserved-first keeps ordinary slots free
   for stage work. Each acquire takes one slot from one pool and the idempotent release
   returns that same pool's, so no deadlock is introduced (ADR 2115 decision 3).
4. **A count above 1 is allowed.** Up to that many fix rounds then run beyond
   `max_concurrent`, so a host can run `max_concurrent + fix_round_slots` (plus the
   train's slot) Claude processes. No upper bound is enforced.
5. **Accounting.** `in_use` is derived from the Store's worker records, so a round on
   the reserved slot would be counted there and could exceed `max_concurrent`, where the
   clamp would hide it. `WorkerSlotAcquired` therefore carries `Reserved`, recorded on
   `WorkerHandle.ReservedSlot` (the `ConflictResolution.GeneralSlot` precedent); health
   counts such a worker in `fix_in_use`, never `in_use`. The additive `workers` fields
   are `fix_in_use` and `fix_max` (a known zero when disabled), the TUI title appends
   ` + fix N/M` when `fix_round_slots` is set, and the sampled publisher reads
   `len`/`cap` of the channel. The `waiting-for-dispatch` and `retry-starved` no-slot
   checks keep reading only the ordinary pool, because stage runs cannot use the
   reserved one.
6. **Queue-time log.** Each fix round logs one line when it takes its slot,
   including zero waits so the share that waited can be counted:
   `slot acquired: <kind> took <reserved|ordinary> slot after <duration> queued`
   (dispatch to slot held).
7. **Neutralisation seam.** `SetFixRoundReservationDisabledForTest` (per-Engine) makes
   `acquireFixRoundSlot` ignore `fixSem`, so the sim twin shows the same scenario
   waiting (SC-005).

## Consequences

- With the default, nothing changes: no channel, no reserved holder, total item-work
  concurrency never exceeds `max_concurrent`.
- Operators who turn it on get short rounds that start immediately while stage runs hold
  every ordinary slot, at the cost of up to `fix_round_slots` more concurrent Claude
  processes.
- It does not help new stage work waiting for a slot, which the data shows is the main
  source of waiting; raising `max_concurrent` is the remedy for that.
- Slot occupancy is in memory only; restarts are unaffected.
