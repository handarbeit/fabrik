# Fabrik v0.0.35

## Features

- **TUI `?` help panel** (#343) — Press `?` in the TUI to open a help overlay showing all keybindings and a reference for Fabrik's labels (yolo/cruise, paused/blocked/awaiting-*, locked, stage:*, model:, effort:, unrestricted). Press `?` or `esc` to dismiss.
- **SSH clone support** (#356) — New `--ssh` flag and `git_ssh: true` config option to clone repos via SSH instead of HTTPS. Useful for private repos where SSH keys are configured. Startup checks verify SSH agent is available.
- **Fine-grained PAT detection** (#358) — Fabrik detects when a `github_pat_*` token is in use and warns at startup with guidance to switch to a classic PAT (which Fabrik requires for project board access). 401/403 errors now include actionable hints about token type.
- **`.env` works without a git repo** (#351) — Loosened the `.env` safety check: when no `.git/` directory is present, Fabrik loads `.env` without requiring it to be in `.gitignore`. Useful for bare directories, containers, and CI workspaces.

## Fixes

- **Default branch detection in bare clones** — Fabrik no longer assumes `main`; it detects the actual default branch from `git ls-remote`. Repos using `master` or other defaults now work correctly.
- **Worktree error messages include repo name** — Easier to diagnose which repo had a worktree issue in multi-repo mode.
- **Robust SSH/HTTPS URL rewriting** — Startup checks order and URL-rewrite logic improved to handle edge cases.

## Internal

- Test coverage for fine-grained PAT warning and 401/403 auth hints.
- Documentation updates for v0.0.34 features and v0.0.35 additions.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo shadoworg/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
