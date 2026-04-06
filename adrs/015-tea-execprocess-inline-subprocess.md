# ADR 015: Use `tea.ExecProcess` for Interactive Subprocess Invocations from Bubbletea Programs

## Status

Accepted

## Context

The `fabrik watch` TUI allows users to resume a Claude Code session for the issue being watched. Previously this launched Claude in a new terminal window using platform-specific terminal emulator commands (AppleScript for iTerm2/Terminal.app on macOS, gnome-terminal/xterm/konsole on Linux, etc.). This approach required:

- A `--terminal` flag and environment variable resolution
- Platform-specific branching (`runtime.GOOS`)
- Shell quoting of the command string
- External terminal emulator availability

Additionally, the worktree directory had to be passed via a `cd <dir> && claude` shell string rather than setting the working directory directly, introducing shell quoting complexity.

Bubbletea v1.3.10 (in use) provides `tea.ExecProcess(*exec.Cmd, func(error) tea.Msg)` which natively suspends the bubbletea alt-screen, hands the terminal over to the subprocess (setting `Stdin`/`Stdout`/`Stderr` to the process's TTY), waits for the subprocess to exit, then restores the bubbletea program. This is the standard bubbletea pattern for interactive subprocesses (vim-style suspend/restore).

## Decision

All interactive subprocess invocations from bubbletea programs in Fabrik use `tea.ExecProcess`. Specifically:

```go
cmd := exec.Command("claude", args...)
cmd.Dir = worktreeDir(m.issueNumber)  // set working directory directly, no shell needed
return tea.ExecProcess(cmd, func(err error) tea.Msg {
    return ClaudeFinishedMsg{Err: err}
})
```

The callback receives the subprocess exit error (nil on clean exit) and returns a message that the bubbletea program handles after the TUI is restored.

The `--terminal` flag is removed from `fabrik watch`. Terminal-emulator-specific launch code (`openTerminal`, `linuxFallbackTerminal`, `shellQuote`) is deleted.

## Consequences

**Benefits:**
- No terminal emulator dependency — works in any terminal
- No shell quoting — `cmd.Dir` sets the working directory directly
- No platform-specific branching — bubbletea handles TTY handoff uniformly
- Clean TUI suspend/restore — the TUI reappears exactly as left when Claude exits
- Simpler code — eliminates ~60 lines of platform-specific terminal launch logic

**Constraints:**
- Requires `tea.WithAltScreen()` on the program — `watch/model.go` already uses this
- Visual artefacts are possible if Claude crashes or the terminal is resized during the subprocess; this is an accepted limitation of the pattern
- The pattern is inherently synchronous from the TUI's perspective: background goroutines continue running and their messages queue while Claude is active, flushing when the TUI resumes

**Future guidance:**
Any future interactive subprocess (e.g. editor integration, shell escape) from a bubbletea program in Fabrik should use `tea.ExecProcess` following this pattern. Terminal-emulator-specific launch code should not be reintroduced.
