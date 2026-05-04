# Phase 2 — Reactive Cache Architecture Design

**Read this after `01-state-inventory.md`.**

This document specifies the unified single-owner cache architecture that replaces the fragmented state model documented in Phase 1. The goal is captured in the conversation that initiated this work:

> *Refactor it to have a "reactive" architecture with a single cache owner and then validate that polling via GraphQL and webhook based incremental updates are both correctly updating that single cache. Once this is clean, we can check all the paths that are downstream of the cache that should be reactive.*

The design is split into:
- **§1** the core abstractions (ItemState, Store, mutator, observer);
- **§2** the data model (what fields are stored per item, including the consolidation of the 8+ engine maps);
- **§3** the input-channel contracts (poll, webhook, self-mutation);
- **§4** the output-channel contracts (downstream readers and reactive change notifications);
- **§5** the migration strategy (how to land this incrementally without a Big Rewrite);
- **§6** invariants and how they will be tested.

The companion ADR `03-adr-reactive-cache.md` records the design *decisions* and rejected alternatives. This doc describes the design *itself*.

## §1 Core abstractions

### 1.1 ItemState

The unit of state. One `ItemState` per `(repo, issueNumber)` pair. Replaces the 8+ engine maps and the boardcache `items` map for a single item.

```go
package itemstate

// ItemState is the canonical per-item state. All mutations flow through Store.Apply;
// all reads flow through Store.Get or change subscriptions.
//
// Field grouping by lifecycle:
//   - Identity: never changes after first Apply
//   - GitHub state: mirrors GitHub's view of issue, project, and linked PR
//   - Engine state: fabrik's local control state (locks, retries, cycle counts, cooldowns)
//   - Derived state: computed on read, not stored (e.g. "current stage")
type ItemState struct {
    // -------- Identity (immutable post-creation) --------
    Repo   string // "owner/repo"
    Number int    // issue number
    ItemID string // GitHub Project item node ID (or "" if not on the board)

    // -------- GitHub state (mirrored from GitHub via webhook deltas + reconcile) --------
    Title       string
    Body        string
    URL         string
    Author      string
    Assignees   []string
    State       string    // "open" | "closed"
    IsClosed    bool      // derived
    IsPR        bool
    Labels      []string
    Status      string    // project board column ("Specify", "Implement", etc.)
    UpdatedAt   time.Time // max(issue.updatedAt, projectItem.updatedAt, linkedPR.updatedAt)

    BlockedBy []Dependency

    Comments []Comment

    LinkedPR *LinkedPRState // nil if no closing PR

    // -------- Engine state (fabrik's local control) --------
    Lock        *LockState // nil = unlocked; non-nil = our lock or an other-instance lock
    StageState  StageState // per-stage attempt/cycle counters
    CooldownAt  map[string]time.Time // by reason ("retry", "review-blocked", "ci-await", etc.)
    Worker      *WorkerHandle        // present when a worker is in-flight for this item

    // -------- Derived state (computed on read) --------
    // Stage:        derived from Status by FindStage(stages, Status)
    // CurrentRetryCount: derived from StageState
    // EngineWantsToProcess: derived; replaces itemNeedsWork
}

type LinkedPRState struct {
    Number          int
    Mergeable       *bool          // nil = unknown; pointer for tri-state
    MergeableState  string         // "clean", "unstable", "blocked", etc.
    HeadSHA         string
    Reviews         []PRReview
    ReviewRequests  []ReviewRequest
    ThreadComments  []Comment      // unresolved review-thread comments
    ResolvedThreadCount int
    CheckRuns       []CheckRun     // by SHA — but stored on the LinkedPRState for the current head SHA
}

type StageState struct {
    // Per-stage counters, keyed by stage name (not stageKey — the issue identity is the
    // ItemState owner). Replaces e.retryCount, e.reviewCycleCount, e.ciFixCycleCount,
    // e.rebaseCycleCount.
    Attempts        map[string]int       // stageName → number of times Claude was invoked
    LastAttemptAt   map[string]time.Time // stageName → last invocation timestamp
    PausedByEngine  map[string]bool      // stageName → engine-pause vs user-pause distinguisher
    ReviewCycles    map[string]int
    CIFixCycles     map[string]int
    RebaseCycles    map[string]int
    // Comment processing: keyed by comment ID
    ProcessedComments map[string]time.Time // commentID → when fabrik finished processing
}

type LockState struct {
    HolderUser  string    // user identity in the fabrik:locked:<user> label
    HeldByThis  bool      // true if HolderUser == cfg.User AND Worker != nil
    AcquiredAt  time.Time
}

type WorkerHandle struct {
    PID         int
    StageName   string
    StartedAt   time.Time
    LastSignAt  time.Time // updated by worker heartbeats; stale heartbeat → assume worker died
}
```

### 1.2 Store

The single owner. Hands out read snapshots; accepts mutations. All access goes through Store.

```go
package itemstate

type Store struct {
    mu    sync.RWMutex
    items map[string]*ItemState // key: "owner/repo#N"

    observers []Observer // registered listeners; called on every successful Apply

    fallback FallbackFetcher // for cache-miss reads (single-item fetch from GitHub)
    logger   Logger
}

// Apply mutates state. Every mutator flows through here. Returns the new ItemState
// snapshot (immutable copy) and a list of changes for observers.
func (s *Store) Apply(m Mutation) (Snapshot, []Change, error)

// Get returns an immutable snapshot of the current ItemState for an item.
// On cache miss, falls back to GitHub via fallback fetcher, then returns the snapshot.
// Returns ErrNotFound only when the item legitimately does not exist.
func (s *Store) Get(repo string, number int) (Snapshot, error)

// Subscribe registers an observer. Returns an unsubscribe func.
func (s *Store) Subscribe(o Observer) func()

// Snapshot is an immutable copy of an ItemState. Returned by Get and Apply.
type Snapshot struct {
    state ItemState // copy, not pointer
}

// Change describes what fields a mutation altered. Used by observers to decide
// whether to react.
type Change struct {
    Repo   string
    Number int
    Fields ChangeFlags // bitmask: StatusChanged | LabelsChanged | LockChanged | ...
}
```

### 1.3 Mutation

A discriminated union of every possible state change. **Every** code path that wants to update state expresses it as a Mutation and calls `store.Apply(m)`. There is no other write path.

```go
type Mutation interface {
    isMutation()
}

// From inbound webhook deltas:
type IssueOpened struct { Issue gh.Issue }
type IssueLabeled struct { Repo string; Number int; Label string }
type IssueUnlabeled struct { Repo string; Number int; Label string }
type IssueClosed struct { Repo string; Number int }
type IssueReopened struct { Repo string; Number int }
type IssueCommentCreated struct { Repo string; Number int; Comment gh.Comment }
type ProjectV2ItemEdited struct { ItemID string; NewStatus string }
type PRReviewSubmitted struct { Repo string; Number int; Review gh.PRReview }
type PRReviewCommentCreated struct { Repo string; PRNumber int; Comment gh.Comment }
type CheckRunCompleted struct { Repo string; SHA string; Run gh.CheckRun }

// From fabrik's own mutations (write-through):
type LocalStatusUpdated struct { Repo string; Number int; NewStatus string }
type LocalLabelAdded struct { Repo string; Number int; Label string }
type LocalLabelRemoved struct { Repo string; Number int; Label string }
type LocalCommentAdded struct { Repo string; Number int; Comment gh.Comment }
type LocalLockAcquired struct { Repo string; Number int; User string; Worker *WorkerHandle }
type LocalLockReleased struct { Repo string; Number int }

// From periodic reconciliation:
type BoardReconciled struct { Items []gh.ProjectItem } // bulk update, computed against existing
type ItemDeepFetched struct { Repo string; Number int; FreshState gh.ProjectItem } // single-item fresh fetch

// From engine internals:
type StageAttempted struct { Repo string; Number int; StageName string; At time.Time }
type StageRetryIncremented struct { Repo string; Number int; StageName string }
type StageRetryCleared struct { Repo string; Number int; StageName string }
type ReviewCycleIncremented struct { Repo string; Number int; StageName string }
type CIFixCycleIncremented struct { Repo string; Number int; StageName string }
type RebaseCycleIncremented struct { Repo string; Number int; StageName string }
type EngineEnginePaused struct { Repo string; Number int; StageName string }
type CooldownRecorded struct { Repo string; Number int; Reason string; Until time.Time }
type WorkerHeartbeat struct { Repo string; Number int; At time.Time }
type WorkerExited struct { Repo string; Number int }
```

### 1.4 Observer

Anyone downstream of state changes registers an Observer. The observer receives a `Change` describing what changed and a `Snapshot` of the new state. It decides whether to act.

```go
type Observer interface {
    OnChange(change Change, snapshot Snapshot)
}
```

Examples of who observes:
- The dispatcher: subscribes to Status changes, label changes, comment additions, cooldown expiries → decides whether to dispatch a worker.
- The TUI: subscribes to all changes → updates display.
- Logging / debug: subscribes to all changes → emits structured events.
- The wake channel: subscribes to "could this item now have work?" changes → fires the existing wakeCh.

This is the *reactive* part of the architecture. Today, downstream readers poll. With observers, they react.

## §2 Data model — what consolidates into ItemState

This section maps the Phase 1 inventory into the new model. Every existing state structure either:

- **Becomes a field on ItemState** (consolidated into per-item state with single owner), OR
- **Stays at engine-level but reads through Store** (e.g. global counters that aren't per-item), OR
- **Is deleted** (because its reason for existing was an artefact of the fragmentation we're removing).

| Old structure | New home | Rationale |
|---|---|---|
| `boardcache.items[key]` | `Store.items[key].ItemState` (the whole struct) | The new owner consolidates this directly |
| `boardcache.deepFetched[key]` | Derived: `ItemState.LastDeepFetchAt != zero` | Was a flag; becomes a timestamp |
| `boardcache.linkedPRs[prKey]` | `ItemState.LinkedPR` (per-item) | A linked PR belongs to its issue |
| `boardcache.checkRuns[sha]` | `ItemState.LinkedPR.CheckRuns` (per-item, scoped to current head SHA) | Check runs are PR-scoped |
| `boardcache.shaToKey[sha]` | Derived: index built lazily by Store from items[].LinkedPR.HeadSHA | Reverse-lookup index, computed not stored |
| `boardcache.itemIDToKey[id]` | Derived index in Store, same pattern | |
| `boardcache.recentMissCache` | Stays in Store as a separate negative-cache structure (it's not per-item state) | |
| `boardcache.paused` | `Store.paused bool` (top-level) | |
| `engine.lockedIssues[iKey]` | `ItemState.Lock.HeldByThis` | The lock IS per-item state |
| `engine.lastUsage[iKey]` | `ItemState.LastTokenUsage` | TUI mirror state belongs to the item |
| `engine.lastCompleted[iKey]` | `ItemState.LastInvocationCompleted` | |
| `engine.lastBlocked[iKey]` | `ItemState.LastInvocationBlocked` | |
| `engine.lastUpdatedAt[iKey]` | **Deleted.** | The "have I seen this version?" check is replaced by Store change-feed: observers see only NEW changes. Cache is the source; no separate "seen" tracker. |
| `engine.deepFetchFailureTime[iKey]` | `ItemState.LastDeepFetchFailureAt` | Per-item cooldown |
| `engine.processedSet[stageKey]` | **Split.** Retry-suppression cooldown → `ItemState.StageState.LastAttemptAt[stageName]`. "Periodic re-evaluation" cooldown → `ItemState.CooldownAt[reason]` (separate map keyed by reason, not by stage). | The dual-purpose `processedSet` is the root of #504. Split fixes that. |
| `engine.retryCount[stageKey]` | `ItemState.StageState.Attempts[stageName]` | |
| `engine.pausedDueToRetries[stageKey]` | `ItemState.StageState.PausedByEngine[stageName]` | Now persists across restarts via reconcile (durable label-derived) |
| `engine.reviewCycleCount[stageKey]` | `ItemState.StageState.ReviewCycles[stageName]` | |
| `engine.ciFixCycleCount[stageKey]` | `ItemState.StageState.CIFixCycles[stageName]` | |
| `engine.rebaseCycleCount[stageKey]` | `ItemState.StageState.RebaseCycles[stageName]` | |
| `engine.ciMergePendingSince[iKey]` | `ItemState.LinkedPR.CIMergePendingSince` | PR-scoped |
| `engine.prHasHadChecks[iKey]` | `ItemState.LinkedPR.HasHadChecks` | PR-scoped |
| `engine.inFlight (sync.Map)` | `ItemState.Worker != nil && WorkerHeartbeat fresh` | Worker presence becomes part of state |
| `engine.cloneInFlight (sync.Map)` | Stays at engine level (per-repo, not per-item) | Not consolidated |
| `engine.baseBranchWarnedSet (sync.Map)` | `ItemState.BaseBranchWarned[branch]` (small per-item map) | |
| `engine.idleCount`, `idleStart`, `totalTokens`, `lastReportedCost` | Stay at engine level (not per-item) | Global engine state |

## §3 Input-channel contracts

Three inputs feed mutations into the Store:

### 3.1 Webhook stream (incremental, fast)

The webhook subprocess receives events; the manager passes them to a `WebhookDispatcher`, which translates each event into a Mutation and calls `store.Apply(m)`.

**Critical contract:** if a webhook arrives for an issue not in the Store, the dispatcher must:
1. Issue a single-item fetch from GitHub via `Store.fallback.FetchIssue(repo, number)`.
2. `store.Apply(IssueOpened{...})` to populate the cache.
3. THEN apply the original webhook delta.

This replaces the silent `if !ok { return }` no-op pattern that caused "fabrik went deaf to new issues."

**New action coverage required (currently missing):**
- `issues.opened`, `issues.closed`, `issues.reopened`, `issues.transferred`, `issues.deleted`, `issues.assigned`, `issues.unassigned`, `issues.edited`
- `pull_request.opened`, `pull_request.closed`, `pull_request.reopened`, `pull_request.synchronize`, `pull_request.ready_for_review`, `pull_request.converted_to_draft`, `pull_request.review_requested`, `pull_request.review_request_removed`
- `projects_v2_item.edited` (when delivered — App-mode or org-mode webhooks; documented gap in user/repo mode)

### 3.2 Periodic poll (full reconciliation)

A timer (configurable cadence) fetches the full project board from GitHub and submits a `BoardReconciled` mutation. The Store computes per-item deltas (using the existing diff logic) and applies them as a sequence of focused mutations:

```go
// Pseudo-code for reconcile inside Store
func (s *Store) Reconcile(boardItems []gh.ProjectItem) {
    // For each new fresh item:
    //   - if not in s.items: Apply(IssueOpened{Issue: itemAsIssue})
    //   - else: compute diff, Apply each field change as its own Mutation
    // For items in s.items but not in fresh: Apply(IssueRemoved or IssueClosed equivalent)
}
```

Per-Phase-1 §2.1, this replaces today's reconcile semantics that update fields directly on `existing` — it goes through Apply so observers see changes.

The lightweight Status-only sweep from issue #501 lives here too: `BoardReconciled` with a partial set of fields populated (just Status). The Store's diff logic must handle "field X is present in the input, others are zero/nil — only diff the present fields."

### 3.3 Self-mutation (write-through from fabrik's own GitHub mutations)

When fabrik calls a GitHub mutation API (label add, comment post, status field update, lock add, etc.), the call path is:

```go
// Old (broken):
if err := e.client.UpdateProjectItemStatus(...); err != nil { return err }
// — local cache stale until next reconcile

// New (write-through):
if err := e.client.UpdateProjectItemStatus(...); err != nil { return err }
store.Apply(LocalStatusUpdated{Repo: repo, Number: n, NewStatus: newStatus})
```

This is the Layer 0 contract. Every site in `engine/` that mutates GitHub state has a parallel Apply call. **Code review enforcement:** a static check (lint or policy) requires that every site calling a `client.Update*`, `client.Add*`, `client.Remove*`, or `client.Create*` has an adjacent `store.Apply(Local*)` call.

The list of self-mutation sites to add Apply calls to is known from Phase 1 §3 (the bug table). The Phase 4 audit catches any we miss.

## §4 Output-channel contracts

### 4.1 Reads via Snapshot

All read paths go through `Store.Get(repo, number)` returning a `Snapshot` (immutable copy of ItemState). This replaces:

- `c.items[key]` direct reads → `store.Get(...).Item()`
- `e.lastUpdatedAt[iKey]` reads → not needed; observers see changes directly
- `e.processedSet[stageKey]` reads → `snapshot.LastAttemptAt(stageName)` and `snapshot.CooldownAt(reason)`
- ... etc.

Snapshots are cheap (struct copy under read lock) and the consumer can hold them as long as desired without blocking writes. No concurrent-mutation hazards: the Snapshot is a value, not a pointer.

### 4.2 Reactive subscriptions via Observer

Some downstream code wants to *react* to changes, not just read on demand. Examples:

| Today's pattern | Reactive equivalent |
|---|---|
| `wakeCh` triggered on every webhook event | An Observer that filters changes and fires wakeCh only when something interesting (status, labels, lock, etc.) changed |
| TUI re-renders on every poll | Observer that emits TUI events scoped to which item changed |
| `itemMayNeedWork` evaluated on every poll for every item | Observer maintains a "may-need-work set" updated only on relevant Changes; dispatch loop reads from the set |
| `itemNeedsWork` re-evaluated on every poll | Same — derive needs-work from Snapshot, recompute on Change |

The dispatcher itself stays loop-driven (it runs on a poll tick to acquire semaphore slots and start workers in order), but the *deciding which items need work* moves to observer-maintained sets, which collapses a lot of redundant per-poll re-evaluation.

### 4.3 Cache-miss falls back, never silently drops

Every read path has a fallback to GitHub on cache miss. Every write path either (a) creates the item if missing, or (b) explicitly fails with `ErrNotFound` and the caller decides — never silently no-ops. The "silent no-op on missing key" pattern from Phase 1 §3 is banned.

## §5 Migration strategy

The refactor lands as **a sequence of small PRs** — not one monolithic rewrite. Each PR is independently reviewable, ships with tests, and leaves the engine in a working state.

### Phase 3-A: Skeleton (PR 1)

Add `internal/itemstate/` package: ItemState struct, Store, Mutation interface, Apply method, Get method, Snapshot type, Observer interface, basic tests. **Not wired into the engine yet.** Pure addition. CI green.

### Phase 3-B: Boardcache adapter (PR 2)

Make boardcache `CacheImpl` *delegate* to the new Store internally. Existing boardcache APIs (`FetchProjectBoard`, `FetchItemDetails`, `ApplyDelta`, `Bootstrap`, `Reconcile`) keep working but their internal data structures are replaced by `Store`. No engine changes required — engine still talks to `CacheImpl` via `ReadClient`. Tests prove behaviour-equivalence.

### Phase 3-C: Self-mutation write-through (PR 3)

Identify all `engine/` self-mutation sites (advanceToNextStage, AddLabel, RemoveLabel, AddComment, UpdateProjectItemStatus, AcquireLock, ReleaseLock, …). Add `store.Apply(Local*)` calls after each. Tests covering: status-mutation followed by immediate read returns new status. **This single PR fixes the #501/#506 advance-loop class of bug** — independent value before any other phase lands.

### Phase 3-D: Webhook delta complete coverage (PR 4)

Audit `boardcache/delta.go` against the new Mutation list. Add handlers for missing actions (`opened`, `closed`, `reopened`, `transferred`, etc.). Replace silent `if !ok { return }` with single-item fallback fetch + retry. **This PR fixes "fabrik went deaf to new issues."** Tests covering: webhook for unknown issue triggers fallback fetch and applies delta.

### Phase 3-E: Engine state consolidation (PR 5)

Move per-item engine maps (`lockedIssues`, `lastUsage`, `lastCompleted`, `lastBlocked`, `lastUpdatedAt`, `deepFetchFailureTime`, `prHasHadChecks`, `ciMergePendingSince`) into ItemState fields, accessed via Snapshot. Engine's reads/writes redirected. Tests covering: read-write equivalence with old behaviour.

### Phase 3-F: Stage-state consolidation (PR 6)

Move stage-keyed engine maps (`processedSet`, `retryCount`, `pausedDueToRetries`, `reviewCycleCount`, `ciFixCycleCount`, `rebaseCycleCount`) into `ItemState.StageState`. **Split `processedSet` into `LastAttemptAt` and `CooldownAt[reason]`** — fixes #504-class regression structurally. Tests covering: retry cooldown expires when expected; periodic re-evaluation cooldown not affected by retry timestamps.

### Phase 3-G: Worker handle (PR 7)

Add `ItemState.Worker` and heartbeat plumbing. Update lock-acquire / lock-release paths to populate Worker. Add background goroutine that watches for stale heartbeats and clears stale lock-labels. **Fixes the stale-lock recovery gap** observed on #501. Tests covering: worker exit clears lock; stale heartbeat triggers cleanup.

### Phase 3-H: Reactive observer plumbing (PR 8)

Add Subscribe + change-feed. Wire wakeCh as an observer (replacing the existing direct wake-on-webhook). Wire TUI events as an observer. Tests covering: change to a state field fires the right observer events.

Each PR is small (~500-1500 lines), independently testable, and leaves a working engine. The order is chosen to land bug-fix value early (Phase 3-C unsticks advance loops; 3-D unsticks new-issue blindness) before the bigger refactors.

## §6 Invariants and validation

The core invariants the design must hold:

| Invariant | How tested |
|---|---|
| (I1) Every state mutation flows through `Store.Apply`. No bypassing writes. | Linter / static check that no code outside `internal/itemstate/` mutates ItemState fields. |
| (I2) Inbound webhook deltas reach the cache for known and unknown items. | Test: send webhook for unknown item; assert cache populated with single-item fallback fetch. |
| (I3) Self-mutations write-through to cache before returning. | Test: call `engine.advanceToNextStage`; assert `store.Get` immediately reflects new Status. |
| (I4) Observers see every Change exactly once. | Test: register observer; apply N mutations; assert observer received exactly N Changes with correct field flags. |
| (I5) Snapshots are immutable and consistent. | Test: hold a snapshot; mutate state; original snapshot unchanged. |
| (I6) Reconcile is idempotent. | Test: apply the same `BoardReconciled` mutation twice; second has no observer events. |
| (I7) Retry cooldown is independent of periodic-re-eval cooldown. | Test: failed stage; check LastAttemptAt set; deep-fetch the item N times; assert LastAttemptAt unchanged (not refreshed). Then assert: after cooldown elapses, dispatch fires. |
| (I8) Worker death is detected and lock cleaned up. | Test: spawn a worker; kill it; assert WorkerHandle expires; assert lock released; assert next dispatch can re-acquire. |
| (I9) Cache-miss reads fall back to GitHub. | Test: clear cache; read item; assert fallback hit; assert cache populated. |
| (I10) Cache-miss webhook deltas trigger fallback fetch. | Test: clear cache; deliver webhook; assert fallback hit; assert delta applied. |

**Test coverage requirement for build PRs:** every PR in Phase 3-A through 3-H must include unit tests for the invariants it touches. Per the user's explicit ask: *"Make sure all build phase issues specify thorough testing."*

## §7 What this design does **not** do

- **Persistence across restarts.** Store is in-memory only, same as today. The Bootstrap path on startup re-populates from GitHub.
- **Cross-instance coordination.** Multiple fabrik instances each have their own Store, coordinated only through the durable label state on GitHub (`fabrik:locked:<user>`, `stage:X:complete`, etc.) — same as today.
- **Replacing the GitHub state model.** GitHub remains the source of truth for everything observable; the Store is a working copy. The lock-then-verify protocol in `processItem` is unchanged: re-read from GitHub before committing destructive operations.
- **Solving the `projects_v2_item` non-delivery in user mode.** That requires App auth (#85); the cache architecture mitigates by ensuring the Reconcile path correctly updates Status when it does run.
