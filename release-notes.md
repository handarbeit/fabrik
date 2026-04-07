# Fabrik v0.0.13

## Features

### Optimized shallow board query for multi-instance deployments (#230)

The shallow board query has been stripped down to only the fields needed for change detection: `id`, `number`, `status`, `updatedAt`, `state`, `title`, and `labels(first:5)`. Heavy fields like `body`, `author`, `assignees`, and `blockedBy` now load only during deep fetch. This dramatically reduces GraphQL rate limit consumption — critical when running multiple Fabrik instances on the same GitHub token.

### Dynamic poll backoff on rate limit pressure (#230)

When GraphQL rate limit remaining drops below 20%, Fabrik automatically doubles its poll interval and logs a warning. The interval recovers when the rate limit replenishes. This prevents hitting the hard 5,000 points/hour cap during heavy usage.

### Dependency gate at stage start (#231)

Issues with open `Blocked by` dependencies are now checked before running a stage, not just at advance time. Previously, a blocked issue could burn Claude turns on a stage only to be stopped when trying to advance. The gate now fires at the top of `processItem`, preventing wasted work.

### FABRIK_DECOMPOSED marker for issue splitting (#39)

Plan can now signal `FABRIK_DECOMPOSED` when it splits an issue into sub-issues. The engine detects this marker and advances the parent directly to Done, bypassing Implement/Review/Validate.

### Pending-reviewer gate for yolo auto-advance (#227)

In yolo mode, stages that create or update PRs now wait for pending reviewers before auto-advancing, preventing premature advancement past Review.

### Closed issue handling

Closed issues are now skipped entirely during processing. Stale lock labels on closed issues are cleaned up automatically.

## Fixes

- Yolo catch-up loop now only operates on deep-fetched items, preventing actions on incomplete data
- Dispatch loop iterates deep-fetch candidates instead of the full board, matching the shallow/deep split
- `cleanupClosedIssueLocks` suppresses benign 404 warnings for already-removed labels

## Internal

- ADR 020: Shallow board query is a read-only filter — no mutations without deep fetch
- `/cut-release` skill for Claude Code sessions to automate release note generation
- Plan skill updated to support issue decomposition assessment
- `FetchItemDetails` extended to populate body, URL, author, labels, assignees, and blockedBy
- Project context file (`.fabrik-context/project.md`) written before stage invocation

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
