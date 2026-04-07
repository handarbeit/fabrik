# ADR 017: Shallow Board Query is a Read-Only Filter

## Status

Accepted

## Context

The Fabrik engine fetches the GitHub Project board in two phases:

1. **Shallow fetch** (`fetchProjectBoard`): A single GraphQL query that retrieves all board items with their status, labels, updatedAt timestamp, and basic metadata. This query is cheap and fetches the entire board in one round-trip.

2. **Deep fetch** (`FetchItemDetails`): A per-item GraphQL query that populates comments, linked pull request references, and any other fields that require a separate call. This is expensive: one API call per item.

The shallow fetch returns enough data to filter — to decide which items *might* need work. But it does not return complete data. Specifically:

- `BlockedBy` is currently included in the shallow query, but this may change (see issue #230, which proposes moving it to the deep fetch to avoid pagination limits and keep the shallow query minimal).
- `Comments` are never included in the shallow query.
- `closedByPullRequestsReferences` are never included in the shallow query.

Before this ADR was codified, several bugs arose from acting on shallow data as if it were complete:

- **Bug 1** (`issue #231`): `processItem` had no dependency gate before stage work began. An issue manually moved to a column with open `BlockedBy` deps would run a full Claude stage (burning turns and API quota) before the gate fired at advance time.
- **Bug 2** (`issue #231`): The yolo catch-up loop in `poll()` iterated all `board.Items`, including items that were never deep-fetched. If `BlockedBy` were moved to the deep fetch, these items would appear to have no blockers and be incorrectly advanced.

## Decision

**The shallow board query is a pure read-only filter. No mutations or actions may run on shallow-fetched data.**

All engine actions — stage runs, label mutations, advancing to the next stage, comment processing, dependency checks, yolo catch-up — must operate on fully-hydrated items that have received a `FetchItemDetails` call in the current poll cycle.

Concretely:

1. The deep-fetch loop in `poll()` builds a `deepFetchedIDs` set of item numbers that were fully hydrated this cycle.
2. The yolo catch-up loop gates on `deepFetchedIDs[item.Number]` before doing anything. Items not in the set are silently skipped.
3. `processItem` (which is only ever called for items that passed the deep-fetch loop) calls `checkDependencies` before any stage work begins, providing an early gate at stage start in addition to the existing gate at stage advance time.

The shallow query answers exactly one question: "Does this item need a deep fetch?" Based only on `updatedAt`, `status`, `number`, and `repo`.

## Consequences

**Positive:**

- Eliminates a class of bugs where the engine acts on incomplete data. Future contributors cannot accidentally add a mutation to the yolo catch-up (or any other shallow-data loop) without hitting the gate.
- Makes the data boundary explicit and structural, not reliant on the current set of fields in the shallow query.
- Moving fields out of the shallow query (e.g., `BlockedBy` per issue #230) is safe — the engine will not act on items that weren't deep-fetched regardless.

**Negative / Trade-offs:**

- Items that pass the shallow filter but turn out to not need work after deep fetch incur one extra API call. This is acceptable: false positives in the shallow filter are cheap (one API call); acting on incomplete data is expensive (wasted Claude turns, incorrect state transitions).
- The yolo catch-up loop now skips items that haven't changed since the last poll cycle (since those items fail `itemMayNeedWork` and are not deep-fetched). In practice this is correct: a yolo item that truly needs advancing will have had its `updatedAt` updated when the stage-complete label was added.

## Related

- Issue #230: Move `blockedBy` to deep fetch (structural enforcement of this principle for the dependency field)
- Issue #216: Dependency relationships as pipeline gates (parent feature)
- Issue #231: Dependency gate should block stage start, not just stage advance (where this principle was codified)
- ADR 016: GraphQL state for dependency resolution (how dependency state is fetched)
