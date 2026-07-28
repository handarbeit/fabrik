# ADR 1222: Consolidate Merge-Train Worker Liveness Into `itemstate.Store`

**Date**: 2026-07-28
**Status**: Accepted
**Issue**: #1222 — Auto-upgrade `syscall.Exec` can fire mid-merge-train-landing: guard is blind to train workers

## Context

Auto-upgrade's idle guard (`engine/poll.go`, the `dispatched == 0` branch that increments
`e.idleCount` and, at `idleUpgradeThreshold`, calls `checkAndUpgrade()` → `syscall.Exec`) answered "is
any worker in flight" by scanning `e.store.All()` for `snap.Worker() != nil` — i.e. `itemstate.Store`'s
per-`(Repo, Number)` `WorkerEntered`/`WorkerExited` state.

`dispatchMergeTrainWorker` (`engine/merge_train.go`) never applied `WorkerEntered`. It registered
instead in a wholly separate `sync.Map`, `Engine.mergeTrainInFlight` (keyed `"owner/repo"`), used as
an atomic `LoadOrStore` duplicate-launch claim. The idle guard had zero visibility into this second
registry, so a merge-train worker — assembling a trial branch, polling trial CI, running Claude
conflict resolution, or landing a batch (`landSingleton`/`landMergeTrainBatch`) — was invisible to it.
With `AutoUpgrade: true` and `merge_train: on` (both live production configurations), two consecutive
idle polls during a live train landing would call `syscall.Exec`, replacing the process image mid-land
and interleaving arbitrarily with the PR merge, the member Done-advance, and the issue close — the
exact partial-failure class ADR-061 exists to repair after the fact, but here triggered gratuitously
by the upgrade itself rather than by an external fault.

A second consumer already existed for a liveness answer over `mergeTrainInFlight`:
`mergeTrainWorkerActive(repoKey)`, read by `settleClosedItemsToDone` to avoid advancing a closed
`Queued`-column batch member to Done out from under a live train. This confirmed the two-registry
split was not an idle-guard-only problem — it was a systemic "is a worker running" ambiguity, which
the issue's FR-2 explicitly named as the defect to fix: adding a second lookup to the idle guard
(checking both `e.store.All()` and `mergeTrainInFlight`) would reproduce the same defect in a new
place, not resolve it.

Complicating a full merge: `mergeTrainInFlight`'s value type, `mergeTrainWorkerState`, carries richer
per-worker state (`assembling`, `bisecting`, `prNum`, `CIResult`, `trialName`, `projectID`) that
`itemstate.WorkerHandle` (`PID`, `StageName`, `StartedAt`, `LastSignAt` — explicitly documented as
identifying an in-flight *Claude invocation*) has no equivalent for. And a merge-train worker operates
on a *batch* of issue numbers, not the single `(Repo, Number)` `itemstate.Store`'s existing worker
model is keyed on — there is no single item the worker naturally belongs to.

## Decision

Keep `mergeTrainInFlight` for exactly what only it can do — the atomic `LoadOrStore` duplicate-launch
claim and the `assembling`/`bisecting`/`CIResult`/`prNum`/`trialName` sub-state — and make
`itemstate.Store` the **single** source of truth for the liveness question ("is a worker running")
that both the idle guard and `mergeTrainWorkerActive` ask.

Concretely, `internal/itemstate/store.go` gains a second, independent liveness registry alongside the
existing per-`(Repo, Number)` `items` map:

```go
repoWorkers map[string]struct{}  // keyed "owner/repo"

func (s *Store) EnterRepoWorker(repoKey string)
func (s *Store) ExitRepoWorker(repoKey string)
func (s *Store) RepoWorkerActive(repoKey string) bool
func (s *Store) HasInFlightWorker() bool  // ORs per-item Worker() != nil with len(repoWorkers) > 0
```

- `dispatchMergeTrainWorker` calls `Store.EnterRepoWorker(repoKey)` synchronously, immediately after
  its `mergeTrainInFlight.LoadOrStore` claim succeeds and before the worker goroutine is launched.
- `finishTrain` — already the single ADR-067-mandated clear point for `mergeTrainInFlight`, called
  from `prepareTrainWorker`'s own-failure defer and `runMergeTrainWorker`'s top-level defer — also
  calls `Store.ExitRepoWorker(repoKey)`. Every existing early-return path that already funnels through
  `finishTrain` clears both registries in lockstep, with no new call sites to audit.
- `mergeTrainWorkerActive(repoKey)` now returns `Store.RepoWorkerActive(repoKey)` instead of reading
  `mergeTrainInFlight` directly.
- The idle guard in `engine/poll.go` replaces its manual `e.store.All()` scan with a single call to
  `Store.HasInFlightWorker()`.

No `Mutation`/`Apply`/observer wiring was added for `repoWorkers` — nothing needs to *wake* on it.
Merge-train dispatch and the idle check both run synchronously within the same poll cycle
(`dispatchCandidates` → `handleMergeTrainBatch` → `dispatchMergeTrainWorker`, all ahead of the
`dispatched == 0` idle-guard block), so there is no cross-goroutine signal to propagate. `Store`
already exposes plain non-`Mutation` methods (`Get`, `All`, `Remove`); `EnterRepoWorker` et al. follow
that existing precedent rather than introducing a new pattern.

`TestIdleCountNotIncrementedWhileWorkersInFlight` (`engine/engine_test.go`) was rewritten to construct
a real `*Engine` via `NewWithDeps` and drive `eng.poll(ctx)` end-to-end (mirroring
`TestPoll_InFlightWorker_NotSupplanted`'s template), covering three cases: no worker in flight
(`idleCount` increments), a per-item `WorkerEntered` worker in flight (`idleCount` stays 0), and a
`EnterRepoWorker` merge-train worker in flight (`idleCount` stays 0). `AutoUpgrade: true` is set to
exercise the real gate, but the test board has zero dispatchable items, so `idleCount` is never driven
past 1 — `checkAndUpgrade`'s real `syscall.Exec` is never reachable regardless of outcome. Previously
this test never called `poll()` at all; it re-implemented the guard's if/else inline in the test body
and asserted against its own copy, so the production guard could be deleted entirely and the test
would still pass.

## Rationale

### Why extend `itemstate.Store` rather than replace both registries with one new type?

`mergeTrainInFlight` has roughly 40 direct test call sites across `merge_train_test.go`,
`poll_test.go`, `closed_item_advance_settle_test.go`, and `merge_train_member_close_settle_test.go` —
almost all exercising `dispatchMergeTrainWorker`'s dedup logic or `mergeTrainWorkerState`'s sub-fields,
none of which needed to change. Only one production call site outside `merge_train.go` itself
(`closed_item_advance_settle.go`, via `mergeTrainWorkerActive`) read `mergeTrainInFlight` for a
liveness answer. Redirecting that one call site — plus the idle guard — to `itemstate.Store` fully
satisfies FR-2 ("single authoritative answer") without touching the claim/sub-state mechanism or its
large existing test surface at all. A full registry replacement would have been mechanically much
larger for no behavioral gain.

### Why not force the atomic claim into `itemstate.Store` too?

`itemstate.Store.Apply` takes a lock per mutation but has no native check-and-register primitive
across a batch of `(Repo, Number)` keys the way `sync.Map.LoadOrStore` does for a single string key.
Building one in would be scope creep into `itemstate`'s existing per-`(Repo, Number)` charter, for a
requirement (repo-level atomic claim) that `sync.Map.LoadOrStore` already satisfies cleanly. Keeping
the claim mechanism where it is preserves the atomicity guarantee `dispatchMergeTrainWorker`'s
duplicate-launch check depends on without any redesign risk.

### Why a dedicated `repoWorkers` map instead of synthetic `(Repo, Number)` entries?

Threading merge-train liveness through fake per-item `WorkerEntered` calls (e.g. on a sentinel issue
number, or on every batch member) would make it visible to every other `e.store.All()` consumer that
assumes real board items — `worker_liveness.go`'s stale-worker detector, `terminal.go`, startup
cleanup, `poll.go`'s dispatch-dedup guard — for no benefit; none of those consumers have a reason to
know about a repo-level worker. A dedicated `repoWorkers` map keeps the blast radius to exactly the
four new methods and their two call sites, while still living inside `itemstate.Store` — consistent
with ADR-036's charter that `Store` is the single owner of per-item (and, now, per-repo) engine state.

This also sidesteps the batch-shrinks-over-worker-lifetime shape mismatch Research raised: a
merge-train worker's survivor set shrinks via `ejectMember` and bisection while the worker itself is
still running. Tracking liveness at the whole-worker lifecycle (`dispatchMergeTrainWorker` entry →
`finishTrain`) rather than per ejected batch member sidesteps the question of whether an ejected
member should immediately stop counting as "in flight" — out of scope for this issue, which only needs
to prevent the upgrade race, not model per-member ejection precision.

### Why is a PID-less liveness entry safe?

Unlike `itemstate.WorkerHandle` entries (which track a Claude subprocess `PID` for staleness
detection), `repoWorkers` entries carry no PID and are invisible to `worker_liveness.go`'s stale-worker
reaper (`isWorkerStale` only inspects `items`, never `repoWorkers`). This matches pre-existing
behavior — the reaper never touched `mergeTrainInFlight` either — and is intentional: a merge-train
worker is not itself a single subprocess (it spawns per-member Claude conflict-resolution
subprocesses internally), so there is no PID to attach to a repo-level entry, and no independent
staleness signal to derive one from.

## Consequences

- **Single authoritative liveness answer.** `Store.HasInFlightWorker()` is now the one function both
  the auto-upgrade idle guard and (transitively, via `mergeTrainWorkerActive`) `settleClosedItemsToDone`
  read. A future liveness consumer has one obvious place to look, closing the FR-2 gap for good rather
  than adding a third parallel check.
- **Same-cycle fix, not just cross-cycle.** Because `EnterRepoWorker` fires synchronously inside
  `dispatchMergeTrainWorker` before it returns — and `dispatchCandidates` never counted merge-train
  dispatch in its own `dispatched` return value — the very poll cycle that just launched a merge-train
  worker no longer misreports itself as idle either.
- **`mergeTrainInFlight` is not removed.** It remains the atomic duplicate-launch claim and the home
  of `mergeTrainWorkerState`'s richer sub-state. Two registries still exist, but only one answers "is a
  worker running" — the two are no longer in tension, because `mergeTrainInFlight` no longer has any
  liveness readers of its own.
- **No new observer/wake wiring.** `repoWorkers` mutations do not emit `Change`/`ChangeFlags` and do
  not participate in the `wakeChFlags`/`cycleSetFlags` split (ADR-039). This is safe because dispatch
  and the idle check already run synchronously in the same poll cycle; if a future consumer needs to
  *wake* on `repoWorkers` changes, that consumer will need its own justification for why
  (ADR-039's exclusion of `WorkerLifecycleChanged` from `cycleSetFlags` was itself a deliberate,
  reasoned choice, not a default).
- **Docs updated in the same change.** `docs/state-machine.md` §9.2 (auto-upgrade guard) and the
  Merge-Train Landing Lifecycle / Serialization sections (`mergeTrainInFlight` lifecycle,
  `mergeTrainWorkerActive`, the Holding-stage terminal-advance exception) now describe
  `Store.HasInFlightWorker()`/`Store.RepoWorkerActive()` as the liveness path, per this repo's
  canonical-docs convention.

## References

- ADR-036 (`036-reactive-cache-single-owner.md`) — establishes `itemstate.Store` as the single owner
  of per-item engine state; this ADR extends that charter to a new per-repo liveness concept rather
  than introducing a competing owner.
- ADR-039 (`039-cycleset-excludes-worker-lifecycle.md`) — the wake/cycleSet exclusion rationale that
  justified adding no observer wiring for `repoWorkers`.
- ADR-059 (`059-internal-merge-train.md`) — the merge train's design doc; `mergeTrainInFlight`'s
  atomic-claim role, preserved unchanged here, is central to its serialization guarantee.
- ADR-067 (`067-merge-train-centralized-inflight-cleanup.md`) — establishes `finishTrain` as the sole
  legal clear point for `mergeTrainInFlight`; this ADR piggybacks `Store.ExitRepoWorker` onto that same
  invariant rather than introducing new clear-point call sites to audit.
- ADR-1072 (`1072-holding-stage-terminal-advance.md`) — the Holding-stage terminal-advance exception in
  `settleClosedItemsToDone`, whose `mergeTrainWorkerActive` check now reads through this ADR's
  consolidated registry.
