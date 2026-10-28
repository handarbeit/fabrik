# ADR 2459: A closed paused item at Done is cleaned up

## Status

Accepted

## Context

`itemNeedsWork` skipped every `fabrik:paused` item at a cleanup stage, and
`processItem` returned at its awaiting-input and paused gates before the cleanup
branch. A closed issue that reached Done while paused therefore never got
`stage:Done:complete`; archiving and the worktree janitor both key on that label,
so the worktree, the `fabrik/issue-N` branch and the board card leaked until the
pause label was removed by hand. Skipping an *open* paused item is intentional
(its worktree may hold unfinished work); a *closed* one has nothing to protect.

## Decision

1. `closedCleanupItem` (cleanup stage, `IsClosed`, not a PR) is the one
   predicate. `itemNeedsWorkRetryAt` does not apply the pause veto to it
   (`itemMayNeedWork` has no pause check, so both admission functions agree).
2. `processItem` hoists such an item to `handleCleanupStage` right after the
   lock and `fabrik:editing` checks, ahead of the awaiting-input and paused
   gates. Those gates would skip it or, on a human comment, run Claude on a
   closed item via `processComments` (ADR 1387). Neither `resumeAuthorised`
   (ADR 1813) nor `checkDependencies` applies to a terminal item.
3. `handleCleanupStage` clears `fabrik:paused` and `fabrik:awaiting-input`
   (unconditionally and ErrNotFound-tolerantly, since shallow cleanup items can
   have incomplete labels) *before* the worktree removal and the completion
   label. The completion label is the only stop on re-admission, so applying it
   first would strand the pause labels. A real removal error returns early with
   a `periodic-re-eval` cooldown and the whole cleanup retries next poll.
4. No comment is posted (nobody waits on a closed issue); one log line is
   written when the visible labels carried a pause label.
5. One per-Engine seam, `SetClosedPausedCleanupDisabledForTest`, restores the
   old behaviour at all three points for the sim non-vacuity test.

## Consequences

The item ends in the same state as an unpaused closed item and follows the
normal archive schedule. Open paused items are unchanged. A just-reopened issue
could briefly be seen as closed from the listing; the effect is limited to
clearing the pause and reaping the worktree of an item already at Done, the same
edge as `settleClosedItemsToDone` (ADR 064). The janitor, the archive scan and
`isTerminalPredicate` are untouched.
