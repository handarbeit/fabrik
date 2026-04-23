# ADR 029: Proactive Kill Timeouts for Stuck Claude Invocations

## Status

Accepted

## Context

Claude Code invocations can hang indefinitely. Two incident classes have been observed:

1. **Grandchild pipe hold** (ADR 005, issue #429): A background process spawned by Claude (e.g., `tail -f` via Monitor, `sleep` in a test loop) inherits the stdout pipe write-fd and keeps it open after the Claude process exits. ADR 005's `WaitDelay` mechanism bounds this: after Claude exits, `cmd.Wait()` caps the pipe-drain delay at `claudeWaitDelay` (default 30s) and returns `exec.ErrWaitDelay`. The buffered output (complete, since Claude wrote its final result line before exiting) is then parsed normally.

2. **Stuck Claude process** (graphiti#26 and class of related incidents): The Claude CLI process itself hangs — never exiting, producing no output for an extended period. Causes include: model-side infinite loop, stuck network request, deadlocked tool invocation, or a stage whose prompt creates a runaway session. `WaitDelay` is ineffective here because it only fires *after* Claude exits; it provides no bound on a process that never exits.

These are distinct failure modes. ADR 005 addresses the grandchild pipe problem passively (drain-and-discard). This ADR addresses the stuck-process problem proactively (kill with recovery).

## Decision

Add two proactive kill mechanisms to `runClaude()` in `engine/claude.go`:

1. **`max_wall_time` per-stage field**: a Go duration string in stage YAML that caps the total wall-clock runtime of a single Claude invocation via `context.WithTimeout`.

2. **Inactivity timeout (hardcoded 15 minutes)**: a watchdog goroutine that kills the process if no stdout bytes have been received for 15 consecutive minutes, regardless of whether a `max_wall_time` is configured.

Both use the SIGTERM→10s grace→SIGKILL sequence targeting the entire process group.

After either kill, the already-buffered output is scanned for `FABRIK_STAGE_COMPLETE` in intermediate `{"type":"assistant"}` NDJSON lines so completed work is not re-run.

## Rationale

### Why two mechanisms?

`max_wall_time` is opt-in per stage and requires the operator to reason about expected stage durations. It catches the case where a stage runs longer than ever expected. The inactivity timeout is a no-configuration backstop: even a stage with no `max_wall_time` is bounded against processes that produce occasional output but never complete a turn.

### Why proactive kill instead of passive drain?

`WaitDelay` (ADR 005) is passive — it only fires after the Claude process exits. A process that never exits requires a different approach: the engine must actively decide to end the invocation. The `context.WithTimeout` approach reuses Go's existing cancellation machinery; the inactivity watchdog handles the case where the process is still writing (slowly) but will never terminate.

### Why SIGTERM→10s→SIGKILL (not immediate SIGKILL)?

Claude Code may have staged partial file edits, open worktree file handles, or in-progress git operations. SIGTERM gives it a 10-second window to flush and clean up. If it does not exit within 10 seconds, SIGKILL is sent unconditionally. This is the same sequence used by most process supervision systems and is a well-understood contract.

### Why target the process group?

Claude spawns grandchild processes via `run_in_background: true` Bash tool calls (test runners, polling loops) and Monitor tool invocations (`tail -f`, `watch`). Sending the kill signal to the negative PID (`syscall.Kill(-pid, sig)`) targets the entire process group — Claude plus all descendants. This prevents grandchild processes from holding the stdout pipe open after the kill, which would otherwise cause `cmd.Wait()` to block until `WaitDelay` fires (adding 30s to every timeout recovery).

### Why scan the buffer post-kill? (R9: stream marker detection)

A stage that signals completion — then hangs in a post-completion loop — should not be re-run. The Claude CLI emits NDJSON per turn: each assistant turn is a `{"type":"assistant",...}` line with a `content` array. If Claude wrote `FABRIK_STAGE_COMPLETE` in a completed turn before hanging, that text is already in the buffer. `extractTextFromAssistantTurns()` scans the buffer for assistant turns and concatenates their text blocks; if `FABRIK_STAGE_COMPLETE` appears, `completed=true` is returned and the same completion flow runs as for a live marker. Without this scan, a timed-out stage that actually completed would be re-invoked — wasting time and potentially duplicating side effects.

### Why not re-run the stage immediately on timeout?

A timed-out invocation without `FABRIK_STAGE_COMPLETE` follows the same cooldown/retry path as any no-marker run. This gives the operator time to investigate (check the stage comment for partial output, inspect the worktree) before the engine retries. The `wasTimedOut` flag distinguishes our kills from engine-shutdown context cancellation so that only timeout kills enter the retry path, not graceful shutdowns.

### Why hardcode 15 minutes for the inactivity timeout?

15 minutes is chosen to exceed any expected per-turn latency (including reasoning + tool calls + model response) while bounding the worst-case hang to under an hour. It is configurable via the `claudeInactivityTimeout` package variable for tests. If experience shows 15 minutes is too tight, it can be made configurable via an env var; for now, the `max_wall_time` field provides per-stage control for operators who need tighter bounds.

### Why is `cmd.Cancel` used for `max_wall_time` (not direct kill)?

Go 1.20+ allows overriding `cmd.Cancel` — the function called when the context is cancelled. The default `cmd.Cancel` sends SIGKILL to the process (not the process group). Overriding it lets us run the SIGTERM→10s→SIGKILL graceful sequence while still using `context.WithTimeout` for deadline management. This keeps the kill logic in one place (`killProcGroupGraceful`) shared between both mechanisms.

## How It Works

```
runClaude()
│
├── stageCtx = context.WithTimeout(ctx, maxWallTime)   // if maxWallTime > 0
│
├── cmd.Cancel = func() {                               // called on stageCtx deadline
│     killProcGroupGraceful(pid, ...)
│   }
│
├── cmd.Start()
│   pid := cmd.Process.Pid
│
├── watchdog goroutine(pid):
│   │  timer = 15min
│   │  on timer fire:
│   │    if idle >= 15min: inactivityFired=true; killProcGroupGraceful(pid,...)
│   │    else: reset timer for remaining time
│   └── exits on watchdogCtx.Done()
│
├── cmd.Wait()                         // blocks until process group done
├── watchdogCancel()
├── killProcGroup(cmd)                 // cleanup: SIGKILL any remaining grandchildren
│
├── wasTimedOut = inactivityFired || (stageCtx.Err()==DeadlineExceeded && ctx.Err()==nil)
│
└── output parsing:
    ├── if parseClaudeJSON ok → normal path
    ├── else if wasTimedOut:
    │     text = extractTextFromAssistantTurns(rawOutput)
    │     completed = strings.Contains(text, "FABRIK_STAGE_COMPLETE")
    │     return text, completed, usage, nil   // not an error
    └── else → error path (engine-shutdown or parse failure)
```

`extractTextFromAssistantTurns(rawOutput []byte)` reads `rawOutput` line by line, JSON-decodes lines with `"type":"assistant"`, and concatenates all `"text"` blocks from the message content array.

## Stage Config

```yaml
max_wall_time: "45m"   # Optional. Absent or 0 = no wall-clock cap (inactivity backstop still applies).
```

Recommended values: `"45m"` for Implement and Review stages in typical repos. Research and Plan stages are generally faster but can be given `"20m"` as a conservative cap.

## Trade-offs

- **False positives on `max_wall_time`:** If a stage legitimately needs more time than configured, it will be killed and retried. Operators must tune `max_wall_time` to be conservatively large. The inactivity timeout does not have this problem for active (but slow) processes.

- **10-second grace period adds latency:** Every `max_wall_time` kill takes at least 10 seconds past the deadline (the SIGTERM grace window). This is acceptable — hung processes are the exception, not the rule, and partial cleanup is preferable to corruption.

- **Windows:** `killProcGroupGraceful` is a no-op on Windows (process groups work differently). Both mechanisms still fire and set their flags, but the kill is a best-effort `cmd.Cancel` (the default SIGKILL to the main process). Grandchild cleanup is weaker on Windows.

- **Race on PID access:** `cmd.Process` is written by `cmd.Start()` and must not be read before `Start()` completes. The solution is to capture `pid := cmd.Process.Pid` in the main goroutine immediately after `cmd.Start()` succeeds, then pass `pid` as a parameter to the watchdog goroutine closure. This is race-free because the goroutine is launched after `cmd.Start()` completes and only reads the local `pid` copy.

## References

- [ADR 005: Shell Out to Claude Code CLI](005-claude-cli-invocation.md) — subprocess lifecycle; `WaitDelay` and `killProcGroup`; process group isolation
- Issue #429 — grandchild pipe hold problem (led to ADR 005 subprocess lifecycle section)
- Issue #412 — stuck Claude process problem (this ADR)
- graphiti#26 — production incident that motivated this change
