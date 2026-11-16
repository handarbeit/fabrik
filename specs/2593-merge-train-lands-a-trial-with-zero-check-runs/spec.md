# Feature Specification: Merge train must not land a trial whose CI has not started yet

**Feature Branch**: `fabrik/issue-2593`
**Created**: 2026-11-16
**Status**: Specified
**Input**: User description: "Merge train lands a trial with zero check runs while GitHub Actions has not started CI yet. The merge train lands a trial whose CI never ran when GitHub Actions is slow to start workflows. `evalTrialCI` (`engine/train_ci_eval.go`) and `classifyLandingCI` (`engine/merge_train.go`) both have a zero-check-runs branch (ADR-933, #1153). It returns green when all of the following hold: the trial head has no check runs; `mergeable_state` is accepted; the required contexts are satisfied; no workflow run is `queued`/`in_progress` (#2052); `ciSuiteHold` finds no outstanding suite (#1822). When GitHub delays workflow creation, none of these signals exists yet. There are no check runs, no workflow runs and no Actions check suite. So 'CI has not started yet' is indistinguishable from 'this repo has no CI', and the trial is landed. Observed (0.0.93 release gate, 2026-11-16, bed A, leg `pat/on`): GitHub created no `pull_request` workflow runs in `fabrik-test-alpha` between 15:25:22Z and 15:34:03Z. `TestMergeTrainBisectionEjectsPoisoner` hit this window on its integration PR `handarbeit/fabrik-test-alpha#11365` (head `f002e38e`): opened 15:12:11Z with 3 members, one of them the poison member; the engine logged `train CI pending` twice, then `train CI green`, and landing logged `mergeable_state "clean" accepted, zero check runs, required contexts satisfied`; merged 15:15:52Z, all three members moved to Done and closed; at 15:16:15Z GitHub only then created the CI workflows and `train-poison-guard` concluded failure. The poisoned batch landed on its base branch with no CI having run. On a real repository that is the default branch: unvalidated code merged by the engine. All three members' own PRs had green check runs at admission, so the engine had positive evidence that this repository runs check-run CI. Requirements: R1. A merge-train trial (and the landing settle of an integration/singleton PR) must not be classified green on the zero-check-runs path while there is positive evidence that the repository runs check-run CI. At minimum, any batch member's own PR head carrying check runs counts as such evidence. While such evidence exists, zero check runs on the trial means pending, bounded by the existing `CIBackstopTimeout`. R2. The genuine no-CI case keeps working: a repository whose members' PRs carry no check runs (Actions disabled, the #933 local-CI-takeover shape) still lands on the zero-check path exactly as today. R3. Audit every other zero-check-runs green path for the same gap and apply the same rule where a comparable signal exists, or record why it does not apply: the stage `wait_for_ci` gate (`settlePRMergeState` rule 13 / `classifyCIFromMergeableState`); `classifyLandingCI`; the singleton fast path, which already refuses zero runs. R4. One log line names why a zero-check trial is held, for example `trial <sha> has zero check runs but member PR #N has check runs — CI not started yet, waiting`. R5. Sim twin plus unit tests. Each must fail with the fix neutralised: a trial with zero check runs, no workflow runs and no suites, whose members carry check runs, must not land. The no-CI repository case must still land. R6. Update `docs/state-machine.md` (and regenerate `docs/llms-full.txt`) and add an ADR numbered after this issue. Scope: in scope is the zero-check-runs green classification for trials, landings and, per R3, the stage gate. Out of scope: a trial that has some checks but where GitHub reports one stuck `in_progress` (a separate issue); the stage gate clearing on a partial set where only CodeQL's dynamic run exists (observed in the same gate as `TestLateCheckRunSuiteGate`); record it in the ADR as a known residual if R3 does not cover it. Acceptance: during a simulated Actions delay (no runs, no suites), a trial whose members have check runs stays pending and does not land. It lands green once the real CI runs and passes, and is red/bisected if it fails. A no-CI repository's trial still lands on the zero-check path. The neutralisation seam makes the new tests fail. Mentioned for context: #933, #1153, #1822, #2052."

## Background

The merge train decides whether a trial (the integration PR that combines a batch of queued members) is safe to land by classifying its CI. When the trial head has zero check runs, the engine has a "genuine no-CI repository" path, introduced by ADR-933 and #1153 and later tightened by #2052 (workflow runs in flight) and #1822 (outstanding check suites). That path treats the trial as green when `mergeable_state` is accepted, the required contexts are satisfied, no workflow run is queued or in progress, and no check suite is outstanding.

That path assumes any CI that exists will have announced itself by the time the engine looks. GitHub Actions does not always do so. During a delay in workflow creation there are no check runs, no workflow runs and no check suites yet, so a repository that does run CI looks exactly like one that has none, and the trial lands.

This happened in the 0.0.93 release gate (2026-11-16, bed A, leg `pat/on`). GitHub created no `pull_request` workflow runs in `fabrik-test-alpha` between 15:25:22Z and 15:34:03Z. `TestMergeTrainBisectionEjectsPoisoner` opened its integration PR (`handarbeit/fabrik-test-alpha#11365`, head `f002e38e`, three members including the poison member) at 15:12:11Z. The engine logged `train CI pending` twice, then `train CI green`, and the landing logged `mergeable_state "clean" accepted, zero check runs, required contexts satisfied`. The PR merged at 15:15:52Z and all three members went to Done and closed. GitHub created the CI workflows only at 15:16:15Z, and `train-poison-guard` concluded failure. The poisoned batch had already landed with no CI having run. On a real repository that branch is the default branch, so the engine would have merged unvalidated code.

The engine did have positive evidence: all three members' own PRs carried green check runs at admission, which shows the repository runs check-run CI. That evidence is currently not consulted on the zero-check path.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A delayed CI start holds the trial instead of landing it (Priority: P1)

An operator runs the merge train on a repository whose members' PRs carry check runs. GitHub is slow to create workflows for the trial PR. The trial must wait, not land.

**Why this priority**: This is the defect. Landing an unvalidated batch on the base branch is the worst outcome the train can produce, and it is silent.

**Independent Test**: In the sim, build a batch whose members' PR heads carry check runs, and open a trial with zero check runs, no workflow runs and no check suites. Poll repeatedly and confirm the trial is never landed and its members stay queued. Then neutralise the fix and confirm the same scenario lands the trial.

**Acceptance Scenarios**:

1. **Given** a trial with zero check runs, no workflow runs and no suites, and at least one member whose own PR head has check runs, **When** the train evaluates the trial, **Then** the result is pending and nothing is merged.
2. **Given** that held trial, **When** the real CI check runs appear and all pass, **Then** the trial is classified green and lands.
3. **Given** that held trial, **When** the real CI check runs appear and one fails, **Then** the trial is classified red and goes through the existing red/bisect handling.
4. **Given** that held trial and CI never starts, **When** the existing CI backstop timeout elapses, **Then** the train handles it exactly as it handles any other trial pending past that backstop.

---

### User Story 2 - A repository with no CI still lands on the zero-check path (Priority: P1)

An operator runs the merge train on a repository with no check-run CI (Actions disabled, or the #933 local-CI-takeover shape). Behaviour must not change.

**Why this priority**: The fix must not turn a legitimate configuration into a train that never lands anything.

**Independent Test**: In the sim, build a batch whose members' PR heads carry no check runs, and open a trial with zero check runs, no workflow runs and no suites. Confirm the trial lands as it does today.

**Acceptance Scenarios**:

1. **Given** a trial with zero check runs and no member PR head carrying check runs, **When** the other existing zero-check conditions hold, **Then** the trial is classified green and lands exactly as before.

---

### User Story 3 - The landing settle of an integration or singleton PR applies the same rule (Priority: P1)

The landing step re-judges CI on the integration or singleton PR before merging. It must not become a second route to the same defect.

**Why this priority**: The incident's final merge was logged at the landing step. Fixing only trial evaluation would leave that route open.

**Independent Test**: Drive a landing whose integration PR head has zero check runs while members have check runs, with trial evaluation already past. Confirm the landing waits and does not merge.

**Acceptance Scenarios**:

1. **Given** an integration or singleton PR at landing with zero check runs, no workflow runs and no suites, and members that carry check runs, **When** the landing settle runs, **Then** it keeps waiting within the existing CI backstop and does not merge.
2. **Given** the same landing for a repository whose members carry no check runs, **When** the existing zero-check conditions hold, **Then** it merges as today.

---

### User Story 4 - The operator can see why a trial is held (Priority: P2)

The operator reads the engine log to understand why a trial is not landing.

**Why this priority**: A silent hold would look like a stuck train.

**Independent Test**: Reproduce Story 1 and read the log for a line naming the trial, the member PR that supplied the evidence, and that CI has not started.

**Acceptance Scenarios**:

1. **Given** a trial held under Story 1, **When** the engine evaluates it, **Then** one log line states that the trial has zero check runs, names a member PR that has check runs, and says CI has not started yet and the engine is waiting.

---

### User Story 5 - Other zero-check green paths are audited (Priority: P2)

A maintainer wants to know whether the same gap exists elsewhere in the engine.

**Why this priority**: The same "no signal means no CI" assumption may be repeated in other classifiers. The outcome must be recorded, whichever way it goes.

**Independent Test**: Review the ADR and `docs/state-machine.md` for an entry per audited path stating either the rule now applied or why a comparable signal does not exist.

**Acceptance Scenarios**:

1. **Given** the stage `wait_for_ci` gate's zero-check path, **When** the audit is complete, **Then** the same rule is applied if a comparable evidence signal exists for that item, otherwise the ADR records why not.
2. **Given** the landing classification and the singleton fast path, **When** the audit is complete, **Then** the landing classification applies the rule and the fast path, which already refuses zero check runs, is recorded as needing no change.

---

### Edge Cases

- The evidence signal cannot be read for any member (read error). The classification must not silently turn into a landing of an unvalidated trial on a repository that may run CI, and the outcome must be visible in the log.
- A batch mixes members: some with check runs on their PR head, some with none. Any one member carrying check runs counts as evidence.
- A member's PR carries check runs only from a non-Actions source (an external status app). It still counts as check-run CI for this rule.
- The trial legitimately has no CI configured for the trial branch while members do (for example a workflow filtered by branch name). The trial waits until the existing CI backstop timeout, then is handled as any other pending-past-backstop trial; this is accepted.
- The members' check runs were all cancelled, stale or otherwise non-passing at admission. A non-passing result is still evidence that the repository runs CI.
- A trial that has some check runs but one reported stuck `in_progress` is a different case (see Out of Scope).
- A trial re-formed after bisection or a stale-trial rebuild produces a new head with zero check runs. It is evaluated by the same rule, with the surviving members as evidence.
- A singleton batch: the single member's own PR head is the evidence source.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The merge-train trial CI classification MUST NOT return green on the zero-check-runs path while any batch member's own PR head carries check runs. In that case it MUST return pending.
- **FR-002**: The landing settle of an integration or singleton PR MUST apply the same rule as FR-001, so a landing cannot merge a zero-check-run head while members carry check runs.
- **FR-003**: A trial or landing held under FR-001 or FR-002 MUST be bounded by the existing CI backstop timeout and handled on expiry exactly as any other pending-past-backstop trial. No new timeout is introduced.
- **FR-004**: A repository whose batch members' PR heads carry no check runs MUST continue to land on the existing zero-check path with unchanged behaviour, including the existing conditions on `mergeable_state`, required contexts, workflow runs and check suites.
- **FR-005**: When a zero-check trial or landing is held under FR-001 or FR-002, the engine MUST log one line naming the trial head, a member PR that supplied the evidence, and that CI has not started yet and the engine is waiting.
- **FR-006**: The "members carry check runs" evidence MUST be read at decision time from GitHub for each batch member's current PR head, not taken from a cached or earlier-formation value that could be stale.
- **FR-007**: If the evidence cannot be determined (the read fails), the classification MUST NOT land the trial on that basis alone. The outcome (waiting, with the reason) MUST be logged.
- **FR-008**: The stage `wait_for_ci` gate's zero-check path MUST be audited. If a comparable evidence signal exists for a single item's PR, the same rule MUST be applied there; if not, the reason MUST be recorded.
- **FR-009**: The landing classification used by the singleton fast path and the admission check MUST be audited. Its handling of zero check runs MUST be consistent with FR-001, and the fast path's existing refusal of zero runs MUST be recorded as requiring no change.
- **FR-010**: Once the trial's real check runs appear, classification MUST proceed through the existing pending, green and red handling with no change to those outcomes (green lands, red goes to the existing bisect or eject handling).
- **FR-011**: A hold under FR-001 or FR-002 MUST NOT count as a trial failure, an ejection, a retry or a pause for any member, and MUST NOT charge the runaway guard.
- **FR-012**: A test-only neutralisation seam MUST allow the new rule to be disabled per engine so tests can prove they fail without the fix.
- **FR-013**: A sim twin scenario and unit tests MUST cover: a trial held while members have check runs and no workflow runs or suites; the trial landing green once real CI passes; the trial going red once real CI fails; the landing settle path held likewise; and the no-CI repository still landing. Each "held" test MUST fail when the seam neutralises the rule.
- **FR-014**: `docs/state-machine.md` MUST be updated to describe the new rule and the audit results, `docs/llms-full.txt` MUST be regenerated in the same change, and an ADR numbered `2593` MUST be added recording the decision, the audit outcome per path, and the known residuals.

### Key Entities *(if applicable)*

- **Trial / integration PR**: the PR the train opens to validate a batch before landing it; its head is the unit whose CI is classified.
- **Batch member**: a queued issue and its own PR. Its PR head's check runs are the positive evidence that the repository runs check-run CI.
- **Zero-check path**: the classification branch that treats a head with no check runs as green on the strength of several negative signals.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a simulated Actions delay (no check runs, no workflow runs, no suites on the trial) with members that carry check runs, the trial and the landing are never merged across at least N consecutive polls (N covering the full CI backstop window), and 0 members are moved to Done or closed.
- **SC-002**: After the simulated delay ends, a trial whose real CI passes lands, and a trial whose real CI fails is handled by the existing red/bisect path, in both cases without operator action.
- **SC-003**: In a simulated repository with no check-run CI, the trial lands on the zero-check path with the same outcome and the same number of polls as before the change.
- **SC-004**: Each new "held" test fails when the fix is neutralised and passes when it is active; the no-CI test passes in both states.
- **SC-005**: The log of a held trial contains exactly one explanatory line per hold decision naming the trial head and a member PR.
- **SC-006**: The ADR and state-machine doc list every audited zero-check path with an explicit outcome (rule applied, or reason it does not apply), and the docs bundle drift check passes.

## Assumptions

- A batch member's own PR head carrying any check runs is sufficient positive evidence; no wider evidence (repository workflow files, historical runs) is required in this issue.
- Hold behaviour reuses the existing pending handling and the existing CI backstop timeout; the timeout value is not changed.
- When the evidence read fails, waiting (pending) is preferred over landing, consistent with the "never land unvalidated code" intent; the existing behaviour for read errors elsewhere in the classification is retained where it already fails safe.
- The trial branch in a no-CI repository is assumed never to gain check runs from members' PRs, so the no-CI path is unaffected.
- This follows the repository's existing convention that a hold is uncharged: it does not count toward ejection, retry, runaway or pause counters.
- The ADR number follows the issue number (`adrs/2593-...md`).

## Out of Scope *(optional)*

- A trial that has some check runs where GitHub reports one stuck `in_progress` (tracked separately).
- The stage `wait_for_ci` gate clearing on a partial set where only CodeQL's dynamic run exists (observed in the same gate as `TestLateCheckRunSuiteGate`). If the R3 audit does not cover it, the ADR records it as a known residual.
- Changing the CI backstop timeout, adding new timeouts, or adding repository-level CI configuration detection beyond member check runs.
- Changing how GitHub creates workflows, or retriggering workflows (the #2052 retrigger remains specific to startup failures).

## Source References *(optional)*

- Mentioned for context: #933, #1153, #1822, #2052.
- 0.0.93 release gate observation on `handarbeit/fabrik-test-alpha#11365` (2026-11-16, bed A, leg `pat/on`).
- `engine/train_ci_eval.go`, `engine/merge_train.go` (`classifyLandingCI`), `engine/ci_suites.go` (`ciSuiteHold`), and the stage gate's `settlePRMergeState` zero-check rule, as named in the issue.
