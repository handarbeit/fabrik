# Fabrik v0.0.30

## Fixes

- **Fix duplicate stage comments** — Globally-installed Claude Code plugins (especially `superpowers`) were injecting SessionStart hooks into Fabrik's headless Claude sessions, causing parallel Agent subagents to spawn. A single dispatch could produce 7+ comments and waste API credits. Fixed by removing `superpowers` from project plugin settings. Users should also remove it globally: `rm -rf ~/.claude/plugins/cache/claude-plugins-official/superpowers`
- **Prevent multiple Fabrik instances** — Added a PID file lock (`.fabrik/fabrik.lock`) that prevents starting a second Fabrik instance for the same project. Previously, two instances could run undetected, each dispatching workers for the same issues.

## Features

- **Troubleshooting guide** — New docs page at [fabrik.shadoworg.dev/troubleshooting](https://fabrik.shadoworg.dev/troubleshooting) covering common issues: duplicate comments, multiple instances, rate limits, auth errors, and more.

## Internal

- Documentation updates for v0.0.29.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo shadoworg/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```

**Important:** After upgrading, remove the `superpowers` plugin globally if installed:
```bash
rm -rf ~/.claude/plugins/cache/claude-plugins-official/superpowers
```
