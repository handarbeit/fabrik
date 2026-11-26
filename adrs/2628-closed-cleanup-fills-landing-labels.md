# ADR 2628: The Done cleanup of a closed item records a landed PR

## Status

Accepted. Extends decision 3 of ADR 2459.

## Context

A linked PR merged outside Fabrik (a human merge) while its item sits at Validate
closes the issue through `Closes #N`. A board with the built-in "Item closed ->
Done" workflow then moves the card to Done about a second later, usually before
the engine's next poll. The engine's first sight of the item is "closed, at
Done". The Validate terminal path (`advanceValidateTerminalItem`, which calls
`fillGateCheckedCompleteLabels`) and its closed-item owner `settleClosedValidateAdvance`
only act on items at Validate, and `settleClosedItemsToDone` skips cleanup
stages, so nothing records the landing: the Done cleanup applies
`stage:Done:complete` and the item ends without `stage:Review:complete` /
`stage:Validate:complete`. The live test `TestPausedMergedPRRecovery` failed on
the train-off legs of the v0.0.94 gate for this reason.

## Decision

1. `handleCleanupStage` gains one step for a closed non-PR item (the
   `closedCleanupItem` predicate, paused or not) that lacks `stage:<Done>:complete`:
   it reads the linked PR live with `classifyLinkedPRTerminal` (never cached
   state; ADR 2299, ADR 2350) and, when merged, calls `fillGateCheckedCompleteLabels`
   unchanged. The step lives in the cleanup rather than a new settle scan because
   the cleanup is already the only owner of a closed item at Done and
   `stage:<Done>:complete` is its re-admission stop.
2. Order: pause clearing (#2459), then the fill, then the worktree removal and
   the completion label.
3. A failed PR read or a failed fill leaves `stage:<Done>:complete` unapplied and
   records the same `periodic-re-eval` cooldown as a failed pause removal
   (`deferClosedCleanup`), so the whole cleanup retries. No linked PR, an open PR
   and a PR closed unmerged skip the fill and clean up as before. The fill is
   idempotent, so a retry after a partial fill converges.
4. No comment, board move, landing-verification marker or pause is added; one log
   line names the labels filled, only when some were. The selection rule is
   extracted as the pure `missingGateCheckedCompleteLabels`, shared by the fill
   and the log line, so the fill itself is unchanged in effect.
5. An open item, a PR card and an item already carrying `stage:<Done>:complete`
   are unchanged (the `closedCleanupItem` gate and the early return).
6. A per-Engine seam, `SetClosedCleanupFillDisabledForTest`, turns off only the
   fill (the pause clearing keeps working) for the sim non-vacuity test.
   `SetClosedPausedCleanupDisabledForTest` still disables the whole path, because
   the call is gated on `closedCleanupItem`.

## Consequences

An item first seen closed at Done with a merged PR ends with the same completion
labels the Validate terminal path would have given it. Residuals:

- The closed state comes from the shallow listing (the ADR 2459 residual); a
  just-reopened issue with a merged PR would still be filled. The merged PR is a
  durable fact and the fill only records it.
- A persistently failing PR read (for example a 403) keeps the cleanup retrying
  once per `githubRecheckInterval()` with no bound, as ADR 2459 does for the
  pause removal.
- `stage:Review:complete` / `stage:Validate:complete` are recorded though those
  stages never ran, the same meaning as the Validate terminal path.
- Merge-train members already carry `stage:Validate:complete`, so the fill is a
  no-op for them.
