# Feature Specification: Route liveness: track the arrival hook-refresh goroutine so stop waits for it

**Feature Branch**: `fabrik/issue-2480`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `routeLiveness.refreshHookForArrival` (`engine/webhook_liveness.go`, #2452) starts a bare `go func()` that reads the App's hook config and may then call `decide`. `decide` can call `Report` or `Resolve` on the warnings panel. The goroutine is not tracked, so the stop function returned by `startRouteLiveness` does not wait for it.
>
> As a result, after a transport stop or a test cleanup, a refresh that is still in flight can:
> - read engine state;
> - write a warnings-panel entry after the controller has stopped.
>
> In tests, it can still be running when the test returns. That is the same class of leak #2447 closed for the delivery replayer, and the shape of the TempDir races in #2460.
>
> ## Requirements
>
> - Track the refresh goroutine with the controller's own lifecycle (a WaitGroup or equivalent), so that `stop` cancels it and waits for it.
> - The refresh must honour the controller's context, so that a stopped controller makes no further hook-config read or verdict.
> - No change to the refresh's rate limit (`livenessArrivalRefreshGap`, one in flight).
>
> ## Acceptance
>
> - A test stops the controller while a refresh is blocked in the hook-config read, and asserts that `stop` returns only after the goroutine has exited and that no verdict or panel write happens after `stop`.
> - With the tracking neutralised, that test fails.
>
> Mentioned for context: #2452, #2447.

## Background

The webhook route-liveness controller (#2452) decides whether the App's webhook route is reachable. When a real delivery arrives whose URL does not match the cached hook URL, the controller re-reads the App's hook configuration off the request path and judges the arrival against the fresh value. That re-read runs in a goroutine that nothing tracks.

The controller's stop function therefore returns while a refresh may still be running. A refresh that outlives the controller can still:

- read engine state;
- call the verdict logic, which can record or clear an entry in the operator warnings panel, after the controller has been declared stopped.

In production this is a stale write after shutdown. In tests it is a goroutine still running after the test has returned, which produces the cleanup races (such as the temp-directory races in #2460) and the leaked-goroutine failures that #2447 already eliminated for the delivery replayer. The fix brings this goroutine under the same lifecycle discipline as the controller's other goroutines. It changes no liveness behaviour while the controller is running.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Stopping the controller waits for an in-flight arrival refresh (Priority: P1)

An operator (or a test cleanup) stops the route-liveness controller while an arrival-triggered hook-config refresh is in flight. Stop must not return until that refresh has finished.

**Why this priority**: This is the defect itself. A stop that returns while work continues defeats the point of a lifecycle and causes both production stale writes and test leaks.

**Independent Test**: Start a controller, trigger an arrival whose URL mismatches the cached hook URL so a refresh starts, and block the hook-config read. Call stop and verify it does not return until the read is released and the goroutine has exited.

**Acceptance Scenarios**:

1. **Given** a refresh blocked in the hook-config read, **When** the controller is stopped, **Then** stop does not return while the refresh goroutine is still running.
2. **Given** a refresh blocked in the hook-config read, **When** the controller is stopped and the read then returns, **Then** no verdict is produced and no warnings-panel entry is recorded or resolved.
3. **Given** no refresh in flight, **When** the controller is stopped, **Then** stop returns promptly as before.

---

### User Story 2 - A stopped controller makes no further reads or verdicts (Priority: P1)

Once the controller's context is cancelled, a refresh must observe that and abandon its work rather than completing it.

**Why this priority**: Waiting for the goroutine is only safe if the goroutine can actually be cancelled. Otherwise stop could hang on a slow hook-config read.

**Independent Test**: Cancel the controller (via stop) while a refresh is blocked in a context-aware hook-config read; verify the read observes cancellation, the goroutine exits and stop returns.

**Acceptance Scenarios**:

1. **Given** a refresh waiting on the hook-config read, **When** the controller's context is cancelled, **Then** the read is cancelled and the refresh ends without calling the verdict logic.
2. **Given** an already-stopped controller, **When** an arrival is observed, **Then** no new refresh is started and no hook-config read is made.

---

### User Story 3 - Refresh pacing is unchanged (Priority: P2)

Operators and the existing behaviour must not see a different refresh rate.

**Why this priority**: The change is a lifecycle fix; altering pacing would be an unrelated behaviour change.

**Independent Test**: Existing liveness tests for the arrival refresh (one in flight, minimum gap between refreshes) pass unmodified.

**Acceptance Scenarios**:

1. **Given** a refresh in flight, **When** another mismatching arrival occurs, **Then** no second refresh starts (one in flight).
2. **Given** a refresh finished less than the minimum gap ago, **When** another mismatching arrival occurs, **Then** no refresh starts until the gap has passed.

---

### Edge Cases

- Stop called twice, or concurrently with a refresh that is just starting: both calls return, and no refresh outlives the first stop.
- An arrival that races with stop: either the refresh starts and is waited for, or it does not start; it is never left running untracked.
- The hook-config read fails or returns after cancellation: the failure is not turned into a verdict (consistent with "a failed read is never unreachable").
- A refresh that completes normally before stop: its verdict is applied exactly as today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The arrival hook-refresh goroutine MUST be tracked as part of the route-liveness controller's lifecycle, so that the controller's stop function waits for it to exit before returning.
- **FR-002**: Stopping the controller MUST cancel an in-flight arrival refresh.
- **FR-003**: The arrival refresh MUST honour the controller's context. After the controller is stopped it MUST make no further hook-config read and MUST NOT produce a verdict (no `decide`, no panel `Report` or `Resolve`).
- **FR-004**: A stopped controller MUST NOT start a new arrival refresh.
- **FR-005**: The arrival refresh's rate limiting (`livenessArrivalRefreshGap`, at most one in flight) MUST be unchanged.
- **FR-006**: Behaviour of the controller while it is running (verdicts, probes, panel entries, passive and active liveness logic) MUST be unchanged.

### Key Entities

- **Route-liveness controller**: The per-App-receiver component that decides `reachable`/`unreachable`/`unknown` for the webhook route, with a start/stop lifecycle.
- **Arrival hook refresh**: The off-request-path re-read of the App's hook configuration triggered by a real delivery that does not match the cached hook URL.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test that stops the controller while a refresh is blocked in the hook-config read shows stop returning only after the refresh goroutine has exited.
- **SC-002**: The same test shows zero verdicts and zero warnings-panel writes after stop returns.
- **SC-003**: With the tracking neutralised through the project's usual test-seam convention, that test fails (non-vacuity).
- **SC-004**: The existing liveness tests, including those for refresh pacing, pass without modification, and the engine test suite is race-clean.

## Assumptions

- The controller already has a context that stop cancels, or the equivalent can be introduced without changing observable behaviour; the hook-config read can be made to respect cancellation.
- The same lifecycle-tracking pattern used for the delivery replayer (#2447) is an acceptable precedent for this goroutine.
- A neutralisation seam for the new tracking is acceptable, following the repository's existing `Set…ForTest` convention for non-vacuity tests.
- Documentation of the as-built behaviour in the canonical docs is updated only where it currently describes the controller's lifecycle.

## Out of Scope

- Any change to the liveness decision rules, probe scheduling or panel semantics.
- Any change to `livenessArrivalRefreshGap` or the one-in-flight rule.
- Auditing other untracked goroutines in the engine beyond this one.

## Source References

- #2452: webhook route liveness (introduced the refresh)
- #2447: delivery replayer goroutine tracking (the precedent)
- #2460: TempDir races in tests (same class of failure)
