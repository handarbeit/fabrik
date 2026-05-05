# Phase 4 — Downstream Reader Audit

**Date:** 2026-05-04 against commit `036fbbf`. **Read this after** `01-state-inventory.md`, `02-design.md`, and `adrs/036-reactive-cache-single-owner.md`.

## §0 Executive summary

Phase 3 substantially landed the architecture from `02-design.md` across 8 incremental PRs (#521–#535) plus several follow-up fixes. The 25+ overlapping engine state structures from `01-state-inventory.md` are now consolidated. The `processedSet` dual-purpose foot-gun is split. Webhook delta coverage is complete. WorkerHandle + heartbeats give crash recovery. Reactive observers replace per-poll re-evaluation.

What landed differs from the design in one significant way: there are **two `itemstate.Store` instances**, not one — `engine.store` for engine-perspective state and `cacheImpl.store` for GitHub-perspective state. The unification is at the *struct* level (both use `ItemState`) but not at the *instance* level. This is a deviation worth understanding, and it produces a small set of foot-guns and a few residual cleanups, captured as Phase 5 issues below.

The reactive plumbing (wakeCh, mayNeedWork set, TUI events) is wired correctly through observers on both stores. Most downstream readers correctly route to whichever store owns the data they need.

**Phase 5 backlog (filed as separate issues):**

- **F1**: Remove redundant `e.inFlight` sync.Map; reads should go through `ItemState.Worker` on `engine.store` (single source of truth).
- **F2**: Finish migrating `boardcache.linkedPRs` and `prNumToKey` into Store. (TODO present in code at `boardcache/boardcache.go:207`.)
- **F3**: Decide and document the two-store split — either unify or make the split explicit with API guards (so `e.store.Get(...).Status()` can't return a stale-by-design empty value).
- **F4**: Audit `boardcache.checkRuns` retention strategy; the comment at `boardcache/boardcache.go:200-203` notes Store silently drops `CheckRunCompleted` for unlinked SHAs and CacheImpl mirrors them. Validate this is correct or move check-run state into Store-level negative-cache + per-item.
- **F5**: ~~Tighten observer-registration discipline~~ — **Superseded by F3** (PR #538, issue #537). Post-Phase-4, F3 unified `engine.store` and `cacheImpl.store` into a single shared `*itemstate.Store` instance. With only one store, there is no "which store" registration choice to make; the mis-registration risk is structurally eliminated.

Severity: F1 is a duplication that's likely benign in practice (both maps tracked together) but it's the exact "double source of truth" that motivated this refactor and should be eliminated. F2/F3/F4 are technical-debt cleanups. F5 is superseded (resolved structurally by F3's store unification).

The architecture is in a much, much better place than 24 hours ago. The remaining items don't block re-enabling the cache+webhooks in `dev` config — they're follow-up improvements.

## §1 What landed (vs. 01-state-inventory.md catalog)

### 1.1 Engine state migrations — VERIFIED

The 25-map inventory from §1 of `01-state-inventory.md` has been substantially consolidated. Current `engine/engine.go:60-91`:

| Old structure | Migration status | New home |
|---|---|---|
| `lockedIssues` | ✅ Removed from Engine | `engine.store`'s `ItemState.Lock` |
| `lastUsage`, `lastCompleted`, `lastBlocked` | ✅ Removed | `ItemState.LastInvocation*` |
| `lastUpdatedAt` | ✅ Removed; replaced by `mayNeedWork` observer | n/a |
| `deepFetchFailureTime` | ✅ Removed | `ItemState.LastDeepFetchFailureAt` |
| `prHasHadChecks` | ✅ Removed | `ItemState.LinkedPR.HasHadChecks` |
| `ciMergePendingSince` | ✅ Removed | `ItemState.LinkedPR.CIMergePendingSince` |
| `processedSet` (DUAL-PURPOSE) | ✅ **Split** | `StageState.LastAttemptAt[stageName]` + `CooldownAt[reason]` |
| `retryCount` | ✅ Removed | `StageState.Attempts[stageName]` |
| `pausedDueToRetries` | ✅ Removed | `StageState.PausedByEngine[stageName]` |
| `reviewCycleCount`, `ciFixCycleCount`, `rebaseCycleCount` | ✅ Removed | `StageState.{Review,CIFix,Rebase}Cycles[stageName]` |
| `cloneInFlight`, `baseBranchWarnedSet` | ✅ Stay (per-repo / per-issue+branch, not per-item) | n/a — still on Engine |
| `inFlight (sync.Map)` | ⚠️ **Still exists** despite WorkerHandle migration | Duplicated with `ItemState.Worker` — see F1 |
| `idleCount`, `idleStart`, `totalTokens`, `lastReportedCost`, `sem`, `wg` | ✅ Stay (engine-global) | n/a — still on Engine |

Plus new fields:

| Field | Purpose |
|---|---|
| `store *itemstate.Store` | Per-item engine-state owner |
| `mayNeedWork map[string]bool` + `mayNeedWorkMu` | Observer-maintained dispatch set (replaces poll-time re-evaluation) |
| `seededRepos map[string]bool` | Guard against re-seeding labels on every poll |
| `heartbeatIntervalOverride time.Duration` | Test-only override for WorkerHandle heartbeats |

### 1.2 Boardcache state migrations

`boardcache/boardcache.go:177-224`:

| Old field | Migration status | New home |
|---|---|---|
| `items map[string]*ProjectItem` | ✅ Removed | `cacheImpl.store` (the per-item state) |
| `deepFetched map[string]bool` | ✅ Removed | Derived from `LastDeepFetchAt` on ItemState |
| `shaToKey map[string]string` | ✅ Removed | Owned by `cacheImpl.store` (derived index) |
| `itemIDToKey map[string]string` | ✅ Removed | Same |
| `recentMissCache` | ✅ Stays (negative cache, not per-item) | n/a |
| `paused bool` | ✅ Stays (cache-level health flag) | n/a |
| `linkedPRs map[string]*PRDetails` | ⚠️ **TODO** at `boardcache.go:207` to migrate when LinkedPRState gains Title/State/Merged/Draft | F2 |
| `prNumToKey map[string]string` | ⚠️ Stays alongside `linkedPRs` for now | F2 |
| `checkRuns map[string][]CheckRun` | ⚠️ Stays for SHA-keyed pre-linkage | F4 |
| `localDeltaAt map[string]time.Time` | New addition | Used by `FetchProjectBoard` to surface webhook-driven changes before next Reconcile |
| `pauseObservers []func(bool)` | New | Observer fan-out for cache health |

### 1.3 The two-store reality

The largest deviation from `02-design.md`: **there are two `*itemstate.Store` instances**, both constructed via `itemstate.NewStore(nil)`:

- `engine.store` (`engine/engine.go:70`) — holds per-item state from the engine's perspective (Lock, Worker, StageState, CooldownAt, LastInvocation*, LastDeepFetchFailureAt, LastTokenUsage, partial LinkedPR with engine-managed fields like HasHadChecks).
- `cacheImpl.store` (`boardcache/boardcache.go:220`) — holds per-item state from GitHub's perspective (Status, Labels, Comments, UpdatedAt, full LinkedPR fields, Title, Body, etc.).

Both stores share the same `ItemState` struct definition. **Each store populates only the subset of fields its mutations cover.** Neither store sees the other's mutations.

This is workable but creates two implicit rules every reader has to know:

- **GitHub-perspective fields** (Status, Labels, full LinkedPR, Comments, UpdatedAt, Title, Body) come from `cacheImpl.store`. Reads via `e.readClient.FetchProjectBoard()` / `FetchItemDetails()` route through the cache. Direct `cacheImpl.Get(...)` is also possible but isn't typically used outside of boardcache internals.
- **Engine-perspective fields** (Lock, Worker, StageState, CooldownAt, LastInvocation*, LastDeepFetchFailureAt, partial LinkedPR with HasHadChecks) come from `engine.store`. Reads via `e.store.Get(repo, number)`.

When a reader violates the implicit rule (e.g. `e.store.Get(...).Item().Status` — reading a GitHub field from the engine store), they get a zero-value silently. No compile-time or runtime warning. **This is the residual foot-gun.**

In practice, the existing code routes correctly. The reads I audited (§3) all use the right store for the field they want. But the discipline depends on developer knowledge, not enforcement.

The honest options:

1. **Unify into one Store.** Both engine and cacheImpl share a single Store instance (engine creates it, passes it to cacheImpl). Mutations flow through one Apply. Field ownership is by mutation type, not by store identity. This is the design as originally written.
2. **Keep two stores, harden the API.** Two stores stay, but each exposes a *typed read* API: `engine.store.GetEngineState(...)` returns a struct with only engine fields; `cacheImpl.GetGitHubState(...)` returns only GitHub fields. Any "I want a unified view" reader composes from both.
3. **Accept the convention.** Current behaviour. Document the read-routing rules and trust developers.

Phase 5 issue F3 captures the choice.

## §2 Reactive plumbing — verified correct

`engine/poll.go:310-360` is the observer-registration site. The wiring:

- **wakeChObserver** — registered on **both stores** (`e.store.Subscribe(wakeObs)` and `cacheImpl.Subscribe(wakeObs)`). Fires non-blocking wakeCh on relevant Change flags (Status, Labels, Lock, Comments, etc.). Correctly picks up changes from either side.
- **mayNeedWorkObserver** — registered on **both stores**. Populates `e.mayNeedWork` set with iKeys for items that have changed since the last cycle drain.
- **InvocationObserver** — registered on `engine.store` only (InvocationRecorded events fire only there). Emits `tui.JobCompletedEvent`.
- **StageChangeObserver** — registered on `cacheImpl.store` only (StatusChanged from reconcile/webhook fires only there). Emits `tui.StageChangedEvent`.
- **WebhookHealthObserver** — registered via `cacheImpl.SubscribePause(...)`. Fires `tui.WebhookStatusEvent` on cache pause/resume transitions.

The mayNeedWork drain pattern (`engine/poll.go:823-831`) atomically swaps the map with a fresh empty one under lock — no missed events, no double-processing.

**F5 superseded:** The dual-store registration concern captured as Phase 5 F5 is no longer applicable. F3 (PR #538, issue #537) unified `engine.store` and `cacheImpl.store` into a single shared `*itemstate.Store` instance. All Store observers now register on `e.store`, which is the same instance held by `cacheImpl`. There is no "which store" choice; the mis-registration risk is structurally eliminated. (`SubscribePause`, used by `WebhookHealthObserver`, is a separate CacheImpl mechanism unrelated to Store observer registration.)

## §3 Per-reader audit findings

I walked the engine and boardcache code paths that read item state. Each is classified by which store it routes to and whether the routing is correct.

### 3.1 Dispatch loop (`engine/poll.go:1130–1180`)

Reads the dispatch candidate list, checks lock/worker, dispatches if free. Routes:

- `e.inFlight.Load(iKeyDbg)` (line 1149) — **F1 finding**. This is the only remaining read of `e.inFlight`. Should be replaced with `e.store.Get(...).Item().Worker != nil`.
- Everything else routes through the Snapshot API correctly.

### 3.2 itemMayNeedWork (`engine/item.go:108–140`)

Reads the `mayNeedWork` set and falls back to direct snapshot reads for cooldown checks:

- Reads `CooldownAt` and `LastDeepFetchFailureAt` from `e.store.Get(...)` — correct.
- The legacy `seenUpdatedAt` map is gone; the comment at line 124 notes this explicitly. Migration complete.

### 3.3 itemNeedsWork (`engine/item.go:236–340`)

The fuller dispatch filter. All cooldown-related reads route through `e.store.Get(...).Item().StageState.LastAttemptAt[stage.Name]`. Correct.

### 3.4 Catch-up loop (`engine/poll.go:850–1100`)

Phase 1 and Phase 2 of the catch-up loop call into `checkDependencies`, `checkReviewGate`, `checkCIGate`, `checkMergeabilityGate`, `attemptMergeOnValidate`, `advanceToNextStage`. Each:

- Reads board state via `FetchProjectBoard` / `FetchItemDetails` (cacheImpl-backed) — correct.
- Reads engine state via `e.store.Get(...)` — correct.
- Mutates GitHub via `e.client.Update*/Add*/Remove*` and follows up with `e.store.Apply(Local*)` — write-through is honored at every site I checked.

`engine/stages.go:438` (`advanceToNextStage`) writes through to `e.store.Apply(LocalStatusUpdated)` after the GitHub mutation. The Phase 3-C / #515 fix is correctly in place.

### 3.5 ci.go:90–94 LinkedPR.HasHadChecks

```go
if snap, snapErr := e.store.Get(itemRepo, item.Number); snapErr == nil {
    if lpr := snap.LinkedPR(); lpr != nil {
        hadChecks = lpr.HasHadChecks
    }
}
```

Reads `LinkedPR.HasHadChecks` from `engine.store`. The mutation `PRChecksObserved` is applied to `engine.store` at `ci.go:80` whenever non-empty CheckRuns are observed. So `engine.store`'s LinkedPR has the HasHadChecks field populated for items that have ever had checks observed.

**But other LinkedPR fields** (Number, Mergeable, Reviews, ReviewRequests, ThreadComments, etc.) are **only on cacheImpl.store**. A read like `e.store.Get(...).Item().LinkedPR.Number` would return 0 (zero value), not the actual PR number. The current code doesn't make that mistake, but the foot-gun is real. F3 again.

### 3.6 Worker liveness (`engine/worker_liveness.go`)

Reads `snap.Labels()` from `e.store` (lines 124, 142). **This is a foot-gun**: labels are GitHub state, mirrored in `cacheImpl.store`, not `engine.store`. The reads almost certainly return empty `[]string`. Worker liveness still works because the cleanup path queries GitHub directly via REST after a stale heartbeat, so it doesn't actually rely on the in-memory label snapshot for the lock label.

Worth fixing as part of F3: either wire `engine.store` to also receive label mutations (bad — duplicate state), or change these reads to route through `cacheImpl`. Most likely the second.

### 3.7 TUI rendering paths (`tui/`)

Out of scope for this audit (TUI consumes the events emitted by observers; doesn't directly read store state). But worth confirming no `tui/*.go` reads `engine.store` directly. Quick grep: zero hits. ✅

## §4 Concrete Phase 5 issue specifications

### F1 — remove `e.inFlight` (single source of truth)

**Problem:** Engine has `inFlight sync.Map` parallel to `ItemState.Worker` from Phase 3-G. Every `e.inFlight.Store(iKey, ...)` site (ci.go:313, merge_gate.go:151, poll.go:1172, reviews.go:406) is paired with `store.Apply(LocalLockAcquired{Worker:...})`. The matching `Delete` calls pair with `WorkerExited`. Sole reader is `poll.go:1149` ("skip if already in flight").

**Fix:** Replace the single read site with `store.Get(...).Item().Worker != nil`. Remove the `inFlight` field from Engine. Remove all `Store/Delete` calls.

**Tests:** Existing dispatch-skip-inflight tests should pass unchanged. Add a regression test that verifies `Worker != nil` on a freshly-dispatched item.

### F2 — migrate `linkedPRs` and `prNumToKey` to Store

**Problem:** `boardcache/boardcache.go:207-212` still keeps `linkedPRs map[string]*gh.PRDetails` and `prNumToKey map[string]string` outside the Store. A TODO is present.

**Fix:** Extend `LinkedPRState` to carry the missing PR fields (Title, State, Merged, Draft). Add the corresponding Mutation types (`PRDetailsUpdated`?) and wire the existing webhook delta paths through them. Remove the two maps from CacheImpl.

**Tests:** Existing `boardcache` tests must continue passing. Add tests for the new mutations.

### F3 — codify the two-store split (or unify)

**Problem:** Two `itemstate.Store` instances exist, with field ownership split implicitly. Readers need tribal knowledge to route correctly. Foot-guns at ci.go:90-94 (HasHadChecks works because of explicit PRChecksObserved on engine.store) and worker_liveness.go:124/142 (Labels reads from engine.store would return empty).

**Fix:** Decide between:
- **Unify**: have engine create one Store, pass it to CacheImpl. All mutations flow through one Apply. Field ownership is by Mutation type, not by store identity. Closer to the original design.
- **Type-split**: keep two stores, but introduce typed read methods on each (`engine.store.GetEngineSnapshot(...)`, `cacheImpl.GetGitHubSnapshot(...)`) that return narrowed structs containing only the fields each store owns. Compile-time enforcement.

Worker liveness label reads need fixing either way: route to cacheImpl.

**Tests:** the chosen direction is a refactor, not a behaviour change; existing tests should pass.

### F4 — `boardcache.checkRuns` retention strategy

**Problem:** `boardcache/boardcache.go:200-203` documents that Store silently drops `CheckRunCompleted` for SHAs not yet linked to any item. CacheImpl keeps a parallel `checkRuns map[string][]CheckRun` to retain pre-linkage events.

**Fix:** Validate that this dual-track is necessary — if not (e.g. if an event arrives, we can fetch the linkage and apply on the fly), fold checkRuns into Store. If it is necessary, document the invariant in the Store API itself rather than in CacheImpl's comment.

**Tests:** existing tests cover the dual-track flow. Add a test for "CheckRunCompleted arrives before linkage; subsequent linkage causes pre-linkage runs to be replayed."

### F5 — observer-registration discipline (**Superseded by F3**)

**Superseded:** F3 (PR #538, issue #537) unified `engine.store` and `cacheImpl.store` into a single shared `*itemstate.Store` instance — engine creates it and passes it to `NewCacheImpl`. With only one `Store`, the registration discipline concern is structurally eliminated: there is no "which store" choice to make. Every Store observer subscribes once on `e.store`.

The proposed fixes (`Observer.RequiredStores()` method, broadcast-subscribe wrapper) were **not adopted** and are now dead architecture given the single-Store reality. No implementation required.

## §5 What did NOT need fixing

Some subjects that came up during the audit but turned out fine:

- **`mayNeedWork` is correctly drained** under its own mutex with a swap-and-empty pattern (`poll.go:823-831`).
- **Self-mutation write-through is in place at every site I checked** (`advanceToNextStage`, label add/remove, comment add, lock acquire/release).
- **Webhook delta coverage** appears comprehensive — `boardcache/delta.go` handles `opened`, `closed`, `reopened`, `transferred`, `assigned`, `unassigned`, `edited` for issues, plus the full PR lifecycle. The "deaf to new issues" class is closed.
- **Reactive observers cover all the user-visible signals**: TUI events, wakeCh, mayNeedWork. No place I found relies on per-poll re-evaluation when an observer-driven path exists.
- **TUI components don't bypass the event channel** to read store state directly.

## §6 Recommendation for re-enabling cache + webhooks

The architecture is in good enough shape that `webhooks: true` + `board_cache_mode: in-memory` can be re-enabled in dev with low risk. F1-F5 are improvements, not blockers.

Suggested re-enable test plan:

1. Set `webhooks: true` + `board_cache_mode: in-memory` in `.fabrik/config.yaml`.
2. Restart fabrik dev instance.
3. Open a test issue, watch fabrik pick it up via webhook (`[webhook] event: type=issues action=opened`).
4. Move the test issue between columns; verify `Status` updates propagate correctly without Status webhook (relies on `localDeltaAt` + observer + reconcile fallback).
5. Trigger an issue advance via fabrik; verify cache shows new Status immediately (write-through working).
6. Force a worker crash (kill -9 the claude process); verify stale-lock detection cleans up within heartbeat threshold + safety margin.

If all five pass, leave the cache on. If any fails, flip back to passthrough and refile the regression.

## §7 Document update note

`docs/state-machine.md` should reflect the actual landed architecture. PRs in Phase 3-A through 3-H included state-machine updates per acceptance criteria, but the **two-store reality** isn't yet documented. Worth adding under the "Engine internal state" section.
