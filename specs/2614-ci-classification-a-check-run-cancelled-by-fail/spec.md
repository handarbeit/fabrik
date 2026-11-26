# Feature Specification: A fail-fast-cancelled CI run with a failed step is red, not infrastructure

**Feature Branch**: `fabrik/issue-2614`
**Created**: 2026-11-25
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Some repos fail fast in CI: when a job fails, a final step runs `gh run cancel` so sibling jobs stop. After that cancel, the check runs GitHub reports, often including the job that actually failed, conclude `cancelled`, not `failure`. Since #2296 a trial or catch-up whose non-passing runs are only cancelled is CI infrastructure (`TrainCIInfra` via `trialNonPassingVerdict`, `engine/train_ci_eval.go`): the trial is abandoned, members stay Queued uncharged, and the batch is retried. The stage CI-fix path likewise frames the failure as cancelled, not a regression.
>
> Effect: a real test failure is treated as an infrastructure hiccup. The train retries the same failing batch instead of reading it red and bisecting; the CI-fix worker is told it is not a test failure and records a no-op.
>
> Seen (liminisapp/liminis-context-graph, 2026-11-22 ~22:30 UTC): a batch of four members, one of which broke a test; the fail-fast step cancelled the run; the train retried the same batch for about an hour and never isolated the member. It recovered only when that member's own PR CI went red. The CI-fix pass then recorded a no-op until a human comment said it was a real failure. An earlier occurrence on 2026-10-01 had the same shape.
>
> ## Requirements
>
> - **R1.** Before classifying a cancelled check run as non-passing infrastructure, read the Actions job's steps (`GET /repos/{o}/{r}/actions/runs/{run_id}/jobs`, run ID from the check run's `DetailsURL` as `ActionsRunIDFromDetailsURL` does). If any step of any job in that run concluded `failure`, the run is **red**.
> - **R2.** For a trial, a red run per R1 is `TrainCIRed`: it is charged, recorded and bisected exactly like any confirmed red trial (after the existing one-time failed-job re-run, if Plan keeps it for this case; decide and record).
> - **R3.** For the stage `wait_for_ci` gate and the CI-fix dispatch, a red run per R1 is `ciFailure` with regression framing, and the CI-fix context names the failing job and step (`ci-status.md` `## Failing checks`).
> - **R4.** A cancelled run with no failed step (superseded by `cancel-in-progress`, cancelled by hand) stays non-passing infrastructure, unchanged.
> - **R5.** Fail safe: if the jobs read fails (incl. `ErrForbidden`), keep today's classification and log once per head.
> - **R6.** Optional, decide in Plan: tell a self-cancel (cancelled by the run's own token) from an external cancel.
>
> ## Scope
>
> In: `engine/train_ci_eval.go`, `engine/ci_nonpassing.go`, `engine/ci_flake_rerun.go` as needed, the catch-up wait (`memberCINotPassed`), `github/workflowruns.go` (a jobs-with-steps read), CI-fix comment/context, `docs/state-machine.md` §6.29 (regenerate `docs/llms-full.txt`), an ADR numbered after this issue (amends ADR 2296).
> Out: merge-train worktree busy handling (separate work issue).
>
> ## Acceptance
>
> - Unit tests: cancelled runs with a failed step classify red for the trial, the catch-up wait and the stage gate; cancelled runs with no failed step stay infra; a failed jobs read keeps today's result.
> - Sim twin: a batch whose trial run is fail-fast-cancelled after a member breaks a test bisects to that member instead of retrying the batch. Fails with R1 neutralised.
>
> **Report linkage:** this restates community report #2609. Mention #2609 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, not even inside a negation (safe: "mentioned for context: #2609"). The report is closed by hand once a release containing the fix ships.

## Background

Some repositories configure CI to fail fast: when one job fails, a final step cancels the whole workflow run so sibling jobs stop early. GitHub then reports the check runs of that workflow as `cancelled`, frequently including the very job that failed. Since #2296, Fabrik treats a cancelled, stale, empty or unrecognised check-run conclusion as "not a pass" and, when nothing in the set is a genuine `failure`, classifies it as CI infrastructure rather than a test failure. For a merge-train trial or singleton catch-up, that means the trial is abandoned without charge, members stay Queued, and the same batch is retried. For the stage CI gate, it means the CI-fix worker is told the failure is a cancellation and not a regression.

That classification is right for a run cancelled by `cancel-in-progress` or by hand, but wrong for a fail-fast cancel that follows a real failure. The real failure is invisible in the check-run conclusion, yet visible in the run's job steps, where the failing step concluded `failure`. Because of that gap, a real test failure is handled as an infrastructure hiccup:

- The train retries the same failing batch forever instead of reading it red and bisecting to the offending member.
- The CI-fix worker is told it is not a test failure and records a no-op until a human intervenes.

Observed on liminisapp/liminis-context-graph on 2026-11-22 (about 22:30 UTC): a batch of four members, one of which broke a test, was retried for about an hour without ever isolating the member. It recovered only when that member's own PR CI went red, and the CI-fix pass recorded a no-op until a human comment said the failure was real. An earlier occurrence on 2026-10-01 had the same shape. This issue restates community report #2609.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Merge-train trial isolates a member that broke a test under fail-fast CI (Priority: P1)

An operator runs a merge train on a repository whose CI cancels the run after a job fails. A batch of members is assembled into a trial, one member breaks a test, the failing job's step fails, and the fail-fast step cancels the run, so the trial's check runs read `cancelled`. The train must treat this as a red trial and narrow down to the offending member rather than retrying the identical batch.

**Why this priority**: This is the incident that motivated the issue. A train that loops on a failing batch for an hour blocks every member behind it.

**Independent Test**: In the sim, run a four-member batch whose trial run is fail-fast-cancelled after one member's step fails. Confirm the train charges and records the trial as red and bisects to that member, instead of abandoning the trial as infrastructure and retrying the same batch.

**Acceptance Scenarios**:

1. **Given** a trial whose check runs are all `cancelled` and whose Actions run contains a job step that concluded `failure`, **When** the train evaluates the trial, **Then** it is classified red, counted and recorded as a red trial, and bisected like any other confirmed red trial.
2. **Given** the same trial but no step of any job in the run concluded `failure`, **When** the train evaluates it, **Then** it is still classified as CI infrastructure: abandoned, uncharged, members left Queued.
3. **Given** the red classification is neutralised, **When** the same scenario runs, **Then** the batch is retried unchanged, so the test can tell the fix from its absence.

---

### User Story 2 - Stage CI gate and CI-fix treat a fail-fast-cancelled failure as a regression (Priority: P1)

A pull request at a stage that waits for CI has a CI run that failed a step and was then fail-fast-cancelled. The gate must treat this as a CI failure and dispatch the CI-fix worker with regression framing and the identity of the failing job and step, not as a cancellation that needs no fix.

**Why this priority**: Without this, the CI-fix worker is told the failure is not a test failure and records a no-op, and the item stays stuck until a human says otherwise.

**Independent Test**: Seed a stage-gated PR whose check runs are `cancelled` and whose run has a failed step. Confirm the gate returns a CI failure and the CI-fix context names the failing job and step. Repeat with no failed step and confirm today's cancelled handling.

**Acceptance Scenarios**:

1. **Given** a gated PR whose check runs are `cancelled` and whose run has a step that concluded `failure`, **When** the CI gate evaluates it, **Then** it is a CI failure, and the CI-fix comment uses regression wording, not cancellation wording.
2. **Given** that same state, **When** the CI-fix context is written, **Then** its failing-checks section names the failing job and step.
3. **Given** `cancelled` check runs with no failed step, **When** the gate evaluates them, **Then** behaviour is unchanged from today.

---

### User Story 3 - Singleton catch-up wait reads a fail-fast-cancelled member run as red (Priority: P2)

When the train waits on a member PR's own CI after a singleton catch-up, a fail-fast-cancelled run with a failed step must be read as the member's CI being red, not as "not passed, so build the trial".

**Why this priority**: It is the same misclassification on a second train path. It matters less because the member's own PR CI usually goes red on its own, as it did in the observed incident.

**Independent Test**: Drive the catch-up wait with a member run that is cancelled and has a failed step, then with one that has none, and compare the outcomes.

**Acceptance Scenarios**:

1. **Given** a member run that is `cancelled` with a failed step, **When** the catch-up wait evaluates the member's CI, **Then** it reads as red and follows the existing confirmed-red catch-up path.
2. **Given** a member run that is `cancelled` with no failed step, **When** the wait evaluates it, **Then** the existing "not passed" handling applies unchanged.

---

### User Story 4 - Safe degradation when job steps cannot be read (Priority: P2)

If the job-steps read fails for any reason (transport error, permission denied, run gone), the classification falls back to today's result, so the change can never make things worse than the status quo.

**Why this priority**: It bounds the risk of the new read, and the Actions read needs a permission that can be revoked at runtime.

**Independent Test**: Make the jobs read fail with a transport error and with a permission error, then confirm the classification is today's and a single log line is emitted per head.

**Acceptance Scenarios**:

1. **Given** a cancelled run whose jobs read fails, **When** the run is classified, **Then** the result equals today's classification.
2. **Given** repeated evaluations of the same head with the read still failing, **When** they run, **Then** the failure is logged once per head.

---

### Edge Cases

- A run with several jobs where only one has a failed step and the rest are cancelled: red.
- A run that has failed steps in a job whose conclusion is itself `cancelled` or `skipped`: any `failure` step in any job makes the run red.
- A check run with no resolvable Actions run (a third-party check app): no jobs read is possible, so today's classification applies.
- Several cancelled runs in one head where only some have failed steps: the head is red if any cancelled run qualifies.
- A run that is still in progress or whose steps are not yet final: only steps already concluded `failure` count; no failed step means unchanged handling.
- A failed step that concluded `failure` but was marked continue-on-error: reported as `failure` by the API for steps only if it truly failed; the spec treats the reported step conclusion as authoritative.
- A head that already has a genuine `failure` check-run conclusion alongside cancelled ones: unchanged (already red).
- The flake re-run budget (one failed-job re-run per head) interacts with the new red classification; see Assumptions.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Before a cancelled check run is classified as non-passing CI infrastructure, the engine MUST read the steps of the jobs in the check run's Actions workflow run. If any step of any job in that run concluded `failure`, the run MUST be classified red.
- **FR-002**: For a merge-train trial, a run classified red under FR-001 MUST produce the same outcome as any confirmed red trial: counted against the trial-rate and ejection accounting, recorded, and bisected. It MUST NOT be abandoned as infrastructure.
- **FR-003**: For the stage `wait_for_ci` gate, a run classified red under FR-001 MUST be a CI failure, and the CI-fix dispatch MUST use regression framing, not cancellation framing.
- **FR-004**: When a CI-fix is dispatched for a run classified red under FR-001, the CI-fix context's failing-checks section MUST name the failing job and the failing step.
- **FR-005**: The singleton catch-up wait on a member PR's CI MUST read a run classified red under FR-001 as the member's CI being red, following the existing confirmed-red catch-up path.
- **FR-006**: A cancelled run in which no step of any job concluded `failure` (for example one superseded by `cancel-in-progress` or cancelled by hand) MUST keep today's non-passing infrastructure classification and handling in every consumer.
- **FR-007**: If the jobs read fails for any reason, including a permission refusal, the classification MUST remain exactly today's result, and the failure MUST be logged once per head, not on every evaluation.
- **FR-008**: The decision on whether the existing one-time failed-job re-run applies before a trial is declared red under FR-001 MUST be made during Plan and recorded in the ADR for this issue.
- **FR-009**: The as-built documentation of the merge-train CI classification MUST describe the new rule, and the generated documentation bundle MUST be regenerated in the same change.
- **FR-010**: An ADR numbered after this issue MUST record the decisions, as an amendment to ADR 2296.

### Key Entities *(if applicable)*

- **Check run**: GitHub's per-job CI result for a head commit, with a conclusion such as `success`, `failure` or `cancelled`, and a link to its Actions run.
- **Actions workflow run**: the run that owns one or more jobs; its jobs expose per-step conclusions, which is where the failing step shows even when the check-run conclusion is `cancelled`.
- **CI verdict**: the engine's per-head classification (green, red, pending, non-passing/infrastructure, unknown) consumed by the merge train, the catch-up wait and the stage CI gate.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim, a batch whose trial run is fail-fast-cancelled after one member breaks a test bisects to that member without retrying the same batch, and the same scenario fails when the new classification is neutralised.
- **SC-002**: A stage-gated PR in the same shape gets a CI-fix dispatch with regression framing and a failing-checks section naming the job and step, with no no-op CI-fix pass for the cancellation alone.
- **SC-003**: For cancelled runs with no failed step, trial, catch-up and stage-gate outcomes are identical to those before this change.
- **SC-004**: With the jobs read failing, every consumer's outcome is identical to before this change, and one log line per head is emitted.
- **SC-005**: The incident shape (a batch retried for about an hour with no isolation) no longer occurs: such a trial reaches a red verdict on its first evaluation.

## Assumptions

- A step that concluded `failure` in the Actions job data is sufficient evidence of a real failure; the API's step conclusion is authoritative and no log text is parsed.
- Whether the existing one-time failed-job re-run still precedes a red verdict for this case is a design decision for Plan (FR-008), not a product question. Neither choice changes the user-visible requirement that the trial is eventually read red and bisected.
- Telling a self-cancel (cancelled by the run's own token) apart from an external cancel is optional and deferred to Plan. It is not required for acceptance, and omitting it is acceptable.
- The new read needs the Actions read permission the engine already requires at startup; where it is revoked at runtime, the fail-safe in FR-007 applies.
- The PR for this work mentions report #2609 by bare number only, using wording such as "mentioned for context: #2609"; it never uses a closing keyword near that number. The report is closed by hand after a release containing the fix ships.

## Out of Scope *(optional)*

- Merge-train worktree-busy handling (a separate work issue).
- Changing how repositories configure fail-fast cancellation.
- Parsing job log text to find a failure.
- Any change to how a cancelled run with no failed step is handled.
- Changes to retry budgets or bisection behaviour beyond routing a red verdict into them.

## Source References *(optional)*

- Community report #2609 (mentioned for context)
- ADR 2296: a cancelled, stale, empty or unrecognised check-run conclusion is not a pass (this work amends it)
- Observed incident: liminisapp/liminis-context-graph, 2026-11-22 ~22:30 UTC; earlier occurrence 2026-10-01
