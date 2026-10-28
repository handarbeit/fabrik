# Feature Specification: A closed paused item at Done is still cleaned up and archived

**Feature Branch**: `fabrik/issue-2459`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

A closed issue that reaches the Done (cleanup) column while carrying `fabrik:paused` is never cleaned up, labelled complete or archived:

- `itemNeedsWork`'s cleanup branch returns early for any `fabrik:paused` item (`engine/item.go`, the `stage.CleanupWorktree` block), so `handleCleanupStage` never runs and `stage:Done:complete` is never applied.
- Archiving (`engine/archive_done_settle.go`) only considers items with `stage:Done:complete`.
- The worktree janitor (`janitorIsOffBoardOrCleanupComplete`) reaps a worktree on the board only when its cleanup stage carries `stage:<Done>:complete`.

So the worktree and `fabrik/issue-N` branch stay on disk and the card stays on the board for good; the only way out is removing the pause label by hand. Observed 2026-10-13 on a downstream project: a test issue closed as not planned and moved to Done kept its worktree and branch until `fabrik:paused` was removed manually.

Skipping cleanup for an **open** paused item is reasonable — its worktree may hold unfinished work someone is investigating. A **closed** issue has nothing left to protect.

## Requirements

- **FR-001** At a cleanup stage, a closed (non-PR) item is admitted for cleanup even when it carries `fabrik:paused` (and `fabrik:awaiting-input`).
- **FR-002** Cleanup of such an item removes `fabrik:paused` and `fabrik:awaiting-input` along with its other cleanup work, so it ends in the same state as an unpaused closed item (`stage:Done:complete`, worktree removed, then archived on the usual schedule).
- **FR-003** An open paused item at a cleanup stage is still skipped, unchanged.
- **FR-004** No comment is posted for clearing the pause on a closed item (it is closed; nobody is waiting on it). One log line names the item and that the pause was cleared because the issue is closed.
- **FR-005** `itemMayNeedWork` and `itemNeedsWork` agree on admission for this case.

## Scope

In: the cleanup admission in `engine/item.go`, `handleCleanupStage`, the as-built docs (`docs/state-machine.md` on cleanup and pause), a sim twin. Out: changing what pause means for open items; the janitor's own predicate (it follows once `stage:Done:complete` is applied).

## Acceptance

- A sim scenario: a closed issue with `fabrik:paused` + `fabrik:awaiting-input` placed in Done with an existing worktree ends with the worktree removed, both pause labels gone and `stage:Done:complete` applied; the same scenario with the issue open leaves everything untouched.
- The test fails with the fix neutralised."

## Background

A closed issue can reach the Done (cleanup) column while still carrying the engine's pause pair (`fabrik:paused` + `fabrik:awaiting-input`) — for example a test issue that was paused, then closed as not planned and moved to Done. The cleanup stage deliberately skips any paused item, so cleanup never runs and the item never receives `stage:Done:complete`. Everything downstream keys on that completion label: archiving only considers items that carry it, and the worktree janitor reaps an on-board worktree only when its cleanup stage is complete. The result is a permanent leak: the worktree and the `fabrik/issue-N` branch stay on disk and the card stays on the board until someone removes the pause label by hand. This was observed on 2026-10-13 on a downstream project.

Skipping cleanup for an **open** paused item is intentional: its worktree may hold unfinished work someone is still investigating. A **closed** issue has nothing left to protect, so the pause protection should not apply to it. The pause is also meaningless for a closed item, since nobody can be waiting on a human reply on a closed issue.

Prior art in the codebase: closed items are already admitted at cleanup stages even without a worktree (so they receive the completion label), and the closed-to-Done settle scan already treats closed-and-paused as a normal, reachable state. This issue closes the remaining gap where the pause label blocks that same path.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A closed paused item is cleaned up at Done (Priority: P1)

An operator closes an issue that is currently paused (with `fabrik:paused` and `fabrik:awaiting-input`) and it lands in the Done column. The engine cleans it up on its own, without manual label surgery.

**Why this priority**: This is the defect — without it, worktrees, branches and board cards leak indefinitely and require manual intervention.

**Independent Test**: In the sim, seed a closed, non-PR issue in Done with both pause labels and an existing worktree; run polls; verify the worktree is gone, both pause labels are gone, and `stage:Done:complete` is present.

**Acceptance Scenarios**:

1. **Given** a closed issue in the Done column carrying `fabrik:paused` and `fabrik:awaiting-input` with an existing worktree, **When** the engine polls, **Then** the worktree is removed, `fabrik:paused` and `fabrik:awaiting-input` are removed, and `stage:Done:complete` is applied.
2. **Given** the same item after cleanup, **When** the usual archive schedule elapses, **Then** it is archived exactly as an unpaused closed item would be.
3. **Given** a closed paused item at Done with no worktree on disk, **When** the engine polls, **Then** it still receives `stage:Done:complete` and loses the pause labels.

---

### User Story 2 - An open paused item is left alone (Priority: P1)

An open issue that is paused and sits in the Done column keeps its worktree and labels untouched, as today.

**Why this priority**: The existing protection for in-progress investigations must not regress; it is the reason the pause skip exists.

**Independent Test**: Same sim scenario as Story 1 with the issue open; verify no label changes, the worktree is intact and no cleanup occurs.

**Acceptance Scenarios**:

1. **Given** an open issue in Done carrying `fabrik:paused` and `fabrik:awaiting-input` with an existing worktree, **When** the engine polls repeatedly, **Then** the worktree, both pause labels and the absence of `stage:Done:complete` are all unchanged.

---

### User Story 3 - Quiet clearing of the pause (Priority: P2)

Clearing the pause on a closed item produces no comment on the issue, only a log line for the operator.

**Why this priority**: Avoids notification noise on a closed issue; secondary to the correctness fix.

**Independent Test**: In the Story 1 scenario, assert that no comment is posted and that exactly one log line names the item and says the pause was cleared because the issue is closed.

**Acceptance Scenarios**:

1. **Given** a closed paused item being cleaned up, **When** the pause labels are removed, **Then** no comment is added to the issue and one log line identifies the item and the reason (issue is closed).

---

### Edge Cases

- A closed item at Done carrying only `fabrik:paused` (without `fabrik:awaiting-input`), or only `fabrik:awaiting-input`: both are cleared if present; the item is admitted either way.
- A closed item that already has `stage:Done:complete` and the pause labels: not re-admitted for cleanup (existing completion short-circuit is unchanged); the leftover pause labels are not this issue's concern beyond what cleanup of a not-yet-complete item does.
- A closed **PR** item at Done with a pause label: the existing exclusion of PR items from cleanup admission is unchanged (out of scope).
- An open item in Done that is paused, then closed later: admitted on the poll after it is observed closed.
- The admission decision is made at two points (the cheap pre-filter and the per-item check); they must not disagree, or the item is either never reached or reached and then skipped (FR-005).
- A label-removal failure while clearing the pause must not prevent the rest of cleanup from being retried on a later poll; cleanup stays idempotent.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: At a cleanup stage, a closed non-PR item is admitted for cleanup even when it carries `fabrik:paused` and/or `fabrik:awaiting-input`.
- **FR-002**: Cleaning up such an item removes `fabrik:paused` and `fabrik:awaiting-input` together with its other cleanup work, so it ends in the same state as an unpaused closed item (`stage:Done:complete` applied, worktree removed, then archived on the usual schedule).
- **FR-003**: An open paused item at a cleanup stage is still skipped, with behaviour unchanged.
- **FR-004**: No comment is posted when the pause is cleared on a closed item; exactly one log line names the item and states that the pause was cleared because the issue is closed.
- **FR-005**: The cheap pre-filter admission (`itemMayNeedWork`) and the per-item admission (`itemNeedsWork`) agree on whether a closed paused item at a cleanup stage needs work.

### Key Entities *(if applicable)*

- **Closed paused item at a cleanup stage**: a non-PR issue that is closed, sits in the cleanup (Done) column, and carries the pause pair; the subject of this change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A closed issue carrying `fabrik:paused` + `fabrik:awaiting-input` placed in Done with an existing worktree ends, with no manual action, with the worktree removed, both pause labels absent and `stage:Done:complete` present.
- **SC-002**: The same scenario with the issue open leaves the worktree, the labels and the absence of the completion label untouched across repeated polls.
- **SC-003**: No comment is posted on the closed item as a result of clearing the pause, and one log line records it.
- **SC-004**: The new sim scenario fails when the fix is neutralised, demonstrating it exercises the defect.
- **SC-005**: Operators no longer need to remove `fabrik:paused` by hand to get a closed Done item cleaned up and archived.

## Assumptions

- "Closed" means the issue's state is closed on GitHub as the engine observes it (regardless of close reason, including closed as not planned).
- Pull-request items remain excluded from cleanup admission, as they are today; admitting them is a separate, deferred concern.
- Clearing the pause labels on a closed item is safe because no human reply can be pending on a closed issue and the item has already left the active pipeline.
- The janitor and the archive scan need no change: once `stage:Done:complete` is applied they pick the item up through their existing predicates.

## Out of Scope *(optional)*

- Changing what pause means for open items.
- Changing the worktree janitor's own predicate.
- Admitting pull-request items for cleanup.
- Any behaviour change for closed paused items at non-cleanup stages.

## Source References *(optional)*

- Cleanup admission and early return for paused items: `engine/item.go` (the `stage.CleanupWorktree` blocks in `itemMayNeedWork` and `itemNeedsWork`, and `handleCleanupStage`)
- Archive scan: `engine/archive_done_settle.go`
- Janitor predicate: `janitorIsOffBoardOrCleanupComplete`
- As-built docs: `docs/state-machine.md` (cleanup and pause sections)
