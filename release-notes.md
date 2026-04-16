# Fabrik v0.0.40

## Fixes

- **Hotfix for v0.0.39 crash on startup.** `dispatchReviewReinvoke` now calls `ensureRepoReady` before invoking `processComments`. Without this, the Phase 1 catch-up loop introduced in v0.0.39 could reach `processComments` before any `WorktreeManager` was registered for the repo, causing `processComments` → `e.worktreesFor(item.Repo)` to panic on a freshly-started Fabrik that had unresolved PR review threads. On clone failure the goroutine now logs and bails instead of crashing.

## Upgrading

Upgrade is strongly recommended for all v0.0.39 users — v0.0.39 will crash-loop on any multi-repo board with unresolved PR review threads across repos that have not yet been touched by the dispatch loop.

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m); case "$ARCH" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
gh release download --repo handarbeit/fabrik --pattern "*${OS}_${ARCH}*" -O - | tar xz
```
