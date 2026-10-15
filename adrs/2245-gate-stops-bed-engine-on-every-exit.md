# ADR 2245: The gate stops the bed engine on every exit

## Status

Accepted (#2245). Builds on ADR-1994 (Go gate runner), ADR-1976 (multi-bed), ADR-1957 (ownership-checked signalling) and ADR-1393 (engine drain on the first signal, force-quit on the second).

## Context

The gate stopped the bed's engine only in the preflight (before a rebuild) and in the multi-bed scheduler (`stopIdleEngine`). A gate whose suite failed, whose watchdog fired or that was cancelled left the engine polling: in the v0.0.84 gate pids 90575 and 95267 outlived their invocations, one for 13 hours, spending the bed identity's GraphQL budget and acting on the test board.

The engine left behind is almost never the one `StartBed` launched. Each leg's `TestSwitchTrainMode` stops it and the harness starts a successor (`Setpgid`, child of `go test`). A PID recorded at `StartBed` is stale after leg 1.

## Decision

1. **One teardown, deferred in `Gate.run`**, registered just before the bed is first mutated, so every later return path (success, suite failure, watchdog, failed start, cancellation, panic) runs it, and a pre-gate or precondition failure still spends and touches nothing (ADR-1454). It uses no cancelled context and is bounded by counted waits. A multi-bed run tears down each bed view in parallel.
2. **Ownership by exclusion, not by recorded PID.** The gate snapshots the lock-holder (pid + start token) that existed before it touched the bed. At exit the engine in `fabrik.lock` is the gate's only if it is live, has a readable start token different from the snapshot, and still holds the lock's flock. The flock probe rejects a stale pid that an unrelated process recycled. Nothing is persisted; a crashed gate leaves the next invocation's snapshot to treat the orphan as foreign, which is the safe side.
3. **Ladder:** SIGINT, wait `StopWait`; second SIGINT (force-quit), wait `KillGrace`; `sessionreap.SignalGroup` SIGTERM then SIGKILL, `KillGrace` each. `SignalGroup` rather than `Escalate`: `Escalate` is session-keyed and is a no-op for the harness's `Setpgid` engine, whereas `SignalGroup` serves both launch paths and re-checks pid + start token (ADR-1957). Signals before the last stage go to the engine's PID alone so its drain and force-quit semantics apply to the engine, not its workers.
4. **One log line per bed**, and the exit code is never changed; a failed stop warns.
5. **A `Procs` seam** (`Gate.Procs`) covers liveness, signalling, start tokens and the flock probe, which are direct syscalls outside the `Commander`. Tests use a scripted fake.

## Consequences

- `PreflightBed`/`StopBedInstance` keep stopping whatever holds the lock (SIGTERM) because a rebuild needs a stopped bed; that is a preflight requirement, not an exit-time one, and R2 applies to the exit path only.
- Multi-bed cancel: `runOne` releases identities before the root teardown runs. The gate is exiting, so no new cell can use them; this deviates from ADR-1976's idle-stop ordering only on the cancel path.
- Residual gaps: SIGKILL of the gate cannot run a `defer`; an engine an operator starts by hand in the bed during a run looks like the harness's and is stopped; under `E2E_SKIP_PREP` a pre-existing engine that no leg replaced is left running, with the reason logged.
