# Feature Specification: Deflake TestRetryStarved_Reasons/deferred_dispatch_counts_as_no_worker_slot

**Feature Branch**: `fabrik/issue-2283`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`TestRetryStarved_Reasons/deferred_dispatch_counts_as_no_worker_slot` (engine package, #2170's anomaly detectors) fails intermittently in CI's worker-shaped job and passes on re-run. Seen twice on 2026-10-14: on PR #2264 and on PR #2282. Neither PR touched the engine.

## Requirements

- Find the timing dependency. Candidates: the anomaly consumer goroutine has not yet drained when the assertion runs, or the threshold or clock seam races the deferred-dispatch hook.
- Wait on a deterministic signal, as #2210 did with `DrainChannelEventsForTest`, not on wall time.

## Acceptance

- The cause is stated in the PR.
- `go test -race -count=200 -run 'TestRetryStarved_Reasons' ./engine/` passes under added CPU load, and the test still fails when the detector is neutralised."

## Background

The engine's `retry-starved` anomaly detector (#2170) reports why an item's scheduled retry has not run. The reason is derived from facts the engine records: the retry's due time and cause, and whether a dispatch was deferred for lack of a free worker slot. One subtest, `deferred_dispatch_counts_as_no_worker_slot`, asserts that a deferred dispatch is reported as "no worker slot".

That subtest fails intermittently in CI's worker-shaped job and passes on re-run. It has now been seen three times:

- 2026-10-14, PR #2264.
- 2026-10-14, PR #2282.
- 2026-10-15, PR #2217 (run 38662480796, job "Test and vet"). None of the three PRs touched the engine.

The failure on PR #2217 is the most informative. The assertion at `engine/channel_anomaly_test.go:133` received a `retry-starved` event whose text read "the Implement retry after max-turns was due at …". The subtest therefore saw the retry reason (`max-turns`), not the deferred-dispatch reason it expects. This suggests the deferred-dispatch fact was not yet recorded, or had been overwritten, when the detector ran.

Hypotheses from the report, none confirmed yet:

- The anomaly consumer goroutine has not drained when the assertion runs.
- The threshold or clock seam races the deferred-dispatch hook.
- The ordering between the deferred-dispatch hook and the retry hook lets the retry reason win.
- #2232's time-slice cooldown changed when a retry becomes due and so changed that ordering.

A flaky test erodes trust in CI and costs a re-run each time. It also hides whether the detector itself has an ordering defect: if production can report the wrong reason, this is a product bug and not only a test bug. #2210 already solved the same class of problem for channel tests with a deterministic drain signal (`DrainChannelEventsForTest`) in place of wall-clock sleeps.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Reliable CI for the retry-starved reasons test (Priority: P1)

A maintainer opening a PR that does not touch the engine sees `TestRetryStarved_Reasons` pass on the first CI run, every time, regardless of runner load.

**Why this priority**: The flake already cost re-runs on three unrelated PRs and will keep doing so.

**Independent Test**: Run `go test -race -count=200 -run 'TestRetryStarved_Reasons' ./engine/` while the machine is under added CPU load. All 200 iterations pass.

**Acceptance Scenarios**:

1. **Given** the engine test suite running under CPU contention, **When** `TestRetryStarved_Reasons` runs 200 times with `-race`, **Then** every run passes.
2. **Given** the deferred-dispatch subtest, **When** the test asserts the event, **Then** it waits on a deterministic signal that the anomaly consumer has processed the recorded facts, not on elapsed wall time.

---

### User Story 2 - The cause is understood and recorded (Priority: P1)

A reviewer reading the PR can see what actually raced, so they can judge whether the fix addresses the cause or only masks the symptom.

**Why this priority**: The acceptance bar requires the cause to be stated. Without it, a timing-only fix could hide a real ordering defect in the detector.

**Independent Test**: The PR description names the specific ordering or race and ties it to the observed `max-turns` versus deferred-dispatch outcome.

**Acceptance Scenarios**:

1. **Given** the PR, **When** a reviewer reads its description, **Then** it states the cause of the intermittent failure, including why the retry reason (`max-turns`) was sometimes reported instead of the deferred-dispatch reason.
2. **Given** the investigation concludes the detector can report the wrong reason in production, **When** the fix is made, **Then** the detector is corrected as well as the test, and the PR says so.

---

### User Story 3 - The test still guards the behaviour (Priority: P2)

A maintainer can trust that the fixed test is not vacuous: removing the detector's behaviour still makes it fail.

**Why this priority**: A deflaked test that can no longer fail is worse than a flaky one.

**Independent Test**: Neutralise the detector (using the existing disable seam, or an equivalent) and run the subtest. It fails.

**Acceptance Scenarios**:

1. **Given** the anomaly detector neutralised, **When** the deferred-dispatch subtest runs, **Then** it fails.
2. **Given** the detector active, **When** the subtest runs, **Then** it passes and asserts the "no worker slot" reason specifically.

---

### Edge Cases

- The retry reason and the deferred-dispatch fact are both present for the same item. The expected reason must be defined and asserted deterministically, not left to scheduling.
- The fix must not turn the wait into a hang. A signal that never arrives must fail the test with a clear message, not block the suite.
- Sibling subtests in `TestRetryStarved_Reasons` may share the same timing dependency, and should not be left flaky.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The root cause of the intermittent failure of `TestRetryStarved_Reasons/deferred_dispatch_counts_as_no_worker_slot` is identified and stated in the PR description. It explains the observed `max-turns` reason appearing where the deferred-dispatch reason was expected.
- **FR-002**: The test synchronises on a deterministic signal, such as the existing channel-events drain seam from #2210 or an equivalent, and never on wall-clock sleeps or polling intervals.
- **FR-003**: If the cause is an ordering or overwrite defect in the detector (the deferred-dispatch fact versus the retry fact), the detector is corrected so production reports the right reason, in addition to the test fix.
- **FR-004**: Other subtests of `TestRetryStarved_Reasons` that depend on the same timing are brought onto the same deterministic signal.
- **FR-005**: With the detector neutralised, the deferred-dispatch subtest fails. Its non-vacuity is demonstrated in the PR.
- **FR-006**: A timeout while waiting for the signal fails the test with a clear message and does not hang the suite.

### Key Entities *(if applicable)*

- **retry-starved event**: The anomaly event stating why a scheduled retry has not run. It carries a reason such as `max-turns` or "no worker slot".
- **Deferred-dispatch fact**: The recorded fact that a candidate was not dispatched because no worker slot was free.
- **Retry fact**: The recorded fact that a retry is scheduled, with its due time and cause.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -race -count=200 -run 'TestRetryStarved_Reasons' ./engine/` passes with 0 failures under added CPU load.
- **SC-002**: No further intermittent failures of this test are reported in CI after the fix merges.
- **SC-003**: With the detector neutralised, the deferred-dispatch subtest fails.
- **SC-004**: The PR description states the cause in a form a reviewer can check against the code.

## Assumptions

- The failure comes from an ordering or visibility problem between recorded facts and the consumer goroutine, as the issue and the third-occurrence evidence suggest. It does not come from CI environment instability.
- The existing channel-events drain seam from #2210 is the preferred model for the wait, but the right signal is a Research and Plan decision.
- "Added CPU load" can be produced with ordinary local means, such as running concurrent CPU-bound processes or a reduced `GOMAXPROCS`. No new tooling is required.

## Out of Scope *(optional)*

- Changing the retry-starved detector's thresholds or semantics beyond correcting a confirmed wrong-reason defect.
- Auditing every test in the engine package for similar flakes. Only this test's siblings are in scope.
- Changes to CI configuration, runner sizing or the worker-shaped job.

## Source References *(optional)*

- #2170: anomaly detectors, introduced `retry-starved`.
- #2210: per-Engine channel test seams and `DrainChannelEventsForTest`, the model for a deterministic wait.
- #2232: time-slice cooldown, which may have changed when a retry becomes due.
- `engine/channel_anomaly_test.go:133`: the failing assertion in the third occurrence (PR #2217, run 38662480796).
