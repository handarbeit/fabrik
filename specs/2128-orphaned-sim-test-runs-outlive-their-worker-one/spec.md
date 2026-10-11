# Feature Specification: Test binaries die with their owning run; reap residue by owner, never by process-tree shape

**Feature Branch**: `fabrik/issue-2128`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description:

```markdown
## Problem

Go test binaries started by stage workers, mostly `tests/sim`'s `sim.test`, keep running long after the run that started them is gone. They keep burning CPU and slow every other worker on the host.

Observed on the dev host, 2026-10-11 ~13:25Z, with load average ~34 on 28 cores. Four `sim.test` processes had ppid 1, no children, and no other member left in their process group:

| pid | age | %CPU | cwd |
|---|---|---|---|
| 43138 | **32 h 07 m** | 70–81 | `$TMPDIR/TestYoloRemovedMidValidateBlocksMerge_WaitForCI…/001/acme/widgets.git` |
| 56218 | 19 m | 70–80 | `$TMPDIR/TestStatusLine_Sim_GreenTrainLifecycle…/003/worktrees/acme-widgets/merge-train-main-…` |
| 25664 | 47 m | 85 | `.fabrik/worktrees/handarbeit-fabrik/issue-2097/tests/sim` |
| 49066 | 46 m | 85 | `.fabrik/worktrees/handarbeit-fabrik/issue-2097/tests/sim` |

A sim run 32 hours old is unambiguously orphaned. The existing reapers missed it:
- #1989 / ADR-1989, session-wide worker reap
- ADR-1798, session-scoped descendant reaper
- ADR-063, the cwd-rooted reaper

Its cwd is a test `t.TempDir()`, not a worktree, so the ADR-063 cwd reaper cannot match it. #1989 documents a known residual gap: "a command that orphans itself between two samples, with the worker then exiting cleanly".

**The obvious heuristic is wrong, and this is the most important finding here.** While investigating, I killed 25664 and 49066 on the strength of "ppid 1, group otherwise empty". They were very probably #2097's Validate worker's own **live background** test runs. Claude's Bash tool runs each command under `setsid()`, and a backgrounded command is reparented to init once its shell exits. So a legitimate in-use background run looks exactly like an orphan by ppid/pgid. Killing them broke #2097's Validate run.

Any fix must tell *orphaned* from *backgrounded but still owned* by something other than the process tree's shape.

## Requirements

**R1 — test binaries die with the run that owns them.** A `sim.test` (and any repo test binary that re-execs itself, for example `os.Args[0]` subprocess tests in `tests/sim/advance_until_test.go`) must exit on its own when the thing it exists for is gone. Pick the right owner signal:
- **The `go test` parent.** It is legitimately absent for a `go test -c` binary run directly, and for a background Bash run whose shell has exited. So parent death alone is not the signal.
- **An explicit owner.** The worker can mark its process tree, for example with an env var naming the worker's PID and its start token, which is the same identity `internal/sessionreap` already records (`RecordStart`, start tokens). The binary exits once that owner is provably gone.

Use the start-token check, so that a recycled PID never keeps an orphan alive and never kills a live one.

**R2 — hard ceiling.** Independently of R1, a sim test binary must never outlive a bounded wall time, even when `-test.timeout` is 0 or huge. Put a ceiling in `tests/sim`'s `TestMain`, generous enough for the slowest full run under `-race` on a loaded host, and documented. The 32-hour case must be impossible.

**R3 — the engine reaps the residue without the bad heuristic.** Extend `internal/sessionreap` and the worker-record sweep, rather than adding a new scanner. When a worker exits, or at startup sweep, every process carrying that worker's owner marker (R1's env/identity) is reaped through the existing `Escalate` and `SignalGroup` ownership checks. A process with ppid 1 and **no** owner marker is never killed on shape alone; it is logged at most. The R4 refusals of SID 0 and 1 and the caller's own SID still apply.

**R4 — no kill while the owner lives.** Nothing reaps a marked process while its owning worker is alive, whatever its ppid or session. This is the failure that broke #2097.

## Scope

- In: `tests/sim` `TestMain` (watchdog and ceiling), the worker-env plumbing for the owner marker (classify it in `internal/workerenv`, ADR-2027: engine-set, forwarded), `internal/sessionreap`, `engine/command_sessions_unix.go`, the `workers.json` sweep, docs (`docs/stage-lifecycle.md` "Kill escalation") and an ADR.
- Out: changing how the Bash tool or `go test` itself spawn processes; the e2e gate runner (`tests/gate`) unless it shares the gap.

## Acceptance

1. A `sim.test` started under a worker whose PID is then killed with SIGKILL exits within a bounded time, measured in seconds to a minute and stated in the ADR. Its cwd may be a `t.TempDir()`.
2. A background `sim.test` whose launching shell has exited, but whose worker is alive, keeps running to completion.
3. A sim binary forced past the R2 ceiling exits with a clear message naming the ceiling.
4. The engine's post-exit sweep reaps a marked orphan and leaves an unmarked ppid-1 process untouched.
5. Each test is shown non-vacuous by neutralising its mechanism.
```

## Background

Go test binaries started by stage workers, mostly the sim-bed test binary (`sim.test`), keep running long after the run that started them is gone. They burn CPU and slow every other worker on the host.

Observed on the dev host on 2026-10-11 at about 13:25Z, with load average around 34 on 28 cores: four `sim.test` processes had parent PID 1, no children and no other member left in their process group.

| pid | age | %CPU | cwd |
|---|---|---|---|
| 43138 | 32 h 07 m | 70–81 | a test temp directory (`.../TestYoloRemovedMidValidateBlocksMerge_WaitForCI…/001/acme/widgets.git`) |
| 56218 | 19 m | 70–80 | a test temp directory (`.../TestStatusLine_Sim_GreenTrainLifecycle…/003/worktrees/acme-widgets/merge-train-main-…`) |
| 25664 | 47 m | 85 | the issue-2097 worktree's `tests/sim` |
| 49066 | 46 m | 85 | the issue-2097 worktree's `tests/sim` |

A sim run 32 hours old is unambiguously orphaned. The existing reapers missed it: the session-wide worker reap (#1989 / ADR-1989), the session-scoped descendant reaper (ADR-1798) and the cwd-rooted reaper (ADR-063). Its cwd is a test temp directory, not a worktree, so the cwd reaper cannot match it. ADR-1989 documents a known residual gap: a command that orphans itself between two samples, with the worker then exiting cleanly.

**Correction from later investigation (2026-10-11 13:30).** The Validate transcript for issue 2097 (`.fabrik/logs/handarbeit-fabrik/issue-2097/Validate-20261011-122644-*.log`) shows:

- Processes 25664 and 49066 were genuine orphans, not live background runs. They started at about 12:39Z, from the worker's earlier test run, which ended with `panic: test timed out after 9m0s`. When they were killed, the worker was waiting on a different, later run (started about 13:14Z) that was left alone.
- Killing them did not end the 2097 Validate run. It hit its own 60-minute wall-time limit at 13:26:41Z (3597 s, 19 turns). Most of that time went to test commands that exceeded the Bash tool's 590 s limit under a host load of 34–41, a load the orphans added to.
- New evidence: **orphans survive a `go test -timeout` panic.** After the parent test binary timed out and exited, two `sim.test` processes from that run kept going at about 85 % CPU, sharing one process group, for 47+ minutes. The likely mechanism, to be confirmed in Research, is that re-exec'd test subprocesses (a test running its own binary as a child, for example the subprocess tests in `tests/sim/advance_until_test.go`) run with no timeout of their own. A test binary's default timeout is 0, and only `go test` passes 10 minutes. They also outlive the parent that spawned them.

**Why process-tree shape is not a safe signal.** The original report concluded that a process with parent PID 1 and an otherwise empty group may be a live background run, because Claude's Bash tool runs each command under `setsid()` and a backgrounded command is reparented to init once its shell exits. A legitimate in-use background run therefore looks exactly like an orphan by parent/group shape. In this particular case the heuristic happened to be right, but that does not make it safe for a worker's processes in general. Any fix must tell orphaned from backgrounded-but-still-owned by something other than the shape of the process tree.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A test binary exits when its owning worker is gone (Priority: P1)

An operator runs stage workers that start test binaries. If a worker dies abruptly, the test binaries it started must stop on their own instead of burning CPU indefinitely, wherever their working directory happens to be.

**Why this priority**: This is the direct cause of the 32-hour orphan and the host-wide slowdown.

**Independent Test**: Start a sim test binary under a stand-in worker that carries the owner marker, kill the worker with SIGKILL, and observe that the binary exits within the stated bound, with its working directory in a test temp directory.

**Acceptance Scenarios**:

1. **Given** a sim test binary started under a worker that carries an owner marker, **When** the worker is killed with SIGKILL, **Then** the binary exits within the bounded time stated in the ADR (seconds to a minute).
2. **Given** the owner marker names a PID that has since been recycled by an unrelated live process, **When** the binary checks its owner, **Then** the start-token mismatch is treated as "owner gone" and the binary exits.
3. **Given** the owning worker is alive and its start token matches, **When** the binary checks its owner, **Then** it keeps running.

---

### User Story 2 - A backgrounded run keeps running while its worker is alive (Priority: P1)

A worker's Bash command starts a test run in the background and the launching shell exits. The run is reparented to init and looks orphaned, but its worker is alive and waiting on it. It must not be killed by anything, and must not exit on its own.

**Why this priority**: Wrongly killing a live run breaks the owning stage, as happened during the 2097 investigation. This is the safety property that makes the fix acceptable.

**Independent Test**: Start a sim test binary in the background from a shell that then exits, keep the worker alive, and confirm the binary runs to completion and is not signalled by the engine.

**Acceptance Scenarios**:

1. **Given** a test binary whose launching shell has exited and whose parent PID is 1, **When** the owning worker is still alive, **Then** the binary runs to completion.
2. **Given** a marked process with any parent PID or session, **When** an engine sweep runs while its owning worker is alive, **Then** no signal is sent to it.

---

### User Story 3 - Re-exec'd test subprocesses are bounded and die with their parent (Priority: P1)

A test that re-executes its own binary as a child must not leave that child running after the parent test process has exited or timed out.

**Why this priority**: The newest evidence shows orphans surviving a `go test -timeout` panic. Fixing only the top-level binary leaves this path open.

**Independent Test**: Run a test that spawns a re-exec'd child, make the parent exit or time out, and confirm the child exits within a bounded time.

**Acceptance Scenarios**:

1. **Given** a re-exec'd child with no timeout of its own by default, **When** it is started by a repo test, **Then** it is started with an explicit deadline.
2. **Given** a parent test process that exits or panics on timeout, **When** its re-exec'd child is still running, **Then** the child exits within a bounded time.

---

### User Story 4 - A sim binary can never outlive a hard ceiling (Priority: P2)

Even with no owner marker, with an unlimited or huge test timeout, or when a binary is run directly by hand, a sim test binary must stop after a bounded wall time.

**Why this priority**: A backstop that makes the 32-hour case impossible regardless of how the binary was started.

**Independent Test**: Run a sim binary with the ceiling forced to a small value, and confirm it exits with a message naming the ceiling.

**Acceptance Scenarios**:

1. **Given** a sim binary with `-test.timeout=0`, **When** it runs past the ceiling, **Then** it exits with a message that names the ceiling and its value.
2. **Given** the slowest full run under `-race` on a loaded host, **When** it runs normally, **Then** it finishes well inside the ceiling.

---

### User Story 5 - The engine reaps marked residue after a worker exits, and only that (Priority: P2)

When a worker exits, and at engine startup, the engine finds any processes still carrying that worker's owner marker and stops them through the existing, ownership-checked kill escalation. Processes without a marker are never killed on shape alone.

**Why this priority**: Covers residue that the in-process watchdog cannot handle (for example a wedged binary), without reintroducing the unsafe heuristic.

**Independent Test**: Create a marked orphan and an unmarked process with parent PID 1, run the post-exit sweep, and check that only the marked one is reaped.

**Acceptance Scenarios**:

1. **Given** a worker that has exited and a process still carrying its marker, **When** the post-exit sweep or startup sweep runs, **Then** the process is reaped through the existing escalation and signal-ownership checks.
2. **Given** a process with parent PID 1 and no owner marker, **When** the sweep runs, **Then** it is not signalled; it may be logged.
3. **Given** a process in session 0, session 1 or the engine's own session, **When** the sweep considers it, **Then** it is refused as before.

---

### Edge Cases

- The owner's PID has been recycled by an unrelated process: the start-token comparison must treat the original owner as gone, and a live owner must never be mistaken for gone.
- The owner's start token cannot be read (transient failure): the binary must not conclude the owner is gone and exit on that basis alone.
- A test binary run directly by a person, or with `go test -c`, has no owner marker and no `go test` parent: only the hard ceiling applies.
- The `go test` parent is absent but the worker is alive (background Bash run): parent absence alone must not cause an exit.
- The engine restarts while workers and their marked processes are still running: the startup sweep must not reap marked processes whose owner is alive.
- Re-exec'd subprocesses inherit the owner marker and must keep the original owner, not the intermediate test process, as the owner for the R4 check.
- The host is heavily loaded (load 34–41 observed): the ceiling and the owner-check interval must not turn a slow but healthy run into a failure.
- A marked process lives in a different session or process group from the worker (the Bash tool uses `setsid()`): ownership comes from the marker, not from session or group.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A sim test binary, and any repo test binary that re-executes itself, started under a stage worker MUST exit on its own once its owning worker is provably gone. The owner is identified by an explicit marker naming the worker's PID and process start token (the identity the worker-reap code already records).
- **FR-002**: The owner check MUST compare the start token, not only the PID, so that a recycled PID neither keeps an orphan alive nor causes a live owner to be treated as gone.
- **FR-003**: Absence of the `go test` parent alone MUST NOT cause a test binary to exit; the parent is legitimately absent for a directly-run `go test -c` binary and for a background run whose shell has exited.
- **FR-004**: After the owning worker is gone, a marked test binary MUST exit within a bounded time, and that bound MUST be stated in the ADR.
- **FR-005**: Every re-exec'd test subprocess (for example the self-re-exec tests in the sim bed) MUST be started with an explicit timeout or deadline, and MUST exit within a bounded time when the test process that spawned it has exited or timed out.
- **FR-006**: The sim test binary MUST enforce a hard wall-time ceiling, independent of the `go test` timeout value (including 0 or very large values) and independent of the owner marker. The ceiling MUST be documented and generous enough for the slowest full run under `-race` on a loaded host.
- **FR-007**: When the ceiling is reached, the binary MUST exit with a clear message naming the ceiling and its value.
- **FR-008**: The engine MUST put the owner marker into the environment of every worker it starts, and the marker MUST be classified in the worker-environment classification as engine-set and forwarded to the worker, so the existing classification checks keep passing.
- **FR-009**: When a worker exits, and at the engine's startup sweep, every process still carrying that worker's owner marker MUST be reaped through the existing kill escalation and signal-ownership checks, by extending the existing worker-reap and worker-record sweep rather than adding a new scanner.
- **FR-010**: A process with parent PID 1 and no owner marker MUST NOT be killed on the basis of process-tree shape; it MAY be logged.
- **FR-011**: The existing refusals for session 0, session 1 and the caller's own session MUST continue to apply to every process considered for reaping.
- **FR-012**: No marked process MUST be signalled while its owning worker is alive, regardless of the process's parent PID, session or group.
- **FR-013**: Each behavioural test added for FR-001 through FR-012 MUST be shown to fail when its mechanism is neutralised (non-vacuity), using the repo's existing neutralisation-seam convention.
- **FR-014**: The kill-escalation documentation MUST describe the owner marker, the in-process watchdog, the ceiling and the "never reap by shape" rule, and an ADR numbered after this issue MUST record the decision and the bounds.

### Key Entities

- **Owner marker**: Environment-carried identity of the worker that owns a process tree (worker PID plus process start token), inherited by everything the worker starts, including re-exec'd subprocesses.
- **Marked orphan**: A process carrying an owner marker whose owner is provably gone.
- **Hard ceiling**: The maximum wall time a sim test binary may run, independent of test timeout and owner.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After the owning worker is killed with SIGKILL, a marked sim test binary exits within the bound stated in the ADR (seconds to a minute), including when its working directory is a test temp directory.
- **SC-002**: A background test run whose launching shell has exited and whose worker is alive runs to completion, and no engine sweep signals it during that time.
- **SC-003**: No sim test binary runs longer than the documented ceiling; a binary forced past a reduced ceiling exits with a message naming it.
- **SC-004**: After a parent test process exits or panics on timeout, none of its re-exec'd subprocesses remains running beyond the stated bound.
- **SC-005**: The post-exit and startup sweeps reap a marked orphan and leave an unmarked parent-PID-1 process unsignalled.
- **SC-006**: For each new test, neutralising the mechanism it covers makes the test fail.

## Assumptions

- The mechanism by which orphans survive a `go test` timeout panic (re-exec'd children with no timeout of their own and no parent-owned lifetime) is the working hypothesis and is to be confirmed in Research; the requirements above hold whichever way it turns out.
- The ceiling value is chosen during planning and recorded in the ADR. It must be comfortably longer than the 9-minute run that timed out during the 2097 investigation and than the slowest full `-race` run under host load of 30–40.
- A binary with no owner marker (run by hand, in CI, or via `go test -c`) is covered only by the hard ceiling; the owner watchdog is inactive without a marker.
- An unreadable owner start token is treated as "owner alive" so a live run is never killed on a transient failure; the hard ceiling remains the backstop.
- The owner marker is propagated by the existing worker-environment mechanism and is not treated as a secret.
- The e2e gate runner is only in scope if Research shows it has the same gap.

## Out of Scope

- Changing how the Bash tool or `go test` itself spawn processes.
- The live e2e gate runner, unless it is shown to share the gap.
- Killing or reaping any process without an owner marker on the basis of parent PID, group or session shape.

## Source References

- ADR-1989 / #1989 (session-wide worker reap, known residual gap), ADR-1798 (session-scoped descendant reaper), ADR-063 (cwd-rooted reaper), ADR-2027 (worker-environment classification)
- Validate transcript for issue 2097: `.fabrik/logs/handarbeit-fabrik/issue-2097/Validate-20261011-122644-*.log`
- Self-re-exec tests in the sim bed: `tests/sim/advance_until_test.go`
