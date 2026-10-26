# Feature Specification: Engine tests must not leak delivery-replay goroutines

**Feature Branch**: `fabrik/issue-2447`
**Created**: 2026-10-25
**Status**: Specified
**Input**: User description: "## Problem

`go test -race ./engine` intermittently fails with a DATA RACE between a delivery-replay goroutine left running by an earlier test and a later test's `captureStdout`:

- write: `captureStdout` (`engine/poll_test.go:2518`) swapping `os.Stdout`, from `runSeamTrain` (`engine/status_line_train_test.go:48`, `TestStatusLine_Train_GreenBatchSequence`)
- read: `deliveryReplayer.log` → `Engine.logf` → `fmt.Printf` (`engine/delivery_replay.go:204/281/444`), goroutine started by `startDeliveryReplayAfter` (`engine/delivery_replay.go:524`)

The race detector then fails whichever test is running (`TestStatusLine_Train_BisectSteps` in CI run of PR #2446, 2026-10-25). Some test starts a replayer (`go r.Run(ctx)`) with a context that is never cancelled when the test ends, so its pass keeps logging into later tests.

## Requirements

- Every engine test that starts a delivery replayer (directly, or through `startDirectTransport` / `startTailscaleTransport`) cancels its context in `t.Cleanup`, and waits for the replayer to stop, before the test returns.
- A guard that fails a test which leaves a replayer goroutine running (for example, a goroutine count/registry check in the replay test helpers), so the leak cannot come back.

## Acceptance

- `go test -race -count=20 -run 'TestTailscaleReplay|TestDeliveryReplay|TestStatusLine_Train' ./engine` passes.
- The guard fails when one cleanup is removed (neutralised run recorded in the PR)."

## Background

The engine's test suite is run with the Go race detector on every PR. The engine's delivery replayer (the background component that, for the direct and Tailscale webhook receivers, lists failed App webhook deliveries and asks GitHub to redeliver them) runs as a long-lived goroutine started with a context. Some engine tests start one, either directly or indirectly by starting a direct or Tailscale transport, and end without cancelling that context or waiting for the goroutine to stop.

The leaked goroutine keeps running after its test returns. When it later logs, it reads the process-wide standard output, while an unrelated later test that captures standard output (by swapping it temporarily) writes it. The race detector reports this as a data race and fails whichever test happens to be running at that moment. The failure is therefore intermittent and misattributed: it surfaced in `TestStatusLine_Train_BisectSteps` in the CI run of PR #2446 on 2026-10-25, although the leak originates in a different, earlier test. This makes CI flaky for unrelated PRs and erodes trust in the race-detector signal.

The fix has two halves: remove the existing leaks, and add a guard so a new test cannot reintroduce one unnoticed.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Replayer tests clean up after themselves (Priority: P1)

A contributor runs the engine tests with the race detector, or CI does. Every test that starts a delivery replayer stops it, and waits for it to stop, before the test returns, so no replayer activity can overlap a later test.

**Why this priority**: This removes the actual cause of the flaky failures that currently hit unrelated PRs.

**Independent Test**: Run the replay, Tailscale replay and status-line train tests together repeatedly under the race detector; they pass every time.

**Acceptance Scenarios**:

1. **Given** a test that starts a delivery replayer directly, **When** the test finishes, **Then** the replayer's context has been cancelled and its goroutine has fully exited before the test is reported as done.
2. **Given** a test that starts a direct or Tailscale transport (which starts a replayer), **When** the test finishes, **Then** the replayer started by that transport has been stopped and waited for in the same way.
3. **Given** a replayer test followed immediately by a test that captures standard output, **When** both run under the race detector, **Then** no data race is reported between them.

---

### User Story 2 - A guard fails any test that leaks a replayer (Priority: P1)

A contributor writes a new engine test that starts a replayer and forgets to stop it. The test suite itself catches the omission and fails that test, naming the leak, instead of failing some unrelated later test at random.

**Why this priority**: Without a guard the leak class will recur, since each new replay or transport test is one forgotten cleanup away from reintroducing the flake.

**Independent Test**: Temporarily remove one test's cleanup; the guard fails that test. Restore it; the test passes.

**Acceptance Scenarios**:

1. **Given** a test that starts a replayer and does not stop it, **When** the test ends, **Then** the test is failed with a message identifying that a replayer goroutine is still running.
2. **Given** a test that starts and correctly stops a replayer, **When** the test ends, **Then** the guard raises no failure.
3. **Given** a test that never starts a replayer, **When** it ends, **Then** the guard has no effect on it.

---

### Edge Cases

- A test that starts several replayers (for example a transport restart within one test) must have every one of them stopped and waited for.
- A replayer that is mid-pass (waiting on a follow-up delay or on a paced redelivery) when the test ends must still stop promptly once its context is cancelled; the wait must not hang the test indefinitely.
- A replayer whose startup delay (such as the Tailscale startup grace) has not yet elapsed when the test ends must not run its pass afterwards.
- The guard must be per-test: a leak attributed to one test must not be reported against, or masked by, tests running before or after it.
- Tests that use a replayer through a fake or injected API seam are covered the same as tests using a real one.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every engine test that starts a delivery replayer, whether directly or through the direct or Tailscale transport start paths, MUST cancel the replayer's context at test end.
- **FR-002**: Every such test MUST wait for the replayer goroutine(s) to have fully exited before the test returns.
- **FR-003**: The engine's replay test helpers MUST provide a guard that fails a test if, when it ends, any replayer goroutine started during that test is still running.
- **FR-004**: The guard failure message MUST make it clear that a delivery replayer was left running, so the author can find the missing cleanup without bisecting unrelated failures.
- **FR-005**: The guard MUST NOT produce false failures for tests that stop their replayers correctly, nor for tests that start none.
- **FR-006**: The fix MUST NOT change the replayer's production behaviour (pass scheduling, caps, logging, health reporting); it is test-side cleanup plus whatever minimal hook is needed for tests to observe and await replayer exit.

### Key Entities

- **Delivery replayer**: the background component that re-requests failed App webhook deliveries for the direct and Tailscale receivers; started once at transport start-up and again on reconnect, running until its context is cancelled.
- **Replayer registry / guard**: the test-support mechanism that records replayers started during a test and verifies they have exited when the test ends.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -race -count=20 -run 'TestTailscaleReplay|TestDeliveryReplay|TestStatusLine_Train' ./engine` passes with zero failures.
- **SC-002**: With one replayer-stopping cleanup deliberately removed, the guard fails the affected test; a neutralised run demonstrating this is recorded in the PR description.
- **SC-003**: The full engine test suite under the race detector shows no data race attributable to a delivery-replay goroutine logging into a later test.
- **SC-004**: No existing engine test that starts a replayer is left without both cancellation and an exit wait.

## Assumptions

- The leaking test(s) can be identified by auditing all engine tests that start a replayer directly or via `startDirectTransport` / `startTailscaleTransport`; the issue does not name the specific offender, so the audit is part of the work.
- The guard may be implemented inside the existing replay test helpers (a registry or goroutine-tracking check) and applied through those helpers, so that tests starting replayers get it without per-test boilerplate where possible.
- A bounded wait for replayer exit is acceptable; if a replayer fails to exit within that bound, the guard reports a failure rather than hanging.
- Only the engine package's tests are affected; other packages' tests are not known to start delivery replayers.

## Out of Scope *(optional)*

- Changing how the replayer logs (for example routing its output away from the process-wide standard output) or making `captureStdout` race-safe. That would be a broader fix; this issue is about test lifecycle hygiene.
- Auditing or guarding other kinds of leaked goroutines (transports, intake workers, pollers) in the engine tests.
- Any change to production replayer behaviour.

## Source References *(optional)*

- Failing CI run of PR #2446 (2026-10-25), `TestStatusLine_Train_BisectSteps`
- Delivery replay behaviour: `docs/state-machine.md` §7.8b; ADR 2144 and ADR 2143
