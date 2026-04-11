# ADR 023: All Fabrik Runtime State Lives Under `<cwd>/.fabrik/`

**Status**: Accepted  
**Date**: 2026-04-11

## Context

Fabrik is invoked from a project directory. Worktrees, stage configs, and
repos were already stored under `<cwd>/.fabrik/`. However, Claude session
files and stage logs were stored under `~/.fabrik/sessions/` and
`~/.fabrik/logs/` respectively.

This caused three problems:

1. **Test isolation failure**: Tests had to redirect both `CWD` and `HOME`
   to isolate session/log state, making them fragile.
2. **Cross-project migration noise**: On every startup, Fabrik scanned
   `~/.fabrik/sessions/` for old-style files. Different projects triggered
   each other's migrations.
3. **Sessions don't travel with the project**: Moving or copying a project
   directory moved the worktrees but left sessions and logs behind.

## Decision

All Fabrik runtime state is stored under `<cwd>/.fabrik/`:

| Path | Content |
|------|---------|
| `<cwd>/.fabrik/worktrees/` | Git worktrees (unchanged) |
| `<cwd>/.fabrik/repos/` | Bare clones (unchanged) |
| `<cwd>/.fabrik/stages/` | Stage YAML configs (unchanged) |
| `<cwd>/.fabrik/sessions/` | Claude session ID files (moved from `~/.fabrik/sessions/`) |
| `<cwd>/.fabrik/logs/` | Stage run logs (moved from `~/.fabrik/logs/`) |

All path-resolution functions (`SessionDir`, `sessionDirForItem`, `LogDir`,
`logDirForItem`, `ReadSessionID`, `tuiReadSessionID`, `issueLogDir`,
`sessionDir`) now call `os.Getwd()` instead of `os.UserHomeDir()`.

At engine startup, `migrateHomeToProject` moves any existing content from
`~/.fabrik/sessions/` and `~/.fabrik/logs/` to the CWD-relative paths. The
migration is idempotent (skips files whose destination already exists) and
handles cross-device moves via a `renameWithFallback` helper that falls back
to `io.Copy` + `os.Remove` when `os.Rename` fails with `EXDEV`.

## Constraint: The Engine Must Never Call `os.Chdir()`

Session and log paths are resolved by calling `os.Getwd()` at path-resolution
time, not at startup. This is safe because **the engine process never changes
its own working directory**. `runClaude` uses `cmd.Dir` to set the subprocess
working directory, which does not affect the engine process's CWD.

This constraint is non-obvious. A future contributor who wants to `os.Chdir()`
for any reason must understand that doing so will silently break session and log
path resolution for all subsequent calls. Use `cmd.Dir` instead.

## Consequences

- Session and log files created under a project directory travel with the
  project when moved or copied.
- Tests can isolate session/log state with `t.Chdir(t.TempDir())` alone —
  no need to also redirect `HOME`.
- Users with multiple projects get separate session stores automatically.
- On first startup after the upgrade, existing sessions are migrated
  transparently. Sessions in progress at migration time would have been
  interrupted by the restart anyway.
- `fabrik watch` and `fabrik resume` agree on the session path because they
  are invoked from the same project CWD as `fabrik` itself.
