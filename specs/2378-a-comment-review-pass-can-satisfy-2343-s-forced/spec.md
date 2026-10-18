# Feature Specification: A comment-review pass must not satisfy a forced Validate re-run

**Feature Branch**: `fabrik/issue-2378`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: "> ## Problem
>
> #2343 (ADR-2343, R3) re-runs Validate when no Validate completion SHA can be recovered, so an item is never landed unverified. It does this by stripping `stage:Validate:complete`. If a human comment is pending at that moment, the comment-review pass for Validate runs first, and when it emits `FABRIK_STAGE_COMPLETE` it re-applies `stage:Validate:complete`. The item then lands on that pass alone, without a Validate stage run.
>
> Observed in the v0.0.88 gate (bed alpha #8586, pat/off, 2026-10-17): the label was stripped at 15:47:03Z, a 4-turn `Validate-comment-review` pass (\"acknowledge only\") re-applied it at 15:47:22Z, and the PR merged at 15:48:18Z. No Validate stage run happened in between.
>
> ## Requirements
>
> - After R3 strips the completion labels, only a Validate **stage** run may restore `stage:Validate:complete`. A comment-review pass completing must not count as the re-run.
> - The pending comment is still processed (stage first or comment first, per #2220), and the landing still waits for both.
> - The sim twin `tests/sim/validate_sha_restart_test.go` gains a scenario with a pending human comment at the restart.
>
> ## Scope
>
> Engine: the R3 path in `engine/validate_sha_rebuild.go` / `handleValidateSHAInvalidation` and how a comment-review completion applies stage labels. Out of scope: the SHA rebuild rule itself.
>
> ## Acceptance
>
> - With a pending comment and no recoverable SHA, Validate's stage run executes before any landing (sim scenario).
> - Neutralising the fix makes that scenario fail."
>
> (Original text reproduced verbatim except that the observed re-apply time reads 15:47:25Z in the issue as filed; see the issue history. Label on the issue: `fabrik:cruise`.)

## Background

After a restart the engine has lost its record of which commit Validate verified. #2343 closed the resulting hole: when that record cannot be rebuilt, the engine strips the Validate completion label so that Validate runs again, and an item is never landed on a head nobody validated.

That guarantee has a gap. If a human comment is waiting on the item at the moment the label is stripped, the comment-review pass for Validate runs first. When that pass finishes with the stage-completion marker, the engine treats it as Validate being complete and puts `stage:Validate:complete` back. The item is then eligible to land with no Validate stage run having happened. The forced re-run was supposed to be a full re-validation of the current head; a short comment pass that only acknowledges a comment is not that.

Observed in the v0.0.88 release gate (bed alpha, issue #8586, PAT auth, auto-merge off, 2026-10-17): the label was stripped at 15:47:03Z, a 4-turn Validate comment-review pass ("acknowledge only") restored it, and the PR merged at 15:48:18Z. No Validate stage run took place in between.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A forced Validate re-run really runs, even with a comment pending (Priority: P1)

An operator restarts the daemon while an item sits at Validate with a completion label, no recoverable completion record, and an unprocessed human comment. The engine forces a Validate re-run. The comment is processed, but the item does not land until a Validate stage run has completed on the current head.

**Why this priority**: This is the defect. It lets an unvalidated head merge, which is the exact outcome the forced re-run exists to prevent.

**Independent Test**: In the sim, seed an item at Validate with the completion label, a linked PR, no recoverable completion SHA and one pending human comment; restart the engine and poll until quiescent. Assert that a Validate stage run executes before any landing, and that the completion label is present only after that stage run.

**Acceptance Scenarios**:

1. **Given** an item at Validate with `stage:Validate:complete`, no recoverable completion SHA and a pending human comment, **When** the engine forces the Validate re-run, **Then** a Validate stage run executes before the item lands.
2. **Given** the same state, **When** the comment-review pass runs and ends with the stage-completion marker, **Then** `stage:Validate:complete` is not restored by that pass.
3. **Given** the same state, **When** the Validate stage run subsequently completes, **Then** the completion label is applied and the item may land under the normal gates.

---

### User Story 2 - The pending comment is still handled (Priority: P1)

The human who left the comment still gets it processed (reaction, response, any requested change) exactly as before; the fix must not drop or defer it indefinitely.

**Why this priority**: Fixing the landing hole by ignoring or starving the comment would trade one defect for another.

**Independent Test**: In the same sim scenario, assert the pending comment receives its processed marker and that landing waits for both the comment pass and the Validate stage run.

**Acceptance Scenarios**:

1. **Given** the forced re-run is outstanding and a comment is pending, **When** the engine polls, **Then** the comment is processed, in the order the existing stage-first routing (#2220) dictates.
2. **Given** both the comment and the re-run are outstanding, **When** only one of them has finished, **Then** the item does not land.

---

### User Story 3 - Unaffected paths behave as before (Priority: P2)

Items with no forced re-run outstanding, and Validate stage runs themselves, keep their current behaviour.

**Why this priority**: Guards against over-reach; an ordinary comment review on a validated item must still be able to complete as it does today.

**Independent Test**: Existing sim and engine tests for comment review and Validate completion continue to pass unchanged.

**Acceptance Scenarios**:

1. **Given** a validated item with a recorded completion SHA and a pending comment, **When** the comment-review pass completes, **Then** behaviour is unchanged from today.
2. **Given** a forced re-run is outstanding and no comment is pending, **When** the Validate stage run completes, **Then** the completion label is applied as today.

---

### Edge Cases

- The daemon restarts again between the label strip and the Validate stage run: the next start must not treat any intervening comment pass as the re-run either.
- The comment-review pass makes a code change and pushes a new head: the new head still has to be validated by a Validate stage run before landing.
- A comment pass ends with the completion marker while the re-run is outstanding: the comment is still marked processed and the pass is not treated as a failure or retried endlessly.
- The Validate stage run itself fails or is paused: the completion label stays absent and the item does not land.
- A comment arrives after the label strip but before the Validate stage run starts: same handling as a comment pending at strip time.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After the engine strips the Validate completion labels to force a Validate re-run, `stage:Validate:complete` must be restored only by the completion of a Validate stage run; the completion of a Validate comment-review pass must not restore it.
- **FR-002**: A pending human comment on such an item must still be processed (acknowledged, reacted to, and acted on as today), in the order the existing stage-first routing decides.
- **FR-003**: The item must not land until both the pending comment has been processed and a Validate stage run has completed on the current head.
- **FR-004**: The restriction in FR-001 must hold across a daemon restart that occurs after the labels are stripped and before the Validate stage run completes.
- **FR-005**: When a comment-review pass ends with the stage-completion marker while a forced re-run is outstanding, the comment must still be marked processed, the pass must not count against retry or failure budgets, and the engine must not loop re-dispatching it.
- **FR-006**: Comment-review passes where no forced re-run is outstanding, and all Validate stage runs, must behave exactly as they do today.
- **FR-007**: The sim twin for the post-restart SHA rebuild gains a scenario with a pending human comment at the restart, asserting FR-001 to FR-003 end to end.
- **FR-008**: That scenario must fail when the fix is neutralised (via a test-only seam, following the repository's existing neutralisation-seam convention), proving it is not vacuous.

### Key Entities

- **Forced Validate re-run**: the state, entered when the engine strips the Validate completion labels because no verified-head record is recoverable, in which only a Validate stage run may restore the completion label.
- **Comment-review pass**: the engine's processing of human comments on an item in a stage, which can emit the stage-completion marker but is not a run of the stage's own work.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim scenario with a pending comment and no recoverable SHA, a Validate stage run executes before the first landing in 100% of runs.
- **SC-002**: With the fix neutralised, the same scenario fails (the item lands without a Validate stage run).
- **SC-003**: The pending comment in that scenario is processed exactly once and the item lands only after both it and the Validate stage run have finished.
- **SC-004**: All pre-existing sim and unit tests covering Validate completion, comment review and the post-restart rebuild pass unchanged.

## Assumptions

- The same hazard exists for any other path that strips the Validate completion labels to force a re-run through the same mechanism (for example a new head invalidating the verified commit), so the restriction applies there as well; the acceptance scenario targets the post-restart case from the observed incident. This is low-impact and reversible.
- "Pending at the restart" means a human comment the engine has not yet marked processed; bot comments do not count, consistent with existing comment handling.
- The existing stage-first routing for pending comments (#2220) is unchanged and decides the order of the comment pass and the Validate stage run.
- A comment pass that changes the code is still followed by a Validate stage run before landing, as the head-verification rules already require.

## Out of Scope *(optional)*

- The rule for rebuilding the Validate completion SHA after a restart (#2343 itself).
- Changing when a comment-review pass may complete in the ordinary, non-forced case.
- Changes to the comment-routing order (#2220) or to the feedback gate.
- Making the comment-review pass itself run a full Validate.

## Source References *(optional)*

- #2343 / ADR-2343: Validate completion SHA rebuilt from the stage comment (R3 forced re-run)
- #2220 / ADR-2220: stage runs before pending comments
- `docs/state-machine.md`, "Post-restart rebuild (#2343, ADR-2343)"
- Observed in the v0.0.88 gate: bed alpha #8586, PAT auth, auto-merge off, 2026-10-17
