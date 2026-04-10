# Fabrik v0.0.29

## Fixes

- **Auto-upgrade now fetches from handarbeit/fabrik** — The upgrade check was still pointing at the private repo. Now fetches releases from the public `handarbeit/fabrik` repo where binaries are published.

## Improvements

- **Documentation site at fabrik.handarbeit.io** — Public docs are now served from a custom domain. All links updated to point at the public `handarbeit/fabrik` repo for releases, issues, and discussions.
- **Hero screenshots** — Replaced video placeholders with TUI and GitHub Project Board screenshots on the docs landing page.
- **Discussions link in footer** — Added link to GitHub Discussions in the docs site footer navigation.

## Internal

- Docs site cleanup: removed open source references, redundant cards, auto-archive feature card, and internal skill references.
- CSS fix for terminal code block line wrapping.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
