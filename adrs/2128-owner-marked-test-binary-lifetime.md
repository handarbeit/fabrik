# ADR 2128: Reap by owner, never by process-tree shape — owner-marked workers and bounded test binaries

## Status

Accepted (#2128). Extends ADR-1989 (session-wide worker reap), ADR-1798 and ADR 063; uses ADR-1957's ownership-check pattern and ADR-2027's worker-env classification.

## Context

`sim.test` processes started by stage workers kept running long after the run that started them: one ran 32 hours, four ran at ~80% CPU each with the host at load 34 on 28 cores. They had ppid 1, no children and no other member left in their group.

The existing reapers all missed them:

- ADR-1989 / ADR-1798 key on session ID. A command session is only known if a 2-second sample saw its shell as a live descendant of the worker; ADR-1989 records the residual gap ("a command that orphans itself between two samples, with the worker then exiting cleanly").
- ADR 063 matches by cwd, and a sim run's cwd is a `t.TempDir()`, not a worktree.
- Orphans created **during** a live worker are deliberately never reaped (ADR-1989 §7).

A second mechanism was found in the issue discussion: `tests/sim/advance_until_test.go` re-execs its own test binary (`exec.Command(os.Args[0], …)`). `go test` passes `-test.timeout` only to the binary it starts, so the re-exec'd helper had none, and after the parent `sim.test` died on a `go test -timeout` panic the helpers ran on with no bound.

**The obvious heuristic is wrong.** "ppid 1 and the group is otherwise empty" cannot tell an orphan from a live background run: Claude's Bash tool runs each command under `setsid()`, and a backgrounded command is reparented to init once its shell exits. Killing on that shape is how a live Validate run was damaged during the investigation of this issue. Any fix has to separate *orphaned* from *backgrounded but still owned* by something other than the shape of the process tree.

## Decision

Ownership is established by **positive evidence carried by the process**, and a process with no such evidence is never killed.

### 1. The owner marker

`runClaude` allocates an owner-file path under `.fabrik/state/worker-owners/` and sets `FABRIK_WORKER_OWNER=<that path>` in the worker's environment (`internal/workerenv.OwnerVar`, classified `EngineSet`, hence `Protected`; `buildClaudeEnv` also emits the bare removal sentinel so a worker never inherits its parent's marker). The worker's PID does not exist when the environment is built, so the engine writes `{pid, start_token}` into the file immediately after `cmd.Start()` (`internal/workerowner`, atomic write, mode 0600). The start token is `sessionreap.StartToken`, the same function `Tracker` and `RecordStart` use. Every descendant inherits the variable — `go test`, `sim.test`, its re-exec'd helpers — whatever its session, group, parent or cwd.

The marker is also stored on the worker's `workers.json` record (`Owner`), so the janitor reaches it after an engine restart.

Rejected: a literal "PID in the env" (the PID is unknown until after `Start`); the engine's own PID as owner (ties a worker's test binaries to the engine's lifetime rather than the worker's); finding the worker by walking the PPID chain (the chain is already broken for a backgrounded run whose shell exits).

### 2. Test binaries die with their owner (`internal/testlife`)

`testlife.Guard`, called first in `tests/sim`'s new `TestMain`, starts three independent mechanisms:

- **Ceiling (R2).** After `SIM_TEST_CEILING` (default **30 minutes**) the binary prints `tests/sim: test binary exceeded its hard wall-time ceiling of <d> (override with SIM_TEST_CEILING); exiting` and exits with status 2. It ignores `-test.timeout`, so a value of 0 or a huge one cannot defeat it. Sizing: the ceiling bounds a runaway, and an hour of a runaway `sim.test` at ~80% CPU is the host-load damage this issue is about, so it is no longer than it needs to be. `-parallel 4` was measured at ~661 s (~11 min) on the 28-core host, CI's full suite takes ~7–8 min, and `scripts/sim/run.sh` allows `SIM_TIMEOUT=20m`, so an ordinary `-test.timeout` panic still fires first and keeps its goroutine dump; 30 minutes is clear of all of that and makes a 32-hour run impossible. `SIM_TEST_CEILING` remains the override for a deliberately long run.
- **Owner watch (R1).** When `FABRIK_WORKER_OWNER` is set, the guard resolves the owner file (retrying for up to 30 s, since the engine writes it just after `Start`), then polls the recorded identity every **5 s**. The owner is *gone* when its PID is absent, or alive with a **different start token** (a recycled PID). It is never inferred from the parent: a `go test -c` binary run directly, or a background run whose shell has exited, legitimately has ppid 1, and a live worker with a gone shell is untouched. **Exit latency after SIGKILL of the worker is therefore at most the 5 s poll plus exit time — seconds, well under a minute.**
- **Parent watch.** A helper started through `testlife.ReExecCommand` gets `SIM_TEST_PARENT=<pid>:<start token>` of its parent, an explicit `-test.timeout`, a context deadline and its own ceiling a little past that timeout. It exits when the parent is gone (polled every 1 s). The parent watch exists only for helpers the test itself spawns; it is not applied to a top-level binary.

Every ambiguity — no marker, no file yet, an unreadable file, an unreadable start token, a platform without start tokens — means *keep running*. A false exit is the failure that damaged a live run; the ceiling is the backstop for every ambiguous case.

### 3. The engine reaps by marker, only when the owner is provably gone

`sessionreap.ReapMarked` lists processes, reads each one's initial environment (`sessionreap.Environ`: `kern.procargs2` on macOS, `/proc/<pid>/environ` on Linux, a stub elsewhere) and matches the exact `FABRIK_WORKER_OWNER=<value>`. An unreadable or redacted environment is "not marked". It sends SIGTERM, then SIGKILL after a grace window, **to exact PIDs**, re-verifying immediately before each signal that the start token is unchanged and the marker is still present, and applying the existing refusals (`CheckSID` and `CheckPGID`: SID/PGID 0 and 1 and the caller's own; never its own PID).

It is called from the two places that already reap dead workers' sessions — `sweepWorkerSessionAtInvocationEnd` and the periodic `sweepOrphanedWorkerSessions` (which also serves as the startup sweep, since it reads `workers.json` across restarts) — **only for records `classifyWorker` reports as dead or recycled**. A live or inconclusive worker's marked processes are never reached (R4). A marker alone never authorises a kill: the worker record must independently say the owner is gone. A record is pruned (and its owner file removed) only once its session *and* marked set have been empty on two consecutive scans; the janitor also removes owner files that no record references and that are older than 24 h.

A process with ppid 1 and **no** marker is never signalled on shape alone.

`ReapMarked` signals exact PIDs rather than going through `Escalate` (whole sessions) or `SignalGroup` (a whole group). This is a deliberate deviation from the literal wording of the requirement: marker membership is the precise set, and a session or group can contain unmarked processes. It reuses the same refusals and the start-token re-check that those entry points are built on.

## Consequences

- A marked orphan is now found whatever its session, parent or cwd, so the ADR-1989 "orphans itself between two samples" gap is closed for marked processes.
- **Residual gaps.** (a) macOS returns only a stub environment for SIP-protected platform binaries, so a system `sleep` or `git` child is invisible to the marker; Go test binaries — the actual targets — are readable, and session discovery remains the cover for the rest. (b) A process started with a scrubbed environment carries no marker. (c) Orphans of workers that predate this change have no record `Owner`. For test binaries, the ceiling bounds (b) and (c).
- **Adoption is `tests/sim` only in this change.** `internal/testlife` is a shared package, but `engine`'s `TestMain` has modes that deliberately build orphans, and a parent watch there would change what those tests prove. The `engine/*_test.go` re-exec sites (`grandchild_test.go`, `descendant_reap_unix_test.go`, `shutdown_test.go`, `sentinel_probe_unix_test.go`) are a recorded follow-up. They are still covered by the engine-side marker reap, since they inherit the marker. `tests/gate` reaps through `sessionreap` and has no gap.
- Reading every process's environment is more expensive than one `Getsid` pass. It runs only when a dead or recycled record carries an `Owner`, once per sweep pass for all owners together.
- `sessionreap` is shared with Pruefer and the gate runner; every addition is additive and has a Windows or other-platform stub.
- Each mechanism has a neutralisation seam (`Options.IgnoreMarkers`, `sessionReapIgnoreMarkers`, `testlife.Options.Disable{Ceiling,OwnerWatch,ParentWatch}`) and a twin test that fails when the mechanism is switched off.
