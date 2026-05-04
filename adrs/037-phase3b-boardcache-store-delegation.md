# ADR 037 — Phase 3-B: CacheImpl Delegates to itemstate.Store

**Status:** Accepted (2026-05-04)
**Builds on:** ADR 036 (Reactive Cache with Single State Owner)

## Context

ADR 036 introduced `itemstate.Store` as the single owner of per-item state. Phase 3-B wires it into `boardcache.CacheImpl`: CacheImpl drops its own `items`, `deepFetched`, `shaToKey`, and `itemIDToKey` fields and delegates all state reads/writes to the Store.

Three non-obvious constraints shaped the implementation. They are recorded here because violating any one silently reintroduces the cache-coherency failures ADR 036 was written to fix.

## Decisions

### 1. Lock-ordering invariant: never hold `c.mu` during any Store call

`CacheImpl` has its own mutex (`c.mu`) protecting CacheImpl-local fields. `Store` has an independent internal mutex. The invariant is:

> `c.mu` **must be released before any call to** `store.Apply`, `store.Get`, `store.All`, `store.Remove`, or `store.Reset`.

Rationale: Store observers (registered via `store.Subscribe`) may call back into CacheImpl methods. If `c.mu` is held when the observer fires, the callback deadlocks. Because observers run outside the Store's own lock (by design in `Store.notify`), the only way to prevent deadlock is to ensure CacheImpl never holds its lock into a Store call.

Pattern in practice:
```go
// Correct
c.mu.Lock()
key := c.prNumToKey[prKey(repo, prNum)]
c.mu.Unlock()
c.store.Apply(SomeMutation{...}) // no lock held

// Wrong — deadlocks if an observer calls back into CacheImpl
c.mu.Lock()
defer c.mu.Unlock()
c.store.Apply(SomeMutation{...}) // MUST NOT hold c.mu here
```

### 2. CacheImpl/Store field split — what stays on CacheImpl

Five field families remain on CacheImpl after Phase 3-B:

| Field | Reason it stays |
|-------|----------------|
| `paused bool` | Control flow only; no state semantics |
| `recentMissCache map[string]time.Time` | Negative cache for REST calls; purely CacheImpl policy |
| `checkRuns map[string][]gh.CheckRun` | Holds check runs for pre-linkage SHAs that Store silently drops |
| `linkedPRs map[string]*gh.PRDetails` | `gh.PRDetails` has `Title`, `State`, `Merged`, `Draft`; `LinkedPRState` does not — migration deferred until `LinkedPRState` gains those fields |
| `prNumToKey map[string]string` | PR-number → issue key index; not in Store because it is a CacheImpl routing concern, not item state |
| `localDeltaAt map[string]time.Time` | Webhook-driven `UpdatedAt` override (see Decision 3) |

**`linkedPRs` is the most important deferred migration.** A `// TODO(phase3-x)` comment marks the site. Do not silently remove it without also adding `Title`/`State`/`Merged`/`Draft` to `LinkedPRState` and migrating the callers.

**`prNumToKey`** must be kept consistent with `Store` state. Every code path that learns of a PR-number → issue mapping (auto-heal in PR-delta, PR-review, PR-review-comment, and check-run handlers) must update both `c.prNumToKey` and the Store.

### 3. `localDeltaAt` — webhook-driven UpdatedAt bumping

`ItemState.UpdatedAt` mirrors GitHub's server-side timestamp. Webhook deltas arrive before GitHub updates that timestamp, so `FetchProjectBoard` would return a stale `UpdatedAt` for an item that was just modified — causing `itemMayNeedWork` to miss it.

`c.localDeltaAt[key]` records the wall-clock time of the most recent webhook delta for an item. `FetchProjectBoard` overrides `UpdatedAt` at reconstruction time:

```go
updatedAt := snap.State().UpdatedAt
if local, ok := c.localDeltaAt[key]; ok && local.After(updatedAt) {
    updatedAt = local
}
```

`localDeltaAt` is not in the Store because `UpdatedAt` is meant to be the GitHub timestamp; adding a separate `DeltaUpdatedAt` field to `ItemState` would be premature.

## Consequences

- All per-item state reads and writes now flow through `itemstate.Store`. CacheImpl is a routing and policy layer, not a state owner.
- The lock-ordering invariant is the key invariant to maintain. Any new code path that holds `c.mu` while touching Store state will introduce a latent deadlock.
- `localDeltaAt` entries accumulate forever for items that receive webhook deltas. This is acceptable for the expected cardinality (tens to hundreds of items per board). If boards grow large, a TTL eviction pass could be added.
- The `linkedPRs` deferred migration creates a split where LinkedPR *routing* state lives in Store (via `LinkedPRState`) but *display* state lives in CacheImpl. This is intentional and documented with a TODO comment.
