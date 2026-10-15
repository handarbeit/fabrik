# Feature Specification: A stage runs before any pending comment is processed

**Feature Branch**: `fabrik/issue-2220`
**Created**: 2026-10-13
**Status**: Specified
**Input**: User description: "Seen on #2218. A steering comment posted while it sat in Backlog was waiting when it entered Specify. The engine ran the `specify-comment` skill instead of `specify`. That pass emitted `FABRIK_STAGE_COMPLETE` and the item advanced to Research. The `specify` skill never ran and no Specify stage comment was posted."

## Background

`processItem` (`engine/item.go`) sends an item to `processComments` whenever `findNewComments` returns anything, ahead of the stage-run path. It does not check whether the current stage has run on this entry. `finalizeComments` (`engine/comments.go`) then accepts `FABRIK_STAGE_COMPLETE` from the comment pass ("comment processing completed stage") and completes the stage.

The comment skills are written for a stage that has already produced its output. So a stage entered with a pending comment can be completed by its comment skill alone:

- **Specify:** a thin issue is not expanded into a spec (compare #2219, which got a real Specify run, with #2218).
- **Plan:** no Plan comment is posted, and Implement's checklist update depends on it.
- **Research:** no research comment for Plan to read.
- **Implement:** no draft PR or `FABRIK_PR_CREATE` handling from the stage path.

Commenting on an item before it reaches a stage is normal practice, so this is easy to trigger.

## Approach

Run the stage first, and leave the pending comments for the comment path afterwards, unchanged. The stage run already sees every issue comment under "Prior Discussion" (`buildPrompt`). After it completes, `feedbackGateBlocks` (#1953) holds the advance while any comment is unprocessed. The comment pass then handles the comments as today, and the item advances. The cost is one extra comment-pass invocation when a comment was waiting. No skill, prompt or comment-bookkeeping change is needed.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stage runs before a pre-existing comment is processed (Priority: P1)

An operator comments on an item while it is parked or in an earlier column. When the item enters a stage (Specify, Research, Plan, Implement), the stage's own skill runs first and produces its output. The waiting comment is then handled by the comment skill, and only after that does the item advance.

**Why this priority**: This is the reported defect. Today the stage's output (spec, research, plan, draft PR) is silently skipped and the item advances without it.

**Independent Test**: In the sim bed, place an item with one unprocessed human comment into Specify. Assert the `specify` skill runs first, then exactly one comment pass on that comment, then the advance, and that the advance never precedes the comment pass.

**Acceptance Scenarios**:

1. **Given** an item entering Specify with a pending comment, **When** the engine processes it, **Then** the Specify stage run happens before any comment pass, and the comment gets no 👀/🚀 and no watermark from the stage run.
2. **Given** that stage run has completed, **When** the comment is still unprocessed, **Then** the advance is held until the comment pass has processed it, and then the item advances.
3. **Given** an item entering Plan with a pending comment, **When** the engine processes it, **Then** a Plan stage comment is posted before the comment pass runs.

---

### User Story 2 - Comments after the stage has run behave as today (Priority: P2)

A comment posted once the stage has already run on this entry (stage completed, blocked on input, or being reworked) goes straight to the comment path. The stage is not re-run.

**Why this priority**: Guards against regressing the existing, working comment-review behaviour, including pause-resume by a human comment.

**Independent Test**: Run a stage to completion or block, post a comment, and assert only the comment pass runs with no stage re-run.

**Acceptance Scenarios**:

1. **Given** a stage that has run on this entry, **When** a new comment arrives, **Then** it is routed to the comment path and the stage does not re-run.
2. **Given** an item paused with `fabrik:paused`, **When** a human comment resumes it (ADR-1813), **Then** behaviour is unchanged.

---

### User Story 3 - A restart does not repeat the stage (Priority: P2)

If the engine restarts between the stage run and the comment pass, it recognises from durable state that the stage already ran and goes to the comment pass.

**Why this priority**: Duplicate stage runs waste budget and could overwrite output.

**Independent Test**: In the sim, run the stage with a pending comment, restart the engine before the comment pass, and assert the stage skill is not invoked a second time.

**Acceptance Scenarios**:

1. **Given** a stage has run on the current entry and the engine restarts, **When** polling resumes with the comment still pending, **Then** only the comment pass runs.

---

### Edge Cases

- The stage blocks on input (`FABRIK_BLOCKED_ON_INPUT`) with comments pending: it has run, so the comments take the comment path.
- The item is moved out of and back into the column: "current entry" is the latest entry, so the stage counts as not yet run on the new entry.
- A comment arrives while the stage run is in progress: it stays pending and is handled by the comment path after the stage run.
- Durable state is unreadable or ambiguous: the stage runs.
- No comment is pending: behaviour is unchanged.
- A paused item resumed by a human comment: the stage has already run, so the comment path applies.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When an item is in a stage that has not run on the current entry into its column, `processItem` runs the stage, even when `findNewComments` returns comments. The comments are left unprocessed: no 👀/🚀 and no watermark from the stage run.
- **FR-002**: "Has run on the current entry" is decided from durable state that survives a restart. For example: a stage output comment newer than the item's latest move into the column, or the stage's `in_progress`/`complete`/`failed` label history. It is not decided from in-memory state alone. If the state cannot be determined, the stage runs.
- **FR-003**: Once the stage has run on this entry, pending comments are routed to the comment path exactly as today, whether the stage completed, blocked on input or is still being reworked.
- **FR-004**: The advance after the stage completes is held by `feedbackGateBlocks` until the pending comments are processed. If any advance path that a stage completion can reach does not consult the gate, it is brought under it.
- **FR-005**: A paused item resumed by a human comment (ADR-1813) is unchanged. The stage has already run, so FR-003 applies.
- **FR-006**: `docs/state-machine.md` (comment processing lifecycle) and `docs/stage-lifecycle.md` describe the rule. Regenerate `docs/llms-full.txt`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A sim test moves an item with a pending comment into Specify. The `specify` stage skill runs first, then one comment pass on that comment, then the item advances. It never advances before the comment pass. Shown non-vacuous by restoring today's routing (the comment pass runs first and the stage skill never runs).
- **SC-002**: The same test for Plan asserts a Plan stage comment is posted before the comment pass runs.
- **SC-003**: A comment posted after the stage has run is handled by the comment path with no stage re-run, unchanged from today.
- **SC-004**: A restart between the stage run and the comment pass does not run the stage a second time (FR-002).

## Assumptions

- Comment skills remain unchanged and continue to assume the stage's output already exists.
- The stage run needs no changes to see pending comments; it already receives them as "Prior Discussion".
- One extra comment-pass invocation per entry that had a waiting comment is an acceptable cost.
- "Current entry" means the item's most recent move into the stage's column, as recorded in durable GitHub state.
- Where durable state conflicts or cannot be read, running the stage again is the safer default than skipping it.

## Out of Scope

- Passing pending comments to the stage run as "New Comments", or changing stage or comment skills.
- PR review-thread feedback, which has its own reinvoke path.

## Notes

- Target release: 0.0.85.
