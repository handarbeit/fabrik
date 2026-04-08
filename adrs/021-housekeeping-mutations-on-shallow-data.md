# ADR 021: Housekeeping Mutations Are Exempt from the Shallow-Data Read-Only Rule

## Status

Accepted

## Context

ADR 020 establishes that the shallow board query is a read-only filter: no mutations or actions may run on shallow-fetched data. All engine actions that transition state — stage runs, label mutations for advancing to the next stage, dependency checks, comment processing — must operate on fully-hydrated items that have received a `FetchItemDetails` call in the current poll cycle.

However, two existing functions in `poll()` perform label or GraphQL mutations on shallow-fetched data:

1. **`cleanupClosedIssueLocks(board)`** — removes stale `fabrik:locked:<user>` labels from issues that were closed while a stage was in-flight. It operates on `board.Items` (shallow data) and calls `RemoveLabelFromIssue`, a mutation.

2. **`archiveDoneCompleteItems(board)`** — archives project items whose status maps to a `CleanupWorktree` stage and that already carry the `stage:<Name>:complete` label. It operates on `board.Items` (shallow data) and calls `ArchiveProjectItem`, a mutation.

Both functions were added intentionally after ADR 020 was codified, and both conflict with ADR 020 in letter. This ADR documents the rationale for the exemption so future contributors can reason about when shallow-data mutations are acceptable.

## Decision

**Housekeeping mutations — idempotent, terminal, and non-advancing — are exempt from the shallow-data read-only rule.**

A mutation qualifies as housekeeping if it satisfies all three conditions:

1. **Idempotent**: Calling it multiple times produces the same result as calling it once. The operation is safe to repeat across poll cycles without accumulating side effects.
2. **Terminal**: The mutation applies only to items already in a final or closed state. It does not advance an item to a new stage or alter the item's pipeline progression.
3. **Non-advancing**: The mutation cannot cause `processItem` to be dispatched for the item, cannot change the item's `updatedAt` in a way that triggers a deep fetch, and cannot alter any state that a subsequent stage reads.

### Canonical examples

- **`cleanupClosedIssueLocks`**: Removes `fabrik:locked` labels from closed issues. Closed issues are terminal (they cannot re-enter the pipeline). Removing a stale lock label is idempotent and does not advance the issue.

- **`archiveDoneCompleteItems`**: Archives project items in the Done column that already carry `stage:Done:complete`. Done+complete items are terminal (the cleanup stage has already run and the completion label is already set). Archiving is idempotent per GitHub API docs. Once archived, items disappear from board results and the operation converges to a no-op. The archive does not affect any label or field that a subsequent stage reads.

### Non-examples (mutations that must not be exempted)

- Adding or removing pipeline labels (`stage:X:complete`, `stage:X:in_progress`, `stage:X:failed`) on non-terminal items — these advance the pipeline.
- Advancing an item's column status — this triggers dispatch.
- Any mutation that could cause `processItem` to run for the item in a subsequent poll.

## Consequences

**Positive:**

- Housekeeping passes keep the board clean without requiring a full deep-fetch cycle for each terminal item. This is efficient: Done items are never deep-fetched (the cleanup stage skips `FetchItemDetails`), so requiring a deep fetch for archiving would force an unnecessary API call per item on every poll until the board shrinks.
- The exemption is narrow and well-defined. The three conditions (idempotent, terminal, non-advancing) provide a clear test for whether a new housekeeping mutation qualifies.

**Negative / Trade-offs:**

- Future contributors may cite these functions as precedent for adding mutations to shallow-data loops. The three-condition test makes it harder to incorrectly extend the exemption. Any mutation that could affect pipeline progression must still go through a deep-fetch path.
- Shallow labels (`labels(first:5)`) may not include `stage:<Name>:complete` for items with many labels, causing the lazy migration in `archiveDoneCompleteItems` to silently skip them. This is an accepted trade-off: the item remains unarchived but the board is otherwise correct, and the cleanup stage path in `processItem` (which applies the label itself) is not affected by the shallow limit.

## Related

- ADR 020: Shallow Board Query is a Read-Only Filter (the rule this ADR exempts)
- Issue #247: Archive project items after Done cleanup to reduce board pagination (where `archiveDoneCompleteItems` was introduced)
