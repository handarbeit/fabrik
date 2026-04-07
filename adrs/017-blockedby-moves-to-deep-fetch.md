# ADR 017 — `blockedBy` moves from shallow board query to deep fetch

**Status**: Accepted
**Date**: 2026-04-07
**Supersedes**: ADR 016 (partially — see below)

## Context

ADR 016 placed the `blockedBy(first: 25)` fragment in the shallow board query
(`FetchProjectBoard`) so that the dependency gate check in `itemMayNeedWork`
could filter out blocked items before incurring the cost of a deep fetch
(`FetchItemDetails`). The stated rationale was to avoid per-item REST API calls
and to keep the decision logic in the cheapest possible phase.

Issue #230 aims to reduce overall GraphQL cost for multi-instance deployments.
The shallow query was identified as the primary cost driver: with `first: 25` on
`blockedBy`, `first: 100` on `labels`, and `first: 10` on `assignees`, a
100-item board page consumed ~14,000 GraphQL nodes. Removing these connections
from the shallow query is the single largest cost reduction available.

## Decision

`blockedBy` is moved from the shallow board query to `FetchItemDetails` (the
deep fetch). The dependency gate check moves from `itemMayNeedWork` to
`itemNeedsWork`, which runs after deep fetch.

Key implications:

1. **`blockedBy(first: 10)`** is now part of `FetchItemDetails` (reduced from
   `first: 25` per ADR 016; 10 blockers is sufficient for almost all cases with
   a warning logged for overflow).

2. **The dependency gate in `itemNeedsWork`** preserves all existing behavior:
   - First-stage items bypass the gate (no change).
   - Items blocked by open issues return `false` from `itemNeedsWork` (no dispatch).

3. **Items with changed blockers incur one extra deep-fetch call** compared to
   the ADR 016 approach. However, items whose `blockedBy` state changed will
   have a bumped `updatedAt` and would be deep-fetched anyway — so in practice
   the extra cost is negligible.

4. **Items with unchanged open blockers are still skipped cheaply**: the
   `updatedAt` filter in `itemMayNeedWork` catches them before deep fetch.

## Consequences

**Savings**: Removing `blockedBy(first: 25)`, `assignees(first: 10)`,
`labels(first: 100)`, `body`, `url`, and `author` from the shallow query saves
approximately 13,000–14,000 nodes per 100-item board page (down from ~14,000 to
~500 nodes/page for the label connection alone).

**Trade-off**: Items that were previously filtered by the dependency gate at the
`itemMayNeedWork` stage (saving a deep-fetch call) will now incur a deep fetch
before being filtered in `itemNeedsWork`. Items with open blockers that have NOT
changed are excluded by the `updatedAt` check and never reach deep fetch, so the
practical impact is limited to items with recently-changed (but still open)
blockers.

**Supersession of ADR 016**: ADR 016's placement of `blockedBy` in the shallow
query was correct for the optimization problem it was solving (avoiding per-item
REST calls). This ADR supersedes that placement decision. The avoidance of REST
calls is not affected — deep fetch is still a single GraphQL query per item, not
a REST call.
