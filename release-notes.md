# Fabrik v0.0.33

## Features

- **`fabrik:cruise` label mode** (#325) — Auto-advances issues through all pipeline stages like `fabrik:yolo`, but stops at Validate without auto-merging the PR or moving to Done. The human takes over for the final step.
- **`disable_adaptive_thinking` and `effort_level` stage config** (#321) — New stage YAML fields to control Claude Code's thinking budget. `disable_adaptive_thinking` (default: true) prevents auto-reduced thinking; `effort_level` accepts `low`, `medium`, `high`, `max` (default: `high`).
- **Rate limit display in TUI footer** (#319) — REST and GraphQL rate limit stats are now shown in the TUI status bar.

## Fixes

- **Prevent parent env vars from leaking into Claude subprocess** (#328) — Claude is now invoked with a clean environment built from scratch, preventing ambient variables from affecting behavior.
- **Yolo catch-up merges PR before advancing from Validate** (#316) — Fixed ordering bug where catch-up could advance to Done before the PR merge completed.
- **Skip yolo catch-up advance when unprocessed comments exist** (#317) — Catch-up no longer skips pending comments when auto-advancing.

## Improvements

- Default `effort_level` changed from `max` to `high` to reduce token usage without sacrificing quality.

## Internal

- Documentation updates for v0.0.32.
- WIP stage-incomplete marker support (partial progress, not yet user-facing).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*<os>_<arch>*' -O - | tar xz
```
