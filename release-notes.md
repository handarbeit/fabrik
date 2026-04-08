# Fabrik v0.0.24

## Features

- **Always bare-clone repos** (#249) — Fabrik no longer has two worktree modes. All repos are bare-cloned to `.fabrik/repos/`, eliminating the "single-repo git mode" that caused repo pollution (#240), dev-upgrade-against-wrong-repo bugs, and `.git/info/exclude` workarounds. The `jobControlMode` flag, `devCheckout()`, and the dev auto-upgrade path have been removed. This is a significant simplification — 571 lines deleted, 123 added.
- **`/cut-release` now files doc update issues** — After cutting a release, the skill automatically creates a `fabrik:yolo` documentation issue at Specify so user docs stay current.

## Upgrading

Existing single-repo setups will bare-clone on first run after upgrade (a few seconds). Existing worktrees continue to work — the bare clone is only used for new branch creation and fetching.

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
