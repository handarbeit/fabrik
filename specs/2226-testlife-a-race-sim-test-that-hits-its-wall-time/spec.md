# Feature Specification: testlife: a test binary that hits its wall-time ceiling must actually exit

**Feature Branch**: `fabrik/issue-2226`
**Created**: 2026-10-13
**Status**: Specified
**Input**: User description: "## Problem

`internal/testlife`'s hard wall-time ceiling (#2128) calls `os.Exit(ExitCode)` after printing `test binary exceeded its hard wall-time ceiling of 30m0s (override with SIM_TEST_CEILING); exiting`. In the v0.0.84 release gate's pre-gate (`go test -race -count=1 -parallel 4 -timeout 40m ./tests/sim/...`), the message was printed and `go test` reported `FAIL tests/sim 1800.238s`. However, the `sim.test` process **did not exit**:

- It was reparented to launchd (ppid 1) and spun at about 97% CPU for 8 hours, until it was killed by hand.
- It was started at 2026-10-13 06:18 UTC with argv `sim.test -test.paniconexit0 -test.count=1 -test.parallel=4 -test.timeout=40m0s`.
- Its cwd was a `TestQueuedCommentEject_WorkerInFlight_BatchContinues…` temp repo.
- The gate's next host-load probe found it as an orphan. It probably slowed the following run as well.

A 3 s `sample` of the stuck process showed the main thread entirely in a `cthread_yield` → `swtch_pri` (`sched_yield`) spin, in frames that could not be symbolized because the temporary `go-build` binary is gone. That fits a spin inside the race runtime's exit path (`os.Exit` → exit hooks / race finalisation) while other goroutines are still live, but this is unconfirmed.

## Why it matters

- The ceiling exists so a test binary cannot outlive its run. Here the ceiling itself left exactly the long-lived, CPU-burning orphan it was meant to prevent.
- A release gate's later runs then compete with it for a core.

## Requirements

- **R1:** After the ceiling fires, the process is gone within a bounded time (seconds), even under `-race` and even when `os.Exit` does not return promptly. For example, arm a watchdog before calling `exit` that sends `SIGKILL` to the process's own PID if it is still alive after a short grace.
- **R2:** The same guarantee holds for the owner-watch and parent-watch bail paths, which share `bail`.
- **R3:** A test reproduces "exit does not return" through the existing `exit` seam (a blocking fake) and asserts the process-level fallback fires. Shown non-vacuous by removing the fallback.

## Notes

- Seen on macOS arm64, Go 1.26.2, during the v0.0.84 gate on 2026-10-13.
- Related: #2210 (sim suite duration). The ceiling was only reached because the suite now runs close to 30 minutes under `-race -parallel 4` on a loaded host."

## Background

The test-binary lifetime guard (#2128, ADR-2128) gives the sim test binary a hard wall-time ceiling, an owner watch (exit shortly after the worker that owns it is gone) and a parent watch for re-exec'd helpers. Its purpose is that a test binary can never outlive its run. All three share one "bail" path that prints a message and then asks the process to exit.

In the v0.0.84 release gate's pre-gate (`go test -race -count=1 -parallel 4 -timeout 40m ./tests/sim/...`) the ceiling fired: the message was printed and `go test` reported `FAIL tests/sim 1800.238s`. The `sim.test` process nevertheless did not exit. It was reparented to launchd (ppid 1) and spun at about 97% CPU for 8 hours until killed by hand. It started 2026-10-13 06:18 UTC with argv `sim.test -test.paniconexit0 -test.count=1 -test.parallel=4 -test.timeout=40m0s`, and its cwd was a `TestQueuedCommentEject_WorkerInFlight_BatchContinues…` temp repo. The gate's next host-load probe found it as an orphan, and it probably slowed the following run too.

A 3 s `sample` of the stuck process showed the main thread entirely in a `cthread_yield` → `swtch_pri` (`sched_yield`) spin, in frames that could not be symbolized because the temporary `go-build` binary was gone. That fits a spin inside the race runtime's exit path while other goroutines are still live, but the cause is unconfirmed. This spec therefore does not depend on the root cause: it requires that a bail leaves no surviving process, whatever the exit path does.

The failure defeats the guard's own purpose: the ceiling produced the long-lived, CPU-burning orphan it exists to prevent, and a later gate run then competes with it for a core. Seen on macOS arm64, Go 1.26.2. Related: #2210 (sim suite duration) — the ceiling was only reached because the suite now runs close to 30 minutes under `-race -parallel 4` on a loaded host.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A ceiling-fired test binary is gone within seconds (Priority: P1)

An operator runs the release gate's sim pre-gate under `-race`. The suite exceeds the hard wall-time ceiling. The guard reports the ceiling breach and the test binary terminates within a short, bounded time, even if the ordinary exit does not return.

**Why this priority**: This is the incident. A surviving process burns a core for hours and degrades every later gate run.

**Independent Test**: Drive the ceiling path with the existing exit seam replaced by a fake that never returns; assert the process-level fallback termination is invoked within the grace period.

**Acceptance Scenarios**:

1. **Given** the hard ceiling has fired and the ordinary exit returns promptly, **When** the bail completes, **Then** the process exits with the guard's exit code and the fallback never needs to act.
2. **Given** the hard ceiling has fired and the ordinary exit never returns (it blocks or spins), **When** the grace period elapses, **Then** the process is forcibly terminated by the fallback.
3. **Given** the same hang under the race detector with other goroutines still running, **When** the grace period elapses, **Then** the process is still terminated.

---

### User Story 2 - Owner-watch and parent-watch bails are equally bounded (Priority: P1)

A test binary whose owning worker has disappeared, or whose parent process has gone, bails through the guard's other watches. These bails carry the same exit guarantee as the ceiling.

**Why this priority**: These paths exist precisely to stop orphans, and share the failing exit step. A fix covering only the ceiling would leave the same orphan reachable through them.

**Independent Test**: Trigger each watch's bail through the exit seam with a non-returning fake and assert the fallback fires for each.

**Acceptance Scenarios**:

1. **Given** the owner watch decides to bail and exit never returns, **When** the grace period elapses, **Then** the process is forcibly terminated.
2. **Given** the parent watch decides to bail and exit never returns, **When** the grace period elapses, **Then** the process is forcibly terminated.

---

### User Story 3 - The regression test is demonstrably non-vacuous (Priority: P2)

A maintainer can show the new test actually guards the behavior: with the fallback removed, the test fails.

**Why this priority**: A test that passes without the fix would give false confidence that the orphan class is closed.

**Independent Test**: Remove or neutralise the fallback and run the new test; it must fail.

**Acceptance Scenarios**:

1. **Given** the fallback is removed, **When** the new test runs, **Then** it fails because no forced termination was requested.
2. **Given** the fallback is present, **When** the new test runs, **Then** it passes without actually killing the test process.

---

### User Story 4 - The main test binary exits when the `go test` that started it is gone (Priority: P1)

An operator's `go test` process dies or exits (killed, crashed, or finished after reporting `FAIL`) while the sim test binary it started is still running. The binary notices that its parent is gone and exits through the same bounded bail, instead of being reparented to the init process and running on unnoticed.

**Why this priority**: This is the exact state the incident was found in: the binary was reparented (ppid 1) when `go test` exited and nothing noticed. Bounding the bail alone does not help if no bail is ever triggered; today the parent watch covers only re-exec'd helpers, not the main test binary.

**Independent Test**: Start the guard in the main test binary with a parent identity that is then reported as gone, using a fake exit and a fake forced-termination step; assert the bail is taken with a parent-gone reason and, with a non-returning exit, that the fallback fires.

**Acceptance Scenarios**:

1. **Given** the main test binary's parent process is alive, **When** the guard checks it, **Then** the binary keeps running and no bail occurs.
2. **Given** the parent process has exited (the binary has been reparented), **When** the next parent check runs, **Then** the binary bails and the process is gone within seconds.
3. **Given** the parent's PID has been reused by an unrelated process with a different start token, **When** the guard checks it, **Then** the original parent is treated as gone and the binary bails; the recycled PID is never mistaken for the parent.
4. **Given** the parent's identity cannot be read (the read fails or is inconclusive), **When** the guard checks it, **Then** the binary keeps running.

---

### Edge Cases

- Ordinary exit returns promptly: the fallback must not fire and must not change the exit code.
- Several bail triggers fire at once (ceiling, owner watch, parent watch): the process is still terminated once, with no panic or double-arming problem.
- The fallback must only ever target the guard's own process, never another process or process group.
- The test for the fallback must not terminate the test process itself; the forced-termination step needs a substitutable seam, like the exit step.
- A guard that is not installed (engine-package test binaries do not adopt it yet) is unchanged.
- The parent identity cannot be established at startup (unreadable, or the binary is already reparented): the main-binary parent watch does not arm and the binary keeps running; the ceiling and owner watch still apply.
- Parent identity is unreadable at a later check: keep running, as with the owner watch; a single failed read never causes a bail.
- The parent's PID is recycled by another process: matching uses PID plus start token, so the recycled process is not the parent.
- The existing helper parent watch (re-exec'd helpers) keeps its current behavior and is not weakened by the main-binary watch.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the hard wall-time ceiling fires, the test binary's process must be gone within a bounded time of a few seconds, including under `-race` and including when the ordinary exit call does not return promptly.
- **FR-002**: The guarantee in FR-001 must hold for the owner-watch bail and the parent-watch bail, which share the ceiling's bail path.
- **FR-003**: The forced-termination fallback must be armed before the ordinary exit is requested, so a hang inside the exit step cannot prevent it from being armed.
- **FR-004**: The fallback must act only if the process is still alive after a short grace period, and must terminate only the guard's own process.
- **FR-005**: When the ordinary exit returns promptly, behavior is unchanged: the same message is printed and the same exit code is used.
- **FR-006**: The forced-termination step must be substitutable in tests, alongside the existing exit seam, so tests can observe it without killing the test process.
- **FR-007**: A test must reproduce "exit does not return" with a blocking fake exit and assert that the forced-termination fallback is invoked, for the ceiling path and for the owner-watch and parent-watch paths.
- **FR-008**: That test must fail when the fallback is removed, and this non-vacuity must be demonstrated before the change is accepted.
- **FR-009**: The as-built documentation of the guard must record the fallback and its grace period, and the documentation bundle must be regenerated if a canonical doc page is touched.
- **FR-010**: The main test binary (not only re-exec'd helpers) must watch the process that started it, and must bail, through the same bounded bail path as the other watches, once that parent is gone.
- **FR-011**: The parent's identity must be captured as PID plus start token when the guard is installed, using the guard's existing identity check, and a later check must treat the parent as gone when the PID no longer exists or its start token differs from the captured one, so a recycled PID is never mistaken for the parent.
- **FR-012**: If the parent's identity cannot be read, whether at installation or at a later check, the binary must keep running (fail safe, the same as the owner watch); an unreadable identity never causes a bail.
- **FR-013**: The main-binary parent watch must detect a gone parent within a bounded time of a few seconds to tens of seconds, and a bail it triggers must carry the FR-001 guarantee that the process is gone within seconds of the decision.
- **FR-014**: The main-binary parent watch must have a test that shows a gone parent causes a bail, a live parent and an unreadable identity do not, and a start-token mismatch (recycled PID) does; this test must be demonstrated non-vacuous by neutralising the watch so it fails.
- **FR-015**: The as-built documentation of the guard must record the main-binary parent watch alongside the fallback.

### Key Entities *(if applicable)*

- **Bail path**: The single routine shared by the wall-time ceiling, the owner watch and the parent watch that reports why the binary is stopping and requests exit.
- **Exit seam**: The existing substitutable exit step tests use to observe a bail without ending the test process.
- **Forced-termination fallback**: The new last-resort step that kills the process itself if the ordinary exit has not taken effect after a grace period.
- **Parent identity**: The PID and start token of the process that started the test binary, captured at guard installation and compared on each check to decide whether the parent is still the same live process.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a non-returning exit step, the fallback is invoked after the grace period in 100% of runs, for each of the bail triggers (ceiling, owner watch, parent watch).
- **SC-002**: With a normally returning exit step, the fallback is never invoked and the exit code is unchanged.
- **SC-003**: Removing the fallback makes the new test fail.
- **SC-004**: After a ceiling breach in a release-gate run, no test binary from that run remains alive beyond the grace period plus a few seconds; the gate's next host-load probe finds no orphaned test binary from it.
- **SC-005**: The full suite, including the new tests, passes under the race detector.
- **SC-006**: When the `go test` process that started a sim test binary is killed or exits, the binary is gone within the parent-watch detection interval plus the bail's bounded exit time; no reparented test binary remains.
- **SC-007**: With a live parent, an unreadable parent identity, or an unchanged identity, the parent watch causes zero bails.
- **SC-008**: Neutralising the main-binary parent watch makes its new test fail.

## Assumptions

- The grace period is short (on the order of a few seconds) and fixed; it need not be operator-configurable. It must be long enough that a healthy exit is never preempted.
- Forced self-termination by signal is acceptable on the supported platforms (macOS and Linux); the guard already treats these as its targets.
- The root cause of the spin (suspected race-runtime exit path) is not diagnosed here; a defence that works regardless of cause is sufficient.
- The ceiling value, its override variable and the guard's other behavior are unchanged.
- "The parent" of the main test binary is whichever process started it (normally `go test`); if it is launched by hand from a shell, the shell is the parent and its exit also ends the run. This is the intended meaning of "its run is over".
- The parent-watch detection interval is fixed and need not be operator-configurable; it can be as coarse as the owner watch's, since the aim is to avoid hours-long orphans, not instant exit.

## Out of Scope *(optional)*

- Diagnosing and fixing the underlying hang in the race runtime's exit path.
- Reducing the sim suite's duration so it stays below the ceiling (tracked by #2210).
- The release gate's pre-gate policy (what it runs, how it retries, how it treats a failed pre-gate).
- Adopting the guard in engine-package test binaries (a recorded follow-up of #2128).
- Changing the ceiling's default value or its override variable.
- Reaping other processes the dead test binary may have spawned (covered by the existing worker-reap machinery).

## Source References *(optional)*

- #2128 / ADR-2128 — owner-marked workers and bounded test binaries (introduced the guard)
- #2210 — sim suite duration
- v0.0.84 release gate pre-gate run, 2026-10-13
