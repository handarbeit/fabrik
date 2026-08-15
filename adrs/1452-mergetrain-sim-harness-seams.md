# ADR 1452: Merge-Train Sim Harness — Seams, the Verdict Seeder, and a `RestartEnv` Fix

**Date**: 2026-08-13
**Status**: Accepted
**Issue**: #1452 — test: merge-train scenarios in the simulated bed (real git assembly,
per-SHA bisection)

## Context

The merge-train subsystem (`engine/merge_train.go`) is the highest-consequence, worst-
covered part of Fabrik: it assembles a trial branch from N member PRs via real git,
resolves conflicts inline via Claude, opens a CI PR, bisects on red, ejects members, and
lands — every decision destructive if wrong. Live coverage was four scenarios requiring
real PRs, real CI runs, and real merges on the test bed (`tests/e2e/mergetrain_*.go`),
enough to check the happy path and one bisection shape but nowhere near enough for a
subsystem whose interesting behavior is combinatorial in (batch size × which members are
red × which members conflict × where the process dies). #1440 — a red singleton
mis-bisected and ejected rather than reported as its own validation failure — was exactly
the kind of batch-size-1 edge a sim could have enumerated exhaustively for free.

This issue's own Research and Plan stages (see the issue's stage comments) established
the requirement: drive scenarios through the *real* `assembleTrialBranch` against a real
bare origin — never `trainValidateFn`, the existing membership-keyed test seam, since
using it would bypass assembly, conflict resolution, trial branches, and SHAs entirely
(the very things this issue exists to exercise). `reconstructTrainState`'s restart-
recovery logic is itself gated on `trainValidateFn == nil`, so using the seam anywhere in
scope would also silently disable R7's restart coverage.

## Decision

### Two new, additive `Engine` seams

**`Engine.SetTrainCIPollIntervalForTest(d time.Duration)`** overrides the two hardcoded
`time.After(30 * time.Second)` literals in `pollTrainCI`/`pollForMergeable`
(`engine/merge_train.go`) via a new `trainCIPollInterval time.Duration` field, read
through `trainCIPollIntervalOrDefault()` (zero value → unchanged 30s default). Every
CI-wait retry — the initial trial, every bisection sub-trial, every landing poll — pays
this literal in production; at 30s each, a sim suite covering the poison matrix's dozen-
plus trials would either blow past `RunPoll`'s `workerQuiescenceTimeout` (60s) or make
the suite take many real minutes. `tests/sim` scenarios set this to 15ms.

**`Engine.SetGeneratedFilesForTest(path string, command []string)`** overrides the
`generatedFilesOverride` field merge-train's mixed-mode conflict handling
(`regenerateAndCommit`) reads to decide which conflicted path is a "generated" file that
must never be committed directly, vs. regenerated via a declared command. This field was
previously reachable only from within `engine`'s own unit tests; R4's third failure shape
(a scripted resolver committing a generated path it was told not to) structurally
requires declaring one from `tests/sim`, an external test package.

Both are placed beside the existing `RegisterWorktreeManagerForTest` in the engine's
"ForTest" seam family — additive, zero behavior change to any caller that never invokes
them, exactly the "documented minimal seam if one proves genuinely necessary" this
issue's own Scope anticipated.

### The verdict seeder: seeding by SHA without predicting one

`FetchCheckRuns` is strictly keyed by trial SHA, and neither the trial SHA nor the trial
branch/PR name is predictable in advance — `assembleTrialBranch`'s trial SHA is a genuine
`git merge` result, and its branch-name generator uses real `time.Now()`, not the
engine's injectable clock. `startTrialVerdictSeeder` (`tests/sim/mergetrain_helpers.go`)
resolves this as a background goroutine that polls `ListPRs` every 2ms, and the first
time it observes a new open PR whose head branch carries the `fabrik/merge-train/`
prefix, parses the member issue numbers out of the PR body's `Closes #N` lines and seeds
the scenario's declared verdict on that PR's real head SHA — via the raw (uninstrumented)
`*simgh.Sim`, so seeding itself never appears in a scenario's own mutation-log ordering
assertions.

This races the engine's own first `FetchCheckRuns` read on the same trial. The race is
accepted, not eliminated: a lost race reads zero check runs on a still-draft PR, which
`pollTrainCI` classifies as pending (never green — draft's `mergeable_state` fallback is
unreachable), so the worst case is one extra `SetTrainCIPollIntervalForTest`-scaled retry
before the seeder catches up, never a wrong verdict. `mergeTrainEnv`'s `CIBackstopTimeout`
is set to 10s (not the seeder's own sub-millisecond happy-path latency) specifically to
keep this safety net wide enough that ordinary contention in this package's own heavy
parallel real-git load — many scenarios' worth of concurrent merge/push/`git gc` — cannot
turn a transient scheduling delay into a spurious pending/timeout. An earlier 4s value
was raised to 10s after exactly that failure mode was observed empirically under `-race`
with the full merge-train suite running in parallel.

A formalized helper (rather than per-scenario inline seeding inside an `AdvanceUntil`
condition, the alternative Research's own Technical Questions raised) was chosen because
nearly every scenario in R2/R3/R5/R7 needs this exact mechanism, and duplicating a racy
polling loop per scenario file is a flakiness risk not worth accepting repeatedly.

**A discovered hazard in this helper's use, not the helper itself**:
`startTrialVerdictSeeder` starts one long-lived background goroutine per call, keyed on
its own private `seen` map. Calling it twice against the same `*Env` — even to
"override" an earlier declaration — starts two independent goroutines racing each other
against the *same* fresh trial PR, each seeding its own (potentially conflicting) verdict
on the same SHA. This produced a real, order-dependent flaky failure in an early version
of `mergetrain_redsingleton_test.go` (fixed in this issue's own PR, before landing) —
call it exactly once per `*Env`.

### R7 restart kill points: direct seeding, never a racing goroutine

Every return path inside `runMergeTrainWorker`'s loop calls `cleanupTrialArtifacts`
before returning, even on error — confirmed by reading every return site. No natural
in-process failure in this harness ever leaves a trial branch or PR orphaned the way a
real `SIGKILL` would. R7's three scenarios (`mergetrain_restart_test.go`) therefore
construct each of `reconstructTrainState`'s three durable-state shapes directly via
`simgh` seeding calls, then drive a genuine `RestartEnv` discard-and-rebuild
(`tests/sim/restart.go`, ADR-1451) and a subsequent poll — mirroring #1451's own
`restart_recovery_test.go` precedent exactly: fault-inject or seed the one state "the
last thing that happened" would leave, never interrupt a live dispatch.

Route 1 (`completeDeferredLanding`, a merged landing PR whose member is still Queued)
needed one further deliberate choice, worth recording since it is not obvious from the
scenario alone: the seeded landing PR's base is a throwaway non-default branch, not
`main`. `landMergeTrainBatch`'s integration PR body unconditionally carries `Closes #N`
per survivor, and GitHub (faithfully reproduced by `simgh`) auto-closes every such issue
the instant a PR carrying it merges into the repo's *actual default branch* — before the
engine's own follow-up bookkeeping ever runs. On the default branch there is therefore no
reachable window, in this harness, where a member is genuinely still open after a real
merge (`mergetrain_landing_test.go`'s `TestMergeTrainLanding_BatchCloseAsymmetryPin`, this
issue's own R6 scenario, hit the identical fact from the `landMergeTrainBatch`-asymmetry
angle). `reconstructTrainState`'s own Route-1 matching never inspects the found PR's base
branch, so seeding against a non-default base exercises the real recovery code
faithfully, not around it; in live production the equivalent trigger is understood to be
a narrow GitHub-side propagation window between the merge API call returning and the
auto-close side effect being processed, which `FIDELITY.md`'s Absent section already
excludes from this model's scope.

### A fix to `RestartEnv` itself, found (not invented) by this issue

Constructing the R7 scenarios surfaced a real, previously-undetected defect in
`tests/sim/restart.go`'s `RestartEnv` (ADR-1451): the reused `WorktreeManager`'s own
`origin` git remote was never re-pointed at the restored `Sim`'s new backing repository.
`RestoreInstrumented` always allocates a fresh `t.TempDir()` for the restored git state, but
`RestartEnv` reuses `env.WM` (`*engine.WorktreeManager`) *unchanged* — by design, per
ADR-1451's own "reuse, never re-clone" decision, which is correct for on-disk worktree
content but does not, on its own, keep `WorktreeManager.BaseDir()`'s own `origin` remote
config in sync with wherever the restored model's git content now actually lives.

A push through the stale remote does not error — it lands in a directory nothing reads
from afterward. This went undetected through #1451's own four kill-point scenarios and
`TestRestartEnv_RoundTrip` because every one of them reuses the same `fabrik/issue-N`
branch name across pre- and post-restart stages: that branch already existed (and was
copied forward by `Snapshot`) before the restart, so its *mere existence* — the only
thing `CreateDraftPR`'s branch check inspects — was enough to mask a misdirected push of
*new* commits after it. Merge-train's trial branches are a brand-new name every single
time (`baseTrialName`'s own `time.Now()`-based generator), with no pre-restart existence
to hide behind, which is what surfaced this: every real trial assembled after a restart
failed with `CreateDraftPR`'s own "head branch does not exist" error, on every attempt,
regardless of which R7 scenario was running or whether it seeded anything at all.

Fixed once, in `RestartEnv` itself (`git remote set-url origin <new bare repo dir>`
immediately after `RestoreInstrumented`, scoped to `env.OwnerRepo`), rather than patched
around in the merge-train-specific caller that happened to notice — every future
`RestartEnv`-based scenario that pushes a genuinely new branch post-restart benefits.
Confirmed the existing `restart_recovery_test.go` and `restart_roundtrip_test.go` suites
still pass unchanged. Not fixed: an analogous gap for a second-repo `Env`
(`EnvOptions.SecondRepo`)'s own beta `WorktreeManager`, which `RestartEnv` has no way to
reach at all today (`env.go` builds and registers it locally, never storing it back on
`Env`) — a separate, pre-existing limitation outside this issue's single-repo merge-train
scope, left as-found.

## Consequences

- `tests/sim/mergetrain_*.go` (nine files, one shared helper file) exercises the real
  assembly/bisection/conflict-resolution/ejection/landing/restart/mode-switch surface
  R1–R9 specify, none of them setting `trainValidateFn`.
- The `RestartEnv` origin-staleness fix is a durable improvement to the shared sim
  harness, not merge-train-specific, even though merge-train's own trial-naming scheme is
  what exposed it.
- `mergeTrainEnv`'s `CIBackstopTimeout` (10s) and `SetTrainCIPollIntervalForTest` (15ms)
  values are a deliberately generous safety margin against this package's own heavy
  parallel real-git contention, not a claim about the verdict seeder's typical latency
  (sub-millisecond in the common case) — see `tests/sim/README.md`'s Runtime section for
  the suite's measured wall-clock cost with this addition.
