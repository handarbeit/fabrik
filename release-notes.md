# Fabrik v0.0.51

This release adds resilience to GitHub Projects v2 indexer degradation. During an April 2026 GitHub outage, the same project-board GraphQL query — hit back-to-back within a single second — returned either {nodes:100, totalCount:136} or {nodes:0, totalCount:0} at random, with no errors and HTTP 200 in both cases. Fabrik accepted the empty responses verbatim, logged "found 0 items on board", and silently went idle. Items on page 2 of large project boards (>100 items) were especially affected — they were never deep-fetched, so auto-merges and cross-stage advancements stalled even though the issues themselves were healthy on GitHub.

## Fixes

- **Retry project board fetch on indexer-degraded responses.** `FetchProjectBoard` now queries `totalCount` alongside the items pagination, tracks the maximum totalCount observed across pages (in case the indexer flaps mid-pagination), and retries the entire fetch up to 3 times (1s, 2s linear backoff) when the raw node count is below the reported totalCount. After the last attempt, accept whatever we got — a project that consistently reports 0/0 is treated as genuinely empty. The comparison is against the raw GraphQL node count, not Fabrik's filtered `board.Items` list, so projects with non-Issue/non-PR items don't false-positive on the mismatch detector.

- **Visibility for indexer retries.** When the retry fires, Fabrik logs `[warn] project board fetch returned N items, totalCount=M (attempt X/3) — retrying in case of indexer hiccup`. Lands in `fabrik.log` alongside other engine logs so future GitHub-side flapping is diagnosable from the local fabrik.log without needing reproduction.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
