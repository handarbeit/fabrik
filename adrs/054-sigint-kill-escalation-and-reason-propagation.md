# ADR 054 — SIGINT→SIGTERM→SIGKILL escalation gradient and kill-reason propagation

## Status
Accepted

## Context

Before this change, Fabrik killed Claude subprocesses with a single immediate SIGKILL whenever a stage exceeded its `max_wall_time` or the inactivity timeout fired. This was safe but gave CI test-runner wrappers (the primary real-world user of `FABRIK_STAGE_COMPLETE`) no opportunity to post a final Commit Status before the process group was destroyed. Such wrappers catch SIGINT, post a "cancelled" status to GitHub, then exit — a pattern that requires a grace window.

Additionally, all kill events produced identical log output, making it impossible to distinguish between a max_wall_time kill, an inactivity kill, a daemon shutdown, and a "supplanted by new invocation" cancellation from post-mortem logs.

## Decision

### 1. Three-signal escalation gradient

`killProcGroupGraceful` replaces the previous unconditional SIGKILL. The sequence is:

```
SIGINT  → sleep(sigintGrace)  → liveness probe
SIGTERM → sleep(sigtermGrace) → liveness probe
SIGKILL
```

Each step is skipped if the grace duration is zero (see §3). Each step logs a structured line including the signal name and the kill reason (see §4). An `ESRCH` liveness probe (process group already gone) short-circuits the sequence immediately.

The unconditional `killProcGroup` (grandchild cleanup SIGKILL) that fires after `cmd.Wait()` returns is kept: it cleans up any grandchildren that survived the Claude subprocess exit.

### 2. Configurable grace windows

Two defaults are set at startup in `engine.New()`:

```go
claudeKillGraceSigInt  = cfg.KillGraceSigInt   // default 10s if zero
claudeKillGraceSigTerm = cfg.KillGraceSigTerm  // default 10s if zero
```

These can be overridden globally via `--kill-grace-sigint` / `--kill-grace-sigterm` flags (or `FABRIK_KILL_GRACE_*` env vars).

Per-stage overrides are expressed in stage YAML:

```yaml
kill_grace:
  sigint: 10s    # "" → inherit engine default; "0s" → skip SIGINT step
  sigterm: 10s
```

The sentinel values in `InvokeOptions`:
- `SigIntGrace == 0` → use engine default (`claudeKillGraceSigInt`)
- `SigIntGrace < 0` → skip SIGINT step entirely
- `SigIntGrace > 0` → use this exact duration

Stage YAML translates: empty raw field → sentinel 0 (inherit); raw "0s" → sentinel -1 (skip).

### 3. Why `SigIntRaw` / `SigTermRaw` sentinels are needed

YAML unmarshals "0s" as `time.Duration(0)`, which is the same as the zero value for an absent field. `SigIntRaw string` preserves whether the author wrote "0s" (explicit skip) vs. omitted the field (inherit engine default). The raw string is compared in `engine/item.go` to resolve the correct sentinel before populating `InvokeOptions`.

### 4. Kill-reason propagation via `killReasonHolder`

Kill reasons need to reach `cmd.Cancel` — a closure that captures context and runs inside Go's exec machinery, not in the issue-dispatch call stack. The chosen mechanism is a mutable atomic cell stored in the issue's context:

```go
type killReasonHolder struct{ val atomic.Value }
type issueCtxEntry   struct{ cancel context.CancelFunc; holder *killReasonHolder }
```

A `killReasonHolder` is created per-issue at dispatch time and stored in the issue context via `context.WithValue`. Writers call `holder.val.Store("reason_string")` before cancelling the context. `cmd.Cancel` reads the stored value and passes it to `killProcGroupGraceful`.

Reason codes: `max_wall_time`, `inactivity_timeout`, `supplant_by_new_invocation`, `daemon_shutdown`, `user_stop`, `context_cancel` (fallback).

### 5. Per-issue context map (`issueCtxs sync.Map`)

The engine maintains `issueCtxs sync.Map` (keyed by `issueKey`) holding `issueCtxEntry` values. This enables two operations that cannot happen from within `InvokeClaude`:

- **Daemon shutdown**: the poll loop iterates `issueCtxs` and stores `"daemon_shutdown"` in every holder before cancelling the root context.
- **Supplant-by-new-invocation**: when the board shows a different worker has claimed an issue, the dispatcher stores `"supplant_by_new_invocation"` in the holder and cancels the per-issue context, cleanly stopping the in-flight Claude session.
- **TUI manual stop**: `handleStopRequest` stores `"user_stop"` in the targeted issue's holder and cancels its per-issue context; the worker exits via the normal SIGTERM→SIGKILL escalation path; `fabrik:paused` + `fabrik:awaiting-input` labels are applied and a stop comment is posted on the issue.

Entries are deleted from the map with a `defer` immediately after the per-issue goroutine returns, so the map never accumulates stale entries.

### 6. Why concurrent dispatch is not changed

The `supplant_by_new_invocation` path simply cancels and yields to the next poll cycle, where the newly assigned worker will be dispatched. This is intentional: concurrent dispatch would require coordinating two Claude sessions against the same worktree, which is unsafe for write stages.

## Alternatives considered

**Single SIGKILL with pre-kill hook callback**: simpler, but the hook would run in Go rather than in the subprocess, defeating the purpose of letting the subprocess (e.g. a shell wrapper) catch the signal and do its own cleanup.

**Reason codes via a separate channel or shared struct**: a channel requires the sender to block until the receiver is ready; a shared struct needs a lock. `atomic.Value` is the lightest-weight solution: lock-free, no blocking, and the value is valid for the lifetime of the issue goroutine.

**Named context key (string) vs. unexported struct**: using a private struct type (`killReasonCtxKey{}`) prevents external packages from accidentally shadowing or reading the value, following the Go convention for context keys.
