# Fabrik v0.0.45

## Fixes

- **`FindPRForIssue` now uses core REST instead of search API (#430).** Previously hit `/search/issues` which has a 30/minute rate limit — heavy polling on boards with many items exhausted the search quota (observed as `REST: 28/30 remaining` in logs) even when core REST and GraphQL had plenty of headroom. Now uses `/repos/{owner}/{repo}/pulls?head=...` via `FetchLinkedPR`, which is core REST with a 5000/hour limit — ~167x more quota headroom. Same function signature, zero caller changes.

## Improvements

- Clarified `fabrik:blocked` re-evaluation documentation — blocked items are deep-fetched on cooldown expiry, not forced on every poll (stale doc from before #420's bypass removal).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
