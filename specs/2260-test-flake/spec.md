# Feature Specification: Fix timing flake in TestStreamReportsSupersededBeforeStopping

**Feature Branch**: `fabrik/issue-2260`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`TestStreamReportsSupersededBeforeStopping` (`internal/localapi/stream_test.go`) failed in CI's \"Test and vet\" on PR #2240, an e2e-only change that does not touch the package. It timed out after 5 s waiting for a stream state. The worker-shaped job passed on the same commit, and a re-run passed. So it is a timing flake.

## Requirements

- Find the race: what the test waits on, and why the state can arrive late or not at all under CI load.
- Make the wait deterministic (wait on the event, not a fixed 5 s window) or fix the ordering bug in `RunStream` if it is real.

## Acceptance

- The cause is stated in the PR.
- `go test -race -count=200 -run TestStreamReportsSupersededBeforeStopping ./internal/localapi/` passes, with CPU contention added (for example `-cpu 1,2` plus a parallel load)."

## Background

`TestStreamReportsSupersededBeforeStopping` in the local API package (`internal/localapi`) failed once in CI's "Test and vet" job on PR #2240. That PR is an e2e-only change and does not touch the package, so the failure is not caused by the PR's content. The test timed out after 5 seconds waiting for a stream state. The worker-shaped CI job passed on the same commit and a re-run passed, which points to a timing-dependent flake rather than a deterministic regression.

A flaky test in the required CI gate costs contributors a re-run and teaches them to distrust red builds. The flake can mean one of two things, and the work must tell them apart:

1. The test's wait is not tied to the event it is checking. It relies on a fixed 5 s window, so under CPU contention the expected state arrives late or the test samples at the wrong moment.
2. A real ordering bug exists in the stream client (`RunStream`): under some schedules the "superseded" state may be reported late, out of order relative to stopping, or not at all.

The fix depends on which of the two it is.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - CI no longer fails intermittently on this test (Priority: P1)

A contributor opens a PR that has nothing to do with the local API stream client. CI's "Test and vet" job runs `TestStreamReportsSupersededBeforeStopping` on a loaded runner and it passes, regardless of how the scheduler orders the client, server and test goroutines.

**Why this priority**: This is the flake itself. It blocks unrelated PRs and wastes re-runs.

**Independent Test**: Run `go test -race -count=200 -run TestStreamReportsSupersededBeforeStopping ./internal/localapi/` with `-cpu 1,2` and concurrent CPU load. Every iteration passes.

**Acceptance Scenarios**:

1. **Given** a CPU-starved machine (`-cpu 1` plus parallel load), **When** the test is run 200 times under `-race`, **Then** all 200 runs pass and none times out.
2. **Given** the same test on a machine with more CPUs (`-cpu 2`), **When** it is run 200 times under `-race` with parallel load, **Then** all runs pass.

---

### User Story 2 - The root cause is documented (Priority: P1)

A maintainer reading the PR can see what the test waits on, why the awaited state could arrive late or never, and whether the fix changed the test, the stream client, or both.

**Why this priority**: The issue explicitly requires stating the cause; without it nobody can judge whether the fix is real or just a longer timeout.

**Independent Test**: Read the PR description. It names the awaited state, the race or ordering problem, and the rationale for the chosen fix.

**Acceptance Scenarios**:

1. **Given** the merged PR, **When** a maintainer reads its description, **Then** it states the cause of the flake in terms of the specific wait and event ordering.
2. **Given** the cause is a real ordering bug in the stream client, **When** the PR is reviewed, **Then** the description says so, and the client behaviour is fixed rather than only the test.

---

### Edge Cases

- If the cause is a real ordering bug in the stream client, the test must continue to assert the intended contract: "superseded" is reported before the stream stops. It must not be weakened to tolerate either order.
- If the fix only changes how the test waits, the 5 s upper bound may remain as a failure backstop. It must not be the mechanism by which the test synchronises.
- Other tests in the package that share the same wait pattern may have the same latent flake. They are noted, not necessarily fixed here (see Out of Scope).
- The test must still fail when the behaviour it covers is broken, that is, when "superseded" is not reported or is reported after stopping. A deterministic wait must not turn it into a test that cannot fail.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The cause of the timeout in `TestStreamReportsSupersededBeforeStopping` is identified: what state the test waits for, and why under load it can arrive late or not at all.
- **FR-002**: The test synchronises on the event it is waiting for, not on elapsed wall-clock time, so a slow scheduler cannot make it fail while the behaviour is correct.
- **FR-003**: If the investigation shows a real ordering defect in the stream client (`RunStream`) in how it reports "superseded" relative to stopping, that defect is fixed in the client, and the test asserts the corrected ordering.
- **FR-004**: The test still asserts the behaviour its name states: "superseded" is reported before the stream stops. Removing or breaking that behaviour makes the test fail.
- **FR-005**: The PR description states the identified cause (FR-001) and whether the fix is in the test, the client, or both.
- **FR-006**: No other behaviour of the local API stream client changes as a side effect, other than the ordering fix permitted by FR-003.

### Key Entities *(if applicable)*

- **Stream client (`RunStream`)**: The held-connection client that attaches to the daemon, reads pushed frames, and reports state transitions (such as "superseded") to its caller before stopping.
- **Stream state notification**: The state report the test waits for; it is the event the wait must be tied to.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -race -count=200 -run TestStreamReportsSupersededBeforeStopping ./internal/localapi/` passes with 0 failures, with CPU contention added, at both `-cpu 1` and `-cpu 2`.
- **SC-002**: The PR description names the awaited state and the cause of the late or missing state.
- **SC-003**: The rest of `./internal/localapi/` passes under `go test -race` with no new failures.
- **SC-004**: The test's passing no longer depends on a fixed time window, so CI runner speed does not affect the outcome.

## Assumptions

- The flake reproduces locally with enough CPU contention and a high repeat count; if it does not, the cause is established by reasoning about the scheduling and the PR says so.
- The CPU-contention method is left to the implementer; the issue's example (`-cpu 1,2` plus a parallel load) is illustrative, not prescriptive.
- Keeping a generous timeout as a failure backstop is acceptable, provided it is not what the test synchronises on.
- A test-only fix is preferred when the client's behaviour is correct; a client change is made only if the ordering defect is real.

## Out of Scope *(optional)*

- Auditing or hardening every other timing-based test in `internal/localapi` or elsewhere; any similar flakes found are reported as follow-up issues.
- Changing the stream protocol, heartbeat or reconnect behaviour beyond what is needed to fix a confirmed ordering defect.
- CI configuration changes (retries, job splitting, runner sizing) as a way to mask the flake.
- Investigating why PR #2240's worker-shaped job passed while the other job failed beyond what is needed to explain the timing.
