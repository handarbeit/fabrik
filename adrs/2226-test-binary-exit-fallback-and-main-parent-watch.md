# ADR 2226: A test-binary bail must actually end the process — self-kill fallback and a main-binary parent watch

## Status

Accepted (#2226). Amends ADR 2128 §2 (owner watch "never inferred from the parent" and parent watch "not applied to a top-level binary").

## Context

In the v0.0.84 release gate the `internal/testlife` ceiling fired (message printed, `go test` reported `FAIL tests/sim 1800s`), but the `sim.test` process did not exit. It was reparented to launchd (ppid 1) and spun at ~97% CPU in `sched_yield` for 8 hours. The cause is probably a spin in the race runtime's exit path while other goroutines are live, but is unconfirmed. Two things failed: the shared `bail` trusted `os.Exit` to return control to the OS, and nothing noticed that the `go test` that started the binary was gone, because the parent watch covered only `ReExecCommand` helpers.

## Decision

1. **Self-kill fallback.** `bail` arms `time.AfterFunc(KillGrace, kill)` inside its `sync.Once`, *before* printing the message and calling `exit`. `kill` is `os.FindProcess(os.Getpid()).Kill()` (SIGKILL on unix, `TerminateProcess` on Windows, no build tags) and so can only ever target the guard's own process. The grace is a fixed 5 s, not operator-configurable. If `exit` returns (only a test fake does) the timer is stopped, so in-process tests with a returning fake `Exit` never kill the test binary. The ceiling, owner watch and both parent watches all go through `bail`, so one mechanism covers all of them. `Options.Kill`, `Options.KillGrace` and `DisableKillFallback` are the seams.
2. **Main-binary parent watch.** `Guard` also watches `os.Getppid()` (PID plus start token captured at install) through the existing `watchIdentity`, so a recycled PID is gone and an unreadable identity keeps the binary running. It is not armed when: `ParentEnv` names an explicit parent (that watch applies), `FABRIK_WORKER_OWNER` is set (the owner watch is then the authoritative lifetime), the ppid is 1 or less, or no start token can be read. Same 1 s poll as the helper parent watch. Seams: `Options.Getppid`, `DisableMainParentWatch`.

## Why this amends ADR 2128

ADR 2128 refused to infer death from process-tree shape because killing on ppid 1 damaged a live Validate run. The new watch is not shape-based: it compares the *original* parent's PID and start token, and the owner-marker exemption keeps the ADR 2128 case unchanged (a worker-launched binary whose shell exited, with its worker alive, keeps running; `TestGuard_SurvivesLaunchingShellWhileOwnerLives`). What changes is the unmarked case: a hand-run background binary now ends when its launching shell exits, which is "its run is over".

## Alternatives rejected

- Arm the main parent watch only when `-test.paniconexit0` is in `os.Args`: fragile, and narrower than the intended meaning.
- An external killer process: adds PID-reuse and portability problems for no gain over a goroutine.
- A configurable grace: a few seconds is enough that a healthy exit is never preempted.

## Residual risk

If the hang stalled the Go scheduler as a whole, the watchdog goroutine would not run. The observed spin was a single thread in `sched_yield`, so other Ms should still run the timer, but this cannot be verified without reproducing the hang. SIGKILL also loses buffered output and a goroutine dump; the ceiling message has already been printed by then.
