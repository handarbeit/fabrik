# Feature Specification: Stop the bed engine on every gate exit, including a failed suite

**Feature Branch**: `fabrik/issue-2245`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

When a gate invocation exits after a failed suite, the test bed's engine is left running. In the v0.0.84 gate this happened twice:
- after run 1, an orphaned bed engine (pid 90575) kept polling for 13 hours;
- after the 2026-10-13 run (`GATE-EXIT=1`), pid 95267 was still running and was stopped by hand.

A leftover bed engine spends the bed identity's GraphQL budget, keeps acting on the test board between invocations, and can race the next invocation's own bed start.

## Requirements

- R1: every gate exit path (success, suite failure, watchdog, preflight/precondition failure after the bed was started, SIGINT/SIGTERM of the gate) stops the bed engine the gate started, gracefully (SIGINT, bounded wait, then escalate through `internal/sessionreap`).
- R2: the gate never stops a bed engine it did not start (identify by recorded PID + start token).
- R3: the gate logs one line naming the stopped PID, or why it left one running.

## Acceptance

- A gate unit test (fake Commander) proves the bed stop runs on the suite-failure exit and on cancellation.
- After a live invocation that exits non-zero, no `fabrik -notui` process remains under the bed directory.

## Scope

`tests/gate` only. No engine change."

## Background

The live e2e gate runner (`tests/gate`) starts a Fabrik engine ("the bed engine") against the test bed so the live suite has something to exercise. Today the engine is stopped only at specific points: after the preflight build, and when a multi-bed scheduler parks an idle bed. A gate invocation whose suite fails, is killed by its watchdog, is cancelled by a signal, or fails a later precondition does not stop the engine it started, and the engine outlives the gate.

This happened twice during the v0.0.84 gate. After run 1, an orphaned bed engine (pid 90575) kept polling for 13 hours. After the 2026-10-13 run (`GATE-EXIT=1`), pid 95267 was still running and was stopped by hand.

A leftover bed engine causes three problems:
- It spends the bed identity's GraphQL budget with no gate running.
- It keeps acting on the test board between invocations.
- It can race the next invocation's own bed start, so two engines contend for one bed.

The fix belongs in the gate runner only. The engine already stops cleanly on a signal, and no engine change is needed.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A failed suite leaves no engine behind (Priority: P1)

A release engineer runs the gate. The live suite fails and the gate exits non-zero. The engine the gate started for that run is stopped before the gate process exits.

**Why this priority**: This is the incident as observed twice in the v0.0.84 gate, and the failure mode with the highest cost: hours of unattended polling and budget burn.

**Independent Test**: Run a gate invocation in which the suite reports failure (using a fake `Commander`) and assert that the bed stop was issued for the engine the gate started, and that the exit code is still the suite's own.

**Acceptance Scenarios**:

1. **Given** the gate started a bed engine, **When** the suite fails and the gate returns a non-zero exit code, **Then** the bed engine is stopped before the gate returns and the exit code is unchanged.
2. **Given** the gate started a bed engine, **When** the suite passes, **Then** the bed engine is also stopped before the gate returns.

---

### User Story 2 - Every other exit path also cleans up (Priority: P1)

The gate can end in ways other than a clean suite result: the post-suite watchdog fires, a preflight or precondition fails after the bed was started, or the operator sends SIGINT or SIGTERM to the gate. In each case the bed engine started by the gate is stopped.

**Why this priority**: A stop that covers only the suite-failure path leaves the same leak on every other path. The cleanup must be a property of gate exit, not of one branch.

**Independent Test**: With a fake `Commander`, drive the gate to each exit path (watchdog, post-start preflight or precondition failure, context cancellation) and assert the bed stop runs once on each.

**Acceptance Scenarios**:

1. **Given** the gate started a bed engine, **When** the gate's context is cancelled (SIGINT/SIGTERM), **Then** the bed stop still runs, using a context that is not already cancelled, and completes within its bounded wait.
2. **Given** the gate started a bed engine, **When** a preflight or precondition fails after that start, **Then** the engine is stopped before the gate returns.
3. **Given** the gate started a bed engine, **When** the post-suite watchdog fires, **Then** the engine is stopped before the gate returns.
4. **Given** the gate exits before it started any bed engine, **When** it returns, **Then** no stop is attempted and nothing is logged as left running.

---

### User Story 3 - The gate never touches an engine it did not start (Priority: P1)

An operator, or another tool, has a Fabrik engine running against the same bed directory that this gate invocation did not launch. The gate does not stop it.

**Why this priority**: Stopping someone else's engine on exit would be a new harm. It would pause in-flight work that belongs to another run.

**Independent Test**: With a fake `Commander`, present a live process at the bed's lock PID whose identity does not match the engine the gate recorded at start, and assert no signal is sent and the log line says why it was left running.

**Acceptance Scenarios**:

1. **Given** the gate recorded the engine's PID and start token when it started it, **When** the gate exits and the live process at that PID has the same start token, **Then** the gate stops it.
2. **Given** the PID is now held by a process with a different start token (the PID was recycled), **When** the gate exits, **Then** the gate sends no signal and logs that it left the process running and why.
3. **Given** the bed's lock names a live process the gate never recorded, **When** the gate exits, **Then** the gate sends no signal.

---

### User Story 4 - Graceful stop with bounded escalation (Priority: P2)

The gate stops its engine with a graceful signal first, so the engine's clean-stop path runs. It waits a bounded time, and escalates through `internal/sessionreap` only if the engine does not exit.

**Why this priority**: A graceful stop keeps in-flight issues durably paused rather than abandoned mid-stage. The escalation guarantees the gate cannot hang or leak on a wedged engine.

**Independent Test**: With a fake `Commander` and fake clock, assert that SIGINT is sent first, that no escalation occurs if the process exits inside the wait, and that escalation occurs through the session-reap path if it does not.

**Acceptance Scenarios**:

1. **Given** an engine that exits promptly on SIGINT, **When** the gate stops it, **Then** only SIGINT is sent.
2. **Given** an engine that ignores SIGINT, **When** the bounded wait elapses, **Then** the gate escalates through `internal/sessionreap` rather than leaving it running.
3. **Given** an engine that survives escalation, **When** the stop gives up, **Then** the gate logs that the engine was left running, with its PID.

---

### User Story 5 - The operator can see what happened (Priority: P2)

On every exit, the gate prints exactly one line about the bed engine: either the PID it stopped, or the reason it left an engine running.

**Why this priority**: Without the line, the operator cannot tell a clean exit from a silently leaked engine. That ambiguity is how the 13-hour orphan went unnoticed.

**Independent Test**: Capture the gate's output across the exit paths and count the lines naming the bed engine's fate.

**Acceptance Scenarios**:

1. **Given** the gate stopped its engine, **When** it exits, **Then** one line names the stopped PID.
2. **Given** the gate left an engine running (not its own, an unreadable identity, or a stop that failed), **When** it exits, **Then** one line names the PID and the reason.

---

### Edge Cases

- The engine already exited on its own before the gate's exit cleanup runs. Nothing is signalled, and the log says there was nothing to stop.
- The lock file is missing or unreadable at exit, though the gate recorded a started engine. The recorded PID and start token decide, and the lock file is not relied on.
- The recorded start token cannot be read at exit (for example a SIP-protected process on macOS). Identity cannot be confirmed, so the gate does not signal, and the log says so.
- The gate's context is already cancelled when cleanup runs. The stop does not inherit the cancellation, and it stays bounded.
- The gate is interrupted a second time during cleanup. Cleanup is idempotent and does not stop the same engine twice or hang.
- The suite's exit code and the stop's outcome disagree. The gate's exit code is the suite's own and is never changed by the stop, whether the stop succeeds or fails.
- Multi-bed (`E2E_BEDS`) runs. Each bed's engine is covered, and only the ones that bed's gate view started.
- A bed engine restarted mid-run (the train-mode switch restarts the bed). The record tracks the currently running gate-started engine, not only the first one.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the gate starts a bed engine, it records that engine's PID and process start token.
- **FR-002**: Every exit path of a gate invocation that has started a bed engine stops that engine before the gate process exits. The paths are: suite success, suite failure, post-suite watchdog, a preflight or precondition failure after the start, and cancellation by SIGINT or SIGTERM.
- **FR-003**: The stop sends SIGINT first, waits a bounded time for the engine to exit, and then escalates through `internal/sessionreap` if it is still alive.
- **FR-004**: The stop runs even when the gate's own context is already cancelled, and is bounded in time on its own.
- **FR-005**: The stop acts only on an engine whose live PID and start token both match what the gate recorded at start. A PID held by a process with a different start token, an engine the gate did not record, or a token that cannot be read results in no signal.
- **FR-006**: The gate prints exactly one line about the bed engine on exit. It names the stopped PID, or names the PID (when known) and the reason the engine was left running. If the gate never started an engine, it prints no such line.
- **FR-007**: The stop does not change the gate's exit code or any of the documented exit-code contract values. A failed stop is reported only in the log line.
- **FR-008**: The change is confined to `tests/gate`. No engine, CLI or `scripts/` behaviour changes.
- **FR-009**: A unit test using a fake `Commander` shows the bed stop runs on the suite-failure exit.
- **FR-010**: A unit test using a fake `Commander` shows the bed stop runs on cancellation.
- **FR-011**: Unit tests cover the identity refusal (recycled PID / unrecorded engine) and the escalation path.
- **FR-012**: In multi-bed runs, each bed's exit cleanup applies the same rules to that bed's own recorded engine, and never to another bed's.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After any gate invocation that exits non-zero, zero `fabrik -notui` processes remain under the bed directory that the gate started.
- **SC-002**: After any gate invocation, including one interrupted by SIGINT or SIGTERM, zero engines the gate started remain running. An interrupted invocation is allowed the bounded stop wait before the process exits.
- **SC-003**: An engine the gate did not start is signalled in 0% of exits, including a recycled PID and a lock held by an unrecorded process.
- **SC-004**: Every gate exit that started a bed engine produces exactly one log line stating the engine's fate.
- **SC-005**: The full gate unit-test suite passes in plain `go test ./...` with no bed, network or `gh`, and includes tests for the suite-failure exit and for cancellation.

## Assumptions

- "Started" means launched by this gate invocation through its own bed start. An engine already running when the invocation begins is not "started by the gate" for this purpose. The existing preflight behaviour, which stops whatever engine holds the lock before a rebuild, is unchanged and out of this issue's scope.
- SIGINT, as the issue specifies, is treated as a clean-stop signal by the engine, equivalent to the SIGTERM the gate sends today. This is why the engine needs no change, and it is the premise of FR-008. The escalation target after the bounded wait is the existing `internal/sessionreap` machinery, not a new kill path.
- The bounded wait reuses the gate's existing stop-wait configuration rather than introducing a new knob. The wait is the same order of magnitude as today's.
- Cleanup applies to `gate run`, the invocation that starts a bed engine. Read-only subcommands (`gate coverage`, `gate report`) start no engine and are unaffected. `gate reset` already refuses to run against a live instance.
- The live acceptance check (no `fabrik -notui` remaining after a non-zero live invocation) is a manual or release-gate verification. CI covers the behaviour through the fake-`Commander` unit tests.

## Out of Scope *(optional)*

- Any engine change, including how the engine handles SIGINT versus SIGTERM.
- Reaping orphaned engines left by earlier invocations or by other tools. This issue only prevents the gate from creating new ones.
- Changing the preflight's stop of a pre-existing lock holder.
- Killing worker or test processes below the engine beyond what `internal/sessionreap` escalation already does.
- Changes to the exit-code contract, the coverage ledger, or the scheduling of cells.
- The mid-run train-mode restart logic, except that exit cleanup tracks the engine that is currently running.
- Cleanup of orphaned engines in the e2e harness paths outside `tests/gate`.

## Source References *(optional)*

- Observed incidents: the v0.0.84 gate, orphaned bed engines pid 90575 (run 1) and pid 95267 (2026-10-13 run, `GATE-EXIT=1`).
- `tests/gate/README.md` and ADR-1994 (Go gate runner), ADR-1976 (multi-bed), ADR-1957 (ownership-checked signalling via `internal/sessionreap`).
