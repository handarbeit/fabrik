# Feature Specification: A cancelled, stale or unrecognised check-run conclusion must not count as passing

**Feature Branch**: `fabrik/issue-2296`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "CI classification: a cancelled, stale or unrecognised check-run conclusion must not count as passing

`gh.ClassifyCheckRuns` (`github/checkruns.go:31-52`) reduces check runs to the latest run per name and puts a completed run in `failed` only when its conclusion is `failure`, `timed_out` or `action_required` (`github/checkruns.go:37-40`). Every other completed conclusion falls through to `CheckRunsReady`. That includes `cancelled`, `stale`, an empty conclusion on a completed run, and any value GitHub adds later.

`ClassifyCheckRuns` is the shared classifier behind every "is this head green" decision, so the permissive reading reaches all of them:

- **Stage `wait_for_ci` gate.** `settlePRMergeState` returns `PRMergeReady` ("all CI checks passed") when the classifier says ready (`engine/pr_settle.go:321-342`), and `classifyCIFromCheckRuns` (`engine/ci.go:283`) uses the same result. A PR whose required check was cancelled clears the gate and the stage completes.
- **Merge-train landing classification.** `classifyLandingCI` (`engine/merge_train.go:4274-4300`) returns `TrainCIGreen` for a cancelled set. Through it, `singletonFastPathEligible` (`engine/merge_train.go:1971`) can land a member's own PR directly, with no trial, on a cancelled run. `admitTrainMembers` admits the member (`engine/merge_train_admission.go:34`).
- **Trial CI.** `evalTrialCI` (`engine/train_ci_eval.go:71-127`) declares a trial green when its latest `test` run is cancelled.
- **Singleton catch-up.** `waitMemberCI` (`engine/merge_train_catchup.go:474`, classification at `:524`) accepts the same.
- **Flake re-run.** `ciFlakeRerun` (`engine/ci_flake_rerun.go:175`) never sees a cancelled run, because it acts only on `CheckRunsFailed`.
- **Read-side.** `boardcache` serves a cached READY set on trust (`boardcache/boardcache.go:995`), so a cancelled set is never confirmed live. `cachedCIVerdict` (`engine/channel_settled.go:403`) and `settlePRFacts` (`engine/pr_facts.go:225`) use the same classification.

The engine already knows. `ciStatusVerdictFor` (`engine/ci_status.go:181-202`) demotes a classifier-green verdict to `pending` unless every latest run is `success`/`neutral` (`allCheckRunsPassed`, `engine/ci_status.go:209-221`). Its comment at `:192-194` says \"a cancelled or unrecognised conclusion classifies as ready\". That demotion covers only the worker's `.fabrik-context/ci-status.md`. The gate and the train still use the permissive result. `docs/state-machine.md` (around line 632, the `PRMergeReady` paragraph) documents the same behaviour: \"only skipped/neutral/cancelled conclusions are present\" still yields ready.

How the reported failure arises: a downstream operator cancels CI runs that are about to be thrown away, to free a single self-hosted runner (paused issue, `fabrik:rebase-needed` branch, closed trial). When the issue resumes, the latest `test` run on its head is `cancelled`, the gate reads it as green, and the stage advances or the member is queued on a run that never finished. GitHub's own required-check rule still stops a real merge. But Fabrik advances a stage, admits a member, or takes the singleton fast path on no evidence.

`skipped` and `neutral` are different and correct as ready. A skipped check means the workflow decided the check does not apply to this head (for example a path filter on a docs-only PR). A cancelled check was supposed to run and has no result."

## Background

Fabrik decides whether a PR head is "green" from the check runs on it, through one shared classifier. That classifier currently treats a completed run as failing only for `failure`, `timed_out` or `action_required`; every other completed conclusion, including `cancelled`, `stale`, an empty conclusion and any value GitHub introduces later, counts as passing. Because every green decision uses this classifier, a cancelled check can clear the stage `wait_for_ci` gate, let a merge-train member be admitted or take the singleton fast path, declare a trial green, and be served as ready from the board cache, all with no evidence that CI ran.

The failure arises in practice when an operator cancels CI runs that are about to be discarded (paused issue, `fabrik:rebase-needed` branch, closed trial) to free a single self-hosted runner. When the issue resumes, its latest `test` run is `cancelled`, Fabrik reads it as green, and the stage advances or the member is queued on a run that never finished. GitHub's own required-check rule still blocks a real merge, but Fabrik advances on no evidence.

The engine already treats this case correctly in one place: the verdict written to the worker's CI status context file demotes a classifier-green result unless every latest run is `success` or `neutral`. The gates and the train do not share that behaviour, and the state-machine documentation records the permissive reading as intended.

`skipped` and `neutral` stay passing. A skipped check means the workflow decided the check does not apply to this head (for example a path filter on a docs-only PR). A cancelled check was supposed to run and has no result.

Related report: #2215 (mentioned for context only).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stage CI gate treats a cancelled check as a non-pass (Priority: P1)

An operator cancels the CI run on an issue's PR head. When the issue resumes and reaches a stage with `wait_for_ci`, the gate must not read the cancelled run as green. It gets the same re-run treatment as a failure, and if that does not produce a result, it falls back to the existing CI-fix cycle with an honest description of what happened.

**Why this priority**: This is the reported failure. Without it, stages advance on a run that never finished.

**Independent Test**: In the sim, seed a Validate PR whose required `test` check is cancelled and verify that the stage does not complete, that one re-run fires, and that the outcome then depends on the re-run result.

**Acceptance Scenarios**:

1. **Given** a Validate PR whose latest `test` run is `cancelled`, **When** the gate evaluates the head, **Then** the stage does not complete and the cancelled jobs are re-run once under the per-head flake re-run budget.
2. **Given** that re-run, **When** the scripted re-run result is `success`, **Then** the gate clears and the stage completes.
3. **Given** that re-run, **When** the scripted re-run result is `cancelled` again, **Then** a CI-fix reinvoke is dispatched whose context states the check was cancelled (not failed) and that a fresh run is needed (push, or re-run the check), counted under the existing CI-fix cycle accounting.
4. **Given** a cancelled latest run and an exhausted or unavailable re-run (no permission, no Actions run), **When** the gate evaluates, **Then** it dispatches the CI-fix reinvoke immediately and never waits silently for `CIBackstopTimeout`.

---

### User Story 2 - Merge train never treats a cancelled check as green or as the members' fault (Priority: P1)

An operator cancels a trial's CI run, a member's own CI run, or the run on a landing PR. The train must not land, admit or declare green on that basis, and must not charge, eject or pause members for a run that was merely cancelled.

**Why this priority**: The train lands code, so a false green is the most consequential case, and charging members for an infrastructure cancellation would be a new wrong behaviour.

**Independent Test**: In the sim, script a trial whose `test` is cancelled and a singleton whose own head is cancelled, and verify the dispositions below.

**Acceptance Scenarios**:

1. **Given** a trial whose latest `test` run is `cancelled`, **When** trial CI is evaluated, **Then** the failed-job re-run happens once; if the trial is still cancelled and no `failure`/`timed_out`/`action_required` appears among the latest runs, the trial is abandoned as an infrastructure result: uncharged, no bisection, no ejection, no pause, members stay Queued, with the same partition cooldown as #2052.
2. **Given** a trial set containing both a cancelled run and a genuine failure, **When** trial CI is evaluated, **Then** the trial is red exactly as today.
3. **Given** a singleton catch-up waiting on the member's own CI, **When** the member CI is still cancelled after the one re-run, **Then** the catch-up falls through to the trial path, uncharged, and never reaches the red-singleton ejection.
4. **Given** a singleton whose own head's latest run is `cancelled`, **When** the fast path is considered, **Then** the member is ineligible and the trial path runs.
5. **Given** a Queued-candidate member whose own head's latest run is `cancelled`, **When** batch admission runs, **Then** the member is deferred off Queued exactly as a red member is, and the deferral comment names the check as cancelled, not failing.
6. **Given** a landing PR with a cancelled run, **When** the landing poll evaluates it, **Then** the landing does not proceed, members stay Queued and nothing is charged.

---

### User Story 3 - Diagnostics and the worker CI status file tell the truth about the conclusion (Priority: P2)

A person or worker reading a log line, comment or context file about non-passing checks sees a cancelled or stale check labelled with its real conclusion, and the worker's CI status verdict agrees with the gates on the same head.

**Why this priority**: Misdescribing a cancellation as a failure sends a worker or human chasing a code problem that does not exist, and a verdict that disagrees with the gate undermines trust in both.

**Independent Test**: Unit-test the CI status verdict for a head with a cancelled latest run; assert it is `red` and lists the check with conclusion `cancelled`.

**Acceptance Scenarios**:

1. **Given** a head whose latest run is `cancelled`, **When** the CI status context is written, **Then** the verdict is `red` and the "Failing checks" section lists the check under its real conclusion `cancelled`.
2. **Given** a head whose only non-success runs are `skipped`, **When** the CI status context is written, **Then** `skipped` still never licenses a local test skip.
3. **Given** any log line, comment or context file listing non-passing checks, **When** a check is cancelled or stale, **Then** it is shown with that conclusion and never described as "failing" or "failed".

---

### User Story 4 - Read-side consumers never serve a cancelled set as ready (Priority: P2)

Cached and derived views of CI (board cache, settled-event verdict, PR facts) must not report a cancelled set as ready, and the cache must confirm such a set live instead of serving it on trust.

**Why this priority**: These are the places where the permissive result leaks without any gate involved.

**Independent Test**: Unit-test that the board cache does not serve a cached cancelled set without a live confirmation.

**Acceptance Scenarios**:

1. **Given** a cached check-run set containing a cancelled, stale or unrecognised latest run, **When** a consumer reads it, **Then** it is not served as READY without a live confirm.
2. **Given** the settled-event verdict and PR-facts builders, **When** a latest run is cancelled, stale or unrecognised, **Then** neither reports green.

---

### Edge Cases

- A newer `success` run of the same name supersedes an older `cancelled` run (latest-per-name wins), and the reverse (a newer `cancelled` supersedes an older `success`) is non-passing.
- A pending run takes precedence over every other class, as today.
- A completed run with an empty conclusion, or a conclusion value GitHub adds later, is non-passing and distinguishable from a failure.
- `skipped` and `neutral` remain passing everywhere, including `ADR-933` `required_status_contexts` semantics.
- A zero-job `cancelled` workflow *run* is still not a #2052 startup failure.
- The flake re-run being forbidden, or no Actions run existing for the head, degrades to the CI-fix dispatch as it does for failures today, never to a pause.
- A mixed set of one cancelled and one genuinely failed run is handled as a failure everywhere (the failure takes the stronger disposition).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A completed check run whose conclusion is `success`, `neutral` or `skipped` is passing. Any other completed conclusion is not passing: `failure`, `timed_out`, `action_required`, `cancelled`, `stale`, an empty conclusion, or an unrecognised value.
- **FR-002**: The shared classifier reports non-passing, non-failure conclusions (`cancelled`, `stale`, empty, unrecognised) in a way callers can tell apart from `failure`/`timed_out`/`action_required`. (Plan chooses the shape: a separate status, a separate slice, or a predicate.) Pending still takes precedence over everything, and latest-per-name selection is unchanged.
- **FR-003**: None of the following returns ready or green when any latest-per-name run is cancelled, stale or unrecognised: the PR merge-state settle, the stage CI gate check, the merge-train landing CI classification (and through it the singleton fast-path eligibility check, the landing mergeable poll and train member admission), trial CI evaluation, the singleton catch-up member CI wait, the settled-event CI verdict, the PR-facts builder, and the board cache's READY-on-trust rule.
- **FR-004**: At the stage `wait_for_ci` gate, a cancelled check on the PR head reaches the existing #2072 flake re-run exactly as a failure does: the cancelled jobs are re-run once under the same per-head budget, stale-failure guard and `ErrForbidden`/no-Actions-run degradation. Research must confirm that `POST /actions/runs/{id}/rerun-failed-jobs` re-runs cancelled jobs; if it does not, Plan selects the equivalent re-run call, with the budget and degradation rules unchanged.
- **FR-005**: At the stage gate, if the re-run is spent or unavailable and the latest run is still cancelled, the existing CI-fix reinvoke dispatches. Its context says the check was **cancelled**, not that it failed, and that a fresh run is needed (push, or re-run the check). It uses the existing CI-fix cycle accounting and is not a new pause path.
- **FR-006**: The stage gate never waits silently for `CIBackstopTimeout` on a cancelled check; treating cancelled as pending is ruled out.
- **FR-007**: Trial CI: the first cancelled latest run uses the #2052 R5 single failed-job re-run. If the trial is still cancelled after that, with no `failure`/`timed_out`/`action_required` among the latest runs, the trial is abandoned as the infrastructure result: uncharged, no bisection, no ejection, no pause, members stay Queued, partition cooldown as for #2052. A genuine failure in the same set still makes the trial red, as today.
- **FR-008**: Singleton catch-up: the same rule as FR-007. After the one re-run, a still-cancelled member CI falls through to the trial path, uncharged, and never reaches the red-singleton ejection.
- **FR-009**: Singleton fast path: a cancelled latest run on the member's head makes it ineligible; the fast path declines and the trial path runs.
- **FR-010**: Admission: a member whose own head's latest run is cancelled is deferred off Queued exactly as a red member is, so the stage gate gets a fresh run. The deferral comment names the check as cancelled, not failing.
- **FR-011**: Landing poll: a cancelled run on the landing PR is not green and the landing does not proceed. Today's not-landable handling applies: members stay Queued, nothing is charged.
- **FR-012**: Every log line, comment and context file that lists non-passing checks shows a cancelled or stale check with its real conclusion and never describes it as "failing" or "failed". This covers the CI status file's "Failing checks" section, the admission deferral comment, train diagnostics and the CI-fix context.
- **FR-013**: The CI status verdict must not disagree with the gates on the same head. Once FR-001..FR-003 make a cancelled set non-green in the landing classification, the CI status verdict is `red` with the cancelled checks listed under their real conclusion. The existing demotion for `skipped` stays (a skipped check must still never license a local test skip). The stale source comment that says a cancelled or unrecognised conclusion classifies as ready is corrected.
- **FR-014**: `skipped` and `neutral` remain passing for every gate. `ADR-933` `required_status_contexts` semantics are unchanged. #2052 startup-failure detection is unchanged: a zero-job `cancelled` workflow run is still not a startup failure (`docs/state-machine.md` §6.29).
- **FR-015**: Tests and docs. Add tests per the Success Criteria, including non-vacuity checks. Update `docs/state-machine.md` (the `PRMergeReady` paragraph that lists `cancelled` among ready conclusions, around line 632; §6.4 settling primitive; §6.5 CI gate / CI-fix; §6.29 flake re-run and train re-run now covering cancelled; §6.20 fast path; §6.30 catch-up; and the merge-train trial verdict text), and regenerate `docs/llms-full.txt` (`bash scripts/generate-llms-full.sh`) in the same commit.

### Key Entities *(if applicable)*

- **Check run**: A GitHub check on a PR head with a status (pending/completed) and, once completed, a conclusion. Latest run per check name is what counts.
- **Conclusion classes**: passing (`success`, `neutral`, `skipped`), failure (`failure`, `timed_out`, `action_required`), and non-passing-non-failure (`cancelled`, `stale`, empty, unrecognised).
- **Trial / member CI**: The CI evidence for a merge-train trial PR, or for a member's own PR head.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A table test of the classifier covers `success`, `neutral`, `skipped` (passing); `failure`, `timed_out`, `action_required` (failure); `cancelled`, `stale`, empty conclusion on a completed run and an unrecognised value (non-passing, distinguishable from failure); pending precedence over each; and a newer `success` superseding an older `cancelled` of the same name and the reverse. All pass.
- **SC-002**: Engine unit tests show: the PR merge-state settle does not return ready for a cancelled latest run; the landing CI classification does not return green for one; the singleton fast-path eligibility declines; the CI status verdict returns `red` and lists the check with conclusion `cancelled`; and the board cache READY-on-trust rule does not serve a cached cancelled set without a live confirm.
- **SC-003**: A stage-gate sim scenario shows a Validate PR with a cancelled required `test` check does not complete the stage, the flake re-run fires exactly once, a scripted re-run to `success` clears the gate, and a second scenario with a re-run scripted to `cancelled` again produces a CI-fix reinvoke whose context names the cancellation.
- **SC-004**: A train sim scenario shows a trial whose `test` is cancelled is re-run once and, still cancelled, abandoned as infrastructure with members Queued and nothing charged; a second shows a singleton whose own head's latest run is cancelled does not take the fast path.
- **SC-005**: The sim GitHub model represents a cancelled run and its re-run; any remaining fidelity gap is recorded in the sim's `FIDELITY.md`.
- **SC-006**: Each new test fails when the fix is neutralised: with the classifier change reverted so `cancelled` reads as ready, and with the train infrastructure disposition reverted to red. This is recorded in the PR.
- **SC-007**: No existing test for `skipped`/`neutral`, required-status-context, or startup-failure behaviour changes outcome.
- **SC-008**: `docs/state-machine.md` no longer describes `cancelled` as a ready conclusion, the sections named in FR-015 describe the new behaviour, and `docs/llms-full.txt` is regenerated in the same commit (the docs-drift check passes).

## Assumptions

- GitHub's `rerun-failed-jobs` endpoint re-runs cancelled jobs; this is to be confirmed in Research (FR-004). If it does not, an equivalent re-run call is used with the same budget and degradation rules.
- The non-passing, non-failure classification is distinct from the failure classification so that dispositions (R3/R4) can differ; the concrete representation is a Plan decision.
- A cancelled check at the stage gate is routed through the existing flake re-run and CI-fix machinery and consumes the existing CI-fix cycle budget; no new counter or pause path is introduced.
- `ADR-933` required-status-context filtering is applied as today and is orthogonal to this change.
- A genuine failure takes the stronger disposition when it coexists with a cancelled run in the same set.

## Out of Scope *(optional)*

- Changing which conclusions GitHub's own `mergeable_state` treats as blocking.
- The integration-PR mark-ready landing race (#2297).
- Base-moved trials (#2298).
- Any new configuration knob for which conclusions pass.
- Closing or otherwise acting on #2215 (mention it in the PR body only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2215. This issue's own number gets `Closes`).

## Source References *(optional)*

- Classifier: `github/checkruns.go` (`ClassifyCheckRuns`)
- Stage gate: `engine/pr_settle.go`, `engine/ci.go`, `engine/ci_flake_rerun.go`
- Train: `engine/merge_train.go` (`classifyLandingCI`, `singletonFastPathEligible`), `engine/merge_train_admission.go`, `engine/train_ci_eval.go`, `engine/merge_train_catchup.go`
- Read side: `boardcache/boardcache.go`, `engine/channel_settled.go`, `engine/pr_facts.go`
- CI status context: `engine/ci_status.go` (`ciStatusVerdictFor`, `allCheckRunsPassed`)
- Docs: `docs/state-machine.md` §6.4, §6.5, §6.20, §6.29, §6.30
- Sim: `tests/sim/ci_flake_rerun_test.go`, `tests/sim/mergetrain_ci_infra_test.go`, `tests/sim/mergetrain_singleton_fastpath_test.go`, `tests/sim/simgh/ci.go`
- Related: #2215, #2052, #2072, #2297, #2298, ADR-933
