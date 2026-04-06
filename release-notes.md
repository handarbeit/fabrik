# Fabrik v0.0.4

## What's New

### Mouse Support (#203)
The main TUI and `fabrik watch` now respond to mouse events. Click a job row in the active pane to select it; double-click to open `fabrik watch` inline (same as pressing `l`). In `fabrik watch`, click the stage tabs to switch between them. Scroll wheel scrolls the log viewport.

### Quit Confirmation for Active Jobs (#201)
Pressing `q` or Escape in the main TUI now shows a confirmation prompt when jobs are still running, preventing accidental interruptions. Ctrl+C still quits immediately at any time.

### Word-Wrapped Logs in `fabrik watch` (#207)
Long lines in `fabrik watch` (both the live log and historical stage tabs) are now wrapped at the current terminal width. Wrapping is ANSI-escape-sequence-aware and updates automatically on resize.

### Improved Stage Tabs in `fabrik watch` (#199)
Stage tabs are now sorted by pipeline order (Specify → Research → Plan → Implement → Review → Validate) instead of alphabetically. Comment-review logs are grouped under their parent stage tab rather than appearing as separate tabs. The live/active tab now shows a `●` prefix in orange for clearer visibility.

## Bug Fixes

### Context Files No Longer Pollute Commits or Cause Rebase Conflicts (#202)
`.fabrik-context/` files (spec, prior stage outputs) were occasionally getting staged into WIP commits or causing merge conflicts during rebase. Three-part fix: the directory now carries a `.gitignore` that excludes all files, any tracked context files are unstaged before WIP commits, and the directory is cleaned before rebasing onto main.

### Escape Key Fixed in TUI (#203, #201)
The Escape key was silently non-functional in several TUI dialogs due to a bubbletea API mismatch (`"escape"` vs `"esc"`). Now correctly cancels the clear-history and quit-confirmation dialogs.

### Mouse Click on Ellipsis Row (#203)
When the active pane truncates the job list with a `… N more` indicator, clicking that row no longer incorrectly selects a non-visible job.

### Log Viewer State Reset on Stage Transition
`fabrik watch` briefly showed a stale log when transitioning to a new stage; `currentLogPath` is now reset correctly on each stage transition.

## Documentation

- Binary install one-liner added to README and User Guide Quick Start sections
- Fixed broken `curl` install command on the documentation site
- Removed stale Terminal Auto-Detection section from the User Guide (the `--terminal` flag was removed in v0.0.2)

## Upgrading

```bash
# From a previous release binary
fabrik upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*.tar.gz' -O - | tar xz
```
