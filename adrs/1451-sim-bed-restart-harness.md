# ADR 1451: `RestartEnv` — Restart Recovery Across a Genuine Process-State Boundary

**Date**: 2026-08-12
**Status**: Accepted
**Issue**: #1451 — test: sim scenarios for settle escalation, cycle limits, restart
recovery, and rate limits

## Context

Fabrik's durable-state contract is that GitHub-side state (labels, comments, board
position, the git repo itself) survives an engine restart, while in-memory state
(`processedSet`, the worker semaphore, per-item locks) does not. Nothing tested that
contract before this issue: every existing `tests/sim` scenario drives one long-lived
`*engine.Engine` from `NewEnv` to completion and never discards it. Proving restart
recovery needs a genuine process-state boundary — discarding the `*engine.Engine` value
and its in-memory maps entirely and reconstructing a fresh one — not just another poll
cycle against the same process, which is what every other scenario in this package
already exercises.

`tests/sim/simgh`'s `Instrumented.Snapshot`/`simgh.RestoreInstrumented` (added for R8 of
the sim harness's own build-out) is the mechanism this ADR builds on: it round-trips the
GitHub-side model, the git repos, the fault schedule, and the mutation log to a fresh
`baseDir`. It was confirmed unused outside `simgh`'s own package tests before this issue
— R3's restart scenarios are its first real consumer.

## Decision

Add `tests/sim/restart.go`'s `RestartEnv(t *testing.T, env *Env) *Env`: snapshots
`env.Sim` via `Instrumented.Snapshot`, restores it via `simgh.RestoreInstrumented`, and
builds a fresh `*engine.Engine` via `engine.NewWithDeps` — but reuses three pieces of
`env`'s own state rather than rebuilding them:

- **`env.WM` (`*engine.WorktreeManager`)** — the exact manager the original `Env` built,
  not a freshly-cloned one.
- **`env.Clock`** — the same injected clock, so a restart does not rewind simulated
  wall-clock time.
- **`env.Claude`** — the same scripted invoker, so a restart does not forget which
  scripts a scenario registered.

The returned `*Env` is otherwise independent: its own `Engine` and `Sim` fields point at
the newly restored model and engine. The caller must not poll the original `env` again
afterward — nothing enforces this, mirroring how a real restart leaves the old process
simply gone.

### Two load-bearing correctness properties

**1. The snapshot must go through `Instrumented.Snapshot`, never the bare
`Sim.Snapshot`.** `RestoreInstrumented` refuses a snapshot that didn't carry the fault
schedule and the mutation log. This is deliberate, not an oversight to route around: a
scenario relying on a persistent fault (e.g., an in-progress settle-scan retry) to
survive the restart must not silently see GitHub "heal" itself across the boundary,
which is exactly what a fault-schedule-losing snapshot would produce — a restart
scenario that looks like it proved recovery but actually just proved the fault was
gone.

**2. The rebuilt Engine must reuse the original `Env`'s `WorktreeManager`, never a
freshly-cloned one.** This is the single highest-risk mistake `RestartEnv` exists to
prevent. A `WorktreeManager` built by re-cloning `origin.git` into a new `t.TempDir()`
would produce an empty worktree with nothing to recover — any scenario built on top of
it would then pass by accident (there was never anything to lose) rather than by
proving recovery, which is exactly the AC8 non-vacuity risk this issue's own Plan stage
flagged before a single scenario was written. The `WorktreeManager`'s own worktree root
is just a directory under `t.TempDir()`; it survives constructing a second
`engine.Engine` in the same test process with no filesystem-level restart simulation
needed — discarding only the Go `*engine.Engine` value is sufficient to model "the
process restarted," because the git-side state was never engine-owned in the first
place.

`Env` gained a new exported field, `WM *engine.WorktreeManager`, populated in `NewEnv`,
purely so `RestartEnv` has something to reuse — `Env` did not previously retain the
manager it built (only threaded it into `engine.NewWithDeps` once, at construction).

### Why this is not a third production engine seam

ADR-1449 deliberately minimized the sim harness's footprint in production code to two
seams: `Engine.PollOnce` and `Engine.Clock`/`SetClock`. `RestartEnv` adds no new
production-code seam — it is pure test-side composition of `engine.NewWithDeps`
(already exported for `tests/sim`'s own construction path) and
`RegisterWorktreeManagerForTest` (already exported for the multi-repo `SecondRepo`
path). A restart is legitimately just "construct a new Engine value," which the
existing constructor already supports; nothing about simulating a restart required
touching `engine/` itself.

### `TestRestartEnv_RoundTrip` as `RestartEnv`'s own foundation test

Every R3 scenario (`restart_recovery_test.go`) depends on `RestartEnv` reusing the
right worktree state. `TestRestartEnv_RoundTrip` (`tests/sim/restart_roundtrip_test.go`)
isolates that dependency. Note that a *pushed* commit SHA is not, on its own, a
sufficient check here: it already exists on `simgh`'s backing remote, so even a broken
`RestartEnv` that discarded `env.WM` and rebuilt a fresh, re-cloned `WorktreeManager`
would still observe it — a head-SHA comparison alone would pass by accident (this
inconsistency was caught in review of PR #1584 and fixed there; an earlier draft of
this ADR and the test's own doc comment both asserted a SHA check existed and was
load-bearing when neither was true). The check that actually distinguishes genuine
`env.WM` reuse from a fresh re-clone is *local, unpushed* worktree content — exactly
the "partial work" CLAUDE.md's Worktrees section says must never be destroyed — so the
test plants an uncommitted marker file directly in the worktree before restarting and
asserts it survives, byte-for-byte, at the identical path afterward. The test also
confirms the rebuilt `Engine` is not merely present but functional (it keeps driving
the same issue to `Done`). A defect in `RestartEnv` itself is meant to surface here,
not as a confusing failure three layers into an unrelated kill-point scenario.

## Consequences

- Four kill-point scenarios (`restart_recovery_test.go`) and one partial-mutation
  scenario (`partial_mutation_test.go`'s spawn-sequence coverage) are built directly on
  `RestartEnv`, sharing one restart mechanism rather than each reimplementing a
  discard-and-rebuild step.
- Two of those scenarios pin genuine as-found defects — `MarkPRReady`'s failure is never
  retried by anything (a draft PR can stay draft forever, permanently blocking the
  direct-merge fallback), and `spawnChildren`'s per-child sequence has no memory of a
  partial prior attempt across a pause/retry cycle (an operator-directed retry creates a
  duplicate child issue). Both are filed as follow-up issues per this issue's own
  explicit "discover, don't fix" scope — see the PR description for the tracking issue
  numbers.
- `RestartEnv` is intentionally narrow: it does not attempt to simulate a restart mid
  in-flight-worker (a worker goroutine cannot itself be "killed" mid-invocation this
  way — the fault-then-restart pattern instead controls exactly which GitHub mutation
  landed before the discard, which is the granularity every kill-point scenario
  actually needs).
