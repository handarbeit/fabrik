# Fabrik v0.0.18

## Fixes

- **Dev auto-upgrade ran against user's project repo** (#240) — `devCheckout()` now verifies the git remote matches `tenaciousvc/fabrik` or `handarbeit/fabrik` before attempting `git pull` + rebuild. Previously, running Fabrik from source inside a target project's git repo would pull and rebuild against the wrong repo.
- **`fabrik init` writes .git/info/exclude** (#240) — `.fabrik/worktrees/`, `.fabrik/repos/`, `.fabrik/plugin/`, and `.fabrik/debug/` are added to `.git/info/exclude` during init, preventing Fabrik's working directories from polluting the target repo's git status. Skipped when running inside the Fabrik source repo itself.
- **Terminal left in broken state after TUI exit** — `ReleaseTerminal()` is now called after the TUI exits to restore raw mode and mouse tracking. Fixes `^M` on enter and other terminal corruption after quitting.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
