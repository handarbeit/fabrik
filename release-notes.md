# Fabrik v0.0.55

This release makes the in-memory board cache the unified source of truth regardless of whether webhooks are configured, and fixes a dependency-resolution race that could re-block issues immediately after they were correctly unblocked.

## Features

- `--auto-upgrade` now checks for new releases at startup, not only after the first idle window. Busy boards that never enter the idle state will still self-upgrade (#655).

## Fixes

- `checkDependencies` now prefers the cache's view of a blocker's `IsClosed` over the GraphQL deep-fetch `blockedBy.nodes.state` field. GitHub's indexer lags on the dep edge by seconds-to-minutes after an issue closes, so the previous code could re-apply `fabrik:blocked` to a dependent that the `PushUnblockObserver` had just correctly unblocked. The cache is now consistently the source of truth for blocker state in both the push-path observer and the pull-path dependency gate (#664).

## Improvements

- The in-memory board cache is wired unconditionally as the unified source of truth. Every poll cycle now performs a shallow `FetchProjectBoard` from GitHub and applies the result via `Bootstrap` (first poll) or `Reconcile` (subsequent polls). Observer notifications — `StateChanged`, `LabelsChanged`, `BlockedByChanged`, etc. — fire on every relevant transition regardless of whether webhooks are configured. As a result, `PushUnblockObserver` and other reactive observers operate correctly in polling-only mode, where they were previously structurally dormant. Webhooks remain an optional acceleration that update the cache between polls; the per-poll Reconcile is the universal baseline freshener (#660).
- `PushUnblockObserver` decisions are now logged to `fabrik.log` under the `[push-unblock]` tag — visible signals for "blocker X#N closed → removing fabrik:blocked from dependent Y#M". Push-path activity was previously completely silent, making dep-unblock issues hard to diagnose (#664).

## Breaking changes

- The `--board-cache` flag and `FABRIK_BOARD_CACHE` environment variable are removed. The cache is now always wired in production. Existing configs that referenced `--board-cache=in-memory` should drop the flag (that behavior is the default); references to `--board-cache=none` are no longer supported.

## Internal

- gofmt alignment fixes; documentation updates for the cache lifecycle (`docs/state-machine.md` Appendix D, `docs/USER_GUIDE.md` "In-Memory Board Cache" section); follow-up review fixes for #653 and #655.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
