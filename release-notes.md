# Fabrik v0.0.32

## Features

- **Persistent poll log file** (#307) — All poll-level output (deep-fetch decisions, dispatch, upgrade checks, rate limit stats) is now written to `.fabrik/fabrik.log` in both TUI and non-TUI modes. Essential for post-mortem debugging.
- **Sessions and logs moved to `<cwd>/.fabrik/`** (#313) — Session files and stage logs are now stored under the project's `.fabrik/` directory instead of `~/.fabrik/`. Each project is fully self-contained — moving the directory preserves all state. Includes one-time migration from the old location.

## Fixes

- **TUI: history/active hint wrapping no longer pushes footer off screen** (#305) — Menu hints are constrained to available width, preventing the status bar from being pushed off the bottom.
- **Test suite no longer hangs** — Tests now isolate HOME and CWD to avoid interference from running Fabrik instances and stale session files. Full suite completes in ~80 seconds.

## Internal

- ADR 023: all Fabrik control files live under `<cwd>/.fabrik/`.
- Documentation updates for v0.0.31.
- Removed accidentally committed `engine/.fabrik/fabrik.lock`.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```

**Note:** On first startup after upgrading, sessions and logs are automatically migrated from `~/.fabrik/` to `<cwd>/.fabrik/`.
