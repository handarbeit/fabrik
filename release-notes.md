# Fabrik v0.0.14

## Fixes

- **New items invisible after shallow query optimization** — The `updatedAt` cache was storing timestamps for all board items, including those that were never deep-fetched. New items that appeared on the board would be marked as "unchanged" on subsequent polls and never processed. Fixed to only cache items that were actually deep-fetched. (#230)
- **Closed issues bypass cleanup stage** — The `IsClosed` guard unconditionally skipped closed issues, preventing the cleanup stage from running after a PR merges. Worktrees accumulated on disk indefinitely. Fixed to allow closed issues through when the current board status maps to a cleanup stage. (#238)

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
