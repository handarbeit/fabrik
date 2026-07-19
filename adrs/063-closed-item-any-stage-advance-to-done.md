# ADR 063: Closed-Item-At-Any-Stage Advance To Done

**Date**: 2026-07-19
**Status**: Accepted
**Issue**: #1014 — fix(engine): advance closed-but-not-Done board items to Done so the Done cleanup + archive run on them

## Context

`itemMayNeedWork`/`itemNeedsWork` (`engine/item.go:118`/`:195`) admit a closed board item only if its current stage is a cleanup stage, carries `stage:<stage>:complete`, `fabrik:awaiting-ci`, `fabrik:auto-merge-enabled`, or is gate-checked (`stageIsGateChecked` — currently only Validate). A closed item sitting at any other column — Specify, Plan, Implement, Review, Backlog — matches none of these and is permanently dropped by the admission guard: it is never dispatched again.

`runValidatePRTerminalAdvance` (ADR-057) already solves this exact transition for Validate-stage items: it fills any missing gate-checked completion labels, clears gate labels, and calls `advanceToNextStage` once a linked PR reaches a terminal state. But it is deliberately scoped to `stage.Name == "Validate"` only, and — more fundamentally — it iterates `deepFetchCandidates`, which a closed item at a non-admitted stage never reaches in the first place. Widening that function's scope would not fix the bug; the item never reaches its input set.

At scale, this produced four boards (observed 2026-07-19) with hundreds of stale closed items accumulated at non-Done columns, each with a leaked worktree that was never reaped and a board row that was never archived, requiring hand-archival to clear.

ADR-060/061/062 established the settle-owner pattern for structurally similar problems — a durable marker plus an unconditional per-poll scan sourced from `board.Items` (not `deepFetchCandidates`) for items that can never pass the ordinary dispatch admission gate. This ADR applies that same `board.Items`-sourcing insight, but — as detailed below — does not need the marker/retry/escalation half of the pattern.

## Decision

Add a new unconditional per-poll settle scan, `settleClosedItemsToDone` (`engine/closed_item_advance_settle.go`), that iterates `board.Items` directly. For every item matching `item.IsClosed && stage != nil && !stage.CleanupWorktree && !stage.HoldingStage && !stageIsGateChecked(stage)`, it moves the item's board Status directly to the cleanup (Done) stage via `UpdateProjectItemStatus`, mirroring `advanceToQueued`'s shape (status-field lookup, API call, `boardcache.CacheImpl` write-through, webhook echo) — but with no completion label to add, since this scan has no per-stage bookkeeping responsibility.

The cleanup stage is located via a new helper, `cleanupStage(cfg Config) *stages.Stage` (`engine/stages.go`, next to the existing `holdingStage`), which returns the lowest-`Order` stage with `CleanupWorktree: true` — never a hardcoded stage name, per the existing convention pinned by `TestRunStartupTerminalScan_UsesCleanupStageNotHardcodedDone`.

Gate-checked stages (currently only Validate) are explicitly excluded, so this scan never touches an item `runValidatePRTerminalAdvance` is already the exclusive owner of — no double-advance, no race.

**No durable marker, no retry counter, no escalation.** Unlike ADR-060/061/062, this scan's trigger condition is a pure function of durable board state (`item.IsClosed`, `item.Status`) re-derived fresh from `board.Items` on every poll. There is no multi-step sequence to protect against losing on restart, and no marker that could leak or go stale. The predicate itself is the idempotency check: once the item reaches the cleanup column, `!stage.CleanupWorktree` is false and the scan stops touching it. A failed `UpdateProjectItemStatus` call is simply retried on the next poll — logged as a warning, nothing more.

**Deliberately ignores all other label state.** `fabrik:paused`, `fabrik:awaiting-input`, `fabrik:blocked`, etc. are not checked. A closed issue sitting at a non-terminal column supersedes any in-flight gate or lock label: no further pipeline work can occur on it regardless of what else is set, so there is nothing left for those labels to protect.

## Rationale

### Why not widen `runValidatePRTerminalAdvance` itself?

Its completion-label-filling logic exists specifically because Validate is gate-checked and downstream tooling depends on gate-checked stages' `stage:X:complete` labels being present before Done. A closed item at an ordinary, non-gate-checked stage has no such labels to fill and no such downstream dependency — it only needs to *reach* Done so the pre-existing `CleanupWorktree` dispatch branch takes over. More fundamentally, `runValidatePRTerminalAdvance` iterates `deepFetchCandidates`; a closed item stranded at a non-admitted stage never reaches that input set at all, so widening the function's internal scope check would be dead code without also re-sourcing it from `board.Items` — at which point it is no longer the same function, and conflating the two responsibilities (Validate-specific label-filling vs. universal reachability repair) would make both harder to reason about. Two focused settle-owners, disjoint by `stageIsGateChecked`, is simpler than one function serving both purposes.

### Why no marker, breaking from ADR-060/061/062's pattern?

Those markers exist because the recovery they protect spans a *sequence* of calls (ADR-060: up to ten; ADR-061: a single at-risk call positioned after several already-completed side effects; ADR-062: one call among three failure branches) where losing track mid-sequence on an engine restart would silently resurrect the original bug. This scan has no sequence — it is a single, self-contained `UpdateProjectItemStatus` call whose precondition (`item.IsClosed` at a non-Done, non-Holding, non-cleanup, non-gate-checked column) is itself durable GitHub state, not an engine-side decision that could be forgotten. Adding `itemstate.StageRetryIncremented`/`MaxRetries`/escalation machinery here would copy ADR-060's shape without its justification.

### Why is gate-checked exclusion sufficient to prevent racing with `runValidatePRTerminalAdvance`?

Both settle-owners run unconditionally every poll in the same main poll goroutine (no concurrent dispatch, no `e.sem` acquisition), and their scopes are provably disjoint by construction: `runValidatePRTerminalAdvance` only ever touches items where `stage.Name == "Validate"`; `settleClosedItemsToDone` explicitly skips every item where `stageIsGateChecked(stage)` is true (which includes Validate, since Validate sets `wait_for_ci: true` in the default pipeline). No hardcoded `"Validate"` string appears in the new code — using the same `stageIsGateChecked` helper `runValidatePRTerminalAdvance` itself is scoped by keeps the two functions correct even if a second gate-checked stage is ever configured.

## Consequences

**Positive:**
- A closed issue can no longer be permanently stranded outside the Done cycle regardless of which column it was closed at. Worktree reaping, `stage:Done:complete` labeling, and terminal-skip cheapening on subsequent polls all now happen automatically via existing, unmodified machinery.
- The fix is small and proportionate: one new ~70-line file, one new ~10-line helper, one call-site wiring in `poll.go`. No new label, no new `itemstate.Mutation` type, no new configuration surface.
- `settleClosedItemsToDone` and `runValidatePRTerminalAdvance` remain provably non-overlapping via `stageIsGateChecked`, following ADR-057's own precedent for avoiding settle-owner races.

**Negative / Trade-offs:**
- **Archival gap persists.** `archiveDoneCompleteItems` remains disabled in production (`engine/poll.go`, tracked separately by #687). This fix resolves the worktree-leak and permanent-dispatch-stall halves of the reported bug, not the "board fetch stays inflated forever" framing — a formerly-stranded item now reaches a cheap, terminal-skip-eligible Done state, but still occupies a board row until #687 ships or an operator archives manually.
- **One extra `stages.FindStage` + label-set scan per closed item per poll**, bounded by total board size — the same constant-time-per-poll cost already paid by `cleanupClosedIssueLocks`/`cleanupClosedIssueTransientLabels`/the child-placement scan, all of which iterate `board.Items` unconditionally every cycle.
- **No retry-count visibility.** Because this scan has no marker or counter, an operator cannot distinguish "will retry next poll" from "has been silently failing for weeks" the way `fabrik:awaiting-placement`'s `Attempts` counter allows. This was judged acceptable: the failure modes that could cause a persistent failure here (missing status field, missing status option) are themselves visible via existing warning logs and, in the missing-status-option case, would also block every other Status-mutating call site in the engine — not a silent, isolated failure.

## Related Work

- [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md) — the direct precedent this ADR generalizes: "why only Validate" scoping is preserved via `stageIsGateChecked` exclusion rather than reopened.
- [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md), [ADR-061: Merge-Train Singleton Member-Issue Close Retry](061-merge-train-member-close-retry.md), [ADR-062: Child Board-Placement Retry Marker](062-child-board-placement-retry-marker.md) — the settle-owner pattern family this ADR joins; explicitly deviates from their marker/retry/escalation half for the reasons above.

**References:** [docs/state-machine.md §6.11](../docs/state-machine.md)
