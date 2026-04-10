# Fabrik v0.0.27

## Fixes

- **Disable auto-archive of Done items** — Auto-archive was removing completed items from the project board before users could see them. Disabled until the timing logic is reworked to track actual Done stage completion time.
- **Fix TestMain_Help timeout** — Test was hanging because the subprocess found `.env` and `.fabrik/config.yaml` in the repo root and started the engine. Now runs from a temp directory.

## Internal

- Documentation updates for v0.0.26 changes (USER_GUIDE, README, stage-lifecycle, marketing site).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
