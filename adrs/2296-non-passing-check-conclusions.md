# ADR 2296: Non-passing check-run conclusions

## Status

Accepted (#2296).

## Context

`gh.ClassifyCheckRuns` is the shared classifier behind every "is this head green" decision. It put a completed run in `failed` only for `failure`, `timed_out` and `action_required`; every other completed conclusion — `cancelled`, `stale`, an empty conclusion, any value GitHub adds later — fell through to `CheckRunsReady`. A required check that an operator cancelled (to free a self-hosted runner) therefore cleared the stage `wait_for_ci` gate, let a merge-train member be admitted or take the singleton fast path, declared a trial green and was served as ready from the board cache. The worker's `ci-status.md` already demoted such a head, so the worker and the gates disagreed.

## Decision

Three conclusion classes: **passing** (`success`, `neutral`, `skipped`), **failure** (`failure`, `timed_out`, `action_required`) and **non-passing** (everything else). `ClassifyCheckRuns` keeps its three-value signature and gains a fourth status, `CheckRunsNonPassing`, with precedence pending > failed > non-passing > ready; `NonPassingCheckRuns`, `IsPassingConclusion` and `IsFailureConclusion` are the shared primitives. A mixed set is `Failed`: the stronger disposition wins. Every consumer that compares `== CheckRunsReady` (board cache READY-on-trust, `pr_facts`) is correct without an edit; the ones that switch on `Failed`/`Pending` are edited explicitly.

Dispositions, because "no result" is neither green nor the members' fault:

- **Stage gate:** a verdict, never a wait (ADR-1410). `settlePRMergeState` returns `PRMergeBlocked`; `classifyCIFromCheckRuns` returns `ciFailure`; `ciFlakeRerun` re-runs the cancelled jobs under the existing #2072 budget; then the existing CI-fix reinvoke dispatches under `MaxCiFixCycles` with wording that says *cancelled*. Treating cancelled as pending is rejected: it would wait up to `CIBackstopTimeout` on a run that will not finish. For `wait_for_ci` stages the `mergeable_state == "clean"` shortcut falls through to this classification for a non-passing set only, using the cached read it already makes for #2052 R6; failure behaviour of the shortcut (ADR-1441) is untouched.
- **Trial CI and singleton catch-up:** one re-run (the #2052 R5 `rerunState`, persisted with the trial), then `TrainCIInfra` (trial) or `memberCINotPassed` (catch-up → build the trial). Reusing the infrastructure carrier gives "uncharged, no bisection, no ejection, no pause, members Queued, partition cooldown" without a new counter or persisted field. A still-non-passing run is not red: nothing the members did produced it.
- **Landing classification:** `classifyLandingCI` returns a new `TrainCINotPassed` (appended last), not `Red` (the catch-up would eject and pause) and not `Pending` (the landing poll would wait for the 4h backstop). The fast path declines; admission defers like a red member with cancelled wording and a kind-aware dedup key; `pollForMergeable` returns not-landable at once.
- **Read side:** `cachedCIVerdict` and the CI status verdict report `red` for a non-passing set, so no new enum value reaches MCP consumers. The "Failing checks" heading in `ci-status.md` is kept (skill wording is pinned); each non-failure entry carries its real conclusion and a note that it is not a test failure.

`skipped` and `neutral` stay passing everywhere: a skipped check means the workflow decided it does not apply to this head. ADR-933 explicitly rejected tightening the classifier for skipped/neutral; this ADR does not reopen that — it tightens only conclusions that mean "supposed to run, has no result". ADR-933 required-context semantics (only `success` satisfies a required context) and #2052 startup-failure detection (a zero-job `cancelled` workflow run is not a startup failure) are unchanged.

## Re-run endpoint

GitHub's documentation describes `POST /actions/runs/{id}/rerun-failed-jobs` only as re-running "all of the failed jobs and their dependent jobs"; it does not say whether cancelled jobs count, and this was not verified with a live call. The implementation relies on it. If it silently no-ops, the existing stale-run dwell (`rerunSettleDwell` + `rerunMaxWait`) bounds the delay before the CI-fix dispatch or the infra abandon — a delay, never a wrong result. The switch to `POST …/rerun` would be a one-place change in `rerunFailedWorkflowRuns`. Recorded in `tests/sim/simgh/FIDELITY.md`.

## Consequences

- Operators who cancel CI runs see the stage hold and re-run instead of advancing; a train never lands on a cancelled run and never charges members for one.
- Operators who cancel a *non-required* check on a head GitHub reports `clean` will see a `wait_for_ci` stage re-run it rather than clear (ADR-1153's "no escape hatch" note applies equally).
- `TestSettle_MergeableStateUnstable_SkippedChecksOnly_ReturnsReady` listed `cancelled` among ready conclusions and was corrected; the `ci_status_test.go` rows for `cancelled`/`stale` became `red`.
