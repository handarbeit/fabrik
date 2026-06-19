# ADR 057: Single-Owner Validate PR Terminal Advance

**Date**: 2026-06-18  
**Status**: Accepted  
**Supersedes**: ADR-053 (in structure; ADR-053's constraints carry forward)  
**Issue**: #887 — feat(engine): single authoritative "settle PR → advance to Done" owner

## Context

The "Validate-stage linked PR reached a terminal state (merged or closed) → advance the board to Done" transition had no single owner. Three separate code paths managed it:

1. **Phase-2 Validate merged-PR fallback** (`poll.go`): when `attemptMergeOnValidate` fails (e.g. repo has auto-merge disabled), a fallback checked whether the PR was already merged and advanced inline. Yolo-only; gated behind `attemptMergeOnValidate` failure.

2. **`runPausedItemMergedPRRecovery`** (`engine/paused_recovery.go`): handled `fabrik:paused` AND (`fabrik:awaiting-ci` OR `fabrik:awaiting-review`) items. Iterated pipeline stages, filled missing gate-checked `stage:<X>:complete` labels, cleared gate labels, advanced.

3. **Convergence-paused recovery loop** (`poll.go`): handled `fabrik:paused` AND NOT(`fabrik:awaiting-ci` OR `fabrik:awaiting-review`) AND `stage:X:complete` items. Removed pause labels and advanced.

The non-overlap between paths 2 and 3 was maintained by hand-crafted label negations. Issue #874 (wait_for_reviews paused items not advancing) was a direct consequence of this fragility: path 2 guarded on `awaiting-ci OR awaiting-review`, path 3 guarded on NOT(`awaiting-ci OR awaiting-review`) — and a third pause variant would strand items in both. The bug class was structurally open; each instance required adding another loop.

ADR-056 (§D2) identified this pattern as non-composing and specified consolidation into a single owner that runs regardless of gate label.

## Decision

Replace the three paths with a single function, `runValidatePRTerminalAdvance`, which:

- Runs after Phase 1 and Phase 2 of the catch-up loop, over all `deepFetchCandidates`.
- Scopes exclusively to Validate-stage items (items in other stages with merged PRs and `fabrik:paused` are out of scope — see below).
- Skips items with `fabrik:auto-merge-enabled` (handled exclusively by `checkAutoMergeConvergence`, Phase 1).
- Skips items already in `advancedItems` (prevents double-advance).
- For each remaining item: calls `e.client.FetchLinkedPR` (direct REST — not `e.readClient` — for freshness); then:
  - **Merged PR**: fills missing gate-checked `stage:<X>:complete` labels in ascending Order from the highest already-complete stage, fail-fast on label-add error for idempotent retry; clears all gate labels (`removeAwaitingCILabel`, `removeAwaitingReviewLabel`, `removeRebaseNeededLabel`, removes `fabrik:paused`/`fabrik:awaiting-input`); calls `advanceToNextStage()`; marks `advancedItems`.
  - **Closed-without-merge PR**: calls `pauseForPRClosedNotMerged()` unless the item is already paused (to avoid duplicate comments on re-poll).
  - **Open PR**: skip.
- Runs **regardless of which gate label** is present. No label negation required. A new pause-label variant cannot strand a Validate item.

## Rationale

**Why Validate-only scope?**  
The spec (issue #887 requirement 1) explicitly restricts the single owner to Validate-stage items. Non-Validate items with merged PRs and `fabrik:paused` were only handled by `runPausedItemMergedPRRecovery` when the item was at Review stage — a case now intentionally dropped. Such items require manual label intervention (`fabrik:paused` removal), after which the normal catch-up loop advances them. This is a rare edge case and the behavioral change is documented.

**Why direct `e.client.FetchLinkedPR` (not `settlePRMergeState`)?**  
`settlePRMergeState` uses `e.readClient` (boardcache) for performance in Phase 1. The single owner runs after Phase 1 and needs live PR state — the boardcache may still reflect pre-merge data. This mirrors the choice made in the deleted `runPausedItemMergedPRRecovery` and `checkAutoMergeConvergence`. See ADR-053 §Constraints (carried forward).

**Why delete, not make dormant?**  
Dead code cannot rot safely. The deletion makes the structural invariant — "single owner" — explicit and verifiable. Any future contributor who adds a new pause label will see `runValidatePRTerminalAdvance` and understand that no additional recovery loop is needed.

**Why no goroutines or `e.sem` acquisition?**  
Carries ADR-053's constraints. The function is a label-mutation-only scan: it touches GitHub API for label changes and `advanceToNextStage`, but never dispatches Claude invocations. All code runs synchronously in the main poll goroutine.

## Constraints Carried Forward from ADR-053

- Runs in the main poll goroutine; no `go` statements; no `e.sem.Acquire`.
- Uses `e.client` (direct REST) for PR state, not `e.readClient` (boardcache).
- `advanceToNextStage()` called unconditionally on confirmed merged PR (no yolo/cruise gate check at this layer).
- All `AddLabelToIssue` / `RemoveLabelFromIssue` calls include the `boardcache.CacheImpl` write-through pattern.
- Fail-fast on label-add error (idempotent retry on next poll).

## Consequences

**Positive:**
- A new gate label (`fabrik:awaiting-X`) cannot strand a Validate item. No disjointness maintained by label negation anywhere.
- The board-drift repair intent of closed #883 / PR #885 is absorbed as a natural consequence of the advance path — no separate race-guarded scanner needed.
- Codebase removes ~120 lines of recovery loop code and ~220 lines of tests, replaced by a single ~120-line function with a more comprehensive test suite.

**Negative / Trade-offs:**
- **Validate-only scope change**: Review-stage items with `fabrik:paused` + `fabrik:awaiting-review` + merged PR previously auto-recovered via `runPausedItemMergedPRRecovery`. They now require manual `fabrik:paused` removal. This is rare and the tradeoff is explicitly accepted.
- **Extra REST calls**: The single owner calls `e.client.FetchLinkedPR` for every Validate-stage item on every poll, including non-paused items that Phase 1 already evaluated via `settlePRMergeState`. The `advancedItems` guard eliminates double-advance but not the extra call. For typical boards (few Validate-stage items), this overhead is negligible.
