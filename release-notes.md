# Fabrik v0.0.21

## Fixes

- **Plugin skills now refresh from the new binary, not the old one** — v0.0.20 called `RefreshPlugin()` before `syscall.Exec`, extracting skills from the old binary's embedded FS. Now the new binary refreshes its own skills on startup via the existing `FABRIK_AUTO_UPGRADED=1` mechanism.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
