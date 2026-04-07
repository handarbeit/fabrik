# Fabrik v0.0.17

## Fixes

- **Stuck labels after stage setup failure** — When worktree setup or context cancellation caused an early error in `processItem`, the `fabrik:locked` and `stage:in_progress` labels were never removed. Fabrik would never retry the issue until the labels were manually cleaned up. Both error paths now call `releaseLock()` before returning.

- **Ambiguous ref when creating worktree branch** — `git branch fabrik/issue-N origin/main` could fail with "refname is ambiguous" in some repos. Fixed to use the fully-qualified `refs/remotes/origin/main` form.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
