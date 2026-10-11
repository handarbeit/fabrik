# ADR 1805: Bound the macOS `lsof` worktree reap

**Date**: 2026-10-11
**Status**: Accepted
**Issue**: #1805 — reaper: bound the macOS lsof worktree-reap call with a timeout, and cut its per-call cost
**Amends**: ADR 063 (macOS enumeration paragraph and its TOCTOU note)

## Context

`reapWorktreeProcessesDarwin` (`engine/reaper_unix.go`) ran `lsof -a -d cwd -n -P -Fpcn` with no context and no timeout, on every worktree teardown. Two costs were observed during #1798:

- It can block cleanup indefinitely. A full `go test -race ./engine/...` run timed out at 300s with a goroutine blocked in exactly this call. In production the blast radius is wider than one cleanup: `CleanupWorktree`, `ensureTrainWorktreeFromRef` and `CleanupTrainWorktree` call the reaper while holding the per-repo worktree mutex (`wm.mu`), so a wedged `lsof` stalls every `EnsureWorktree`, `PushBranch` and cleanup for that repo.
- Each call is expensive: `lsof` children sat at 98–99% CPU for 2–4s each during a sim-suite run.

## Decision

### R1 — bound the call

`lsof` runs through `exec.CommandContext` under `context.WithTimeout(worktreeReapLsofTimeout)`, **10s**. On timeout the process is killed, partial output is discarded, one warning is logged — `warn: worktree process reap: lsof timed out after <timeout> for <worktree>; skipping reap` — and the reap is skipped. The caller's worktree removal proceeds as before.

- The timeout is detected from `ctx.Err()`, not from the exit error, which is a generic `signal: killed`.
- The warning is distinct from the existing `lsof failed` warning (missing binary, permission failure).
- `lsof` does not fork, so killing the direct child suffices; no process-group machinery is used.
- 10s rather than `sentinelProbeTimeout` (3s): `lsof` measured ~0.35s on an idle host and 2–4s under load (#1798). 3s would skip the reap in exactly the loaded case that motivated this issue, while the call is made under `wm.mu`, so it still has to be finite. 10s covers the observed range 2.5×.
- The value is a package var so tests can shorten it. There is no config, flag, env var or label.

A skipped reap on a very loaded host can leave an orphan. The warning is the signal; the session-scoped reaper (ADR 1798/1989) remains the primary defence.

### R2 — per-call cost: keep `lsof`, add a test seam

Measured on macOS arm64 (~1,800 processes, idle host):

| Operation | Time |
|---|---|
| `lsof -a -d cwd -n -P -Fpcn` | ~0.35s (3 runs: 0.35, 0.36, 0.35) |
| `ps -eo pid=,args=` | ~0.085s |
| in-process `proc_info(PROC_PIDVNODEPATHINFO)` over all PIDs from `kern.proc.all` | ~3ms |

Adopted: a **test seam**. `SetWorktreeReapLsofDisabledForTest` makes the macOS enumerator return before spawning anything. The engine package `TestMain` and the sim bed (`NewEnv`) turn it on, so the ~50 engine test files and every sim scenario that tears down a worktree no longer spawn a real `lsof`. The real-path reaper tests re-enable it for their own duration, so "kills a process whose cwd is under the worktree" keeps coverage. The production per-call cost is unchanged by design.

Rejected:

- **Skip the reap when no worker or session is known live.** The reaper exists for orphans left after the worker and its sessions are gone; Done teardown and the janitor run only then. The gate would disable the reap in its target cases.
- **Narrow `lsof`.** `-d cwd` is already the narrowest selector; per-PID `lsof -p` multiplies spawns.
- **In-process `proc_info` lookup** (deferred, not rejected on merit). A raw `syscall.Syscall6(SYS_proc_info, PROC_INFO_CALL_PIDINFO, pid, PROC_PIDVNODEPATHINFO, …)` reads a process's cwd from the `proc_vnodepathinfo` buffer in microseconds, works with `CGO_ENABLED=0`, uses `kern.proc.all` as the PID source (as `internal/sessionreap` does), and matched `lsof`'s coverage (1,038 readable PIDs vs 1,040). It would be ~100× cheaper, remove the hang source, and allow a Linux-style cwd re-check before the kill (closing ADR 063's macOS TOCTOU). It was not adopted because the syscall number and struct layout are Apple's undocumented ABI called through libc's deprecated `syscall()` trampoline, CI is Linux-only so a breakage would surface only on developer Macs, and it needs a darwin/non-darwin file split. That fails the issue's "cheap *and safe*" bar for now. If `lsof` cost or timeouts become a real problem, this is the path.

### R3 — Linux parity

`reapWorktreeProcessesLinux` makes no exec call: it uses `os.ReadDir("/proc")` and `os.Readlink`. There is no process to hang and no timeout to add. No change.

## Consequences

- Worktree teardown can no longer block on `lsof` for more than 10s.
- A timed-out reap is skipped with a warning; the orphan, if any, is not killed by this path.
- ADR 063's macOS paragraph is unchanged otherwise (still `lsof -d cwd`, TOCTOU still accepted).
