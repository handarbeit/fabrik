# ADR 016: Use GraphQL `blockedBy` State for Cross-Repo Dependency Resolution

**Date**: 2026-04-07  
**Status**: Accepted

## Context

Issue #216 adds GitHub-native "blocked by" dependency relationships as pipeline gates. When an issue is blocked by another issue (same-repo or cross-repo), Fabrik must determine whether the blocking issue is closed before allowing advancement.

The spec originally called for a REST API call (`GET /repos/{owner}/{repo}/issues/{number}`) to check the closed state of cross-repo blocking issues. However, the `blockedBy` GraphQL field (GA since August 21, 2025) returns the `state` field for each blocking issue, **regardless of which repository it lives in**. This means the shallow board query already contains closure state for all blocking dependencies — including cross-repo ones.

## Decision

Use the `state` field returned by the `blockedBy` GraphQL nodes for dependency resolution for all cases (same-repo and cross-repo). Do not make per-gate REST calls to verify state.

The `blockedBy(first: 25)` fragment is added to the `... on Issue {}` fragment in the shallow board query (`fetchProjectBoard`). This makes dependency state available in `itemMayNeedWork` (which only has shallow data) and in `checkDependencies`.

## Rationale

- **No rate-limit amplification**: A per-gate REST call adds one API request per blocked issue per poll cycle. With multiple blocked issues and short poll intervals, this quickly accumulates against GitHub's API limits. The GraphQL state costs nothing extra per call — it's included in the already-required board fetch.
- **No latency overhead**: REST calls add synchronous latency at gate-check time (inside worker goroutines). GraphQL state is already fetched before any gate logic runs.
- **GraphQL is fresh enough**: The `blockedBy` state in the shallow query is at most one poll cycle old. For an SDLC workflow where pipeline stages take minutes to hours, one poll cycle of staleness (~30–60 seconds) is negligible.
- **Simpler implementation**: No extra client interface methods, no fallback logic, no per-issue caching.

## Constraints

- **At most one poll cycle stale**: If a blocking issue is closed between poll cycles, the gate will not unblock until the next board fetch. This is acceptable for SDLC workflows.
- **GHES compatibility**: Older GitHub Enterprise Server instances may not support the `blockedBy` field. If the field is absent from the GraphQL response, `BlockedByNodes.Nodes` will be nil/empty (JSON unmarshaling is lenient), and the item is treated as having no dependencies — fail open, not fail closed.
- **25-dep limit**: The `blockedBy(first: 25)` query covers up to 25 blocking dependencies per issue. Issues with more than 25 blockers will log a warning; only the first 25 are checked. This limit is generous for typical use.
- **Cross-repo permissions**: If the API token lacks read access to a cross-repo blocking issue, GitHub returns an empty `blockedBy` node list for that dependency. The engine will treat the dependency as non-existent and not gate on it. This is fail-open behavior.

## Alternatives Considered

### REST per-gate call for cross-repo dependencies

The `FetchIssue(owner, repo, number)` function in `github/issues.go` returns issue state via `GET /repos/{owner}/{repo}/issues/{number}`. This was the spec's original intent for cross-repo resolution.

Rejected because:
- GraphQL already returns cross-repo state in the same request
- REST adds latency and rate-limit pressure with no accuracy benefit
- Would require adding `FetchIssue` to the `GitHubClient` interface, updating all mocks

### Cache REST results within a poll cycle

If REST calls were used, caching the result per poll cycle would reduce repeated calls for the same blocking issue. This optimization is unnecessary since we use GraphQL.

## Consequences

- `checkDependencies` is purely in-memory — it reads `item.BlockedBy` (populated by the board fetch) and does not make any API calls for state resolution.
- Label mutations (`AddLabelToIssue`, `RemoveLabelFromIssue`) and comment posting (`AddComment`) are still made by `checkDependencies`, but these are write operations for status tracking, not reads for state resolution.
- `FetchIssue` remains in `github/issues.go` but is not added to the `GitHubClient` interface. It can be added later if a use case requires fresh REST-based state verification.
