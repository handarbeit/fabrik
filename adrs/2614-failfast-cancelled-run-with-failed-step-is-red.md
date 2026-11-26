# ADR 2614: A fail-fast-cancelled run with a failed step is red

## Status

Accepted (#2614). Amends ADR 2296. Restates community report #2609 (mentioned for context).

## Context

ADR 2296 reads a `cancelled`, `stale`, empty or unrecognised latest check run as "supposed to run, has no result": the merge train abandons such a trial as infrastructure (`TrainCIInfra`, uncharged, members Queued, partition cooldown) and the stage gate dispatches a CI-fix with wording that says the check was *cancelled*, not that a test regressed. Its premise, stated in the ADR, is that *nothing the members did produced it*.

That premise is false for a repository that fails fast: when a job fails, a final step cancels the whole workflow run so siblings stop. GitHub then reports the check runs of that run — frequently including the job that really failed — as `cancelled`. The failure survives only in the run's job steps, where the failing step concluded `failure`. On liminisapp/liminis-context-graph (2026-11-22) a batch of four was retried for about an hour and never bisected to the member that broke a test, and the CI-fix pass recorded a no-op until a human said the failure was real.

## Decision

1. **Promote after the read, classify as before.** `Engine.promoteFailFast` (`engine/ci_failfast.go`) runs right after every `FetchCheckRuns` that feeds a classification. For each completed, `cancelled`, latest-per-name check run with an Actions run behind it (`ActionsRunIDFromDetailsURL`) it reads the run's jobs and steps; if any step of any job concluded `failure`, the run is returned as a copy with `Conclusion: "failure"`, the real conclusion in `ObservedConclusion` and the culprit in `FailedJob`/`FailedStep`. Every existing consumer then sees an ordinary failure, so the trial, the singleton catch-up wait, admission, the landing settle, the stage gate, the CI-fix comment and `ci-status.md` cannot disagree — the drift ADR 2296 was written to prevent. The rejected alternative, a `stepFailed()` check in each non-passing branch, touches more code and would leave `settlePRMergeState` and `classifyLandingCI`, which the issue's scope list does not name, reading the same head differently. `TestFetchCheckRunsCallersPromoteOrAreAudited` requires every non-test `FetchCheckRuns` call in `engine/` to promote or carry a `// failfast-audited: <reason>` marker, so a new consumer cannot skip it.
2. **Run-level attribution (R1).** Any failed step of any job in the run promotes every cancelled check run belonging to it. Naming prefers the check run's own job (job ID, then name) and falls back to the run's first failed job; all promoted runs are listed as failing in `ci-status.md`.
3. **One-time failed-job re-run is kept (FR-008, R2).** A promoted run falls into each consumer's existing `Failed` branch, which already re-runs the failed jobs once (#2052 R5; #2072 for the gate) and only then returns `TrainCIRed` / `memberCIRed` / a CI failure. This catches a flaky test, needs no new budget or persisted state (check-run IDs are unchanged by promotion, so the stale-failure guard still works), and costs at most one CI cycle bounded by `rerunSettleDwell + rerunMaxWait` against an hour of looping. A re-run that is fail-fast-cancelled again is promoted again and goes red. ADR 2296 never verified that `rerun-failed-jobs` re-runs cancelled jobs; if it no-ops, the dwell bounds the delay and the verdict is still red.
4. **Fail safe (R5).** A failed jobs read (including `ErrForbidden`/`ErrNotFound`), a client without the capability, a check run with no Actions run (a third-party check), a truncated or incomplete job list and a run with no failed step all leave the run exactly as GitHub reported it. A failed read is logged once per `owner/repo@sha` (TTL 24 h), deliberately not through the process-global `logActionsDegradedOnce`, and is never cached as "no failed step".
5. **The jobs read is an optional capability, not a new `GitHubClient` method.** `github.Client.FetchWorkflowRunJobs` (`?filter=latest&per_page=100`, paginated and `total_count`-verified, failing closed like `FetchCheckRuns`) is reached through `workflowJobFetcher`, asserted at the call site like `checkRunAnnotationFetcher`; the interface and every mock stay untouched, and a client without it degrades to today's classification.
6. **Memo.** Definitive answers are memoised per check-run ID (bounded to 512). A re-run produces new check-run IDs, so a stale "no failed step" cannot be served for a re-run attempt. A "no failed step" answer is cached only once every job of the run has completed.
7. **Only `cancelled` is examined.** Fail-fast produces `cancelled`; `stale`, empty and unrecognised conclusions have no known failure shape and would only cost reads. A cancelled run with no failed step (`cancel-in-progress`, cancelled by hand) is unchanged (R4).
8. **R6 (self-cancel versus external cancel) is not implemented.** The jobs API names no cancelling actor, and the step test already separates the cases the requirements need.
9. **The `mergeable_state == "clean"` shortcut reads unpromoted runs.** A cancelled run is non-passing there, so the shortcut falls through to the classified read, which promotes. Promoting inside the shortcut would instead let a `clean` head with a fail-fast failure clear the gate, as a failed non-required check does today.

## Assumptions and risks

- A step's reported `failure` conclusion is authoritative; no log text is parsed. How the API reports a failed `continue-on-error` step, and whether the job ID equals the check-run ID for an Actions check run, were not verified live; attribution falls back to job name and then the run's first failed job.
- **Over-promotion:** a repository whose cancel step follows a deliberately non-blocking failed step (an optional lint job) now reads red and can raise ejection counts. This is the accepted trade-off of R1.
- **Fail-open cases remain** (permission revoked at runtime, third-party checks, truncated lists), where today's behaviour applies and the incident can still recur.
- Cost: about one extra REST call per cancelled check run per head, then memo hits; green and pending sets pay nothing.

## Consequences

A fail-fast trial reaches a red verdict after the single re-run and bisects to the offending member; the CI-fix worker is told the failure is a regression and which job and step failed. `SetFailFastPromotionDisabledForTest` restores the ADR 2296 reading for non-vacuity tests. Sim twins: `tests/sim/mergetrain_failfast_test.go`, `tests/sim/ci_failfast_gate_test.go`; the sim gains a jobs-with-steps model (`SeedWorkflowRunJobs`, `RerunStep.Jobs`, `FIDELITY.md`). Documented in `docs/state-machine.md` §6.29 and `docs/USER_GUIDE.md`.
