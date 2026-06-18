# Feature Specification: e2e — Cruise Full Pipeline (Auto-Advance to Validate, No Auto-Merge)

**Feature Branch**: `fabrik/issue-898`
**Created**: 2026-06-18
**Status**: Draft
**Input**: User description: "test(e2e): cruise full pipeline — auto-advance to Validate, no auto-merge, stop for human merge"

## Background

Only `fabrik:yolo` is exercised end-to-end (`TestYoloAutoMergeLabel`, `TestConvergenceRace`). `fabrik:cruise` — auto-advance through every stage but do **not** auto-merge and do **not** advance to Done at Validate — has no e2e coverage, despite being the default mode for hands-off-but-human-merged work. The convergence-reset chain (issues #828 → #888 → #887 → #889 → #890) runs under cruise in production. A regression that made cruise silently behave like yolo (auto-merging unreviewed PRs) would not be caught by the existing test suite.

The gap is structural: the only existing full-pipeline e2e tests (`TestSmokeSingleRepoFullPipeline`, `TestYoloAutoMergeLabel`, `TestConvergenceRace`) all use `fabrik:yolo` and assert the issue closes via PR merge. Cruise's stop-at-Validate-complete contract — PR ready, issue open, no auto-merge, board stays at Validate — is entirely untested end-to-end.

## User Scenarios & Testing *(mandatory)*

### User Story 1 — Cruise Auto-Advances to Validate and Stops (Priority: P1)

An operator files an issue with `fabrik:cruise` (no yolo). Fabrik auto-advances through all stages (Specify → Research → Plan → Implement → Review → Validate) without any manual column moves. At Validate-complete, Fabrik stops: the linked PR is non-draft and ready, but is **not** merged, the issue is still open, the board column is Validate (not Done), and `fabrik:auto-merge-enabled` was **never** applied at any point.

**Why this priority**: This is the core cruise contract. A silent regression (cruise → yolo behavior) would cause unreviewed PRs to be merged automatically, which is exactly what cruise is designed to prevent.

**Independent Test**: File a cruise-labelled issue in the test bed, wait for `stage:Validate:complete`, then assert the PR and issue state match the stop contract.

**Acceptance Scenarios**:

1. **Given** an issue with `fabrik:cruise` (no yolo) is filed and added to the board at Specify, **When** Fabrik processes it through all stages, **Then** `stage:Validate:complete` is applied without any manual column move.

2. **Given** `stage:Validate:complete` is present, **When** the issue's linked PR and labels are inspected, **Then** the PR exists and is non-draft (ready for review) AND is not merged, `fabrik:auto-merge-enabled` is not present on the issue, the issue state is OPEN, and the board column is Validate.

3. **Given** the cruise stop contract holds, **When** `fabrik:auto-merge-enabled` is checked against the issue's timeline (not just current labels), **Then** the label was never applied at any point during the issue's lifetime.

---

### User Story 2 — After Manual PR Merge, Issue Advances to Done (Priority: P1)

After cruise stops at Validate-complete with an open PR, a human (or the test harness) merges the PR. Fabrik detects the merge on the next poll and advances the issue to Done / CLOSED.

**Why this priority**: Validates the completion half of the cruise contract — cruise must not permanently strand issues at Validate once the PR is merged.

**Independent Test**: Merge the linked PR via the harness `MergePR` helper, then wait for the issue to close and the board column to advance to Done.

**Acceptance Scenarios**:

1. **Given** a cruise issue is stopped at `stage:Validate:complete` with an open, unmerged PR, **When** the linked PR is merged by the test harness, **Then** within the poll timeout the issue transitions to CLOSED and the board column advances to Done.

---

### Edge Cases

- If yolo and cruise labels coexist, yolo takes precedence (existing engine behavior per CLAUDE.md). The test does NOT verify this edge case — it is already unit-tested.
- The `MergePR` harness helper must tolerate the case where GitHub's merge endpoint returns a 405 (branch-protection rules), though for the test bed this should not occur.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A new e2e test `TestCruiseFullPipeline` in `tests/e2e/cruise_test.go` files an issue with `fabrik:cruise` (no yolo), moves it to Specify, and waits for `stage:Validate:complete` within a 60-minute budget.
- **FR-002**: After `stage:Validate:complete`, the test asserts: (a) a linked PR exists and is non-draft/ready; (b) `fabrik:auto-merge-enabled` was never applied (checked via timeline, using a new `AssertLabelWasNeverApplied` harness helper); (c) the issue is OPEN; (d) the board column is Validate (checked via a new `GetIssueStatus` harness helper or equivalent).
- **FR-003**: The test then calls a new `MergePR` harness helper to merge the linked PR, then waits for the issue to close and the board column to advance to Done.
- **FR-004**: A new harness helper `AssertLabelWasNeverApplied(t, env, repo, issueNumber, label)` asserts that the named label never appeared in the issue's timeline — the inverse of `AssertLabelWasApplied`. Implemented by querying the timeline and failing if the label is found.
- **FR-005**: A new harness helper `MergePR(t, env, repo, prNumber)` merges the named PR via `gh pr merge --merge`. Used to simulate the human-merge step in the cruise completion scenario.
- **FR-006**: A new harness helper `GetLinkedPRNumber(t, env, repo, issueNumber)` retrieves the PR number linked to the issue (via `gh issue view --json closedByPullRequestsReferences` or equivalent), to pass to `MergePR`.
- **FR-007**: A new harness helper `GetIssueStatus(t, env, issueNumber)` returns the current board column name for the project item, to assert "column = Validate" and later "column = Done".
- **FR-008**: The `tests/e2e/README.md` scenarios table is updated with a row for `TestCruiseFullPipeline` (approx wall-clock ~30–50 min, cost ~$0.75–2.00).
- **FR-009**: The `tests/e2e/README.md` regression-coverage table is updated with a row mapping `TestCruiseFullPipeline` to cruise label semantics and the risk of cruise silently regressing to yolo behavior.
- **FR-010**: `CLAUDE.md` is updated to note that `fabrik:cruise` has e2e coverage via `TestCruiseFullPipeline` (under the `fabrik:cruise` label entry in the Labels section).

### Key Entities

- **Cruise contract**: At Validate-complete, a cruise issue's linked PR is ready but unmerged, `fabrik:auto-merge-enabled` was never applied, issue is OPEN, board column is Validate (not Done).
- **MergePR (harness)**: A new e2e helper that calls `gh pr merge <number> -R <repo> --merge` to merge a PR on behalf of the test harness.
- **AssertLabelWasNeverApplied**: Timeline-based inverse of `AssertLabelWasApplied` — fails the test if the label appears in the labeled events list.
- **GetIssueStatus**: Reads the project board's Status field for the item and returns the current column name string.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestCruiseFullPipeline` passes end-to-end: cruise-labelled issue reaches `stage:Validate:complete` without manual intervention.
- **SC-002**: At Validate-complete, all four cruise contract assertions pass: PR ready+unmerged, `fabrik:auto-merge-enabled` never applied, issue OPEN, column = Validate.
- **SC-003**: After `MergePR`, the issue closes and the board column advances to Done within the poll timeout (~5 min).
- **SC-004**: `go test ./tests/e2e/ -run TestCruiseFullPipeline -tags e2e` passes cleanly against the live test bed.
- **SC-005**: `AssertLabelWasNeverApplied`, `MergePR`, `GetLinkedPRNumber`, and `GetIssueStatus` helpers are added to `harness.go` and reusable by any future scenario.

## Assumptions

- The test bed Fabrik instance is already running with the test-bed config (same prerequisite as all other e2e tests).
- The test bed GitHub repo (`handarbeit/fabrik-test-alpha`) does not have branch protection rules that would require reviewer approval before merge — the harness merges without an approval.
- The issue body for the cruise test can be a minimal trivial change (same template as `TestSmokeSingleRepoFullPipeline`) — the goal is to exercise the cruise pipeline path, not a realistic payload.
- `WaitForIssueLabel(t, env, repo, num, "stage:Validate:complete", ...)` is sufficient to confirm Validate completed — the board column assertion follows once that label is confirmed.
- `GetIssueStatus` reads the board column via `gh project item-list` or a GraphQL query filtered by issue number; the exact implementation is left to the Research/Plan stage.

## Out of Scope

- Verifying the yolo+cruise coexistence edge case end-to-end (yolo takes precedence; covered by unit tests in `engine/stages_test.go`).
- Paused-item recovery or convergence-reset scenarios (separate issues in the #828 chain).
- Verifying `fabrik:bot-reprompted` or `wait_for_reviews` behavior in the cruise context.
- ADR modifications beyond a note referencing cruise e2e coverage (ADR-056 D2 already documents cruise's stop-at-Validate behavior; no new ADR is needed).

## Source References

- `tests/e2e/harness.go` — existing helpers; `AssertLabelWasNeverApplied`, `MergePR`, `GetLinkedPRNumber`, `GetIssueStatus` are new additions
- `tests/e2e/auto_merge_test.go` — `TestYoloAutoMergeLabel` pattern (full-pipeline with post-Validate assertion)
- `engine/poll.go:1410–1444` — the code gate that makes cruise stop at Validate without auto-merging
- `engine/stages_test.go:60–75` — unit test `TestAttemptMergeOnValidate_CruiseSkipsAutoMerge` confirming the cruise guard exists
- `adrs/056-consolidate-convergence-gate-recovery.md` — D2 describes the single advance owner stopping at Validate for cruise
