# Fabrik v0.0.31

## Features

- **Dev build auto-upgrade restored** — Dev builds (`dev(sha)`) now self-upgrade when idle: detect local commits or new remote commits on `origin/main`, pull if needed, `go build`, refresh plugin skills, and re-exec. Critical for the Fabrik-builds-Fabrik workflow.
- **Silent plugin skill refresh** — In non-interactive mode (headless daemon, CI), plugin skills are auto-refreshed when they differ from the binary's embedded defaults. No manual `fabrik upgrade` needed. Interactive mode still prompts.

## Improvements

- **Reduced API usage for idle boards** — Items with `fabrik:awaiting-input` or `fabrik:awaiting-review` labels no longer force a deep-fetch every poll cycle. Adding a comment or submitting a review bumps `updatedAt`, so the normal cache check detects the change. Only `fabrik:blocked` items still bypass the cache (blocking issue closure doesn't update the blocked issue). Saves ~4 unnecessary GraphQL deep-fetches per poll on boards with paused items.
- **Troubleshooting consolidated** — All troubleshooting content is now in USER_GUIDE section 9, covering duplicate comments, multiple instances, rate limits, and more.

## Internal

- Documentation updates for v0.0.30.
- Plugin skills refreshed to match embedded defaults.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
