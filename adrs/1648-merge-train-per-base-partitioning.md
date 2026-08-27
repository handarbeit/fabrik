# ADR 1648: Merge-Train Batches Partitioned Per (Repo, Base), Not Per Repo

**Date**: 2026-08-27
**Status**: Accepted
**Issue**: #1648 — feat(merge-train): batch per resolved base branch — one train per base, not
one per repo (supersedes [ADR-1647](1647-merge-train-non-default-base-exclusion.md))

## Context

ADR-1647 made the internal merge train (ADR-059) *safe* on `base:<branch>` Queued members by
excluding them from batching entirely — a member targeting a non-default base was left in
`Queued`, untouched, for a human to merge by hand. That fix was deliberately scoped as an interim
guard: its own doc explicitly frames "batch multiple bases at once" as "a follow-up, larger design
effort." This issue is that follow-up.

Leaving `base:<branch>` members permanently unbatchable is a real capability gap, not just an
inconvenience: a release-line branch (`maint/*`, `release/*`) gets neither of the two protections
the default branch enjoys from the train — nothing serializes concurrent merges into it, and
nothing validates a combination of changes as a batch before they land together. The reporter who
originally surfaced this (#1646) hit exactly that consequence: two PRs merged 18 seconds apart,
both touching the same file, with the second's mergeability still `unknown` at merge time — caught
only because the reporter checked by hand instead of trusting the train.

Before this issue, the entire data flow — `groupQueuedByRepo` → `routeQueuedGroup` → one capped
batch → one `dispatchMergeTrainWorker` call → one `mergeTrainWorkerState` at
`mergeTrainInFlight["owner/repo"]` → one goroutine — assumed **at most one merge-train worker per
repo**. `prepareTrainWorker` resolved `baseBranch` exactly once per dispatch, unconditionally, via
`wm.DefaultBaseBranch()`, and threaded that single value through the batch's entire lifetime. Every
guard/counter keyed on bare `"owner/repo"` — `mergeTrainInFlight`, the runaway guard's
`mergeTrainTrials`/`mergeTrainRunawayAlerted`, and `store.repoWorkers` — inherited that same
one-worker-per-repo assumption structurally, not just by convention.

## Decision

### 1. Partition inside the shared grouping function, not a second layer

`groupQueuedByRepo` (free function) is unchanged — it still does the cheap repo-only pass
(holding-column filter, closed/`fabrik:paused` exclusion). A new `(e *Engine)
groupQueuedByRepoAndBase` wraps it and further splits each repo's items by resolved base into one
`queuedRepoGroup` per (repo, base) pair actually present. Both consumers of the old grouping —
`handleMergeTrainBatch` (dispatch) and `settleQueuedReviewFindings` (the #1208 pending-eject
signal) — switch to the new function, so neither can drift out of sync with the other. This mirrors
ADR-1647's own placement reasoning: partition before the batch cap and before
`dispatchMergeTrainWorker` snapshots `batchNumbers`, in the one function every consumer already
shares.

`routeQueuedGroup` now runs once per **partition**, not once per repo. ADR-1647's entire exclusion
chain (`nonDefaultBaseExclusion`, `filterNonDefaultBaseMembers`, `markNonDefaultBaseExcluded`,
`excludeNonDefaultBaseExclusions`, the `fabrik:non-default-base-excluded` label) is deleted
outright — once every item is already routed to the partition matching its own resolved base,
there is nothing left to exclude. The batch cap (`effectiveMaxBatchSize`) and the runaway-guard
pre-check both apply **per partition**: a repo with two active bases gets the cap and the guard
independently for each, not split across a combined total. This is what keeps AC4 (byte-identical
default-only behavior) trivially true — a repo with only the default partition sees no change at
all.

### 2. A zero-cost sentinel for the common case, not eager resolution

The naive approach — resolve every item's real base via `baseBranchForItem`/`wm.DefaultBaseBranch()`
during grouping, including default-base items — was tried and rejected: it makes the *entire*
Queued batch for a repo fail closed on any transient git hiccup (or even a narrow same-poll
`WorktreeManager`-not-yet-registered window), a strictly worse failure mode than what it replaces,
and it silently broke the "zero-cost for default-base items" property that both the old exclusion
and this issue's own R5 require.

Instead, `groupQueuedByRepoAndBase` buckets an item with no `base:` label under
`defaultPartitionBase` — the empty string — without ever touching the `WorktreeManager` or running
git. `baseBranchForItem` (which does need the `WorktreeManager`) is called only for items that
actually carry a `base:` label; on failure (no label but also no registered `WorktreeManager`, or a
resolution error), that one item — not the whole repo group — is excluded from batching this poll,
mirroring ADR-1647's own fail-closed posture but scoped to the item that actually needs it.

This split then threads all the way through: `trialParams` carries both `partitionBase` (the raw
grouping key — the sentinel, or a labeled item's already-resolved real name) as the immutable
`trainKey`, and `baseBranch` (the *real* git branch name, resolved from `partitionBase` inside
`prepareTrainWorker` — calling `wm.DefaultBaseBranch()` only when `partitionBase` is the sentinel,
exactly reproducing the pre-#1648 call). Every guard/counter operation
(`mergeTrainInFlight`/`finishTrain`, `recordTrial`/`isRunawayTripped`/`resetTrialCounter`,
`fireRunawayGuard`'s alert key, `store.EnterRepoWorker`/`ExitRepoWorker`) keys on `trainKey`; every
git-touching operation (SHA pinning, trial branch naming, the integration PR's target,
`trialBehind` comparisons) uses `baseBranch`. Conflating the two — e.g. keying a guard on the real
resolved name instead of the raw partition value — was tried and produces a silent mismatch for
every default-base partition: the guard key computed at dispatch time (before resolution) would
disagree with one computed later from the resolved name, breaking runaway-alert idempotency for
the overwhelmingly common case.

`mergeTrainKey(repoKey, baseBranch)` returns `repoKey` **unchanged, with no delimiter appended**,
when `baseBranch` is the sentinel — not `"owner/repo:"` with a trailing colon naming no base at
all. This is deliberate, not an inconsistency: it makes the default partition's key byte-identical
to the pre-#1648 bare-`repoKey` form everywhere that key also appears in a human-facing log line or
alert comment (the literal wording AC4 asks for), and it stays collision-safe by construction — a
real resolved branch name from `baseBranchForItem` is never empty, so bare `"owner/repo"` can only
ever mean the default partition. For every other base, the delimiter is `:` — illegal in both a
GitHub `owner/repo` string and in any valid git ref name (`git check-ref-format` forbids a bare
`:`), so the two components can never collide by construction; no `sanitizeBranchName`-style
escaping of the key itself is needed.

### 3. `reconstructTrainState` needs an explicit base-identity gate it didn't need before

Before this issue, `reconstructTrainState`'s restart-recovery sweep could safely treat any
`fabrik/merge-train/*` PR or branch it didn't recognize as belonging to *its own* batch as an
orphan/stale remnant, safe to close/delete — at most one worker could ever be live per repo, so an
unmatched artifact was never anything else's. That assumption breaks once concurrent per-base
workers are ordinary steady-state, not just a literal process-restart scenario:
`reconstructTrainState` runs on *every* fresh dispatch, whenever no worker is currently registered
for that specific `trainKey`, and a repo can now have several such dispatches active at once.

The existing "does this PR/branch's trial name belong to *this batch's* members" check (via
`filterBatchByNumbers`) already protects the main `trainPR` selection — issue numbers are
inherently partition-exclusive, so this is safe by construction — but it does **not** protect two
sub-branches that act on *any* unmatched artifact with no base check at all: the stale-open-PR-close
path (an open PR with no still-Queued members from *this* batch) and Route 3's orphan-branch sweep
(any `fabrik/merge-train/*` branch on origin with no relevant PR). Without a fix, a `maint/1.x`
worker's reconstruction could close a live, healthy `main`-partition integration PR, or delete its
live trial branch, purely because its own batch doesn't mention that PR/branch's members.

The fix is a new `trialBelongsToBase(headRef, baseBranch string) bool`, checking that the trial
name's embedded, already-sanitized base segment (`merge-train-<sanitizeBranchName(baseBranch)>-...`,
which `baseTrialName` already always produces) matches the calling worker's own `baseBranch`. Both
sub-branches are gated on it: an unmatched artifact that also fails `trialBelongsToBase` is left
alone rather than swept, on the working assumption that it may be a sibling partition's live trial
rather than a genuine orphan. This is the one place branch-identity-by-construction (R4 — trial
names are already base-qualified via `sanitizeBranchName(baseBranch)` plus a unix timestamp, so
`findIntegrationPR` and the main `trainPR` selection need no code change at all) is *not* already
sufficient on its own, and is the most safety-critical addition in this change.

### 4. Deliberately NOT re-keyed: `queuedReviewEjects`, `mergeTrainCloneSkipCounts`

`queuedReviewEjects` (the #1208 pending-eject signal) stays keyed by bare `"owner/repo"` → issue
number → finding count. An issue number belongs to exactly one partition's live batch at a time, so
two workers sharing one repo-level map bucket never collide — their issue-number keys are disjoint
by construction, and the map is already mutex-guarded. Re-keying it to `(repo, base)` would add
complexity for no correctness benefit.

`mergeTrainCloneSkipCounts` tracks `ensureRepoReady`/bare-clone health — a property of the repo's
git remote, not of which base a worker happens to be forming a batch for. Re-keying it would
fragment one genuine repo-level failure signal into N spurious per-base ones for what is
structurally the same underlying wedge.

### 5. Repo-wide "any base" queries become prefix scans, not new bookkeeping

Two call sites genuinely need a repo-wide "is anything live" answer, not a single (repo,base)-exact
lookup, because they have no way to know (or no reason to care) which specific base a stranded item
belongs to:

- `closed_item_advance_settle.go`'s stranded-closed-item rescue, which only needs "don't race *any*
  live worker for this repo." `mergeTrainWorkerActive` is repurposed (not duplicated) into
  `mergeTrainWorkerActiveForRepo`, backed by a new `Store.RepoWorkerActiveForAnyBase` — an exact
  match on the bare `repoKey`, OR a prefix scan for any key starting `repoKey + ":"`. This mirrors
  `resetTrialCounter`'s pre-existing prefix-scan-and-delete precedent over
  `mergeTrainRunawayAlerted`, rather than introducing a second, separately-maintained reference
  count that could drift from the composite-keyed registry it shadows.
- The runaway-alert settle scan (`runaway_alert_settle.go`), working from a bare already-paused
  board item with no partition context at all. `mergeTrainKeyForItem` reconstructs the composite key
  by re-deriving the item's partition exactly the way `groupQueuedByRepoAndBase` would — the
  sentinel for a no-label item (zero-cost, no `WorktreeManager` needed), or a re-resolution via
  `baseBranchForItem` for a labeled one. This must reproduce the *identical* bucketing rule the live
  dispatch path used, or the reconstructed key silently disagrees with the one the item was actually
  alerted under, breaking `mergeTrainRunawayAlerted`'s idempotency check for exactly the common
  (no-label) case.

## Consequences

- A repo can now run several concurrent merge-train workers, one per active base — previously
  structurally impossible. `e.sem` (the engine-wide worker semaphore) and worktree-per-trial-name
  naming already bound the combined resource use correctly by construction (R5): each worker still
  acquires the same shared semaphore inside `prepareTrainWorker`, so N active bases in one repo
  cannot exceed the same total concurrent-worker budget N issues from N different repos already
  could. `TestDispatchMergeTrainWorker_SameRepoTwoBasesConcurrent` asserts this directly.
- `mergeTrainRunawayMu` remains a single, unsharded, engine-wide mutex (ADR-1533's own explicit
  design) — its blast radius widens to also serialize `fireRunawayGuard`/`settleRunawayGuardAlert`
  across concurrent per-base workers within the *same* repo, not just across repos. Same accepted
  trade-off as before (rare/exceptional event, bounded contention), just a larger set of callers
  that can now legitimately contend for it concurrently.
- ADR-1647's exclusion and its label (`fabrik:non-default-base-excluded`) are fully removed — a
  `base:<branch>` Queued member is now batched like any other, into its own partition, rather than
  being told the work is unbatchable.
- `docs/state-machine.md` and `docs/USER_GUIDE.md` are updated to describe partitioning instead of
  exclusion, and to note that batch-cap/runaway-guard scope is per (repo, base) partition, not per
  repo.

## Rejected Alternatives

- **Re-resolving `baseBranch` independently inside `prepareTrainWorker`** instead of threading the
  partition's already-resolved value through explicitly. Adds a redundant `DefaultBaseBranch()`
  call (or worse, a full `baseBranchForItem` re-resolution) per dispatch, and reopens a narrow risk
  window where the worker's own re-resolution could disagree with the partition that dispatched it
  if a defect ever crept into the grouping logic. Rejected in favor of explicit threading — the
  partition that formed a batch has already resolved its base once; the worker uses that value
  as-is.
- **Eagerly resolving every item's real base at grouping time**, including default-base items — see
  Decision §2. Rejected because it makes the common-case path depend on git succeeding just to form
  the *default* partition, a regression from both ADR-1647's and this issue's own zero-cost
  requirement for the overwhelming common case.
- **A second, separately-maintained repo-level reference count** for "is any base active"
  queries, instead of a prefix scan over the composite-keyed registries. Rejected: it introduces a
  second piece of state that must never drift from the one it shadows, for no benefit over the
  already-precedented prefix-scan pattern.
