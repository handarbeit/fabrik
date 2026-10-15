# Feature Specification: Sim pre-gate: say which test is running, slow or hung, and keep the evidence

**Feature Branch**: `fabrik/issue-2247`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "

> ## Problem
>
> When the sim pre-gate fails slowly, its output does not say which test is responsible. In the v0.0.84 gate (2026-10-13), `go test -race ./tests/sim/...` ran for 2642s and ended with only:
>
> ```
> tests/sim: test binary exceeded its hard wall-time ceiling of 40m0s (override with SIM_TEST_CEILING); exiting
> FAIL	github.com/handarbeit/fabrik/tests/sim	2640.239s
> ```
>
> The same suite at the same sim source had passed in 585s earlier that day. Everything needed to diagnose it was missing:
> - **No test names.** The pre-gate does not run with `-v`/`-json`, so started/finished tests are not recorded. The last lines were an anonymous sim engine polling `[poll] nothing to do`, with no way to tell which test owned it.
> - **No stacks.** The `internal/testlife` ceiling exits without a goroutine dump, so a hang and a merely slow run look identical. The `-test.timeout` panic, which would dump, is set above the ceiling.
> - **No timestamps.** Sim engine log lines carry no time and no test name, so the interleaved output of parallel tests cannot be attributed or timed.
> - **No archive.** The output exists only in the gate's own stdout; nothing is kept per pre-gate run.
>
> The only way forward was to re-run the whole suite by hand with `-v` and a lower `-timeout`, another 10-40 minutes.
>
> ## Requirements
>
> - **R1, per-test record:** the pre-gate runs the sim through `go test -json` (the gate already parses the live legs' JSON stream) and records each test's start, end, outcome and elapsed time.
> - **R2, heartbeat:** while the sim runs, the gate prints every few minutes the tests still running and how long each has run, plus a warning for any test running longer than a threshold (default: 3x its last recorded duration, or 5 minutes for a test with no history).
> - **R3, dump on ceiling:** before the `testlife` ceiling (and the owner/parent watches) exits, it writes every goroutine's stack (`runtime/pprof` goroutine profile, debug=2) to stderr and to a file next to the binary's output, then exits as today. The bounded self-kill of #2226 still applies.
> - **R4, failure summary:** on a sim failure or ceiling, the gate prints the tests that were still running, their elapsed times, and the path of the stack dump.
> - **R5, attribution:** each sim engine log line carries the owning test's name (or a short per-Env prefix) and a timestamp, so parallel tests' output can be separated after the fact.
> - **R6, archive:** the full sim output (JSON stream, rendered log and any dump) is kept per pre-gate run under `.e2e-coverage/pregate/<head>.*` with the existing retention, and its path is printed.
> - **R7, timing summary:** a passing pre-gate prints the slowest 10 tests with their durations, so creeping slowness is visible before it becomes a ceiling hit.
>
> ## Acceptance
>
> - A gate unit test (fake Commander, recorded JSON stream) shows the heartbeat and the still-running list for a test that never finishes.
> - A `testlife` unit test proves the ceiling writes a goroutine dump before exiting, and that a neutralisation seam disables it.
> - Forcing a hang in one sim test (local only) produces a pre-gate failure that names the test and points at a dump showing where it blocked.
>
> ## Scope
>
> `tests/gate` (pre-gate step), `internal/testlife`, the sim harness's logging (`tests/sim`). No engine behaviour change. Related: #2228 (shard the sim), #2226 (bounded test-binary exit).
"

## Background

When the sim pre-gate fails slowly, its output does not say which test is responsible. In the v0.0.84 gate (2026-10-13), the sim run (`go test -race ./tests/sim/...`) ran for 2642s and ended with only the line that the test binary exceeded its hard wall-time ceiling of 40m (override with `SIM_TEST_CEILING`) and a bare `FAIL` for the package. The same suite at the same sim source had passed in 585s earlier that day. Everything needed to diagnose it was missing:

- **No test names.** The pre-gate does not run with `-v`/`-json`, so started and finished tests are not recorded. The last lines were an anonymous sim engine polling `[poll] nothing to do`, with no way to tell which test owned it.
- **No stacks.** The `internal/testlife` ceiling exits without a goroutine dump, so a hang and a merely slow run look identical. The `-test.timeout` panic, which would dump, is set above the ceiling.
- **No timestamps.** Sim engine log lines carry no time and no test name, so the interleaved output of parallel tests cannot be attributed or timed.
- **No archive.** The output exists only in the gate's own stdout; nothing is kept per pre-gate run.

The only way forward was to re-run the whole suite by hand with `-v` and a lower timeout, another 10-40 minutes.

A second gap was found the same day (reported in a follow-up comment): the bounded self-kill of #2226 does not cover a `-race` binary that hangs inside process exit. On 2026-10-13 two `sim.test` binaries hit their 40-minute ceiling, printed "exiting", then spun at roughly 97% CPU for about an hour inside the race runtime's exit path. The self-kill fallback is an in-process timer, which never fires once the process is stuck there. The pre-gate no longer uses `-race` (#2248), so this matters only for `SIM_RACE=1` runs and any other `-race` binary using `testlife`.

Related work: #2228 (shard the sim), #2226 (bounded test-binary exit), #2248 (pre-gate drops `-race`).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Know which test is running, slow or hung (Priority: P1)

A release operator watching the sim pre-gate sees, while it runs, which tests are still in flight and how long each has been running, with a warning for any test that is taking unusually long. When the pre-gate fails or hits the ceiling, the failure output names the tests that were still running and points at a stack dump showing where they blocked.

**Why this priority**: This is the core diagnostic gap. Without it a hang and a slow run look identical and diagnosis costs another 10-40 minute manual re-run.

**Independent Test**: Run the gate's pre-gate step against a recorded JSON stream in which one test starts and never finishes. The output must show the periodic still-running list, the over-threshold warning, and the failure summary naming that test and the dump path.

**Acceptance Scenarios**:

1. **Given** the sim is running and a test has started but not finished, **When** the heartbeat interval elapses, **Then** the gate prints each still-running test with its elapsed time.
2. **Given** a test has run longer than 3x its last recorded duration (or 5 minutes with no history), **When** the next heartbeat prints, **Then** that test is flagged with a warning.
3. **Given** the sim binary exits on its ceiling, **When** the pre-gate reports failure, **Then** it prints the tests still running, their elapsed times and the path of the goroutine stack dump.

---

### User Story 2 - Dump all goroutine stacks before the ceiling exits (Priority: P1)

When a sim test binary reaches its `testlife` hard ceiling (or its owner/parent watch fires), it first writes every goroutine's stack to stderr and to a file next to the binary's output, then exits as it does today.

**Why this priority**: The stack dump is what distinguishes a hang from slowness and shows where it blocked; the test names alone do not.

**Independent Test**: A `testlife` unit test triggers the ceiling and observes that a goroutine dump is written before the exit is requested; a second case with the neutralisation seam set observes no dump.

**Acceptance Scenarios**:

1. **Given** a test binary blocked past its ceiling, **When** the ceiling fires, **Then** all goroutine stacks are on stderr and in a dump file before the process exits.
2. **Given** the dump neutralisation seam is set, **When** the ceiling fires, **Then** no dump is written and the exit behaves as before.
3. **Given** the ceiling fires, **When** the dump is written, **Then** the bounded self-kill of #2226 is still armed and still bounds the exit.

---

### User Story 3 - Attribute sim log lines to their test (Priority: P2)

A developer reading the pre-gate log can tell which parallel test produced each sim engine log line and when, so interleaved output can be separated after the fact.

**Why this priority**: It makes the archived log usable for diagnosis, but the heartbeat and dump already give the main answer.

**Independent Test**: Run two sim tests in parallel; every engine log line in the output carries a timestamp and the owning test's name (or a short per-Env prefix), and lines can be grouped by test.

**Acceptance Scenarios**:

1. **Given** two sim tests running in parallel, **When** both engines log, **Then** each line carries a timestamp and an identifier of its owning test.

---

### User Story 4 - Keep the evidence and see slowness early (Priority: P2)

Every pre-gate run keeps its full sim output (JSON stream, rendered log, any dump) in a per-run archive under the existing retention, and prints its path. A passing pre-gate prints the ten slowest tests with their durations.

**Why this priority**: Preserves evidence for after-the-fact diagnosis and makes creeping slowness visible before it becomes a ceiling hit.

**Independent Test**: Run the pre-gate (passing and failing) against a recorded stream; the archive files exist under `.e2e-coverage/pregate/<head>.*`, the path is printed, and the passing run lists exactly the ten slowest tests in descending order.

**Acceptance Scenarios**:

1. **Given** any pre-gate run, **When** it finishes, **Then** the JSON stream, rendered log and any dump are present under `.e2e-coverage/pregate/<head>.*` and the path is printed.
2. **Given** a passing pre-gate, **When** it finishes, **Then** the slowest 10 tests and their durations are printed.
3. **Given** archives older than the existing retention, **When** a new run is archived, **Then** the existing retention policy applies to them unchanged.

---

### User Story 5 - A `-race` binary hung in exit leaves no process behind (Priority: P2)

When a `testlife`-guarded `-race` test binary prints that it is exiting but then hangs inside the race runtime's exit path, the process is still killed within a bounded time, even though an in-process timer cannot fire. No orphaned, CPU-spinning test binary is left after the run.

**Why this priority**: The 2026-10-13 incident left two binaries spinning for an hour. It only affects `SIM_RACE=1` and other `-race` users now that the pre-gate drops `-race`, so it is lower than the diagnostic work.

**Independent Test**: Force an exit hang in a `-race` test binary (local only); after the run's bounded grace period no process from that run remains.

**Acceptance Scenarios**:

1. **Given** a `-race` test binary that stalls inside exit after its ceiling fired, **When** the bounded kill window elapses, **Then** the process is terminated by something that does not depend on the stalled process's own timers.
2. **Given** the kill is applied, **When** it targets a process, **Then** it targets only the intended test binary (verified by PID and start token) and never an unrelated process.

---

### Edge Cases

- A test with no recorded duration history uses the 5-minute default for the slow-test warning.
- A test that finishes between heartbeats is not reported as still running.
- The JSON stream contains non-JSON lines (build output, the binary's own stderr); the gate must not misparse or drop them and they still reach the archived log.
- The sim binary is killed with no test in flight, or before any test started; the failure summary must still print and say that no tests were running.
- Dump file or archive write fails (full disk, permissions); the failure must not prevent the process from exiting or mask the original failure.
- The ceiling fires while the process is already exiting through the owner/parent watch; only one dump is written.
- Parallel tests produce interleaved output; attribution must not depend on line ordering.
- A hang inside exit under `-race` where the process is still consuming CPU but making no progress; the kill must still succeed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The sim pre-gate runs the sim through `go test -json` and records, for each test, its start, end, outcome and elapsed time.
- **FR-002**: While the sim runs, the gate prints at a regular interval of a few minutes the tests still running and how long each has run.
- **FR-003**: The gate warns for any running test whose elapsed time exceeds a threshold, defaulting to 3x its last recorded duration, or 5 minutes for a test with no history.
- **FR-004**: Before the `testlife` hard ceiling, and the owner and parent watches, request process exit, they write a goroutine profile of every goroutine (full stack detail) to stderr and to a file next to the binary's output, then exit as they do today.
- **FR-005**: The goroutine dump is disabled by a neutralisation seam so it can be tested and bypassed.
- **FR-006**: The bounded self-kill introduced by #2226 remains armed and unaffected by the dump.
- **FR-007**: On a sim failure or ceiling exit, the gate prints the tests still running, their elapsed times, and the path of the stack dump.
- **FR-008**: Each sim engine log line carries a timestamp and the owning test's name (or a short per-Env prefix), so parallel tests' output can be separated.
- **FR-009**: The full sim output (JSON stream, rendered log and any dump) is kept per pre-gate run under `.e2e-coverage/pregate/<head>.*` under the existing retention, and its path is printed.
- **FR-010**: A passing pre-gate prints the 10 slowest tests with their durations.
- **FR-011**: A `testlife`-guarded `-race` test binary that hangs inside process exit after its ceiling fired is terminated within a bounded time by a mechanism that does not depend on an in-process timer in that process, and leaves no process behind.
- **FR-012**: The external termination in FR-011 is gated on ownership confirmed by PID and process start token, and never signals an unrelated process.
- **FR-013**: No engine behaviour changes; the work is confined to the gate's pre-gate step, `internal/testlife` and the sim harness's logging.

### Key Entities *(if applicable)*

- **Test record**: a per-test entry (name, start, end, outcome, elapsed) built from the JSON stream.
- **Pre-gate archive**: the per-run set of files under `.e2e-coverage/pregate/<head>.*` (JSON stream, rendered log, dump).
- **Goroutine dump**: the file of all goroutine stacks written when a `testlife` bail-out fires.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A sim pre-gate that hits its ceiling names every test that was still running and the path of a goroutine dump, with no manual re-run needed to identify the culprit.
- **SC-002**: During a pre-gate run, an operator can see at every heartbeat which tests are in flight and for how long; a test exceeding its threshold is flagged.
- **SC-003**: Every pre-gate run, passing or failing, leaves an archive under `.e2e-coverage/pregate/<head>.*` whose path is printed.
- **SC-004**: Any sim engine log line in the archive can be attributed to a single test and a time.
- **SC-005**: A passing pre-gate reports its 10 slowest tests.
- **SC-006**: A forced exit hang under `-race` leaves no test-binary process behind after the bounded grace period.
- **SC-007**: A gate unit test (fake Commander, recorded JSON stream), a `testlife` unit test (including the seam) and a local forced-hang run each demonstrate their respective acceptance behaviour.

## Assumptions

- The gate already parses a `go test -json` event stream for the live legs and the same parsing can serve the pre-gate step.
- "Last recorded duration" comes from the per-test records of previous pre-gate runs kept in the archive.
- The heartbeat interval is a few minutes and need not be configurable beyond a sensible default.
- The dump file location is next to the binary's output so the gate can find and archive it.
- The choice of mechanism for FR-011 (runner kills the binary by PID and start token after ceiling plus grace, versus a pre-armed external watchdog process) is left to the planning stage; either satisfies the requirement.
- FR-011 applies to the pre-gate/`scripts/sim/run.sh` runner path and to other `-race` binaries using `testlife`; it is not required to change live e2e behaviour.

## Out of Scope *(optional)*

- Sharding the sim suite (#2228).
- Changing the pre-gate's use of `-race` (#2248).
- Any engine behaviour change.
- Diagnosing or fixing the root cause of the 2026-10-13 slow run itself.

## Source References *(optional)*

- v0.0.84 gate, 2026-10-13: 2642s run ending at the `testlife` ceiling versus a 585s pass earlier the same day.
- #2226: bounded test-binary exit (the self-kill fallback this builds on).
- #2228: shard the sim.
- #2248: pre-gate no longer uses `-race`.
