# Fabrik v0.0.22

## Fixes

- **Bare clone never fetched — new branches forked from stale base** — `git clone --bare` doesn't configure a default fetch refspec, so subsequent `git fetch origin` was silently a no-op. New issue branches were forked from the original clone point, not current `origin/main`. All worktrees created after the initial clone were working against stale code.
- **Self-healing for existing projects** — On startup, Fabrik now repairs the missing fetch refspec on all existing bare clones before fetching. All projects across all users will self-heal on the next restart or auto-upgrade.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
