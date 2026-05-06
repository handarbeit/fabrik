# ADR-040: Push-based unblock via engine-side scan (Variant B)

**Status:** Accepted  
**Issue:** #580  
**Date:** 2026-05-06

## Context

`fabrik:blocked` was only removed by `checkDependencies` inside `processItem`, which only ran when `itemMayNeedWork` admitted the item. `itemMayNeedWork` requires the item's Status to map to a configured stage via `stages.FindStage(...)`. Items in non-stage columns (Backlog, Done, custom columns) never passed this gate and therefore never reached `checkDependencies` — `fabrik:blocked` would remain indefinitely after all their blocking issues closed.

Live evidence confirmed on 2026-05-06: issue #575 stayed labeled `fabrik:blocked` for 30+ minutes after its blocker (#574) auto-merged and closed, because #575 was dragged to Backlog to avoid a wake-loop, so `itemMayNeedWork` returned false for every poll cycle.

Two implementation variants were evaluated:

- **Variant A — Store-side dependent walk**: Extend `IssueClosed` handler in `internal/itemstate/store.go` to walk `s.items` for dependents and emit `BlockedByChanged` for each. Engine observes `BlockedByChanged`.

- **Variant B — Engine-side scan**: A new observer in `engine/observers.go` subscribes to `StateChanged`, filters for close transitions, then scans `store.All()` to find dependents and dispatches label removal.

## Decision

**Implement Variant B (engine-side scan).**

### Rationale

**Store contract:** `applySingleItem` applies one mutation and emits one `Change` for one item. Walking all items to update dependent BlockedBy views inside `applyToItem` would require either (a) holding `s.mu.Lock()` during an O(n) scan — blocking all concurrent readers — or (b) restructuring `applySingleItem` to return multiple `Change` values, breaking its single-item contract. Neither is acceptable.

**Separation of concerns:** The Store is a per-item state container with no knowledge of dependency semantics. Dependency graph reasoning belongs in the engine layer.

**Observer call path is already safe:** `store.Apply` releases `s.mu` before calling `s.notify()`, so observers may safely call `store.Get()` and `store.All()` without deadlock. No structural changes to the Store are required.

**Consistent with existing observer patterns:** `InvocationObserver`, `StageChangeObserver`, and the wake-channel observer all follow the same `StateChanged`-subscription pattern.

**`StateChanged` is already excluded from `wakeChFlags` and `cycleSetFlags`:** Registering `PushUnblockObserver` has no effect on poll-wake behaviour — the label removal is a direct side effect, not a dispatch trigger.

## Implementation

**New types:**

- `PushUnblockObserver` (`engine/observers.go`): subscribes to `StateChanged`; on close events scans `store.All()` for items with `fabrik:blocked` whose all blockers are now closed; dispatches `go o.Remove(owner, repo, number)` for each.

- `removeBlockedIfResolved` (`engine/dependencies.go`): slim helper that removes `fabrik:blocked` without a `*stages.Stage` parameter and without posting comments. Mirrors `removeEditingLabel`: 3 attempts, exponential backoff (`blockedLabelRetryDelay`), `ErrNotFound` idempotency, `cacheImpl.ApplyLabelRemoved` write-through.

**Blocker state resolution preference:** For each blocker Z in X's `BlockedBy` list, `store.Get(Z.Repo, Z.Number)` is preferred over `dep.State` because the store view reflects `IssueClosed` mutations applied since the last board fetch. Fall back to `dep.State == "CLOSED"` only if `store.Get` returns `ErrNotFound`.

**Registration:** `PushUnblockObserver` is registered on `e.store` only (post store-unification; see ADR-038 and `poll.go:316`). It is NOT registered on `cacheImpl` because the shared store already receives all mutations post-unification.

**Concurrency:** Label removal is dispatched on a goroutine inside `OnChange` to avoid blocking the store notification call path. Double-removal races (two blockers closing in rapid succession) are handled correctly — `ErrNotFound` in `removeBlockedIfResolved` is treated as success.

## Consequences

- Items in any column (Backlog, Done, non-stage columns) will have `fabrik:blocked` removed within one poll cycle after their last blocking issue closes — no longer requiring a human to manually remove the label or drag the item to a stage column.

- The existing `dep-blocked` cooldown-retry pull path (`processItem` → `checkDependencies`) is preserved as defense-in-depth for missed `IssueClosed` webhook events.

- `StateChanged` events for issue closes will invoke `PushUnblockObserver.OnChange` for every close on the board. For each close, the observer scans all ~130 items typically held in the store (O(n) scan). This is cheap — the scan is a simple in-memory slice iteration with no I/O.

- `BlockedByChanged` (already present in `change.go`) is not used by the push path. Variant B subscribes to `StateChanged` on the blocker; it does not rely on or emit `BlockedByChanged`.

## Cross-references

- ADR-038 (`038-dual-store-observer-wiring.md`): documented mandatory dual-store registration for `wakeChFlag` observers. `PushUnblockObserver` is NOT a `wakeChFlag` observer and registers on `e.store` only, consistent with the post-unification comment at `poll.go:316`. ADR-038 is effectively superseded by store unification for `wakeChFlag` observers; its guidance does not apply to `StateChanged` observers.

- ADR-039 (`039-cycleset-excludes-worker-lifecycle.md`): `cycleSetFlags` excludes `WorkerLifecycleChanged`. `PushUnblockObserver` subscribes independently to `StateChanged` and never goes through `wakeChFlags` or `cycleSetFlags`.

- Issue #540, #549, #569: prior push-based observer introductions (Layer 1 status fetch, worker exit, CI check runs). This ADR follows the same pattern.
