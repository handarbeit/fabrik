# Fabrik v0.0.26

## Fixes

- **Done items no longer archived immediately** — Items reaching the Done column were archived from the project board the instant the cleanup stage ran, before users could see what completed. Archive now deferred to the 24-hour grace period as intended.
- **Bare clone race condition** (#288) — When multiple issues from the same new repo appeared on the board simultaneously, concurrent workers would race to bare-clone the repo, causing all but the first to fail with `info/exclude: File exists`. Fixed via per-repo singleflight coordination.
- **FABRIK_STAGE_COMPLETE honored on non-zero exit** — Claude sessions that emitted the stage-complete marker but exited non-zero were incorrectly treated as failures. The marker is now respected regardless of exit code.

## Improvements

- **Multi-repo mode** — Fabrik can now process issues from multiple repos on the same project board. Comment out `repo:` in `.fabrik/config.yaml` to enable. Needed for cross-org repos (e.g. issues on a public repo managed alongside the private source repo).
- **`fabrik init` excludes `.git/info/exclude`** — Running `fabrik init` in a git repo now adds Fabrik-managed paths to `.git/info/exclude` so they don't show up as untracked.
- **`.fabrik/repos/` gitignored** — Bare clone directories are now excluded from git tracking by default.

## Internal

- ADR 022: per-repo singleflight coordination for bare clones.
- Reverted accidental LICENSE/copyright commit that targeted the wrong repo.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo tenaciousvc/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
