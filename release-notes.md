# Fabrik v0.0.16

## Fixes

- **User comments on paused issues now trigger unpause** — A user comment on a `fabrik:paused` issue is an implicit "resume." Fabrik removes the pause label, clears any failed-stage state, and processes the comment. Previously, paused issues silently ignored all comments.
- **Comments missed during rate limit exhaustion** (#232) — Three bugs caused comments to be dropped when GraphQL rate limits were hit: deep-fetch failures weren't retried on the next poll, awaiting-input items weren't re-checked after recovery, and the `lastUpdatedAt` cache wasn't evicted on failure. All three fixed.
- **Blocked items not unblocked when dependency closes** — Closing a blocking issue doesn't change the blocked item's `updatedAt`, so it was never re-evaluated. Items with `fabrik:blocked` now bypass the `updatedAt` cache and are deep-fetched every poll to detect dependency closure.
- **Auto-upgrade temp file cleanup** (#241) — `syscall.Exec` replaces the process so deferred cleanup never runs. The download tarball is now explicitly removed before re-exec.

## Internal

- Additional tests for deep-fetch failure handling, awaiting-input bypass, and `lastUpdatedAt` eviction.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
