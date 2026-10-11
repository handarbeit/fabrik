# ADR 2115: Merge train gets one reserved worker slot on top of `max_concurrent`

## Status

Accepted. Amends decision 5 of
[ADR 2046](2046-merge-train-slot-only-around-claude-calls.md) ("No `merge_train_slots`
key" / no dedicated pool). Decisions 1–4 of ADR 2046 (bracket only the Claude call,
visible waits, cancellation is "not attempted", suspension re-check) are unchanged.

## Context

ADR 2046 declined a dedicated train pool "for no demonstrated need" and said to file
one if saturation delayed conflict resolution in practice. It has: with every
`max_concurrent` slot held by stage workers the train waits for a Claude call, Queued
members stop landing, their PRs go stale, more queue up and each later trial carries
more reconciliation and CI. Report #2037 proposed a dedicated train slot.

## Decision

1. **A second semaphore, `Engine.trainSem`, capacity 1**, created by `New()` only when
   `merge_train` is `on` (`trainSlotCapacity`). With `merge_train` off it is nil and
   nothing changes. With it on, the host can run `max_concurrent + 1` Claude processes.
   A separate channel, rather than a larger shared pool, makes "stage work can never
   take the reserved slot" structural: only `acquireTrainSlotEp` references `trainSem`
   (pinned by a source scan test).
2. **Acquire order (R2).** A train tries the reserved slot first (deterministic,
   non-blocking), then a *free general slot*, and only if both are busy announces
   `waiting for slot` and blocks on either. A train therefore never waits on the
   general pool while the reserved slot is free, and stage work is never blocked by the
   reserved slot. Rejected alternative: reserved-only. A second (repo, base) partition
   would then queue behind the first train's Claude call, which can run up to the
   30-minute conflict-resolution cap, even with general slots free — slower than before
   this change. The accepted cost is that a second concurrent train can take a general
   slot, which is exactly the pre-#2115 behaviour.
3. **No new deadlock.** Each acquire takes exactly one slot from exactly one pool and
   the release closure (still `sync.Once`) returns that same pool's slot; a train never
   holds one pool while waiting on the other.
4. **R4: auto-repair Validate does not get the reserved slot.** #2045's repair re-enters
   Validate as an ordinary stage dispatch on the general pool. It is stage work, R1
   forbids stage dispatch from taking the slot, and a long Claude stage on the slot
   that exists to keep short train calls fast would defeat it. Nothing shows starving
   it stalls the train: auto-repair has the `fabrik:revalidate` retry fallback and a
   per-member cap. If measurement later shows a stall, a narrow carve-out can be filed.
5. **Visibility.** The local API's `workers` result gains additive `train_in_use` and
   `train_max` (known zero when `merge_train` is off; `in_use` stays item workers
   only, so it can read lower than the TUI's general-pool figure, which is `len(e.sem)` and
   also counts reinvoke runs and a train that fell back to a general slot). The TUI In Progress title shows `slots 5/5 + train 1/1`, fed by a sampled
   publisher (`WorkerSlotsEvent`, once a second, on change) rather than hooks at
   acquire/release, which keeps the dispatch code untouched. Occupancy is `len`/`cap` of
   the channels.
6. **Fixed count.** There is still no `merge_train_slots` key; one is enough and can
   become configurable if measured.
7. **Test convention.** `NewWithDeps` engines get no reserved slot unless a test calls
   `EnableTrainReservedSlotForTest`, so the #2046 slot tests (which fill `e.sem` inline
   and expect a real wait) run unchanged; the sim merge-train env enables it to match
   production.

## Consequences

- A saturated stage pool can no longer starve train conflict resolution.
- Hosts with `merge_train` on can run one more concurrent Claude process.
- Restarts, persisted run state and every train decision are unchanged; slot occupancy
  is in memory only.
