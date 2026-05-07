# Fabrik v0.0.54

This release stabilizes the post-cache-refactor codebase: webhook reliability, gate latency, and observability are all substantially improved. The headline change is that webhook health is now derived from a periodic reality reconcile rather than from event-arrival timing, eliminating false-positive flapping during quiet periods.

## Features

- **Reconcile-driven webhook health** (#641) — webhook health state is now derived from a periodic light reconcile against GitHub (default 3 min), not from "no events for N seconds" timing. Idle boards no longer trigger spurious `unhealthy` transitions; genuine cache drift is detected and reconciled within one cycle.
- **Mutation echo-check** (#642) — fabrik now tracks each outgoing GitHub mutation and expects a matching webhook event within a short window. K consecutive echo misses across distinct mutations flag the webhook stream as unhealthy. Detects silent webhook stream failure within seconds during active periods.
- **Push-wake on awaiting-review and awaiting-ci** (#616) — gate clearance now fires within ~2 seconds of the relevant webhook (Copilot review submission, CI completion) instead of waiting for the catch-up loop. Direct effect: end-to-end pipeline runs are minutes shorter.
- **Webhook circuit breaker + cache fallback** (#628) — after 3 consecutive HTTP 422 errors creating a webhook subscription, fabrik switches to poll-only mode rather than thrashing indefinitely. Cache reads automatically fall through to GitHub when the webhook stream is unhealthy.
- **Per-repo webhook failure isolation** (#631) — if one repo on a multi-repo board fails webhook subscription (auth scope, deleted repo), fabrik quarantines that repo and continues subscribing to the others rather than crashing the whole loop.
- **Orphan webhook cleanup at startup** (#643) — fabrik now deletes its own previous-session webhook claims at startup, preventing the "Hook already exists" 422 trap when the gh subprocess crashed mid-session.

## Fixes

- **PR-to-issue mismapping from greedy closing-keyword regex** (#605) — PR bodies that mention prior issues in prose ("before fixes #598 and #599 landed") no longer cause fabrik to mismap the new PR to those mentioned issues. Authoritative mapping is now established at PR creation time and confirmed via tightened regex.
- **PR creation before output posting in Implement** (#608) — `post_to_pr: true` stages now reliably post to the linked PR. Previously a race could land output on the issue if posting fired before PR creation completed.
- **Spurious `fabrik:awaiting-review` at Validate-complete** (#617) — the review gate is no longer re-evaluated at Validate completion. Closed issues no longer carry stale `fabrik:awaiting-review` labels into Done.
- **404 noise on label removal** (#607) — `RemoveLabel` calls for labels that aren't present no longer log spurious warnings; `ErrNotFound` is treated as success.
- **`UpdateRepos` is now additive** (#637) — known repos are never dropped from the webhook subscription set based on a single poll's incoming view. Eliminates spurious "new repo discovered" subprocess restarts triggered by transient cache state.
- **PR mergeable retry on transient 5xx** — `markPRReady` now retries with bounded backoff on transient GitHub errors instead of failing silently.
- **Push-based dep-blocked unblock observer extended** — `PushUnblockObserver` now fires on `BlockedByChanged` in addition to `StateChanged`, closing a deep-fetch ordering gap that left some dependents stuck after their blocker closed.
- **bot-reprompted review-gate corrections** — multiple fixes around how fabrik handles bot reviewers and the escalation ladder; review reinvoke is more robust under retry conditions.

## Improvements

- **Logs are quieter and more informative** — the misleading "auto-advance catch-up" prefix on routine stage transitions is dropped (#619); auto-heal logs are suppressed when the prToKey index already resolves the mapping (#618); webhook health transitions now include the elapsed-since-last-event duration for diagnosis.
- **Push-unblock latency** is consistently ~10–25 seconds end-to-end on typical issue close → dependent dispatch.
- **Health threshold tuning** (#638) — initial false-positive flapping was eliminated by raising the silence threshold from 60s to 5min; superseded entirely by #641's reconcile-driven design.

## Internal

~400 commits since v0.0.53. Significant refactoring of the webhook manager, cache layer, and observer wiring. New ADRs for echo-check and reconcile-driven health. Extensive test coverage added across all the new mechanisms. Eight live smoke-test pair runs validated each layer of fixes in production.

## Documentation

- New `docs/design/multi-user-vs-bot-mode.md` design exploration draft (NOT an ADR — explicitly marked as in-progress thinking) covering the per-user vs bot-mode topology tradeoffs surfaced by GitHub's single-user webhook constraint.
- `USER_GUIDE.md` updated with notes on the single-user webhook constraint and operator recovery procedures.
- ADRs added covering recent architectural decisions (echo-check, reconcile-driven health, etc.).

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly (auto-detects OS and architecture)
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```
