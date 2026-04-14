# Fabrik v0.0.34

## Features

- **Click-to-open issue and board links in TUI** (#340) — Cmd+click (or Ctrl+click on Linux) on the `#NNN` column in history/active panels to open the issue in your browser. Cmd+click the board title in the footer to open the project board. Mouse capture removed; keyboard navigation unchanged.
- **`effort:` label override** (#341) — Per-issue thinking effort control via label: `effort:low`, `effort:medium`, `effort:high`, `effort:max`. Overrides the stage YAML's `effort_level` for one issue only. Complements the existing `model:` label override.
- **Refactor: TUI component architecture** (#279) — `tui/model.go` split into component files (header, active, history, footer, detail). Reduces the giant model file and sets up for further TUI enhancements.
- **Release announcements to Discussions** — GitHub Actions release workflow now posts a formatted announcement to the handarbeit/fabrik Discussions "Announcements" category after each successful release.
- **TUI footer shows clickable board title** — "Fabrik PM" (or your project board name) now appears in the footer as an OSC 8 hyperlink on supported terminals.

## Fixes

- **Done stage no longer skipped after restart** — When a Fabrik restart left only cleanup items to process, the Done stage was silently skipped because no WorktreeManager was registered for the repo. Both `itemMayNeedWork` and `itemNeedsWork` now fall back to a direct filesystem path check.
- **Done stage runs after manual Validate→Done column move** — Board column moves don't always bump the issue's `updatedAt`, so cleanup items could be stuck by the updatedAt cache. Cleanup stages now bypass the cache entirely (worktree Stat is local, no GraphQL cost).
- **OSC 8 hyperlinks in footer now actually work** — Lipgloss `Style.Render()` was stripping OSC 8 escape sequences. New `renderWithOSC8` renders dim style first, then injects the raw hyperlink.
- **Rate limit text no longer touches right edge** — Footer width budget leaves a 1-char margin on both sides.
- **Project version no longer redundantly shown alongside board title** — `github.com/handarbeit/fabrik` no longer clutters the footer once "Fabrik PM" is fetched.
- **Board title replaces stale `owner/repo` in footer** — The old footer slot displayed `owner/` with no repo in multi-repo mode. Now shows the project board title with a clickable link.

## Improvements

- **Label auto-seeding at startup** — Fabrik now ensures its managed labels exist on the configured repo at startup (with descriptions and colors), so the GitHub UI shows meaningful hover text.
- **Refactor: worktreeExistsForItem helper** — Eliminated duplicated WM lookup + filesystem fallback logic between the two item filters.
- **Refactor: hasLabel helper** — Replaced inline label-scanning loops with a named predicate.

## Internal

- Test suite reliability: `TestExecute_ValidStagesReachesEngine` and `TestMain_Help` now isolate HOME/CWD so the suite completes in ~80 seconds without hangs.
- `engine/.fabrik/` and `.fabrik/debug-footer.bin` gitignored (test/debug artifacts).
- PR #339 merged with TUI mouse capture removal and OSC 8 hyperlinks for issue numbers.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
