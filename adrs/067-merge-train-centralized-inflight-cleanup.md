# ADR 067: Centralized Merge-Train In-Flight Marker Cleanup

**Date**: 2026-07-21
**Status**: Accepted
**Issue**: #1040 — Decompose remaining oversized functions (deferred from #1029)

## Context

`engine/merge_train.go`'s `runMergeTrainWorker` (197 lines) was one of the remaining oversized functions named by the 2026-07-20 code-health audit. Beyond its size, the function had a latent correctness hazard: `e.mergeTrainInFlight.Delete(repoKey)` — the call that releases the per-repo in-flight guard so `dispatchMergeTrainWorker` can start a fresh train on a later poll — was duplicated at 16 separate call sites: 12 inline early-returns inside `runMergeTrainWorker` itself, plus one each in `landSingleton`, `dissolveBatch`, and the `landMergeTrainBatch`/`resumeTrain` area. Any new early-return added to this call graph without also adding its own `Delete` call would permanently wedge that repo's merge train — `dispatchMergeTrainWorker`'s `LoadOrStore` check would keep finding the stale entry and silently skip every future batch for that repo, with no self-healing path.

All 16 sites are reachable only from `runMergeTrainWorker`'s own synchronous call stack (via `landGreenBatch`/`handleRedBatch`/`landOneAtATime`/`reconstructTrainState`), never from a separate goroutine or entry point — a single `defer`, registered once at the top of the worker, can dominate every return path in the whole call graph, including returns from deeply nested helpers.

## Decision

Split `runMergeTrainWorker` into two functions:

- **`prepareTrainWorker(ctx, state, owner, repo, batch) (p trialParams, members []trainMember, ok bool)`** covers everything before the re-form loop: semaphore acquisition, `ensureRepoReady`, base-branch resolution, holding-stage lookup, extend-turns computation, `trialParams` construction, `reconstructTrainState` (a `true` return means the train was fully handled elsewhere — treated as `ok=false`), base-SHA pinning, and `fetchTrainMembers`. It holds its own `defer func() { if !ok { <-e.sem; e.finishTrain(repoKey) } }()`, registered immediately after acquiring the semaphore, so every one of its four early-return failure paths (context cancelled before acquiring, `ensureRepoReady` failure, base-branch resolution failure, no holding stage configured, `reconstructTrainState` already handling the batch, base-SHA pin failure) collapses into one cleanup call instead of six. On success it returns with the semaphore still held — ownership transfers to the caller.
- **`runMergeTrainWorker`** calls `prepareTrainWorker`; on `!ok` it simply returns (cleanup already ran). On `ok` it registers `defer func() { <-e.sem }()` and `defer e.finishTrain(repoKey)`, then runs the unchanged re-form loop with every inline `e.mergeTrainInFlight.Delete(repoKey); return` collapsed to a plain `return`.
- **`finishTrain(repoKey string)`** is a one-line wrapper around `e.mergeTrainInFlight.Delete(repoKey)`. It is the *only* place that call may appear. `sync.Map.Delete` on an absent key is a safe no-op, so the two remaining call sites (inside `prepareTrainWorker`'s own defer, and `runMergeTrainWorker`'s top-level defer) never need to coordinate.
- The 4 `Delete` calls previously inline in `landSingleton`, `dissolveBatch`, and the `landMergeTrainBatch`/`resumeTrain` area are removed entirely; the top-level defer in `runMergeTrainWorker` (or, for the `reconstructTrainState`-routed paths, `prepareTrainWorker`'s own defer) now covers them, since every one of those helpers is called synchronously from within one of the two functions above.

**Invariant**: any future early-return added anywhere in this call graph (`runMergeTrainWorker`, `prepareTrainWorker`, or any helper they call synchronously) must **not** call `mergeTrainInFlight.Delete` or `finishTrain` directly. It must rely on one of the two existing defers. Adding a new direct call would not be incorrect on its own (the no-op-on-absent-key property is forgiving), but it would reintroduce the exact scattered-call-site pattern this ADR removes, and the next early-return added without one would reproduce the original hazard.

## Rationale

### Why a defer-based design instead of a return-value protocol?

A return-value protocol (each helper reports "did I already finish the train?" up the call chain) was considered, since some helpers (`dissolveBatch`, `landMergeTrainBatch`) previously cleared the marker themselves at what looked like a natural point of "this call fully disposed of the batch." But every one of those call sites is already synchronous with `runMergeTrainWorker` or `prepareTrainWorker` — there is no concurrent path that could race a `defer` against a helper's own cleanup. Threading an `ok`/`done` value up through `reconstructTrainState` → `completeDeferredLanding`/`resumeTrain`/`dissolveBatch` → `landMergeTrainBatch` would add a parallel signaling mechanism to do exactly what a single `defer` already guarantees, for no additional safety.

### Why does `prepareTrainWorker` own the semaphore acquisition and release-on-failure, rather than the caller?

The semaphore must stay held for the entire re-form loop (an unbounded number of trials/bisections), so only `runMergeTrainWorker` can own releasing it on the success path — it doesn't know when the loop will end at construction time. But `prepareTrainWorker` is the one place that decides whether setup succeeded, so it is also the only correct place to release the semaphore on a setup failure: if it returned `ok=false` while leaving the semaphore held, the caller would have no way to know whether a slot was acquired at all (the very first failure branch — context cancelled — never acquires it).

## Consequences

**Positive:**
- A single, auditable invariant ("only two call sites, both `defer`, may ever call `finishTrain`") replaces 16 scattered call sites — a missed `Delete` call in a new early-return is now a build-time-obvious deviation from the pattern rather than a silent, permanently-wedged train discovered only in production.
- `runMergeTrainWorker` itself shrinks to just the re-form loop, with all one-time setup isolated in `prepareTrainWorker` — independently testable (see `TestPrepareTrainWorker_FailurePathClearsMarkerAndSemaphore`).

**Negative / Trade-offs:**
- Several existing unit tests called `landMergeTrainBatch`, `dissolveBatch`, and `reconstructTrainState` directly (bypassing `prepareTrainWorker`/`runMergeTrainWorker`) and asserted `mergeTrainInFlight` was cleared as a side effect of the call under test. Since clearing responsibility moved out of those functions, those specific assertions were removed (not the tests themselves) — end-to-end coverage of the clearing behavior lives in the `runMergeTrainWorker`-level tests (e.g. `TestMergeTrainWorker_CleanBatch`, `TestMergeTrainBisect_GreenCommonPath`) plus the new `prepareTrainWorker`-level regression test above.

## Sibling Audit

None — this ADR addresses the single, fully-enumerated `mergeTrainInFlight.Delete` call-site inventory confirmed during this issue's Research stage (16 sites, all reachable only from `runMergeTrainWorker`'s synchronous call graph). No other subsystem in this codebase uses a comparable scattered-cleanup pattern over a `sync.Map` in-flight guard.

**References:** [ADR-059: Internal Merge Train](059-internal-merge-train.md)
