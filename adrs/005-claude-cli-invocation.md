# ADR 005: Shell Out to Claude Code CLI

## Status

Accepted

## Context

Fabrik needs to invoke Claude Code for each stage. Two main approaches:
use the Claude API directly, or shell out to the `claude` CLI.

## Decision

Shell out to `claude` CLI via `os/exec` with `--print` (non-interactive) mode.

## Rationale

- **Full Claude Code capabilities**: The CLI has tools (Read, Write, Edit,
  Bash, Glob, Grep, etc.), MCP servers, hooks, CLAUDE.md support, and
  session management built in. Using the API directly would mean reimplementing
  all of this.
- **Session resumption**: The CLI supports `--resume <session-id>` for
  continuing a previous conversation with full context.
- **Authentication**: The CLI handles its own auth. No API keys to manage
  in Fabrik.
- **Model selection**: `--model` flag lets stages pick their model.
- **Tool restrictions**: `--allowedTools` lets stages limit what Claude can do.
- **Simplicity**: ~20 lines of Go code to invoke and capture output.

## How It Works

```
fabrik -> claude --print --verbose --model sonnet --max-turns 10
          (prompt delivered via stdin)
       <- stdout (Claude's response)
       <- session metadata (for resumption)
```

The prompt (stage instructions + issue body + comments) is passed to the
`claude` process via stdin rather than as a positional CLI argument. This
avoids OS `ARG_MAX` limits for large issues with long comment histories and
prevents prompt content from appearing in process listings.

Claude runs in the issue's worktree directory (`cmd.Dir = workDir`), so it
operates on the correct branch and file state.

## Trade-offs

- **Subprocess overhead**: Each invocation starts a new process. Acceptable
  since Claude Code invocations take seconds to minutes.
- **Output parsing**: We parse stdout for completion markers and session IDs.
  This is fragile if Claude Code's output format changes.
- **Claude Code dependency**: Requires `claude` CLI installed and on PATH.
  This is a hard requirement, not optional.

## Subprocess Lifecycle Management (added with issue #429)

### Problem

Claude uses `run_in_background: true` on Bash tool calls for test runs and
polling loops, and the Monitor tool spawns `tail -f` processes. These
grandchild processes inherit Fabrik's stdout pipe write-fd. When the Claude
CLI process exits, grandchildren are reparented to PID 1 but keep the pipe
open. Go's `cmd.Wait()` blocks until the pipe's read side reaches EOF, which
never arrives while any process holds the write side — so the worker goroutine
blocks indefinitely.

### Solution

Two mechanisms, applied in `runClaude` after cmd construction:

**1. `cmd.WaitDelay` (Go 1.20+)**

```go
cmd.WaitDelay = claudeWaitDelay  // default: 30s, configurable via --claude-wait-delay
```

After the Claude process exits, Go waits at most `WaitDelay` for I/O copy
goroutines to finish. If the deadline fires, Go closes the pipe and returns
`exec.ErrWaitDelay`. The engine detects this, logs a warning, and falls through
to normal output processing — the buffered output (complete, since Claude wrote
its final result line before exiting) is parsed normally.

**2. Process Group Isolation + SIGKILL (Unix only)**

```go
// engine/procattr_unix.go (//go:build !windows)
cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
// after cmd.Run() returns:
syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
```

Starting Claude with `Setpgid: true` creates a new process group with Claude
as the process group leader (PGID == Claude's PID). After `cmd.Run()` returns
(whether via normal exit or WaitDelay), SIGKILL is sent to the entire group,
cleaning up all grandchildren. This is safe: SIGKILL on an empty/already-dead
group returns ESRCH, which is ignored. It does not affect worktree file state
since grandchild monitoring processes have no filesystem side effects.

This is the first use of build-constrained files in `engine/`:
`engine/procattr_unix.go` (`//go:build !windows`) and
`engine/procattr_windows.go` (`//go:build windows`) provide Unix-specific
syscall code while keeping the Windows build clean.

### Configuration

- `--claude-wait-delay <seconds>` (env: `FABRIK_CLAUDE_WAIT_DELAY`): how long
  to wait after Claude exits before recovering buffered output. Default: 30s.
  Increase if Claude's final output write is unusually slow; decrease in test
  environments where fast recovery is preferred.

### Proactive Kill for Stuck Processes

`WaitDelay` only fires after Claude exits. It provides no bound when Claude itself hangs and never exits. [ADR 029](029-proactive-kill-timeout.md) adds two proactive kill mechanisms on top of this passive layer: a per-stage `max_wall_time` YAML field and a hardcoded 15-minute inactivity watchdog, both using the SIGTERM→10s→SIGKILL sequence via `killProcGroupGraceful`. After either kill, the already-buffered output is scanned for `FABRIK_STAGE_COMPLETE` in intermediate assistant NDJSON turns so completed work is not re-run.

## Future Consideration

If Anthropic releases a Go SDK or agent framework, we could switch to
direct API integration for tighter control. The stage config abstraction
would remain the same.
