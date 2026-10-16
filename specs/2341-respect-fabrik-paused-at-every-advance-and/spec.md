# Feature Specification: Respect fabrik:paused at every advance and landing site

**Feature Branch**: `fabrik/issue-2341`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "## Problem

A `fabrik:paused` label does not stop a landing. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.1.

`handleStageComplete` (engine/stages.go:112) and `runCatchUpPhase2` (engine/poll.go:2103) refresh labels live, and `attemptMergeOnValidate` re-reads the whole item live (engine/stages.go:388). None of them, nor `landValidated`, `advanceToNextStage` or `advanceToQueued`, checks `fabrik:paused`. The only pause filter is the poll loop's snapshot check (engine/poll.go:1749).

- A human pauses a yolo item while its Validate run is in progress. When the run completes, `handleStageComplete` reads the pause live and still merges (or moves the item to Queued under the merge train).
- A human pauses an item before the listing or cache shows the label. Phase 2 reads the pause live and still lands.

A pause is the operator's stop button; it must win over every automatic advance and landing.

## Requirements

- **R1:** Every site that advances an item or lands its PR (`attemptMergeOnValidate`, `landValidated`, `advanceToNextStage`, `advanceToQueued`, and their callers in `handleStageComplete` and `runCatchUpPhase2`) refuses when the labels it is about to act on carry `fabrik:paused`, using the live labels those sites already read.
- **R2:** A refused advance or landing logs one line naming the pause and leaves every label as it was (no completion label removed, no comment).
- **R3:** The merge-train landing paths re-check the pause per member before landing (a member paused after its batch formed is not landed).
- **R4:** Once the pause is lifted by a human comment, the item advances or lands on a later poll as it would have.

## Acceptance

- Unit tests for each site: a paused item is not advanced, merged, enqueued or moved to Queued; with the pause removed the same call proceeds.
- A sim twin in `tests/sim`: a yolo item paused mid-Validate is not merged when Validate completes, and lands after the pause is lifted.
- Non-vacuity: removing the new check makes the sim twin fail."

## Background

A `fabrik:paused` label is the operator's stop button: it is how a human tells Fabrik to stop acting on an item. Today the pause is honoured only at the front door. The poll loop skips a paused item based on its listing snapshot. Once an item is already in motion, or the snapshot is stale, the pause is ignored.

The engine state audit of 2026-10-15 (§1.1) found two ways this loses the stop:

- **Pause during a run.** A human pauses a yolo item while its Validate run is in progress. When the run completes, the completion handling reads the labels live, sees the pause, and still merges the PR, or moves the item to Queued when the merge train is on.
- **Pause ahead of the snapshot.** A human pauses an item before the board listing or cache shows the label. The catch-up phase reads the pause live and still lands the item.

In both cases the engine has the pause in hand and acts anyway. The failure is hard to undo: a merged PR or an item moved to Queued cannot be recalled by removing a label afterwards. A pause must win over every automatic advance and landing.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A pause placed during a run stops the landing (Priority: P1)

An operator pauses a yolo item while its Validate stage is running because they spotted a problem. When the run finishes, Fabrik does not merge the PR, does not enqueue the item for the merge train, and does not advance it. The item stays in Validate, paused, with its labels as they were.

**Why this priority**: This is the data-loss-shaped case. A landing is irreversible in practice, and it happens against an explicit operator instruction.

**Independent Test**: Seed a yolo item in Validate with a running Validate worker, add `fabrik:paused` while the worker runs, let the worker complete, and observe that no merge, enqueue or column move happens.

**Acceptance Scenarios**:

1. **Given** a yolo item whose Validate run is in progress, **When** a human adds `fabrik:paused` and the run then completes, **Then** the PR is not merged and the item is not moved to Done.
2. **Given** the same item with the merge train on, **When** the run completes while the item is paused, **Then** the item is not moved to Queued.
3. **Given** the refusal in scenarios 1 and 2, **When** the labels are inspected, **Then** the completion labels are unchanged and no comment was posted.

---

### User Story 2 - A pause the snapshot has not seen yet still stops the landing (Priority: P1)

An operator pauses an item moments before the board listing or cache reflects the label. When the catch-up phase evaluates the item, it reads the pause live and does not land or advance it.

**Why this priority**: The window between a human action and the cache catching up is routine, and it is exactly when the operator expects the stop to hold.

**Independent Test**: Put an item that is ready to land in a state where the cached snapshot lacks `fabrik:paused` but the live labels carry it, run the catch-up phase, and observe that nothing lands or advances.

**Acceptance Scenarios**:

1. **Given** an item ready to land whose live labels carry `fabrik:paused` but whose snapshot does not, **When** the catch-up phase evaluates it, **Then** it is not merged, enqueued or advanced.
2. **Given** an item ready to advance to the next stage with the same stale snapshot, **When** the catch-up phase evaluates it, **Then** it is not advanced.

---

### User Story 3 - A member paused after its train batch formed is not landed (Priority: P2)

Under the merge train, a batch of members is assembled and validated together. A member paused after the batch formed must not be landed with the rest.

**Why this priority**: The train holds members for a long time, so the pause window is wide. Landing is still the same irreversible act as in the other stories.

**Independent Test**: Form a batch of several members, pause one before landing starts, and observe that the others land and the paused one does not.

**Acceptance Scenarios**:

1. **Given** a formed batch in which one member becomes paused before landing, **When** the train reaches the landing step, **Then** the paused member is not landed.
2. **Given** a batch landing one member at a time, **When** the next member to land is paused, **Then** that member is skipped and none of its labels are changed.

---

### User Story 4 - Lifting the pause resumes normal progress (Priority: P1)

After the operator lifts the pause with a human comment, the item advances or lands on a later poll exactly as it would have if it had never been paused.

**Why this priority**: A stop that cannot be released would trade one failure for another.

**Independent Test**: Run User Story 1 to the refusal, lift the pause with a human comment, poll, and observe the merge.

**Acceptance Scenarios**:

1. **Given** an item that was refused a landing because it was paused, **When** a human comment lifts the pause, **Then** a later poll lands it as it would have.
2. **Given** an item that was refused an advance because it was paused, **When** the pause is lifted, **Then** a later poll advances it to the next stage.

---

### Edge Cases

- The pause appears after the engine has read the labels but before it acts: the engine acts on what it read, and the next poll honours the pause. The requirement covers the labels each site is about to act on, not a lock across the whole operation.
- A site is reached on a path where no live labels were read: it uses the labels it has. This work does not add new reads (see Assumptions).
- An item that is paused and also carries `fabrik:cruise`: the pause wins. Neither autonomy label overrides a pause.
- A refusal repeats every poll while the item stays paused: each refusal logs one line, and nothing is written to GitHub.
- A member removed from a train batch because it is paused must not be counted as an ejection or a failure of the batch.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every site that advances an item to the next stage or lands its PR refuses to act when the labels it is about to act on carry `fabrik:paused`. The sites are `attemptMergeOnValidate`, `landValidated`, `advanceToNextStage` and `advanceToQueued`, plus their callers in `handleStageComplete` and `runCatchUpPhase2`.
- **FR-002**: The refusal in FR-001 uses the live labels those sites already read, and adds no new label read.
- **FR-003**: A refused advance or landing logs exactly one line naming the pause.
- **FR-004**: A refused advance or landing leaves every label as it was: no completion label is removed, no label is added, and no comment is posted.
- **FR-005**: A refused advance or landing makes no board Status move, no merge and no enqueue.
- **FR-006**: The merge-train landing paths check for the pause per member before landing it. A member that is paused when its landing is about to start is not landed, even if it was not paused when its batch formed.
- **FR-007**: A train member skipped under FR-006 is left as it was and is not counted toward ejection or runaway limits.
- **FR-008**: Once a human comment lifts the pause, the item advances or lands on a later poll as it would have if it had not been paused.
- **FR-009**: Each guarded site has a unit test showing that a paused item is not advanced, merged, enqueued or moved to Queued, and that the same call proceeds once the pause is removed.
- **FR-010**: A sim scenario shows that a yolo item paused mid-Validate is not merged when Validate completes, and lands after the pause is lifted.
- **FR-011**: The sim scenario in FR-010 fails when the new pause check is removed.

### Key Entities *(if applicable)*

- **Pause**: the `fabrik:paused` label (always with `fabrik:awaiting-input`), the operator's stop signal. It is lifted only by a human comment created after it was applied.
- **Advance site**: any code path that moves an item to its next stage or to Queued.
- **Landing site**: any code path that merges an item's PR or lands it through the merge train.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Across every guarded site, a paused item is never merged, enqueued, advanced or moved to Queued. Per-site tests prove it.
- **SC-002**: In the end-to-end sim scenario, a yolo item paused during Validate is not merged when Validate completes, and is merged on a poll after the pause is lifted.
- **SC-003**: Removing the new pause check makes the sim scenario fail.
- **SC-004**: A refused advance or landing produces zero label changes, zero comments and exactly one log line per refusal.
- **SC-005**: No change in behaviour for items that are not paused.

## Assumptions

- The pause is read from the live labels the sites already read; no new GitHub reads are added.
- The pause wins over `fabrik:yolo` and `fabrik:cruise` at every guarded site.
- A refusal is silent on GitHub: the engine posts no comment, because the pause already tells the operator what is happening.
- The existing resume rule is unchanged: a human comment created at or after the pause lifts it.
- The pause is checked at the moment each site is about to act. A pause placed after that point is honoured on the next poll.

## Out of Scope *(optional)*

- Changing how a pause is applied or lifted, or the human-comment resume rule.
- Adding new live label reads where none exist today.
- Poll-loop admission and snapshot filtering, which already honour the pause.
- Other findings of the engine state audit.

## Source References *(optional)*

- Engine state audit, `docs/design/engine-state-audit-2026-10.md` (2026-10-15), §1.1.
