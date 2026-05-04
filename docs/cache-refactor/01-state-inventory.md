# Phase 1 — State Map Inventory

**Status:** as-built analysis as of `d4f69a2`. Read this alongside `02-design.md` (the proposed unified architecture) and the cluster of Specify-stage cache-coherency issues filed earlier.

This document enumerates **every in-memory state structure that participates in fabrik's "view of the world"** — both the new `boardcache.CacheImpl` (introduced by issue #452 / PR #455 + follow-ups) and the older shadow caches that were maintained on the `Engine` struct before the cache feature. The premise of this work is that the cache feature was added *on top of* an engine that already kept significant state; instead of consolidating, it added another layer. The bugs we hit on 2026-05-03 (#467 stale Status, #501 advance loop, #504 cooldown regression, "fabrik went deaf" / `applyIssuesDelta` doesn't handle `opened`) are all symptoms of the same root cause: **state mutations through one path do not reach the other paths**.

The remedy is sketched in `02-design.md`. This document is the inventory those design decisions have to address.

## Reading guide

- **§1** is a flat inventory: every structure, where it lives, what its key/value type is.
- **§2** is per-structure analysis for the structures we have direct evidence are buggy or fragile. Each entry covers: purpose, update sites, read sites, invariants the code assumes but does not enforce, and known/observed bugs.
- **§3** is cross-cutting observations about how these structures *interact* — which is where the real bugs live.
- **§4** summarises the design implications carried forward into Phase 2.

## §1 Inventory

### 1.1 Engine state (`engine/engine.go:60-97`)

Per-issue state (keyed by `issueKey` = `"owner/repo#N"`):

| Field | Type | Purpose |
|---|---|---|
| `lockedIssues` | `map[string]bool` | Tracks issues for which `fabrik:locked:<self>` was added; cleanup driver on shutdown |
| `lastUsage` | `map[string]TokenUsage` | Per-issue token usage from last `processItem` (TUI display) |
| `lastCompleted` | `map[string]bool` | Per-issue completion state from last `processItem` (TUI display) |
| `lastBlocked` | `map[string]bool` | Per-issue blocked-on-input state from last `processItem` (TUI display) |
| `lastUpdatedAt` | `map[string]time.Time` | Last-seen `item.UpdatedAt` value; used by `itemMayNeedWork` poll filter |
| `deepFetchFailureTime` | `map[string]time.Time` | When `FetchItemDetails` last failed (cooldown for retry) |
| `prHasHadChecks` | `map[string]bool` | Has `FetchCheckRuns` ever returned non-empty; used to distinguish "checks not yet started" from "checks complete and absent" |
| `ciMergePendingSince` | `map[string]time.Time` | When CI was first observed in_progress in the merge guard |

Per-stage-attempt state (keyed by `stageKey` = `"owner/repo#N-stageName"` or `"owner/repo#N-comment-ID"`):

| Field | Type | Purpose |
|---|---|---|
| `processedSet` | `map[string]time.Time` | Last attempt timestamp; drives both `itemMayNeedWork` cooldown bypass and `itemNeedsWork` retry cooldown. Heavily multi-purpose. |
| `retryCount` | `map[string]int` | Failed-attempt counter per stage; escalates to `pausedDueToRetries` at `MaxRetries` |
| `pausedDueToRetries` | `map[string]bool` | "Engine paused this issue" flag — distinguishes engine-pause from user-pause |
| `reviewCycleCount` | `map[string]int` | Review-reinvoke cycle count per stage |
| `ciFixCycleCount` | `map[string]int` | CI-fix-reinvoke cycle count per stage |
| `rebaseCycleCount` | `map[string]int` | Rebase-reinvoke cycle count per stage |

Concurrency primitives (`sync.Map`):

| Field | Type | Purpose |
|---|---|---|
| `inFlight` | `sync.Map` | issueKey → bool (isPR); "currently being processed" gate against duplicate dispatch |
| `cloneInFlight` | `sync.Map` | "owner/repo" → *cloneCall; per-repo bare-clone coordination |
| `baseBranchWarnedSet` | `sync.Map` | "owner/repo#N:branch" → bool; deduplicates `base:<branch>` fallback comments |

Global / aggregate state:

| Field | Type | Purpose |
|---|---|---|
| `totalTokens` | `TokenUsage` | Accumulated token usage since process start |
| `lastReportedCost` | `float64` | Last printed cost in `[stats]` lines |
| `idleCount` | `int` | Consecutive idle polls; drives self-upgrade trigger |
| `idleStart` | `time.Time` | Start of current idle run; drives backoff calculation |

### 1.2 Boardcache state (`boardcache/boardcache.go:144-176`)

Per-item state (keyed by `itemKey` = `"owner/repo#N"`):

| Field | Type | Purpose |
|---|---|---|
| `items` | `map[string]*gh.ProjectItem` | Full per-issue item including labels, Status, body, comments, linked-PR data |
| `deepFetched` | `map[string]bool` | Has `FetchItemDetails` been called for this key (so reads can skip the network) |

Per-PR / per-SHA / per-item-ID state:

| Field | Type | Purpose |
|---|---|---|
| `linkedPRs` | `map[string]*gh.PRDetails` | PR details by key `"owner/repo#prN"` |
| `checkRuns` | `map[string][]gh.CheckRun` | CheckRun list by commit SHA |
| `shaToKey` | `map[string]string` | SHA → issueKey (reverse lookup for webhook delta application) |
| `itemIDToKey` | `map[string]string` | GitHub Project item ID → issueKey (reverse lookup for `projects_v2_item` delta) |

Negative cache and metadata:

| Field | Type | Purpose |
|---|---|---|
| `recentMissCache` | `map[string]time.Time` | Negative-cache: TTL'd entries for "miss:owner/repo#prN" and "miss:sha:SHA"; prunes lazily |
| `projectID` | `string` | Project ID from last bootstrap/reconcile |
| `projectTitle` | `string` | Project title |
| `projectOwnerType` | `string` | "organization" or "user" |

Stream state:

| Field | Type | Purpose |
|---|---|---|
| `paused` | `bool` | When true, `ApplyDelta` is a no-op and read methods fall back to GitHub. Set during webhook stream unhealthy → recovery cycle. |

### 1.3 Webhook manager state (`engine/webhook.go`)

Outside the scope of this analysis — webhook health/secret/event-counter state is internal to the subprocess supervisor and does not affect the engine's view of *issue/PR* state. Mentioned here only for completeness; the only relevant interaction is that `ApplyDelta` writes to boardcache state (1.2 above) when events arrive, and `Pause`/`Resume` are called on the cache by the health-change handler.

## §2 Per-structure deep dives (high-bug-density structures)

### 2.1 `boardcache.items`

**Purpose:** central per-issue record. The intent is "single source of truth for current state of every project item." In practice it is one of several views.

**Update sites:**

| Path | Effect |
|---|---|
| `Bootstrap` (called once at startup) | Replaces `items` map entirely from a fresh GitHub board fetch. |
| `Reconcile` (periodic 60min + webhook health-change recovery) | For each item in fresh board: if existing in `items`, overlay shallow fields (`Status`, `Labels`, `Title`, `UpdatedAt`, `IsClosed`, `IsPR`, `ItemID`, `URL`). If new, add it. Items not in the fresh board: removed from `items`. |
| `applyIssuesLabeled` / `applyIssuesUnlabeled` | Append/remove a label on `items[key].Labels` |
| `applyIssueCommentCreated` | Append a comment to `items[key].Comments` |
| `applyPullRequestDelta` | Update `items[key]` for ready-for-review / closed / reopened / synchronize on the issue's linked PR |
| `applyPullRequestReviewSubmitted` | Update `items[key].LinkedPRReviews` |
| `applyPullRequestReviewCommentCreated` | Update `items[key].LinkedPRReviewThreadComments` |
| `applyCheckRunCompleted` | Updates `checkRuns` map, not `items` directly |
| `applyProjectsV2ItemEdited` | Update `items[key].Status` (only when delivered — **NOT delivered to repo webhooks**) |
| `FetchItemDetails` cache-miss path | Populate deep fields on `items[key]` via `copyDeepFields(cached, item)` after a fallback-to-GitHub call |

**Read sites:**

| Path | Use |
|---|---|
| `FetchProjectBoard` | Reconstructs and returns a `*gh.ProjectBoard` from `items[]` |
| `FetchItemDetails` cache-hit | Copies deep fields from `items[key]` into the caller's item |
| `applyIssuesLabeled/Unlabeled/etc.` (delta application) | Reads `items[key]`; bails silently with `if !ok { return }` when missing |
| `applyProjectsV2ItemEdited` | Reads `items[key]` via `itemIDToKey`; bails silently |
| `resolvePRLinkage` | Iterates `items` to find the issue closed by a given PR |

**Implicit invariants the code assumes but does not enforce:**

1. *Every issue currently on the GitHub Project board is in `items`.* Violated when a new issue is added: `applyIssuesDelta` does not handle `action == "opened"`, so the new issue is missing from `items` until next `Reconcile`. The label-add path then silently no-ops because the issue is not in the cache. **Direct cause of "fabrik went deaf to new issues" observed 2026-05-04.**
2. *Status reflects current board column.* Violated in user mode where `projects_v2_item` is not delivered: only `Reconcile` updates Status, with worst-case 60min latency. **Direct cause of #467 stale-Status / advance-loop on #506 / advance-loop on #501 (all 2026-05-03).**
3. *Self-mutations (e.g. `advanceToNextStage`'s `UpdateProjectItemStatus`) are reflected in the cache.* Violated: there is no write-through — only inbound webhook deltas and reconcile update `items`. **Direct cause of the multi-minute advance loop captured in `#501` and `#506` logs.**
4. *Labels in the cache are the same set GitHub has.* Violated when the issue is not in the cache (gap 1) — label-add events are silently dropped.
5. *`items` matches the truth across restarts.* Holds: bootstrap fetches fresh from GitHub. But at runtime, drift accumulates between bootstrap and the next reconcile.

**Known bugs/gaps:**

| ID | Symptom | Root cause |
|---|---|---|
| (cluster filed in Specify) | New issues invisible to fabrik | `applyIssuesDelta` missing `opened`, `closed`, `reopened`, `transferred` actions |
| (cluster filed in Specify) | Label changes on new issues silently drop | `applyIssuesLabeled` looks up `items[key]` and bails; combined with gap 1 produces compound stuckness |
| #501 / #506 advance loop | After local `UpdateProjectItemStatus`, cache Status stays stale; next poll re-attempts the advance every 15s | Layer 0 / write-through gap |
| #467 column-move lag (~76min) | Cache Status only updates on `Reconcile`; reconcile fires on 60min timer or webhook health-change | `projects_v2_item` not delivered; periodic reconcile interval too slow |

### 2.2 `engine.processedSet`

**Purpose:** records "fabrik attempted this stageKey at time T." Used for two distinct purposes that are not separated:

1. **Cooldown bypass / aliveness in `itemMayNeedWork`** (`item.go:128`): if `processedSet[stageKey]` is set and `time.Since(lastAttempt) >= 10×PollSeconds`, return true (re-admit the item). This is the path that makes `fabrik:blocked` items re-evaluated periodically.
2. **Retry suppression in `itemNeedsWork`** (`item.go:315`): if `processedSet[stageKey]` is set and `time.Since(lastAttempt) < 10×PollSeconds`, return false (skip dispatch). This is the path that prevents thrashing when a stage attempt just ran.

The same map serves both behaviours, with the same cooldown duration. Reads are at lines 128 and 315; writes are scattered across at least seven sites.

**Update sites:**

| Path | Effect | Notes |
|---|---|---|
| `processItem` after Claude ran (`item.go:411`, `821`) | Set `processedSet[stageKey] = now` | Original purpose: record an attempt happened |
| Comment-handling paths (`comments.go:319, 331`) | Set `processedSet[<comment-key>] = now` | Comment processing has its own variant of the same map, keyed by comment ID |
| Catch-up loop blocked-review-gate path (`poll.go:806`) | Set `processedSet[stageKey] = now` | Added in #497 to make the cooldown retry path drive re-evaluation of awaiting-review items |
| Deep-fetch deferred refresh (`poll.go:674-681`) | Refresh `processedSet[stageKey] = now` if entry already exists | Added in #488 fix; **OVER-BROAD: refreshes for any item with existing entry, not just terminal items.** Direct cause of #504 regression. |
| Comment-clear path (`comments.go:262`) | Delete from `retryCount`/`pausedDueToRetries`; processedSet not deleted but stage advance occurs | (Asymmetric — see invariants below) |

**Read sites:**

| Path | Use |
|---|---|
| `itemMayNeedWork` cooldown bypass (`item.go:128`) | Re-admit after cooldown so blocked items get periodic re-evaluation |
| `itemNeedsWork` retry suppression (`item.go:315`) | Suppress dispatch within cooldown window |

**Implicit invariants:**

1. *processedSet timestamps reflect "when fabrik last actually tried to dispatch."* Violated by the deep-fetch refresh (`poll.go:674-681`) which bumps the timestamp every poll cycle, lying about when the attempt actually occurred. **Direct cause of #504.**
2. *processedSet entries are removed when the underlying state changes (e.g., stage completes, issue advances, fabrik:paused removed).* Partially violated: stage completion does not remove the entry. Comments path deletes related counters but not processedSet itself.
3. *cooldown is the same for "failed retry" and "blocked re-evaluation"* — coincidence rather than design.

**Known bugs:**

- **#504**: deep-fetch refresh is too broad → cooldown never expires → retries blocked indefinitely.
- (Latent) The dual-purpose semantics make it impossible to set a long cooldown for retry-after-failure (where "do nothing for a while") without also slowing down re-evaluation of blocked items (where "check more often" is desired).

### 2.3 `engine.lastUpdatedAt`

**Purpose:** poll-time staleness filter. `itemMayNeedWork` returns false when `item.UpdatedAt <= lastUpdatedAt[iKey]`, suppressing the expensive deep-fetch for items that have not changed.

**Update sites:**

| Path | Effect |
|---|---|
| Poll deferred function (`poll.go:674`) | After successful deep-fetch, set `lastUpdatedAt[iKey] = item.UpdatedAt` (unless item was just advanced) |
| `processItem` claudeRan path (`item.go:861`) | `delete(e.lastUpdatedAt, iKey)` to handle the race where a concurrent poll cycle cached updatedAt while the worker was running and a comment posted during the run would otherwise be missed |

**Read site:** `itemMayNeedWork` (`item.go:122`).

**Implicit invariants:**

1. *`item.UpdatedAt` is monotonic and reflects every change worth reacting to.* Violated for column moves in user mode: `Status` field changes don't bump `updatedAt` of the issue itself, only of the project item — and the project item's `updatedAt` propagates to `item.UpdatedAt` only after the cache-side merge in `github/project.go:309`. Webhook deltas (which now drive `items[].UpdatedAt` updates via `applyIssuesLabeled` etc.) update `UpdatedAt` to `time.Now()`, but those don't bump `lastUpdatedAt` directly until the next poll deep-fetches the item.
2. *Cache-bypass labels (`fabrik:awaiting-ci`, `fabrik:rebase-needed`) cover all cases where periodic re-evaluation is needed independent of UpdatedAt.* Was violated for `fabrik:awaiting-review` until #497 added the cooldown-recording path.

**Known bugs:** the structure itself is mostly fine, but it depends on `item.UpdatedAt` being advanced correctly by *all* paths — and the boardcache's own update logic does that imperfectly (e.g. `applyIssuesLabeled` sets `UpdatedAt = time.Now()`, but `applyProjectsV2ItemEdited` is never called in user mode).

### 2.4 `engine.inFlight` (sync.Map)

**Purpose:** "currently dispatched a worker for this issueKey." Prevents a second poll cycle from dispatching a duplicate worker while the first is mid-flight.

**Update sites:** `Store` at the start of dispatchers (`processItem`, `dispatchReviewReinvoke`, `dispatchCIFixReinvoke`, `dispatchRebaseReinvoke`, `attemptMergeOnValidate`). `Delete` via `defer` when the goroutine exits.

**Read sites:** poll's dispatch loop (`poll.go:838`); review/rebase/CI-fix reinvoke entry points; rebase path in `stages.go:358`.

**Implicit invariants:** *every Store has a matching Delete, even on panic.* Held by `defer` discipline.

**Known bugs:** none observed *in this map*. But it is the only surviving "actually currently running" gate, and it is in-memory. Combined with stale lock-label ghost (`fabrik:locked:<self>` left on issue when worker dies), the engine's view of "is this running" diverges from reality. Lock-label cleanup on shutdown (`cleanupLockedIssues`) helps but does nothing for crashes mid-flight.

### 2.5 `engine.lockedIssues`

**Purpose:** track which issues this engine instance has applied a `fabrik:locked:<self>` label to, so they can be cleaned up on graceful shutdown.

**Update sites:** `processItem` lock acquisition (`item.go:550`); cleanup loop (`item.go:565`, `poll.go:520`).

**Read sites:** shutdown cleanup loop (`poll.go:500-520`).

**Implicit invariants:** *fabrik:locked:<self> on GitHub == lockedIssues[key] == true.* Violated by crash, kill -9, or `pkill`. The label persists on the issue but the in-memory set is gone after restart. Stale labels become a manual-cleanup task forever.

**Known bugs:** **stale-lock recovery has no automated path.** Dispatcher's `itemNeedsWork` correctly admits items locked by *self* (so retry can lock again), but the user-visible "fabrik:locked:verveguy" label persisting confuses the operator. We hit this on #501 today — manual label deletion was required.

### 2.6 `engine.retryCount` + `pausedDueToRetries`

**Purpose:** track failed stage attempts, escalate to engine-pause after `MaxRetries`.

**Update sites:** seven sites (most in `item.go` and `comments.go`). Increments at `item.go:927`; resets/deletes at multiple places — including `comments.go:262-263` (when a new comment arrives), various paths in `item.go`, and `pauseForReviewTimeout`'s removal in `escalateFailedStage`.

**Read sites:** decision in `processItem`'s "did not complete" path; `wasPaused` in `item.go:483`; `unblockAwaitingInput` reset path in `item.go:986-991`.

**Implicit invariants:**

1. *Increments and resets are paired correctly.* Mostly held; when something gets out of sync, the issue ends up paused with `pausedDueToRetries=false` (looking like user-paused) or unpaused with retryCount > 0 (looking like fresh attempts).
2. *The pause originator is identifiable.* `pausedDueToRetries` is the only signal that distinguishes engine-pause from user-pause, and it is in-memory only — does not survive restart.

**Known bugs:** none observed *in current behaviour*, but the in-memory-only nature of `pausedDueToRetries` means **after a restart the engine cannot tell why an issue is paused.** A user comment will resume the issue under "user paused this" semantics regardless.

### 2.7 The "TUI mirror" trio: `lastUsage`, `lastCompleted`, `lastBlocked`

**Purpose:** capture last-known per-issue state so the TUI can display it without re-reading from GitHub.

**Update sites:** end of `processItem` and various comment / review / CI handlers. Each hander writes; some delete. Different handlers update different subsets — e.g. `merge_gate.go` updates `lastUsage` and `lastCompleted` but not `lastBlocked` (it deletes blocked).

**Read sites:** TUI event emission paths in `poll.go:1046` (and equivalents) — read after `processItem` to populate TUI events.

**Implicit invariants:** *all three are updated together.* Not enforced; in practice handlers update a subset and delete the others. The TUI reads what's there and prints stale data when handlers diverge.

**Known bugs:** TUI can show inconsistent state across the three (e.g. completed but blocked-on-input both true). Not catastrophic, but a code smell — three pieces of derived state held separately when they should be one.

## §3 Cross-cutting observations — where the bugs *actually* live

The per-structure analyses above each report a few bugs. The deeper pattern is that **bugs cluster at the seams between structures**. Almost every bug we hit on 2026-05-03 was a state-update path that updated structure A but not structure B, where B was downstream of A in some implicit way:

| Bug | Mutation site | Updated | Forgot |
|---|---|---|---|
| #501 advance loop | `advanceToNextStage` | GitHub project Status | `boardcache.items[key].Status` |
| #467 76-min lag | external column move | GitHub project Status | `boardcache.items[key].Status` (no `projects_v2_item` delivery; reconcile only) |
| "deaf to new issues" | external `issues.opened` | GitHub | `boardcache.items` (delta handler missing `opened`) |
| New-issue label drop | external `issues.labeled` after `opened` | GitHub | `applyIssuesLabeled` bails because `items[key]` doesn't exist |
| #504 cooldown regression | every successful deep-fetch | `processedSet[stageKey] = now` | (over-update — should not refresh when stage incomplete) |
| Stale lock from crashed worker | worker process dies | `fabrik:locked:<self>` label on GitHub | `e.lockedIssues[iKey]` (in-memory only — gone on restart) |
| TUI mirror divergence | various stage handlers | A subset of {lastUsage, lastCompleted, lastBlocked} | The other subset |

The shape: **fabrik treats "the state that drives behaviour" and "the cache of GitHub state" as separate concepts, but in practice they overlap heavily.** No single code path owns the lifecycle of an item's state. Every mutation path is responsible for knowing all the downstream state to update — and this is fragile by construction.

A second pattern: **silent no-ops on missing keys.** `applyIssuesLabeled` does `if !ok { return }` — same in `applyProjectsV2ItemEdited`, in `applyPullRequestDelta`, etc. The intent is "ignore events for items we don't know about." The effect is "silently drop label changes on issues the cache forgot to add." A cache miss should at minimum log; at most, fall back to a single-item GitHub fetch to populate the cache and try again. Neither happens.

A third pattern: **shallow fields vs deep fields are not symmetric.** `boardcache.items[key]` holds both. Reconcile updates only shallow fields and preserves cached deep fields. `FetchItemDetails` updates only deep fields and reads shallow fields from whatever's there. When the two diverge (e.g. labels added to an item whose deep fields are stale), the read paths see inconsistent views.

## §4 Design implications

The unified-cache design in `02-design.md` has to address these directly:

1. **One mutator API.** Every state change — webhook delta, fabrik's own mutation, periodic reconcile — flows through one `Apply(eventOrMutation)` call on the canonical item state. No "mutate GitHub then forget to update cache" gap.
2. **Consolidated per-item state.** The 8+ engine maps that key on `iKey` or `stageKey` collapse into fields on a single `ItemState` struct. Reading that struct returns a consistent view by construction.
3. **Cache-miss behaviour on inbound events.** When a webhook arrives for an unknown issue, the handler must add it (via single-item fetch) before applying the delta — never silently drop.
4. **Self-mutation write-through.** `advanceToNextStage` and any other code path that mutates GitHub state must update the cache as part of the same logical operation, not rely on a webhook echo or periodic reconcile.
5. **Stale-lock detection.** The cache should distinguish "we hold the lock and the worker is alive" from "we hold the lock and the worker is dead" — probably by tracking worker PIDs/heartbeats, with cleanup on process exit and on worker-process disappearance.
6. **TUI mirror state should derive from canonical state.** No three-way map of "last X seen by stage handler"; just a snapshot of the current ItemState.
7. **Separate concerns of "cooldown bypass for periodic re-evaluation" from "retry suppression after failure".** Currently both use `processedSet`, which is why #504 broke retries while trying to fix #488's loop.

These are the foundations the Phase 2 design has to build on.
