# ADR 2072: The stage `wait_for_ci` gate re-runs failed jobs once per head before and after a CI-fix dispatch

## Status

Accepted (#2072). The stage-gate sibling of ADR 2052 decision 6 (the train's R5 re-run).

## Context

The merge train re-runs a trial's failed jobs once before treating it as red (ADR 2052). The stage gate did not: a confirmed check-run failure always dispatched `ci-fix-reinvoke`. A worker that found the failure flaky and pushed nothing recorded a no-op for the head, every later poll skipped dispatch, and the item sat red until `CIBackstopTimeout` paused it (#2044, PR #2061: a human re-run would have taken a minute).

## Decision

1. **A helper called from `handleMergeAndCIGates`, not a change to `checkCIGate`.** `ciFlakeRerun` runs in the `ciFailure` branch ahead of `dispatchWithCycleLimit`. `checkCIGate`'s 4-tuple contract and `classifyCIFromCheckRuns` are untouched, and one change covers both the poll loop and `settleAwaitingCIScan`, which run the same handler chain.
2. **Budget per PR head: one R1 re-run (before the first dispatch) and one R2 re-run (after a recorded no-op).** A new head resets both. `CIFixCycles` is never charged.
3. **A sibling engine-held map (`Engine.flakeReruns`), not an extension of `prStartupState` or an `itemstate` field.** `startupWatches` serves the zero-check-run path and its resets are tied to the startup-failure pause; `itemstate` would need new mutations and snapshot accessors and would not change the in-memory trade-off. The map reuses the same locking, TTL and head-reset pattern. A restart may grant one extra re-run per head (accepted in ADR 2052; persisting it would need a label).
4. **Stale-failure guard across polls.** The train's guard lives in one blocking loop; the gate is re-evaluated once per poll, so the failed check-run IDs at re-run time are stored. Only those IDs failing means the original failure: wait for `rerunSettleDwell`, then while a re-run workflow run is queued/running up to `rerunMaxWait`, then accept it as the verdict. A new failing ID counts at once. The dwell uses the engine clock (`e.now()`), which the sim advances between polls, not `ciInfraTiming.clock`.
5. **Mark before call, under `flakeRerunMu`.** The poll loop and the settle scan can reach the helper in the same window; the budget flag is set before the API call so a head is never re-run twice.
6. **Degrade, never pause (R4).** A check with no Actions run ID, `ErrForbidden`/`ErrNotFound` or any other error marks the head degraded: logged once, not retried for that head, and the gate falls back to today's dispatch. A failed attempt consumes the budget deliberately, so a refusal is not retried every poll.
7. **Scope.** Only `CheckRunsFailed` is re-run; a required-context- or commit-status-only failure has no check run and dispatches as before. A mixed set where one failure has no Actions run makes the whole set degrade (a missing Actions run ID is detected up front, so nothing is re-run). **Accepted edge:** when several workflow runs fail and an API error or refusal hits a later `RerunFailedJobs` call after an earlier one succeeded, the earlier run has already been re-run. The head still degrades to today's dispatch and the re-run's IDs are not tracked, so a CI-fix worker may be dispatched while that one re-run executes. This is the same behaviour as the train's `rerunFailedWorkflowRuns` (issue R4: "treat it as the train does"); it is rare (a refusal is almost always uniform across runs) and bounded by the cycle limit, and the worker only reads the then-current state, so it was not worth a second tracking path. Out of scope: the train, timeouts, the auto-merge convergence dispatch (`merge_gate.go`) and the App-permission notice.
8. **Test-only neutralisation seam.** `SetCIFlakeRerunDisabledForTest` lets each R1/R2 test have a twin that runs the same setup with the re-run off and asserts the opposite (a dispatch, no `RerunFailedJobs`).

## Consequences

A flaky red usually clears with no worker and no cycle spent. Without `actions: write` nothing changes. The cost is paid by every genuinely red head: its first CI-fix dispatch is delayed by at least `rerunSettleDwell`, and by up to `rerunMaxWait` more while the re-run is still queued or running. A head that a CI-fix worker then fixes gets a fresh R1 re-run and dwell before its next dispatch. See `docs/state-machine.md` §6.29.
