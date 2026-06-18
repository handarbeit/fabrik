# ADR 053: Paused-Item CI Recovery Loop

**Date**: 2026-06-07  
**Status**: Superseded by ADR-057 (in structure — the separate-loop pattern is replaced by a single owner; ADR-053's operational constraints carry forward into ADR-057)  
**Issue**: #845 — CI gate: handle merged/closed PRs and required-never-running checks

## Context

The Phase-1 catch-up loop in `poll()` skips all items carrying `fabrik:paused` unconditionally. This is correct for most paused states — a paused issue requires human intervention before any automated work should proceed.

However, one case exists where a paused issue should self-heal without human intervention: **Case A** — a PR that was merged externally while the issue was paused on a CI-gate timeout. The sequence is:

1. Stage completes → `handleStageComplete` → `fabrik:awaiting-ci` applied, `stage:X:complete` withheld
2. CI timeout fires → `pauseForCITimeout` → `fabrik:paused` + `fabrik:awaiting-input` applied
3. PR merged externally (by a human, or by GitHub auto-merge on a different issue)
4. Fabrik detects: issue is paused → skip all processing → issue remains stranded forever

The issue is now an orphan: merged in reality, "paused mid-Validate" in Fabrik's bookkeeping. `stage:Validate:complete` is never added, the board column never advances to Done, and the issue persists on the board indefinitely until manually corrected.

## Decision

Add a **separate lightweight loop** after the main Phase-1 / Phase-2 catch-up loop that scans for items carrying both `fabrik:paused` AND `fabrik:awaiting-ci` on a `wait_for_ci: true` stage. For each such item, the loop calls `e.client.FetchLinkedPR` (direct GitHub API — not `e.readClient`/boardcache). When `pr.Merged == true`:

1. `addCompleteLabelAndRemoveCI` — adds `stage:X:complete` and removes `fabrik:awaiting-ci`
2. Remove `fabrik:paused` and `fabrik:awaiting-input`
3. `advanceToNextStage` — moves the board column to Done

This runs entirely in the main poll goroutine (no new goroutines, no semaphore acquisition). It is read-only aside from label mutations and `advanceToNextStage`.

### Why a separate loop, not an exception in the main catch-up loop

The `isPaused` guard in the main loop exists to prevent automated re-invocation of stages on paused issues. Punching a hole in that guard for the merged-PR case risks accidentally running other catch-up logic (review gate, rebase gate, CI gate, Phase 2 advancement) on a paused item. The separate loop handles exactly the one condition that should self-heal (merged PR) and nothing else — it cannot accidentally trigger `dispatchCIFixReinvoke` or `dispatchRebaseReinvoke`.

### Why `e.client`, not `e.readClient`

The boardcache's `FetchLinkedPR` may return stale `Merged`/`State` from before the PR was merged — the cache is populated by the GraphQL board fetch and delta-applied by webhook events, but does not guarantee freshness when the polling interval is long or a webhook was missed. `checkAutoMergeConvergence` explicitly uses `e.client` (direct REST) for the same reason (documented in its code comment). This loop follows the same pattern.

### Why `advanceToNextStage` is unconditional (no yolo/cruise gate)

A merged PR is a definitive terminal event. The stage's work is done; the PR is on the main branch. There is no meaningful sense in which a "non-yolo" issue should remain stranded in Validate-paused after its PR has merged. Gating advancement on `fabrik:yolo`/`fabrik:cruise` would cause Case A to re-emerge: the issue would remain orphaned unless the user both unpauses it AND has yolo/cruise active.

This matches the behavior of `checkAutoMergeConvergence`, which also calls `advanceToNextStage` unconditionally when the PR merges (line 288 of `engine/merge_gate.go`), without consulting yolo/cruise.

### Why only `pr.Merged` is handled (not closed-not-merged)

A paused issue whose PR was closed without merging is already in the correct state: paused for human action. The issue carries an explanatory `fabrik:paused` + `fabrik:awaiting-input` state. Adding a second comment from the recovery loop would be noisy, and the closed-not-merged case is already actionable by the human (reopen the PR or close the issue). No recovery action is appropriate.

### REST overhead

The recovery loop adds one `FetchLinkedPR` REST call per paused+awaiting-ci item per poll cycle. In practice this is bounded by the number of paused issues with `fabrik:awaiting-ci` — typically zero to a handful. The overhead is negligible.

## Consequences

- Case A (PR merged externally while issue paused on CI timeout) self-heals on the next poll cycle after the merge, without requiring a manual unpause.
- The loop never dispatches workers, acquires semaphores, or invokes stages — it is safe to run unconditionally on every poll.
- The constraint "this loop must never dispatch workers" is enforced by code structure: the loop body contains no calls to `dispatchCIFixReinvoke`, `dispatchRebaseReinvoke`, `dispatchReviewReinvoke`, or `processComments`.
- Non-yolo issues whose PRs are merged while paused will auto-advance to Done. This is intentional and mirrors `checkAutoMergeConvergence`.

## Amendment (issue #874)

**Date**: 2026-06-16  
**Status**: Accepted

### Context

Production incidents #1336 and #1337 (2026-06-14) exposed a symmetric gap: the recovery loop only handled `fabrik:awaiting-ci`. Items paused while waiting on reviewers (`wait_for_reviews: true`) had their PRs merged externally, but `stage:Review:complete` was never added. The board column never advanced, and the issue remained stranded with an inconsistent label set.

### Changes

**Function rename**: The inline loop body was extracted from `poll.go` into a named method `runPausedItemMergedPRRecovery` on `*Engine` (in `engine/paused_recovery.go`). The previous name implied CI-only scope; the new name reflects the broader coverage.

**Expanded trigger condition**: The entry condition is now `fabrik:paused` AND (`fabrik:awaiting-ci` OR `fabrik:awaiting-review`). Items paused on the review gate are now eligible for recovery, not just CI-gated items.

**Multi-stage gap-fill**: Instead of calling `addCompleteLabelAndRemoveCI` for a single stage (the item's current board column), the function now:

1. Finds the highest-order stage already carrying `stage:<Name>:complete` in the item's labels.
2. Iterates `e.cfg.Stages` in ascending `Order` from the next stage onward, stopping before the first `CleanupWorktree: true` stage.
3. For each stage where `WaitForCI || WaitForReviews` is true and `stage:<Name>:complete` is absent, calls `e.client.AddLabelToIssue` with cache write-through (mirrors `addCompleteLabelAndRemoveCI` pattern).
4. Fails fast per item on any label-add error — no gate labels are cleared and `advanceToNextStage` is not called, preserving idempotent retry on the next poll cycle.

This handles the case where multiple gate-checked stages are missing their completion labels (e.g., an item paused at Review with both Review and Validate yet to be marked complete).

**Reviews gate cleanup**: After all completion labels are added, `removeAwaitingReviewLabel` is called when `fabrik:awaiting-review` is present. This removes `fabrik:awaiting-review` and `fabrik:bot-reprompted` with cache write-through, consistent with how `removeAwaitingCILabel` handles the CI gate.

**`addCompleteLabelAndRemoveCI` bypassed**: The new function calls `e.client.AddLabelToIssue` directly rather than going through `addCompleteLabelAndRemoveCI`. This is intentional — `addCompleteLabelAndRemoveCI` couples label-add and CI-gate-removal into one call, which does not compose with the multi-stage iteration. The Validate SHA-recording side-effect in `addCompleteLabelAndRemoveCI` is intentionally skipped: the PR is already merged when this code path runs, so the SHA-invalidation guard has no work to do.

### Preserved constraints

All original constraints from the initial ADR remain in force:
- The function runs in the main poll goroutine, never spawns goroutines, and never acquires `e.sem`.
- No calls to `dispatchCIFixReinvoke`, `dispatchRebaseReinvoke`, `dispatchReviewReinvoke`, or `processComments`.
- `e.client` (direct REST) is used for `FetchLinkedPR` — not `e.readClient` — for freshness.
- `advancedItems` is updated so the cooldown defer skips the healed item.
- `advanceToNextStage` is unconditional (no yolo/cruise gate), for the same reasons as before.
