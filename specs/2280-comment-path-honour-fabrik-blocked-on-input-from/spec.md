# Feature Specification: Honour FABRIK_BLOCKED_ON_INPUT on the comment path

**Feature Branch**: `fabrik/issue-2280`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`FABRIK_BLOCKED_ON_INPUT` is honoured only on the stage-dispatch path (`finalizeStageOutcome`). A comment-review run, or a review, CI-fix or rebase reinvoke, that emits it is not paused: the marker is ignored, the comment is marked processed, and the item carries on as if the question had been answered. Found while tracing #1970 (PR #2278), which left it out of scope.

A comment skill that needs a human answer, for example \"which of these two approaches?\", has no way to stop and wait. It has to guess or complete.

## Requirements

- **R1:** A comment-path run (comment review and every reinvoke kind) that emits `FABRIK_BLOCKED_ON_INPUT` without `FABRIK_STAGE_COMPLETE` pauses the item exactly as the stage path does: `fabrik:paused` + `fabrik:awaiting-input`, one explanatory comment carrying the run's question, and the #1813 resume rule unchanged.
- **R2:** The comment that triggered the run is still marked processed (🚀), so the pause is lifted only by a *new* human comment, never by re-reading the one just handled.
- **R3:** Both markers together keep today's precedence. Check what `finalizeStageOutcome` does and match it.
- **R4:** Update `docs/state-machine.md` (comment-processing lifecycle, §4.4) and regenerate `docs/llms-full.txt`.

## Acceptance

- A sim test where a comment-review run emits `FABRIK_BLOCKED_ON_INPUT` leaves the item paused with the label pair and the question comment, and a later human comment resumes it. The test is shown non-vacuous by disabling the new handling.
- A comment run without the marker is unchanged."

## Background

`FABRIK_BLOCKED_ON_INPUT` is the worker's way of saying "I cannot proceed without a human answer." On the stage-dispatch path the engine honours it: it pauses the item (`fabrik:paused` + `fabrik:awaiting-input`) and posts a comment that @mentions the right people and carries the worker's question. The engine only honours it when the run completed without error and did not also signal stage completion. When both markers appear, completion wins and the blocked marker is ignored.

The comment path has no such handling. Comment review and the review, CI-fix and rebase reinvokes all run comment skills, and those skills are told they may emit the marker. If one does, the engine ignores it, marks the triggering comment processed, and carries on as if the question had been answered. A comment skill that needs a decision, for example "which of these two approaches?", cannot stop and wait. It has to guess or complete, which risks acting on a wrong assumption with no human in the loop. This gap was found while tracing #1970 (PR #2278), which left it out of scope.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A comment-review run can ask a question and wait (Priority: P1)

A human comments on an item during a stage. The comment-review run cannot resolve the request without a human decision, so it emits `FABRIK_BLOCKED_ON_INPUT` with a summary block holding its question. The engine pauses the item and posts the question. The human answers in a new comment, which resumes the item.

**Why this priority**: This is the whole defect. Without it a comment skill cannot ask anything, and the item continues on a guess.

**Independent Test**: In the sim bed, script a comment-review run that emits the marker plus a summary question. Assert the item carries `fabrik:paused` and `fabrik:awaiting-input`, exactly one explanatory comment with the question exists, and the triggering comment has 🚀. Post a new human comment and assert the item resumes and that comment is processed.

**Acceptance Scenarios**:

1. **Given** an item with an unprocessed human comment, **When** the comment-review run emits `FABRIK_BLOCKED_ON_INPUT` without `FABRIK_STAGE_COMPLETE`, **Then** the item gains `fabrik:paused` + `fabrik:awaiting-input` and one comment carrying the run's question is posted.
2. **Given** the item is paused by that run, **When** the next poll runs without any new human comment, **Then** the item stays paused and the already-handled comment is not re-processed.
3. **Given** the item is paused by that run, **When** a new human comment is created at or after the pause, **Then** the pause is lifted under the existing resume rule and the new comment is processed.

---

### User Story 2 - Every reinvoke kind behaves the same (Priority: P2)

The review, CI-fix and rebase reinvokes are comment-path runs too. A skill running in any of them that emits the marker pauses the item the same way as in story 1.

**Why this priority**: The same defect exists in each, and a fix limited to ordinary comment review would leave three silent failure modes.

**Independent Test**: For each reinvoke kind, script a run that emits the marker and assert the same paused state and question comment as in story 1.

**Acceptance Scenarios**:

1. **Given** a review, CI-fix or rebase reinvoke, **When** the run emits `FABRIK_BLOCKED_ON_INPUT` without `FABRIK_STAGE_COMPLETE`, **Then** the item is paused with the label pair and one question comment.

---

### User Story 3 - Runs without the marker, and runs with both markers, are unchanged (Priority: P1)

Comment-path runs that do not emit the marker behave exactly as today. A run emitting both markers resolves as on the stage path.

**Why this priority**: The change must not alter the behaviour of the large majority of runs.

**Independent Test**: Run existing comment-path sim scenarios unchanged and assert identical outcomes. Add a run emitting both markers and assert it follows the stage-path precedence.

**Acceptance Scenarios**:

1. **Given** a comment-path run that does not emit `FABRIK_BLOCKED_ON_INPUT`, **When** it finishes, **Then** labels, comments and reactions are exactly as before this change.
2. **Given** a comment-path run that emits both `FABRIK_BLOCKED_ON_INPUT` and `FABRIK_STAGE_COMPLETE`, **When** it finishes, **Then** the outcome matches what the stage path does with both markers (completion wins; the blocked marker is ignored).

---

### Edge Cases

- The run emitted the marker but ended in an error: the stage path honours the marker only for an error-free run, so the comment path does the same and treats the run as it does any failed comment run.
- The marker is present but no `FABRIK_SUMMARY_BEGIN`/`FABRIK_SUMMARY_END` block is: the pause comment uses the same fallback wording as the stage path.
- The item is already paused or already has `fabrik:awaiting-input` when the run ends: labels are applied idempotently and no duplicate question comment is posted.
- The label write that pauses the item fails: the comment must not be left processed with no pause in force. Match whatever guarantee the stage path gives, or retry on the next poll.
- The triggering comment set contains several comments: all of them are marked processed (🚀), as in a normal run.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A comment-path run (ordinary comment review, review reinvoke, CI-fix reinvoke, rebase reinvoke) that finishes without error and emits `FABRIK_BLOCKED_ON_INPUT` without `FABRIK_STAGE_COMPLETE` MUST pause the item by applying `fabrik:paused` and `fabrik:awaiting-input`.
- **FR-002**: The pause MUST post one explanatory comment carrying the run's question (from its summary block), in the same form the stage path posts, including the same @mention targets.
- **FR-003**: The comment(s) that triggered the run MUST still be marked processed (🚀) when the run pauses the item, so that only a new human comment can lift the pause.
- **FR-004**: The resume rule for the pause MUST be unchanged: a human comment created at or after the latest `fabrik:paused` labeling resumes the item, per the existing shared predicate (ADR-1813). No new resume path is introduced.
- **FR-005**: When a run emits both `FABRIK_BLOCKED_ON_INPUT` and `FABRIK_STAGE_COMPLETE`, or errors, the outcome MUST match the stage path's precedence for the same inputs.
- **FR-006**: A comment-path run that does not emit `FABRIK_BLOCKED_ON_INPUT` MUST behave exactly as before.
- **FR-007**: Detection of the marker MUST use the same rule as the stage path (marker alone on its own line), not a separate matcher.
- **FR-008**: `docs/state-machine.md` (comment-processing lifecycle, §4.4, and the marker-handling section where it says the blocked marker applies to the stage path) MUST describe the comment-path behaviour, and `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-009**: A sim test MUST cover story 1 end to end (pause, label pair, question comment, 🚀 on the triggering comment, no reprocessing while paused, resume on a later human comment) and MUST be shown non-vacuous by disabling the new handling and observing the test fail.

### Key Entities

- **Comment-path run**: A Claude invocation made to process comments or feedback on an item, covering comment review and the review, CI-fix and rebase reinvokes, as opposed to a stage-dispatch run.
- **Pause pair**: The labels `fabrik:paused` and `fabrik:awaiting-input`, which together mean "waiting for a human comment."

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim bed, a comment-review run that emits the marker leaves the item with both pause labels, exactly one question comment, and 🚀 on the triggering comment.
- **SC-002**: A later human comment on that paused item resumes it, and the same sim test fails when the new handling is disabled.
- **SC-003**: Each reinvoke kind (review, CI-fix, rebase) pauses on the marker in the sim bed.
- **SC-004**: All existing comment-path sim and unit scenarios pass unmodified.
- **SC-005**: The documented comment-processing lifecycle and the committed `docs/llms-full.txt` bundle match the new behaviour.

## Assumptions

- "Exactly as the stage path does" means the stage path's pause labels, status-line update, @mention comment and fallback wording are reused, not reimplemented with different text.
- Error and both-marker precedence follow the stage path: the blocked marker is honoured only for an error-free run that did not signal completion.
- Whatever other output or commits a comment run produces before it emits the marker are handled as they are today; this change only adds the pause. The run is not treated as a failure just because it asked a question.
- A paused comment-path item that gets a new human comment is resumed and its comment processed by the comment path, since the existing resume rule already routes it there.
- A pause from a comment-path run does not count against retry or cycle budgets, matching the stage path where an input request is not a failure. The Plan stage can confirm the exact accounting.

## Out of Scope

- Adding a way for the overseer or any tool to lift or answer a pause (ADR-1813 keeps resume as a human comment only).
- Changing the resume rule, the pause comment's wording, or the stage-path behaviour.
- Changing which skills are told they may emit the marker or editing skill prompts.
- Handling any other marker (`FABRIK_NO_WORK_NEEDED`, spawn markers) on the comment path.

## Source References

- Issue #2280, found while tracing #1970 (PR #2278)
- ADR-1813 and `docs/state-machine.md` §4.4 (resume rule), §4 marker priority section (stage-path precedence)
