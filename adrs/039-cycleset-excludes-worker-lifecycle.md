# ADR-039: cycleSetFlags excludes WorkerLifecycleChanged

**Status:** Accepted  
**Issue:** #576 (Fix B)  
**Date:** 2026-05-06

## Context

`newMayNeedWorkObserver` populates `Engine.mayNeedWork` (the cycleSet) — a set of item keys that bypass the cooldown pre-filter in the next poll cycle. Prior to this change, the observer used `wakeChFlags` as its filter, which includes `WorkerLifecycleChanged`.

`WorkerLifecycleChanged` is emitted by both `WorkerEntered` and `WorkerExited`. Including it in the cycleSet filter caused items to bypass the cooldown gate whenever a goroutine entered or exited — regardless of whether the goroutine did any useful work.

For items that return early from `processItem` without invoking Claude (e.g. dep-blocked items), this produced a tight wake-loop:

1. `processItem` early-returns (dep-blocked) → `WorkerExited` fires
2. `WorkerLifecycleChanged` → item added to `mayNeedWork` (cycleSet bypass)
3. Next poll: item bypasses cooldown gate → deep-fetched → passes `itemNeedsWork` (no pre-dispatch gate yet) → goroutine launched
4. Repeat from step 1, at 2-3 Hz

Live observation on issue #576 measured ~750-1500 wasted GraphQL points per 5-minute blocking window.

## Decision

Split `wakeChFlags` into two constants:

- **`wakeChFlags`** (unchanged): used by `newWakeChObserver` for the wake channel. Retains `WorkerLifecycleChanged` so the poll loop wakes immediately on goroutine entry/exit.
- **`cycleSetFlags`** (`wakeChFlags &^ WorkerLifecycleChanged`): used by `newMayNeedWorkObserver` for the cycleSet. Excludes `WorkerLifecycleChanged` so early-return goroutine exits do not bypass the cooldown gate.

```go
const cycleSetFlags = wakeChFlags &^ itemstate.WorkerLifecycleChanged
```

## Rationale

**Why exclude WorkerLifecycleChanged from cycleSetFlags?**

A `WorkerExited` from an early-return path carries no new information about the item. The item's labels, status, and comments are unchanged. Adding it to the cycleSet allows it to bypass the cooldown gate — but the cooldown exists precisely to rate-limit re-evaluation for items that aren't ready to proceed. Bypassing it for no-work exits defeats the purpose.

**Why retain WorkerLifecycleChanged in wakeChFlags?**

When a goroutine that *did* useful work exits (stage complete, comment processed), the poll loop should re-evaluate the item promptly. The wake channel triggers an immediate poll cycle; if the item passes `itemNeedsWork` it will be dispatched. This prompt re-evaluation is correct behavior.

**Why not just remove WorkerLifecycleChanged from wakeChFlags entirely?**

Without `WorkerLifecycleChanged` in `wakeChFlags`, the poll loop would not wake immediately on goroutine exit for items that completed useful work. These items would have to wait for the next scheduled ticker poll (up to `PollSeconds`). This would slow stage-to-stage progression.

**Defense-in-depth relationship with Fix A:**

Fix A (pre-dispatch gate for `fabrik:blocked` in `itemNeedsWork`) eliminates the wake-loop by preventing goroutine launch entirely after the label is set. Fix B ensures that even if a pre-dispatch gate is missing for some future early-return label, `WorkerExited` cannot self-perpetuate a dispatch loop by bypassing the cooldown gate. The two fixes are complementary, not redundant.

## Consequences

1. **Future `newMayNeedWorkObserver` callers must use `cycleSetFlags`, not `wakeChFlags`.** If `wakeChFlags` is used instead, `WorkerLifecycleChanged` will re-enter the cycleSet and restore the wake-loop behavior.

2. **Items that early-return from `processItem` are now re-evaluated via cooldown expiry** (`CooldownAt["dep-blocked"]` etc.) or genuinely new events (`LabelsChanged`, `StatusChanged`). They are not re-evaluated on every goroutine exit.

3. **Self-advance (completed stage → next stage) is unaffected.** Stage completion fires `StatusChanged` (via `handleStageComplete` → `advanceToNextStage`), which is in both `wakeChFlags` and `cycleSetFlags`. Completed items still bypass the cooldown gate and are re-dispatched promptly.

4. **Deferred-Dispatch items are unaffected.** When a prior worker exits normally, it fires `StatusChanged` or the item's own cooldown was already bypassed earlier in the goroutine's lifecycle. See §9.9 in `docs/state-machine.md` for details.

## References

- Issue #576 (root cause analysis and fix)
- ADR-038 (observer wiring — the single-store architecture this change builds on)
- `docs/state-machine.md` §9.2, §9.9
