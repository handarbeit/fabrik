# ADR-038: Dual-Store Observer Wiring

**Status**: Accepted  
**Date**: 2026-05-04  
**Issue**: #520 (Phase 3-H: reactive observer plumbing)  
**Supersedes**: N/A  
**See also**: ADR-036, ADR-037

---

## Context

Phase 3-A through 3-G migrated all per-item engine state into `itemstate.Store` and implemented a fully-functional `Store.Subscribe` observer mechanism. Phase 3-H is the final step: wiring observers into the engine to replace polling-based change detection.

The engine has two separate `*itemstate.Store` instances:

| Store | Owner | Holds |
|-------|-------|-------|
| `engine.store` | `Engine` struct | Locks, invocations, stage state, cooldowns, workers, deep-fetch failures |
| `cacheImpl.store` | `boardcache.CacheImpl` (private) | GitHub board state: status, labels, comments, PR reviews, check runs |

These stores are never unified in Phase 3. (Store consolidation is deferred; the two-store architecture is an artifact of the phased migration in ADR-036.)

The `wakeChFlags` bitmask covers changes that mean "this item may need dispatch":

```
wakeChFlags = StatusChanged | LabelsChanged | CommentsChanged | LockChanged | LinkedPRChanged
```

These flags span both stores:
- `StatusChanged`, `LabelsChanged`, `CommentsChanged`, `LinkedPRChanged` → `cacheImpl.store`
- `LockChanged` → `engine.store`

Any observer that reacts to `wakeChFlags` must be registered on **both** stores to receive all relevant changes.

---

## Decision

### 1. Mandatory dual registration for wakeChFlag observers

The `wakeChObserver` and `mayNeedWorkObserver` are registered on both `engine.store` and `cacheImpl.store`. This is enforced by convention (not by the type system) — a code comment in `Engine.Run()` documents the requirement.

### 2. CacheImpl exposes Subscribe

`boardcache.CacheImpl` gains a new exported method:

```go
func (c *CacheImpl) Subscribe(o itemstate.Observer) func() {
    return c.store.Subscribe(o)
}
```

This is the only mechanism through which engine code registers observers on the cache store. The field `c.store` remains private; no other access path is added.

### 3. CacheImpl exposes SubscribePause for pause/resume notifications

`CacheImpl.Pause()` and `CacheImpl.Resume()` are the only mutation points for the `c.paused` field. A separate observer list for pause transitions is added:

```go
type CacheImpl struct {
    ...
    pauseObsMu  sync.Mutex
    pauseObservers []func(bool) // called with paused=true on Pause(), paused=false on Resume()
}
```

`SubscribePause(fn func(bool)) func()` adds to this list and returns an unsubscribe func that nil-slots the entry (to avoid index shifting). `Pause()` and `Resume()` snapshot the observer list before releasing `c.mu`, then call observers on the snapshot outside any lock — mirroring `Store`'s captureObservers pattern.

**Invariant**: Pause observers MUST NOT call back into `CacheImpl` methods that acquire `c.mu`. Violation causes deadlock.

### 4. Observer-type-to-store mapping

| Observer | Store(s) registered on | Rationale |
|----------|------------------------|-----------|
| `wakeChObserver` | both | `LockChanged` from engine.store; others from cacheImpl.store |
| `mayNeedWorkObserver` | both | same as above |
| `InvocationObserver` | `engine.store` only | `InvocationChanged` is produced exclusively by `InvocationRecorded` on engine.store |
| `StageChangeObserver` | `cacheImpl.store` only | `StatusChanged` is produced by board reconcile and webhook deltas on cacheImpl.store |
| Pause observer | `CacheImpl.SubscribePause` | Not a Store observer; separate mechanism for paused-field transitions |

### 5. mayNeedWork replaces seenUpdatedAt

`Engine.seenUpdatedAt map[string]time.Time` is removed. `Engine.mayNeedWork map[string]bool` (protected by `Engine.mayNeedWorkMu`, a separate mutex) replaces it. The `mayNeedWorkObserver` populates the set when any `wakeChFlag` change fires. Each poll cycle drains the set to a local `cycleSet`; the dispatch pre-filter uses `cycleSet` instead of timestamp comparison.

Bypass conditions that skip the `cycleSet` check (items always eligible regardless of observer state):
- Cleanup stages (`CleanupWorktree: true`)
- Items with `fabrik:awaiting-ci` or `fabrik:rebase-needed` labels
- Items with an expired `CooldownAt` entry (periodic re-evaluation)

### 6. InvocationObserver carries IsComment via data model

`JobCompletedEvent.IsComment` distinguishes stage invocations from comment/reinvoke dispatches. The observer cannot determine this from the Snapshot alone — `IsComment` is not a board-visible attribute. The field is propagated through the data model:

- `InvocationRecorded.IsComment bool` (new field in mutation.go)
- `ItemState.LastInvocationIsComment bool` (new field in itemstate.go)
- Applied in `store.go`'s `InvocationRecorded` case

All call sites of `InvocationRecorded` supply an explicit `IsComment` value.

### 7. StageModel injected at observer construction

`JobCompletedEvent.StageModel` comes from stage configuration, not item state. `InvocationObserver` receives `[]*stages.Stage` at construction time and calls `stages.FindStage(stages, snap.State().Status).Model` at fire time. This avoids storing model names in `ItemState`.

---

## Consequences

**Positive:**
- The poll loop no longer re-evaluates every board item every cycle. Only items with confirmed state changes (or bypass conditions) proceed to deep-fetch.
- `JobCompletedEvent` is emitted by a single authoritative observer, eliminating four ad-hoc emission sites.
- `StageChangedEvent` (new) lets the TUI reactively update the displayed stage without waiting for the next poll.
- `wakeCh` is no longer fired unconditionally on every webhook event — only events carrying `wakeChFlags` wake the poll loop.

**Negative / Tradeoffs:**
- Any new observer added in future must explicitly register on both stores if it cares about cross-store flags. This is a documentation requirement, not a type-system enforcement.
- The `mayNeedWork` set can grow unboundedly if the poll loop never drains it (e.g., if `Run()` is never called). In practice this is not a concern since draining is the first act of each `poll()` call.
- `SubscribePause` uses nil-slot unsubscribe (not index shifting) to avoid iterator invalidation. This means the `pauseObservers` slice can contain nil entries; callers iterate with nil checks.

**Non-decisions (out of scope):**
- Store consolidation (single store for all state). Deferred to a future phase.
- Sender-filter suppression for self-feedback webhook events (see §7.8 of state-machine.md).
- Phase 4 audit of remaining downstream readers that could be converted to observers.
