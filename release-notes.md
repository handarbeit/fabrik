# Fabrik v0.0.15

## Fixes

- **Items invisible after yolo catch-up advance** — Board column moves don't bump the issue's `updatedAt`, so items advanced by the yolo catch-up loop appeared "unchanged" on subsequent polls and were never processed in their new column. Fixed with two complementary changes: evicting the `updatedAt` cache entry after advancing, and using the project item's `updatedAt` (which does reflect column moves) for change detection.
- **Multi-layer change detection** — The shallow query now tracks `updatedAt` from three sources: the project item (column moves), the issue (comments, labels), and linked PRs (reviews, commits). The latest timestamp wins, ensuring activity at any layer triggers a deep fetch.

- **Dev auto-upgrade missed rebuilds** — The dev upgrade path compared the local checkout to origin/main but didn't check if the running binary was built from HEAD. If the checkout was pulled but the binary wasn't rebuilt, upgrades were silently skipped. Now compares the binary's embedded SHA against HEAD.

## Internal

- ADR 020 updated to document the multi-layer change detection principle and the project-item-as-primary-entity design.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
