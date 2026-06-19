# Feature Specification: e2e — Paused-Item Merged-PR Recovery (#874 Class Stays Closed, Gate-Label-Agnostic)

**Feature Branch**: `fabrik/issue-896`
**Created**: 2026-06-18
**Status**: Draft
**Input**: User description: "test(e2e): paused-item merged-PR recovery — #874 class stays closed (gate-label-agnostic)"

## Background

The bug class behind #874 — a paused item whose linked PR merges externally strands forever because the recovery loops are kept disjoint by hand-maintained label negations — is the exact failure that issue #887 (settle-owner) closes structurally. #887 installs a single advance owner that heals the paused+merged state regardless of which gate label is present, so no new gate-label variant can slip through.

No e2e scenario currently exercises this recovery path. The existing full-pipeline e2e tests (`TestSmokeSingleRepoFullPipeline`, `TestYoloAutoMergeLabel`, `TestConvergenceRace`, `TestCruiseFullPipeline`) do not synthesize the stuck state — they rely on the engine's happy-path advance. A regression that re-introduces label-specific negation coupling would not be caught.

This issue adds `TestPausedMergedPRRecovery`: the end-to-end regression guard that the #874 class stays closed. The test is parameterized over three gate-label variants (`fabrik:awaiting-ci`, `fabrik:awaiting-review`, and a control with neither). The "neither" control is the load-bearing case: it proves advancement does **not** depend on a specific gate label and would have stranded permanently on a pre-#887 engine.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Recovery with `fabrik:awaiting-ci` Gate Label (Priority: P1)

A Validate-stage issue is in the stuck state: paused, awaiting input, and held behind the CI gate. A human (or auto-merge bot) merges the linked PR externally while the issue is paused. The settle-owner detects the merged PR and heals the issue: it applies `stage:Validate:complete`, removes the gate and pause labels, advances the board to Done, and closes the issue.

**Why this priority**: Regression guard for the #874 subcase where the CI gate label blocks recovery.

**Independent Test**: Drive a cruise issue to Validate, force `fabrik:paused + fabrik:awaiting-input + fabrik:awaiting-ci` via `AddLabel`, merge the PR via `MergePR`, then wait for `stage:Validate:complete` and issue CLOSED.

**Acceptance Scenarios**:

1. **Given** a Validate-stage issue carries `fabrik:paused + fabrik:awaiting-input + fabrik:awaiting-ci`, **When** its linked PR is merged externally by the test harness, **Then** within the poll budget `stage:Validate:complete` is applied, `fabrik:paused`, `fabrik:awaiting-input`, and `fabrik:awaiting-ci` are removed, the board column advances to Done, and the issue is CLOSED.

---

### User Story 2 — Recovery with `fabrik:awaiting-review` Gate Label (Priority: P1)

Same as User Story 1 but the gate label is `fabrik:awaiting-review`. Verifies that the settle-owner heals this variant as well.

**Why this priority**: Regression guard for the review-gate variant of #874.

**Independent Test**: Same flow as User Story 1 with `fabrik:awaiting-review` substituted for `fabrik:awaiting-ci`.

**Acceptance Scenarios**:

1. **Given** a Validate-stage issue carries `fabrik:paused + fabrik:awaiting-input + fabrik:awaiting-review`, **When** its linked PR is merged externally, **Then** within the poll budget `stage:Validate:complete` is applied, all three labels are removed, the board advances to Done, and the issue is CLOSED.

---

### User Story 3 — Recovery with No Gate Label (Control Case) (Priority: P1)

Same as User Story 1 but no gate label is applied — only `fabrik:paused + fabrik:awaiting-input`. This is the load-bearing control case: a pre-#887 engine strands this variant forever because no recovery loop matched the label combination.

**Why this priority**: This is the structural proof that advancement is gate-label-agnostic. Without this case, the test only verifies that existing gate-specific paths still work, not that the underlying fix is general.

**Independent Test**: Same flow as User Story 1 with no gate label applied.

**Acceptance Scenarios**:

1. **Given** a Validate-stage issue carries only `fabrik:paused + fabrik:awaiting-input` (no gate label), **When** its linked PR is merged externally, **Then** within the poll budget `stage:Validate:complete` is applied, `fabrik:paused` and `fabrik:awaiting-input` are removed, the board advances to Done, and the issue is CLOSED.

---

### Edge Cases

- Each variant runs as a separate sub-test (`t.Run`) under `TestPausedMergedPRRecovery` so a failure in one variant does not mask the others.
- `stage:Validate:complete` must appear even though the Validate stage never emitted `FABRIK_STAGE_COMPLETE` during this run — the settle-owner infers completion from the merged PR state.
- All three sub-tests share the same test-bed Fabrik instance and board; they run sequentially to avoid label-mutation races on the same project board.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A new e2e test `TestPausedMergedPRRecovery` in `tests/e2e/paused_merged_pr_test.go` runs three sequential sub-tests, each parameterized by a `gateLabel` string: `"fabrik:awaiting-ci"`, `"fabrik:awaiting-review"`, and `""` (no gate label).
- **FR-002**: Each sub-test drives a cruise issue to Validate with a linked, open PR using the existing full-pipeline pattern (file issue → add to board → set Specify → wait for `stage:Validate:complete` via `WaitForIssueLabel`).
- **FR-003**: After Validate completes in the happy path, each sub-test forces the stuck state by calling `AddLabel` for `fabrik:paused`, `fabrik:awaiting-input`, and (if non-empty) the `gateLabel`.
- **FR-004**: Each sub-test calls `WaitForLinkedPR` to discover the linked PR number, then `MergePR` to merge it externally, simulating a human/auto-merge while the issue is paused.
- **FR-005**: After `MergePR`, each sub-test waits for `stage:Validate:complete` via `WaitForIssueLabel` (the label is momentarily removed when the stuck state is forced, then re-applied by the settle-owner) and then for `WaitForIssueClosed`. It then asserts `fabrik:paused` and `fabrik:awaiting-input` are absent via `WaitForLabelAbsent`, and (if non-empty) the `gateLabel` is absent via `WaitForLabelAbsent`.
- **FR-006**: All harness helpers required for this test (`AddLabel`, `RemoveLabel`, `MergePR`, `WaitForLinkedPR`, `WaitForLabelAbsent`, `WaitForIssueClosed`, `WaitForIssueLabel`) already exist in `tests/e2e/harness.go` — no new helpers are required.
- **FR-007**: The `tests/e2e/README.md` scenarios table is updated with a row for `TestPausedMergedPRRecovery` (three sub-tests, approx wall-clock ~15–25 min total, cost ~$0.50–1.50).
- **FR-008**: The `tests/e2e/README.md` regression-coverage table is updated with a row mapping `TestPausedMergedPRRecovery` to the #874 class of bugs (paused+merged stranding), the #887 structural fix, and ADR-056 root cause 1.

### Key Entities

- **Stuck state**: An issue at Validate with `fabrik:paused + fabrik:awaiting-input + [optional gateLabel]` whose linked PR is merged — the recovery that #887 guarantees.
- **Settle-owner (#887)**: The single engine path that detects a merged PR on a paused/gated issue and heals it by applying `stage:Validate:complete`, removing gate labels, and advancing to Done.
- **Gate label**: One of `fabrik:awaiting-ci`, `fabrik:awaiting-review`, or absent. The control (absent) is the load-bearing case.
- **`stage:Validate:complete`**: The label applied by the settle-owner when it heals the issue; it must be present even though the Validate stage never emitted `FABRIK_STAGE_COMPLETE` during this run.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestPausedMergedPRRecovery` passes all three sub-tests end-to-end: every gateLabel variant sees its issue closed and `stage:Validate:complete` applied after the PR merges while the issue is paused.
- **SC-002**: No variant ends with `fabrik:paused` or `fabrik:awaiting-input` still present after the budget.
- **SC-003**: No variant ends with its `gateLabel` still present after the budget.
- **SC-004**: `go test ./tests/e2e/ -run TestPausedMergedPRRecovery -tags e2e` passes cleanly against the live test bed.
- **SC-005**: README scenario and regression-coverage tables reference #874, #887, and ADR-056.

## Assumptions

- The test bed Fabrik instance is running with a version that includes the #887 settle-owner fix; this test validates that fix and will fail on a pre-#887 engine (by design — it is a regression guard, not a backport).
- The settle-owner correctly re-applies `stage:Validate:complete` when the stuck state has the label removed; the test re-waits for that label after forcing the stuck state.
- The three sub-tests can re-use the same test-bed board and run sequentially without interfering (separate issues, separate worktrees).
- The test bed GitHub repo does not have branch-protection rules that would prevent the harness from merging without a review approval.
- `WaitForLinkedPR` is called after `stage:Validate:complete` (first occurrence) so the linked PR is already created by the Implement stage and fully visible via GitHub's GraphQL API.

## Out of Scope

- Verifying that a *non-paused* merged PR is handled correctly (that is the `TestCruiseFullPipeline` / `TestYoloAutoMergeLabel` scenario).
- Verifying `fabrik:bot-reprompted` behavior or the review-reinvoke gate in detail.
- Testing what happens when the settle-owner is absent (pre-#887) — the test is a forward regression guard, not a backward compatibility test.
- Adding `GetIssueStatus` (board column assertion) — already specified in #898; this test uses `WaitForIssueClosed` as the terminal assertion, which is sufficient.
- Validating ADR-056 root cause 2 (duplicate advance) — that is a separate concern.

## Source References

- `tests/e2e/harness.go` — existing helpers: `AddLabel`, `RemoveLabel`, `MergePR`, `WaitForLinkedPR`, `WaitForLabelAbsent`, `WaitForIssueClosed`, `WaitForIssueLabel`
- `tests/e2e/auto_merge_test.go` — `TestYoloAutoMergeLabel`: reference for full-pipeline e2e pattern
- `tests/e2e/cruise_test.go` — `TestCruiseFullPipeline`: reference for cruise+`MergePR` usage after Validate
- `adrs/056-consolidate-convergence-gate-recovery.md` — D2 (settle-owner) and root cause 1 analysis; explicitly calls for this regression test at line 125
- `adrs/053-paused-ci-recovery-loop.md` — original #874 amendment; the bug class this test guards against
