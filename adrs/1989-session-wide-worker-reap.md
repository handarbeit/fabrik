# ADR 1989: Reap a killed worker's whole session, not just its process group

## Status

Accepted (#1989).

## Context

When the engine stopped a Claude worker, the worker's `go test` → `sim.test` trees could survive and run on, orphaned, for hours (12 orphaned `sim.test` processes at ~94% CPU on 2026-09-30, load ~30, timing out the sim pre-gate).

`setCmdProcAttr` starts the worker with `Setsid` (#1798), so the worker's PID is both its process-group ID and its session ID. The stop path, `killProcGroupGraceful`, signalled `-pid` — the worker's **process group** only. Claude's Bash tool starts each command in a process group of its own within the same session, so those children were outside the group kill.

#1798 (`reapTrackedDescendants`) and #1814 (the registry-independent worker-session sweep) already SIGKILL session members, but only per PID, only with SIGKILL, and only at invocation end — so such a child never received SIGINT/SIGTERM (no grace, e.g. for test runners flushing Commit Statuses), and it was reaped only if `runClaude` reached its tail and those sweeps' `ps` calls and fingerprint checks succeeded (they fail open by design, which is likelier on a loaded host).

### R1 — recorded outcome (against unmodified code)

`engine/session_reap_unix_test.go` (commit "test(engine): R1 reproduction"):

- **Stop path in isolation** (`killProcGroupGraceful`): a same-session, separate-process-group child was still alive afterwards and had received neither SIGINT nor SIGTERM.
- **Real `max_wall_time` stop through `InvokeClaude`**: the child likewise received no graceful signal; it was SIGKILLed only at invocation end by the #1798/#1814 sweeps.

So the mechanism holds in the report-worthy middle form — *survives the stop path, caught later by the invocation-end sweep* — not "never reaped". The 2026-09-30 orphans therefore needed those later sweeps to fail: load-induced fail-open of their `ps` probes, an engine killed before the tail, a daemon binary predating #1814, or a missing worker record. This change removes the dependence on all of those for the common case.

**Unverified assumption:** the tests use synthetic children (`set -m` backgrounding). Whether real Claude Code's Bash tool shells share the worker's session (rather than calling `setsid()` themselves, as Node's `detached: true` does) was not confirmed against a live worker. ADR-1814 states Bash-tool shells are reaped by SID, which favours same-session. Confirming with `ps -o pid,pgid,sess` from a live worker's Bash tool is the check; if those shells have their own session this change does not reach them.

## Decision

1. **New leaf package `internal/sessionreap`**, shared by `engine` and `pruefer` (neither may import the other; log output goes through a caller-supplied `Logger`). This overrides ADR-1113 §6's duplication choice for the session logic; the small `setCmdProcAttr`/`killProcGroup`/`isProcessAlive` copies in `pruefer` remain.
2. **Enumeration by session ID**: one process listing (`/proc` on Linux, `sysctl kern.proc.all` on macOS, `ps -axo pid=` elsewhere) plus `unix.Getsid` per entry. No `ps` session column (reads 0 on macOS) and no per-candidate `ps` fingerprint subprocess — the call that fails open at load.
3. **`Escalate`** applies today's SIGINT → grace → SIGTERM → grace → SIGKILL, with unchanged durations and zero-grace semantics, to every session member including the leader. The liveness probe is "any member alive", and a grace window ends early when the session empties. `killProcGroupGraceful` keeps its signature and delegates to it. `killProcGroup` stays group-only (the webhook subprocess uses it and has no grace escalation; out of scope).
4. **`Sweep`** is the post-exit step: SIGKILL any remaining member — but if a live process has PID == SID (after the leader exits that can only be a recycled PID that became a session leader itself, so members carrying that SID value may belong to its session) it refuses the whole sweep and logs why, leaving that case to ADR-1814's start-time-bounded sweep — with a brief rescan for a fork that raced the first pass. It runs in `runClaude` after `killProcGroup` and **before** `reapTrackedDescendants` and `sweepWorkerSessionAtInvocationEnd`, which are left untouched as the backstop (ADR-1814's additive stance; its warning against re-adopting "the registry knows every descendant" stands). Running first makes its count the truthful "left running" signal and avoids double-logging.
5. **No fingerprint check for a live worker.** The worker is the process the engine started and holds a `cmd.Process` handle for, so its PID — hence its SID — cannot be recycled while it is live. Post-exit PID-reuse discrimination stays with ADR-1814's fingerprint-based sweeps.
6. **R4 safety**: refuse (and log) SID 0, 1 and the caller's own SID; re-check `Getsid(pid) == sid` immediately before each signal; a process-listing error degrades to the leader's process group (pre-#1989 behaviour), never wider.
7. **R5 visibility**: one line per signal step and per sweep with the member count and truncated command names. A non-zero count with `exit=clean_exit` is the signal that a worker left work running.
8. **Pruefer** starts Claude with `Setsid` instead of `Setpgid` (also a group leader, so group-kill semantics are unchanged), uses the shared `Escalate`, and runs `Sweep` after `Wait`. No worker records or janitor were added to Pruefer.

## Consequences

- A worker's `go test` trees receive SIGINT/SIGTERM and then SIGKILL on every stop path, and a post-exit sweep reaps leftovers on clean exits, without depending on `ps` probes succeeding under load.
- Three SID-based killers now coexist; a duplicate SIGKILL is harmless, and the sweep running first keeps counts and logs consistent.
- **Known gap**: a descendant that calls `setsid()` itself leaves the session and is not reached; the cwd-rooted worktree-teardown reaper (ADR 063) is the partial cover.
- Tests carry neutralised twins (`sessionEscalateFn`/`sessionSweepFn` swapped for the group-only behaviour) asserting the R1 behaviour, so the acceptance assertions are not vacuous.
- The helper API (SID in, refusals, re-check) is reusable by #1957 (group-ownership verification before stored-PID kills).
