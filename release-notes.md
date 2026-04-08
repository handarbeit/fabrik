# Fabrik v0.0.23

## Features

- **Auto-archive Done items after 24 hours** (#247) — Completed items in the Done column are automatically archived from the project board after 24 hours. This dramatically reduces board size and GraphQL pagination cost (Fabrik board went from 132 to 13 items). Items stay visible for 24 hours so users can see what finished while they were away.
- **Mermaid diagram rendering** — GitHub Pages docs now render Mermaid diagrams client-side.
- **Syntax highlighting** — Code blocks on the docs site now have colorized syntax.
- **Formations documentation** — User guide updated with dependency-based issue sequencing patterns and example formations.

## Fixes

- **Archive log corrupted TUI** — Archive status messages used `fmt.Printf` which bypassed the TUI event channel. Fixed to use `logf`.
- **Shallow query labels truncated** — `labels(first:5)` missed completion labels on issues with many labels. Bumped to `labels(first:15)`.
- **Docs content too narrow** — Widened from 800px to 1100px to match the main page width. Tables now scroll horizontally on narrow screens.

## Internal

- ADR 021: Housekeeping mutations exempt from shallow-data read-only rule.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
