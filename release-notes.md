# Fabrik v0.0.47

## Features

- Real-time turn counter in the TUI — visible in the overview pane (width-adaptive `[N/M turns]`), the detail panel, and the `fabrik watch <N>` view so you can see how close a stage is to its turn budget while it's still running and intervene before a retry/cooldown cycle begins (#431).
- Progress-based turn extension via the `fabrik:extend-turns` label — pre-grants 2× the stage's `max_turns` budget, auto-extends to 3× when actual progress is detected, and auto-removes on stage success. The displayed turn counter always reflects the effective (extended) budget (#432).

## Internal

- Unit and integration tests for live turn counting (NDJSON parsing, badge width calculation, `TurnProgressEvent` plumbing) and progress-based turn extension; documentation updates across `state-machine.md`, `stage-lifecycle.md`, `CLAUDE.md`, README, USER_GUIDE, and the marketing index.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
