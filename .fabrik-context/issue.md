## Summary

Add a way to open an interactive Claude Code session in an issue's worktree, resuming the existing conversation from the last stage run. This enables hands-on debugging when a stage fails or gets stuck — the user drops into the same context Claude had and can investigate interactively.

## Motivation

When a stage fails or produces unexpected results, the current options are:
- Read the log files (passive, no interaction)
- Post a comment and wait for the retry loop (slow, indirect)
- Manually `cd` into the worktree and run Claude from scratch (loses session context)

An interactive resume lets the user pick up exactly where Claude left off — same session, same worktree, same conversation history — and drive the investigation themselves.

## Requirements

### TUI integration

- Add an `r` key binding in the TUI:
  - On a selected **History** pane entry → open an interactive Claude session in the issue's worktree (resume behavior described below)
  - On a selected **In-Progress** (active) pane entry → same behavior as existing `enter/l` (opens log viewer for the running job; active sessions must not be interrupted)
- The resume opens a new terminal window running:
  ```bash
  claude --resume <session-id> --model <model> --plugin-dir <plugin-dir>
  ```
  in the issue's worktree directory (`.fabrik/worktrees/issue-<N>/`)
- The session ID is read from `~/.fabrik/sessions/issue-<N>/<stageName>.session`; if the file does not exist or is empty, open a fresh Claude session in the worktree (no `--resume` flag)
- The model is taken from the stage config (the `model` field in the stage YAML); `model:<name>` label overrides are **not** applied (requires GitHub API, out of scope for this command)
- The plugin dir is taken from `FABRIK_PLUGIN_DIR` env var or `--plugin-dir` flag (same resolution as the main engine)
- `--max-turns` is **not** passed (no turn limit for interactive sessions)
- `--allowedTools` is **not** passed (all tools available in debug sessions, regardless of stage config)
- If the worktree directory does not exist, display an error message in the terminal window advising the user that the issue hasn't been processed yet
- The hint bar in the History pane is updated to include `[r]esume`
- The hint bar in the In-Progress pane is updated to include `[r/enter/l]ogs` (aliased with existing log viewer)
- Terminal window behavior uses the existing `openTerminalCmd` mechanism (tracks issue #108 for future improvements)

### CLI subcommand

- Add `fabrik resume <issue-number> [--stage <name>]` as a standalone subcommand
- `--stage` is optional; when omitted, auto-detect the current stage using the config file introduced by #109; if that config is unavailable, require `--stage` explicitly and exit with a helpful message
- The subcommand reads stage config from `--stages` flag or `FABRIK_STAGES` env var (default `.fabrik/stages`)
- Plugin dir is resolved from `--plugin-dir` flag or `FABRIK_PLUGIN_DIR` env var
- Session resolution and model/tool behavior is identical to the TUI History binding (see above)
- On success: exec's (replaces process with) `claude …` in the worktree; does not return
- On error (worktree missing, stage not found, `claude` binary not in PATH, config unavailable): prints a helpful message to stderr and exits non-zero
- The subcommand does not require GitHub credentials or network access

### What the user gets

- Full conversation history from the previous session (via `--resume <session-id>`)
- The worktree at the exact state Claude left it
- All Claude Code tools available (not restricted by `allowed_tools`)
- The Fabrik plugin loaded (via `--plugin-dir`)
- Ability to investigate, make changes, run tests, and commit

### What Fabrik does NOT do

- Does not lock the issue or add labels
- Does not post output back to the issue
- Does not track or record the interactive session
- Does not interfere with the normal retry/cooldown loop

## Scope

**In scope:**
- `r` key binding on selected History pane entries in the TUI (resume interactive session)
- `r` key binding on selected In-Progress pane entries in the TUI (alias for existing `enter/l` log viewer)
- `fabrik resume <issue-number> [--stage <name>]` CLI subcommand
- Session file read logic (already exists in `engine/claude.go`; reused)
- Worktree existence check with helpful error
- Stage config read to determine model (existing `stages` package; reused)

**Out of scope:**
- Resuming an active/in-progress session (concurrent worktree access is risky; active `r` is log tail only)
- Applying `model:<name>` label overrides (requires GitHub API; falls back to stage config)
- Recording or posting interactive session output to the issue
- Automatic pause of the issue before opening the session (user must do this manually)
- Terminal emulator improvements beyond the current `openTerminalCmd` mechanism (tracks #108)
- Stage auto-detection without #109's config file; stubs to require explicit `--stage` until #109 lands

**Assumptions:**
- The user understands they should pause the issue (add `fabrik:paused` label) before starting an interactive session to prevent the engine from retrying in parallel
- `--max-turns` is not needed for interactive sessions
- `r` on active pane items is not a distinct feature — it aliases the existing log viewer (`enter/l`)
- Issue #96 has not been implemented in the codebase; this issue owns the full implementation with no overlap risk

## Prior Art / Context

- `engine/claude.go:97-119` — `SessionDir()` and `sessionFile()` already define and manage the session file format at `~/.fabrik/sessions/issue-<N>/<stageName>.session`
- `engine/claude.go:165-198` — `buildClaudeArgs` shows how `--resume`, `--model`, `--allowedTools`, and `--plugin-dir` are composed; the interactive command intentionally omits `--allowedTools` and `--max-turns`
- `tui/model.go:522-548` — `openLogViewerCmd` implements the log viewer; active `r` reuses this
- `tui/model.go:570-603` — `openTerminalCmd` already implements cross-platform terminal window launching; History pane `r` reuses this
- `tui/model.go:479-483` — existing In-Progress hint bar (`[enter/l]ogs [tab] history`); updated to `[r/enter/l]ogs [tab] history`
- `tui/model.go:494-497` — existing History hint bar (`[l]ogs [c]lear entry [C]lear all [tab] in-progress`); updated to add `[r]esume`
- `cmd/root.go:43-48` — subcommand dispatch pattern; `resume` follows the same pattern as `init`
- Issue #96 — related resume functionality; not yet implemented in the codebase; this issue supersedes it
- Issue #108 — planned terminal emulator improvements; current behavior uses Terminal.app on macOS via osascript
- Issue #109 — planned config file; stage auto-detection stubs until #109 is available

## Risks / Dependencies

- **Dependency on #109 (soft)**: Stage auto-detection for `fabrik resume` (when `--stage` is omitted) depends on the config format from #109. Until #109 lands, the command requires `--stage` explicitly. This is not a blocker.
- **Dependency on #108 (soft)**: Terminal emulator selection tracks #108. Current behavior uses `openTerminalCmd` (Terminal.app on macOS). This is not a blocker.
- **Concurrent worktree access**: If the engine retries a stage while the user has an interactive session open, both could write to the same worktree. Documenting the need to pause first mitigates this risk.
- **Session file staleness**: If a session was terminated abnormally, the session ID may be from a previous partial run. Acceptable — `claude --resume` loads conversation history up to that point.
- **`claude` not in PATH**: The `resume` subcommand checks for the `claude` binary and prints a friendly error if not found.
- **TUI model access to stage config**: The TUI `r` binding needs the model from stage config; `HistoryEntry` does not currently store it. The implementation must either add a `Model` field to `HistoryEntry` or read the stage YAML on demand when `r` is pressed.