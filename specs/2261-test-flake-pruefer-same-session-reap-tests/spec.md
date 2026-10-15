# Feature Specification: Make the Pruefer same-session reap tests deterministic

**Feature Branch**: `fabrik/issue-2261`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

Two Pruefer process-group tests in `pruefer/procattr_unix_test.go` failed in CI on PR #2248, whose change does not touch Pruefer:

- `TestKillProcGroupGraceful_ReapsSameSessionChild`
- `TestReapReviewSession_PostExitSweepKillsSameSessionChild`

A re-run passed, so they are flaky. Both build a fixture process tree and then assert that a same-session child was reaped.

## Requirements

- Find why the fixture is timing-sensitive, for example asserting before the child is started or before it has called `setsid`, or a reaped PID not yet gone from the listing.
- Wait on positive evidence (the child's PID recorded and live before the kill; gone or zombie after) rather than fixed sleeps.

## Acceptance

- The cause is stated in the PR.
- Both tests pass with `go test -race -count=100` under added CPU load, and still fail when the reap is neutralised."

## Background

Pruefer's worker-process reap (the same-session sweep that kills a Claude review worker's descendants after the worker stops or exits) is covered by tests in `pruefer/procattr_unix_test.go`. Two of them failed intermittently in CI on PR #2248, a change that does not touch Pruefer, and passed on re-run:

- `TestKillProcGroupGraceful_ReapsSameSessionChild` builds a session leader whose child moves itself into a separate process group of the same session, stops the leader through the graceful escalation, and asserts the child is gone.
- `TestReapReviewSession_PostExitSweepKillsSameSessionChild` builds the same tree, kills the leader, and asserts that the post-exit sweep reaps the surviving child.

A flaky test of a safety-critical reap path has two costs. It produces false red CI on unrelated PRs, and it teaches maintainers to re-run rather than investigate. If the cause is in the product code rather than the fixture, a real reap gap is being ignored. The cause has not been established and must be found, not guessed.

Places the investigation should look, from reading the fixture (hypotheses to confirm or rule out, not conclusions):

- The fixture reports the child ready once its PID file appears. The child may not yet have finished the steps that give it the shape the test relies on (its own process group, default signal dispositions), so the kill can arrive before the fixture is in the state the test assumes.
- Liveness after the reap is judged through a process listing or signal probe. A killed process that has not yet been reaped by its new parent (a zombie, possibly under a container init that never reaps) may read as alive, or a reaped PID may linger briefly in the listing.
- Fixed grace windows and fixed polling deadlines are tight relative to a loaded CI host.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Same-session reap tests stop flaking (Priority: P1)

A maintainer opening a PR that does not touch Pruefer sees these tests pass or fail only on the behavior they assert, not on host load or scheduling luck.

**Why this priority**: The flake is the reported problem. It blocks unrelated PRs and erodes trust in the suite.

**Independent Test**: Run both tests repeatedly under artificial CPU load and confirm every run passes.

**Acceptance Scenarios**:

1. **Given** a loaded host, **When** both tests are run 100 times with the race detector, **Then** every run passes.
2. **Given** the fixture process tree is being built, **When** the test proceeds to the kill, **Then** it does so only after the child's PID is recorded and the child is observed live and in its expected same-session, separate-group shape.
3. **Given** the reap has run, **When** the test asserts the child is gone, **Then** it waits for positive evidence of absence (gone from the listing, or a zombie awaiting reap) within a bound generous enough for a loaded host, and does not depend on a fixed sleep.

---

### User Story 2 - The tests still detect a broken reap (Priority: P1)

A maintainer who breaks or removes the same-session reap gets a failing test, so the stabilization has not made the tests vacuous.

**Why this priority**: A test made reliable by no longer asserting anything is worse than a flaky one.

**Independent Test**: Neutralise the reap (so it signals nothing in the session beyond the leader's own group) and run both tests; both must fail.

**Acceptance Scenarios**:

1. **Given** the same-session reap is disabled, **When** `TestKillProcGroupGraceful_ReapsSameSessionChild` runs, **Then** it fails because the child survives.
2. **Given** the post-exit sweep is disabled, **When** `TestReapReviewSession_PostExitSweepKillsSameSessionChild` runs, **Then** it fails because the child survives.

---

### User Story 3 - The root cause is on record (Priority: P2)

A reviewer reading the PR can see why the tests were flaky and judge whether the fix addresses that cause.

**Why this priority**: The issue asks for the cause to be stated, and it is what separates a fix from a wider timeout.

**Independent Test**: Read the PR description.

**Acceptance Scenarios**:

1. **Given** the PR, **When** a reviewer reads its description, **Then** it names the specific mechanism by which each test could fail and how the change removes it.

---

### Edge Cases

- The same fix pattern may apply to the other fixture-based tests in the same file (the Bash-tool command-session tests and the post-exit grandchild test). If the shared helper has the same weakness they inherit it, and the shared helper should be fixed once.
- A fixture that cannot reach its expected shape on a given platform should skip or fail with a clear fixture message, not report a reap failure.
- A killed process that lingers as an unreaped zombie must not be reported as a surviving child.
- The tests must not leave stray processes behind when they fail early.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The root cause of the intermittent failure of each of the two named tests is identified, with evidence (a reproduction under load or a demonstrated race), and stated in the PR description.
- **FR-002**: The fixture does not proceed to the kill until it has observed positive evidence that the child's PID is recorded, the child is live, and the child is in the shape the test depends on (same session as the leader, separate process group from it).
- **FR-003**: After the reap, the tests judge the child gone by positive evidence of absence (not in the process listing, or present only as a zombie awaiting reap), with a bounded wait that does not rely on a fixed sleep for correctness.
- **FR-004**: Any fixed timing in the two tests that remains is a timeout bound for a failure case, not a delay the passing path depends on.
- **FR-005**: Both tests pass on every run of `go test -race -count=100` for their package with additional CPU load running on the host.
- **FR-006**: With the reap neutralised, each of the two tests fails; this is demonstrated and reported in the PR.
- **FR-007**: The change does not weaken what the tests assert: each still checks that a same-session child in a separate process group is killed by the corresponding reap path.
- **FR-008**: If the investigation shows the cause is in the product reap code rather than the test fixture, the product defect is stated in the PR and fixed or filed as a follow-up issue. It is not hidden by a wider test timeout.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100 consecutive runs of each of the two tests with the race detector under added CPU load produce zero failures.
- **SC-002**: With the reap neutralised, 100% of runs of each of the two tests fail.
- **SC-003**: The PR description states a specific cause for each of the two failures.
- **SC-004**: The rest of the `pruefer` package and the repository test suite are unaffected: no previously passing test fails.

## Assumptions

- The failures come from timing in the test fixture or its liveness probe, not from a deliberate behavior change in the reap, because PR #2248 did not touch Pruefer. This is the working assumption and FR-008 covers the case where it is wrong.
- "Added CPU load" is any reasonable parallel CPU-bound workload; the exact load generator is the implementer's choice.
- The tests remain Unix-only, as they are today.
- Perl remains an acceptable dependency of the fixture where the tests already use it.

## Out of Scope *(optional)*

- Changing the production reap logic, except under FR-008.
- Stabilizing flaky tests outside `pruefer/procattr_unix_test.go`, including the equivalent engine-side session-reap tests, unless they share a helper that is fixed once.
- Adding a test-retry mechanism or loosening CI failure policy.
- Reworking the sim or live e2e layers.

## Source References *(optional)*

- PR #2248 (the CI run where both tests failed unrelated to its change)
- `pruefer/procattr_unix_test.go`
