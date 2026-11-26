# Feature Specification: Done cleanup of a closed item with a merged PR fills the gate-checked completion labels

**Feature Branch**: `fabrik/issue-2628`
**Created**: 2026-11-26
**Status**: Specified
**Input**: User description: "## Problem

A linked PR can be merged outside Fabrik (a human merge) while its item sits at Validate. GitHub then closes the issue through `Closes #N`, and a board with the built-in **Item closed → Done** workflow moves the item to Done about one second later. That is usually before the engine's next poll, so the engine first sees the item **closed at Done**, not closed at Validate with a merged PR.

The Validate terminal path (`advanceValidateTerminalItem` / `fillGateCheckedCompleteLabels`, `engine/pr_terminal_advance.go`) never runs for such an item. The Done cleanup (`handleCleanupStage`, with the closed-paused cleanup of #2459) clears the pause pair, removes the worktree and applies `stage:Done:complete`. The gate-checked completion labels (`stage:Review:complete`, `stage:Validate:complete`) are never filled, so the item ends at Done with no record that its PR landed.

Evidence: the live test `TestPausedMergedPRRecovery` failed all three variants on both beds in the train-off legs of the v0.0.94 gate (engine SHA a1b27701).
- Timeline of `handarbeit/fabrik-test-gamma#2622`: `ClosedEvent` 05:32:36Z, then `ProjectV2ItemStatusChangedEvent` Validate → Done by `github-project-automation` at 05:32:37Z.
- The engine's next poll (05:33:15Z) logged `pause cleared because the issue is closed (#2459)` and applied `stage:Done:complete`. `stage:Validate:complete` was never added.
- Earlier passes depended on the engine reading the board listing before it showed the automation's move.

## Requirements

- **R1.** In `handleCleanupStage`, for a closed non-PR item (paused or not) that lacks `stage:<Done>:complete`: before applying that label, read the linked PR live through the existing live-read helpers (`livePRMerged` or `classifyLinkedPRTerminal`, never the cached state). When it is merged, fill the missing gate-checked completion labels with the existing `fillGateCheckedCompleteLabels`, unchanged.
- **R2.** A fill that fails, or a PR read that fails, leaves `stage:<Done>:complete` unapplied and records the same `periodic-re-eval` cooldown the #2459 pause-removal failure uses, so the whole cleanup retries on the next poll.
  - A missing linked PR or one closed unmerged skips the fill, and cleanup proceeds exactly as today.
- **R3.** Order is pause clearing (#2459), then the fill, then worktree removal and the completion label.
  - No comment, no board move, no landing-verification marker and no pause are added.
  - One log line names the labels filled.
- **R4.** An open item at Done, a PR card, and an item that already carries `stage:<Done>:complete` are unchanged.
- **R5.** Add a per-Engine neutralisation seam, `SetClosedCleanupFillDisabledForTest`, and extend the sim twin `tests/sim/closed_paused_cleanup_test.go`. The scenario is an item at Validate with a merged linked PR, closed and moved to Done before a poll. Assert the fill happens, and show that the assertion fails with the seam set.
- **R6.** Record the decision as an amendment to ADR 2459 (or a new `adrs/<this-issue>-…` ADR). Update `docs/state-machine.md`'s Done (Cleanup Stage) section in the same change, and regenerate `docs/llms-full.txt`.

## Scope

Engine change in the cleanup path only. No change to the live test `tests/e2e/paused_merged_pr_recovery_test.go`: its R5 assertion (`stage:Validate:complete` applied) is what this fix makes hold.

## Acceptance

- The sim twin passes, and fails with the seam set.
- `go test -race ./...` is green.
- `TestPausedMergedPRRecovery` passes on train-off legs in the next gate."

## Background

A pull request linked to an item can be merged outside Fabrik (a human merge) while the item sits at Validate. GitHub closes the issue through `Closes #N`, and a board using the built-in **Item closed → Done** workflow moves the item to Done about one second later. That is usually before the engine's next poll, so the engine's first sight of the item is "closed, at Done", not "closed at Validate with a merged PR".

The engine already has a path that records a landed PR when it sees the item at Validate: the Validate terminal path fills the gate-checked completion labels (`stage:Review:complete`, `stage:Validate:complete`). That path never runs for an item first seen at Done. The Done cleanup clears the pause pair (#2459), removes the worktree and applies `stage:Done:complete`, so the item finishes at Done with no label record that its PR landed.

Evidence: the live test `TestPausedMergedPRRecovery` failed all three variants on both beds in the train-off legs of the v0.0.94 gate (engine SHA a1b27701). In the timeline of `handarbeit/fabrik-test-gamma#2622`, the issue closed at 05:32:36Z and `github-project-automation` moved it Validate → Done at 05:32:37Z. The engine's next poll (05:33:15Z) logged `pause cleared because the issue is closed (#2459)` and applied `stage:Done:complete`, and `stage:Validate:complete` was never added. Earlier gate passes only succeeded because the engine happened to read the board listing before it showed the automation's move.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A landed PR is recorded when the item is first seen closed at Done (Priority: P1)

An operator or collaborator merges an item's linked PR by hand while the item sits at Validate. The board automation moves the closed item to Done before the engine polls. When the engine cleans up the item at Done, it checks whether the linked PR merged and, if so, records that by filling the missing gate-checked completion labels before completing the cleanup.

**Why this priority**: This is the defect. Without it the item ends at Done with no record of the landing, and the live test `TestPausedMergedPRRecovery` fails on train-off legs.

**Independent Test**: In the sim, seed an item at Validate with a merged linked PR, close it and move it to Done before the first poll, then poll. Assert that `stage:Review:complete` and `stage:Validate:complete` are present alongside `stage:Done:complete`.

**Acceptance Scenarios**:

1. **Given** a closed non-PR item at Done, lacking `stage:Done:complete`, whose linked PR is merged and which lacks the gate-checked completion labels, **When** the engine runs the Done cleanup, **Then** the missing gate-checked completion labels are filled before `stage:Done:complete` is applied.
2. **Given** the same item also carries `fabrik:paused` + `fabrik:awaiting-input`, **When** the cleanup runs, **Then** the pause pair is cleared first (#2459), then the fill happens, then the worktree is removed and `stage:Done:complete` applied.
3. **Given** the same item is not paused, **When** the cleanup runs, **Then** the same fill happens.

---

### User Story 2 - Failures retry instead of silently skipping the record (Priority: P1)

If the engine cannot read the linked PR, or a label fill fails, the cleanup must not complete and thereby lose the record.

**Why this priority**: `stage:Done:complete` is the only re-admission stop for the cleanup. Applying it despite a failed fill would make the missing record permanent.

**Independent Test**: In the sim, inject a failure into the live PR read and, separately, into the label fill. Assert that `stage:Done:complete` is not applied and that the next poll retries the whole cleanup and succeeds.

**Acceptance Scenarios**:

1. **Given** the live PR read fails, **When** the cleanup runs, **Then** `stage:Done:complete` is not applied, a `periodic-re-eval` cooldown is recorded (as for a #2459 pause-removal failure), and the cleanup runs again on the next poll.
2. **Given** the fill fails part-way, **When** the cleanup runs, **Then** `stage:Done:complete` is not applied, the same cooldown is recorded, and the next poll retries.

---

### User Story 3 - Items with nothing to record behave as today (Priority: P2)

Items without a merged linked PR, and items outside the affected shape, are not changed by this fix.

**Why this priority**: Guards against regressions in the existing cleanup behavior.

**Independent Test**: Run the cleanup in the sim for each shape listed below and assert the labels and writes are identical to today's, with no fill.

**Acceptance Scenarios**:

1. **Given** a closed item at Done with no linked PR, or whose linked PR was closed unmerged, **When** the cleanup runs, **Then** no fill is attempted and cleanup proceeds exactly as today.
2. **Given** an open item at Done, a PR card, or an item already carrying `stage:<Done>:complete`, **When** the engine processes it, **Then** behavior is unchanged.

---

### Edge Cases

- The linked PR read must be live; a stale cached "not merged" state must not skip the fill.
- A missing linked PR and a PR closed unmerged are not errors and are not retried.
- Some gate-checked labels may already be present; only the missing ones are filled, using the existing fill unchanged.
- The cleanup retries after a failure; a retry after a partially successful fill must converge to the same end state.
- A closed item that is paused and one that is not follow the same fill rule.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: In the Done cleanup of a closed non-PR item (paused or not) that lacks `stage:<Done>:complete`, the engine MUST read the linked PR's merged state live (through the existing live-read helpers, never cached state) before applying `stage:<Done>:complete`.
- **FR-002**: When the live read shows the linked PR is merged, the engine MUST fill the missing gate-checked completion labels (`stage:Review:complete`, `stage:Validate:complete`) using the existing `fillGateCheckedCompleteLabels`, unchanged.
- **FR-003**: If the PR read fails or the fill fails, the engine MUST leave `stage:<Done>:complete` unapplied and record the same `periodic-re-eval` cooldown that the #2459 pause-removal failure records, so the whole cleanup retries on the next poll.
- **FR-004**: If the item has no linked PR, or the linked PR is closed unmerged, the engine MUST skip the fill and proceed with the cleanup exactly as today.
- **FR-005**: The order of effects MUST be: pause clearing (#2459), then the fill, then worktree removal and the `stage:<Done>:complete` label.
- **FR-006**: The change MUST NOT add any comment, board move, landing-verification marker or pause, and MUST emit one log line naming the labels filled.
- **FR-007**: An open item at Done, a PR card, and an item already carrying `stage:<Done>:complete` MUST behave exactly as today.
- **FR-008**: A per-Engine test neutralisation seam named `SetClosedCleanupFillDisabledForTest` MUST disable the fill so tests can show that the new behavior is what satisfies an assertion.
- **FR-009**: The sim twin `tests/sim/closed_paused_cleanup_test.go` MUST gain a scenario with an item at Validate whose linked PR is merged, closed and moved to Done before a poll; it MUST assert the fill happens and MUST fail when the seam is set.
- **FR-010**: The decision MUST be recorded as an amendment to ADR 2459 or a new ADR numbered after this issue, `docs/state-machine.md`'s Done (Cleanup Stage) section MUST be updated in the same change, and `docs/llms-full.txt` MUST be regenerated.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For an item first seen closed at Done whose linked PR merged, 100% of cleanups end with the gate-checked completion labels present together with `stage:<Done>:complete`.
- **SC-002**: The new sim scenario passes with the fix and fails with `SetClosedCleanupFillDisabledForTest` set.
- **SC-003**: `go test -race ./...` is green.
- **SC-004**: `TestPausedMergedPRRecovery` passes on train-off legs (all variants, both beds) in the next release gate.
- **SC-005**: The unchanged shapes (open item, PR card, item already carrying `stage:<Done>:complete`, no linked PR, PR closed unmerged) produce the same writes as before, shown by existing tests staying green.

## Assumptions

- The existing live-read helpers (`livePRMerged` or `classifyLinkedPRTerminal`) and `fillGateCheckedCompleteLabels` are suitable as-is; this change composes them and adds no new fill logic.
- The #2459 cooldown behavior for a failed pause removal is the right retry model for the new failure modes.
- The live test `tests/e2e/paused_merged_pr_recovery_test.go` already asserts what the fix delivers and needs no change.
- Which ADR form (amendment to 2459 or a new ADR) is used is an implementation-stage choice; either satisfies FR-010.

## Out of Scope *(optional)*

- Changes to the Validate terminal path itself.
- Any change to the live test `tests/e2e/paused_merged_pr_recovery_test.go`.
- Comments, board moves, landing-verification markers or pauses for this case.
- Filling labels for open items at Done, PR cards, or items that already carry `stage:<Done>:complete`.

## Source References *(optional)*

- #2459 (closed-paused cleanup at Done), ADR 2459
- `engine/pr_terminal_advance.go` (`advanceValidateTerminalItem`, `fillGateCheckedCompleteLabels`)
- `tests/e2e/paused_merged_pr_recovery_test.go`, `tests/sim/closed_paused_cleanup_test.go`
- v0.0.94 gate failure on engine SHA a1b27701; `handarbeit/fabrik-test-gamma#2622` timeline
