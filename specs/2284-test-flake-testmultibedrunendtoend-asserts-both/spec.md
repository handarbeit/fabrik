# Feature Specification: Make TestMultiBedRunEndToEnd deterministic

**Feature Branch**: `fabrik/issue-2284`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "test flake: TestMultiBedRunEndToEnd asserts both beds ran legs, which depends on goroutine scheduling

## Problem

`TestMultiBedRunEndToEnd` (`tests/gate/multibed_gate_test.go`) failed in CI on PR #2282 with `legs ran on map[<bed B dir>:true]`: every leg ran on one bed. It passes 80 runs locally, with and without `-race`.

With fake commands every leg finishes instantly, so whichever bed reaches the shared queue first can drain it before the other bed takes a cell. The assertion that both beds ran legs then depends on goroutine scheduling.

## Requirements

- Make the test deterministic. For example, the fake suite command for a leg blocks until both beds have started one, or the assertion checks the scheduler's assignment rather than which bed happened to run the commands.
- Keep the test's intent: each bed is built, reset and started in its own directory with its own token, and the pre-gate runs once.

## Acceptance

- `go test -race -count=500 -run TestMultiBedRunEndToEnd ./tests/gate/` passes with `GOMAXPROCS=1` and under CPU load.
- The test still fails if the scheduler gives every cell to one bed."

## Background

`TestMultiBedRunEndToEnd` is the end-to-end check of the multi-bed live-e2e gate runner. It runs a gate across two beds with fake commands and checks that work is spread over both beds and that each bed is prepared in its own directory with its own credentials.

The test failed in CI on PR #2282, reporting that every leg had run on a single bed. It passes consistently on a developer machine. The cause is that the fake commands complete instantly. Whichever bed reaches the shared queue first can finish every cell before the other bed gets to take one. The test's check that both beds ran legs therefore depends on goroutine scheduling, not on scheduler behaviour. The result is a flaky CI failure on an unrelated PR, plus a loss of trust in the test.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The test passes regardless of scheduling (Priority: P1)

A contributor opens a PR and CI runs the `tests/gate` package. The multi-bed end-to-end test must give the same result whatever order the runtime schedules the two beds' goroutines in, or however loaded the machine is.

**Why this priority**: A flaky required check blocks unrelated PRs and wastes reviewer and contributor time. Removing the flake is the whole point of this issue.

**Independent Test**: Run the test many times with a single scheduler thread and under artificial CPU load, with and without the race detector, and confirm it never fails.

**Acceptance Scenarios**:

1. **Given** the test runs with `GOMAXPROCS=1`, **When** it is repeated 500 times with `-race`, **Then** every run passes.
2. **Given** the machine is under heavy CPU load, **When** the test is repeated, **Then** every run passes.

---

### User Story 2 - The test still catches a one-sided scheduler (Priority: P1)

A maintainer changes the multi-bed scheduler. If the change gives every cell to a single bed, the test must fail, so that determinism is not bought by weakening what the test verifies.

**Why this priority**: A deterministic test that can no longer fail on the behaviour it guards would be worse than a flaky one.

**Independent Test**: Temporarily alter the scheduler so one bed receives all cells, and confirm the test fails reliably, not intermittently.

**Acceptance Scenarios**:

1. **Given** a scheduler that assigns every cell to one bed, **When** the test runs, **Then** it fails every time.
2. **Given** the real scheduler, **When** the test runs, **Then** it passes every time.

---

### User Story 3 - The test keeps its existing intent (Priority: P2)

The test continues to verify the multi-bed behaviours it was written for, apart from the scheduling-dependent assertion.

**Why this priority**: The fix must not shrink coverage of the other multi-bed guarantees.

**Independent Test**: Review the test's remaining assertions and confirm each one still runs and still checks the same thing.

**Acceptance Scenarios**:

1. **Given** a completed multi-bed run, **When** the test inspects it, **Then** each bed was built, reset and started in its own directory using its own token.
2. **Given** a completed multi-bed run, **When** the test inspects it, **Then** the pre-gate ran exactly once.

---

### Edge Cases

- A bed that starts slowly or late: the test must not hang forever waiting for it. A bounded wait that fails with a clear message is acceptable.
- A scheduler that wrongly gives all cells to one bed: with any rendezvous-style approach the test must fail with a clear message, not deadlock or time out opaquely.
- A plan with fewer cells than beds: the test's setup must provide enough cells for the assertion to be meaningful.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The outcome of `TestMultiBedRunEndToEnd` MUST NOT depend on which bed's goroutine happens to run first or on how fast the fake commands complete.
- **FR-002**: The test MUST still verify that the scheduler distributes cells across both beds, and MUST fail if the scheduler gives every cell to a single bed.
- **FR-003**: The test MUST still verify that each bed is built, reset and started in its own directory with its own token.
- **FR-004**: The test MUST still verify that the pre-gate runs exactly once.
- **FR-005**: Any wait the test introduces MUST be bounded and MUST fail with a clear message when it times out, not hang.
- **FR-006**: The change MUST be confined to test code; no production behaviour of the gate runner or scheduler changes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -race -count=500 -run TestMultiBedRunEndToEnd ./tests/gate/` passes with `GOMAXPROCS=1`.
- **SC-002**: The same command passes while the machine is under CPU load.
- **SC-003**: With the scheduler deliberately altered to give all cells to one bed, the test fails on every run.
- **SC-004**: The full `go test -race ./...` suite stays green and its wall-clock time does not increase noticeably.

## Assumptions

- The two beds in the test are meant to share a single queue, so the scheduler's distribution of cells is observable and worth asserting.
- Either approach named in the issue is acceptable (forcing both beds to hold a cell concurrently, or asserting on the scheduler's assignment). The choice is left to later stages.
- Fixing the flake does not require changing the scheduler itself.

## Out of Scope *(optional)*

- Changes to the multi-bed scheduler or the gate runner's production code.
- Auditing other tests in `tests/gate` for similar scheduling dependence. This can be a follow-up issue if any are found.
- Changes to CI configuration.

## Source References *(optional)*

- `tests/gate/multibed_gate_test.go`
- CI failure on PR #2282 (`legs ran on map[<bed B dir>:true]`)
