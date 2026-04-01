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
fabrik -> claude --print --verbose --model sonnet --max-turns 10 "<prompt>"
       <- stdout (Claude's response)
       <- session metadata (for resumption)
```

Claude runs in the issue's worktree directory (`cmd.Dir = workDir`), so it
operates on the correct branch and file state.

## Trade-offs

- **Subprocess overhead**: Each invocation starts a new process. Acceptable
  since Claude Code invocations take seconds to minutes.
- **Output parsing**: We parse stdout for completion markers and session IDs.
  This is fragile if Claude Code's output format changes.
- **Claude Code dependency**: Requires `claude` CLI installed and on PATH.
  This is a hard requirement, not optional.

## Future Consideration

If Anthropic releases a Go SDK or agent framework, we could switch to
direct API integration for tighter control. The stage config abstraction
would remain the same.
