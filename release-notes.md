# Fabrik v0.0.20

## Fixes

- **Release auto-upgrade now refreshes plugin skills** — The release upgrade path was replacing the binary but not extracting updated plugin skills. Stage prompts (Research, Plan, Implement, etc.) would remain stale after an auto-upgrade. Now calls `plugin.RefreshPlugin()` directly before re-exec, matching the dev upgrade behavior.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
