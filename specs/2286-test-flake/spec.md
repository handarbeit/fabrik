# Feature Specification: Deterministic reconnect test for the MCP channel shim (fix flaky TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive)

**Feature Branch**: `fabrik/issue-2286`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive` (`internal/mcpstdio`) failed in CI's \"Test and vet\" on PR #2285 (2026-10-14), a `tests/e2e`-only change, after 0.26 s. The re-run is pending. The test restarts a fake daemon and asserts that the shim reconnects and delivers held, then live, events in order. A short wall-clock window in the reconnect or held-queue drain is the likely cause.

## Requirements

- Find the timing dependency: the reconnect backoff, a heartbeat or idle timeout, or the held-queue drain racing the live event.
- Wait on deterministic signals (the attach, the drain completing), never fixed sleeps.

## Acceptance

- The cause is stated in the PR.
- `go test -race -count=300 -run TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive ./internal/mcpstdio/` passes under added CPU load, and the test still fails if held events are not delivered before live ones."

## Background

The MCP stdio shim (`fabrik mcp`) holds a long-lived stream to the Fabrik daemon and, after a daemon restart, must re-attach and deliver events that were held while it was away before any event produced after the reconnect. `TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive` is the regression test for that ordering guarantee.

The test failed intermittently in CI's "Test and vet" job on PR #2285 (2026-10-14), after only 0.26 s. That PR changed only `tests/e2e`, so the failure is unrelated to the change under test and points to a timing-dependent test rather than a product regression. A re-run was pending when the issue was filed. The likely cause is a short wall-clock window somewhere in the reconnect or held-queue drain path. A flaky required check costs every contributor a re-run and, worse, trains people to dismiss real failures. The merge train also re-runs CI on its own trial PRs, so a flake there can eject innocent members.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The reconnect test passes reliably on any machine (Priority: P1)

A maintainer or CI runner executes the reconnect test on a slow, loaded or highly parallel machine. It passes every time, and its pass/fail depends only on whether the shim's behavior is correct, never on how fast the host is.

**Why this priority**: This is the reported defect. A flaky required check blocks unrelated PRs and erodes trust in CI.

**Independent Test**: Run `go test -race -count=300 -run TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive ./internal/mcpstdio/` while the host is under added CPU load (for example several busy-loop processes, or `GOMAXPROCS` constrained). All 300 runs pass.

**Acceptance Scenarios**:

1. **Given** a fake daemon that has been restarted and holds events queued from while the shim was disconnected, **When** the shim reconnects, **Then** the test proceeds only once the shim has actually re-attached (a signal from the attach itself), not after a fixed delay.
2. **Given** the shim has re-attached, **When** the test emits a live event, **Then** the test waits for the held events to have been fully delivered (a signal from the drain completing) before asserting order or emitting further events, and does not rely on a sleep or timeout window.
3. **Given** a host under heavy CPU contention, **When** the test runs 300 times under `-race`, **Then** every run passes.

---

### User Story 2 - The test still catches an ordering regression (Priority: P1)

Making the test deterministic must not weaken it. If the shim or the held-queue ever delivers a live event before the held ones, or drops a held event, the test fails.

**Why this priority**: A flake fix that silently removes the assertion would hide the very bug the test exists to catch.

**Independent Test**: Temporarily break the behavior (for example, make live events overtake held ones, or skip delivery of held events) and confirm the test fails, reliably and with a clear message. The change is reverted afterwards.

**Acceptance Scenarios**:

1. **Given** the shim delivers a live event ahead of the held events, **When** the test runs, **Then** it fails.
2. **Given** the shim never delivers the held events after reconnect, **When** the test runs, **Then** it fails (bounded by a generous timeout used only as a failure backstop, never as the success path).

---

### User Story 3 - The root cause is documented (Priority: P2)

A reviewer reading the PR can see exactly which timing dependency caused the flake and why the new synchronization removes it.

**Why this priority**: The issue explicitly requires the cause to be stated; it also prevents the same pattern being reintroduced in sibling tests.

**Independent Test**: The PR description names the specific dependency (reconnect backoff, heartbeat or idle timeout, or held-queue drain racing the live event) and explains how the test now synchronizes on a deterministic signal.

**Acceptance Scenarios**:

1. **Given** the PR is open, **When** a reviewer reads its description, **Then** it states the cause and the replacement signal.

---

### Edge Cases

- Reconnect backoff is jittered: the test must not depend on a particular delay, and must not assume the first reconnect attempt succeeds.
- The shim's heartbeat watchdog and the server's idle timeout must not fire during the test's normal path on a slow host; if they can, the test must not depend on them being quiet for a fixed window.
- A live event produced at the same moment the shim re-attaches must still be delivered after the held ones (the ordering property under test), and the test must exercise that race deterministically rather than hope for it.
- A genuine failure (events never delivered) must fail with a bounded timeout, not hang the suite.
- If the root cause turns out to be a real race in product code (not only in the test), it is fixed there too, while keeping the ordering guarantee.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The investigation identifies and states the specific timing dependency behind the intermittent failure (reconnect backoff, heartbeat or idle timeout, or the held-queue drain racing the live event).
- **FR-002**: The test synchronizes on deterministic events, namely the shim's re-attach and the completion of the held-event drain, and contains no fixed sleeps or wall-clock windows on its success path.
- **FR-003**: Any timeout remaining in the test is used only as a failure backstop, is generous enough not to trigger on a heavily loaded host, and produces a clear failure message when it trips.
- **FR-004**: The test continues to assert that all held events are delivered, and delivered before any live event, and fails if either property is violated.
- **FR-005**: If the flake exposes a genuine race in the shim or held-queue code, that race is fixed in the product code without weakening the held-then-live ordering guarantee.
- **FR-006**: The fix introduces no new production-only test hooks that change behavior when unset; any test seam is inert in production.
- **FR-007**: The PR description states the cause of the flake.

### Key Entities

- **Shim**: the stdio MCP server that streams daemon events to the client session and reconnects after the daemon goes away.
- **Fake daemon**: the in-test stand-in for the Fabrik daemon that is stopped and restarted during the test.
- **Held queue**: the per-subscriber queue of events accumulated while no session is attached, drained in order on re-attach.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go test -race -count=300 -run TestShimReconnectsAfterDaemonRestartAndDeliversHeldThenLive ./internal/mcpstdio/` passes with zero failures while the host is under added CPU load.
- **SC-002**: With held-event delivery deliberately broken or reordered, the same test fails on every run.
- **SC-003**: The success path of the test contains no fixed sleep or wall-clock wait; synchronization is on the attach and drain-completion signals only.
- **SC-004**: The full `internal/mcpstdio` package and `go test -race ./...` still pass.
- **SC-005**: The PR description states the root cause.

## Assumptions

- The flake is primarily a test-synchronization problem; product code is changed only if the investigation shows a real race there.
- "Added CPU load" is produced locally by the implementer (busy-loop processes or constrained parallelism); no new CI job or load generator is added.
- The existing in-package test seams and helpers may be reused or extended for waiting on attach and drain, provided they are inert in production.
- Other tests in the package are only touched if they share the same helper and the same defect.

## Out of Scope *(optional)*

- Changing the reconnect backoff, heartbeat or idle-timeout values or the channel protocol itself.
- Sweeping every test in the repository for timing dependencies.
- Adding CI retry logic or marking the test as allowed to flake.
- Any change to the held-queue's delivery semantics.

## Source References *(optional)*

- PR #2285 CI run ("Test and vet", 2026-10-14), where the failure was observed after 0.26 s.
- `internal/mcpstdio` channel tests and `docs/state-machine.md` §7.14 (channel push, restart delivery) for the intended ordering behavior.
