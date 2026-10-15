# ADR 2280: The comment path honours FABRIK_BLOCKED_ON_INPUT

## Status

Accepted. Amends the assumption recorded with #1970 (`clearOrphanAwaitingInput`) that
only a stage run can emit the blocked marker.

## Context

`FABRIK_BLOCKED_ON_INPUT` was honoured only by stage dispatch (`finalizeStageOutcome`).
A comment-review run, or a review, CI-fix or rebase reinvoke (all
`processCommentsClassified`), that emitted it was ignored: the comment was marked
processed and the item carried on as if the question had been answered. A comment skill
that needs a human decision had to guess or complete.

## Decision

1. `processCommentsClassified` pauses the item through the existing `blockOnInput`
   (unchanged) when `!completed && CheckBlockedOnInput(output)`. Completion beats the
   marker, and an erroring run returns earlier, matching `finalizeStageOutcome`'s
   precedence. The check runs after `publishCommentOutput` (the issue-body update and spec
   persistence still happen) and before `finalizeComments`, so the pause exists when
   `clearOrphanAwaitingInput` runs and the triggering comment still gets 🚀. Only a new
   human comment resumes (ADR-1813, unchanged); no new resume path.
2. A blocked run bypasses `checkNoOpCommentCycle` and `checkCommentBreaker`, as the
   tools-denied and skill-load arms do. A question is not "no progress", and the breakers
   could otherwise post a second pause comment.
3. `processCommentsClassified` returns the sentinel kind `commentRunBlockedOnInput`.
   It is deliberately not a did-not-run kind: the run executed, so it must not feed the
   never-ran tally that drives pause messaging. `dispatchReinvoke` refunds the pre-charged
   cycle (a question is not a failed convergence attempt, and the human's answer needs the
   bound) and skips `opts.after`, whose CI no-op-SHA debounce, auto-merge re-enable and
   review refund are wrong on a just-paused item.
4. The pause write is best-effort, like the stage path's. Making it stricter is a separate
   change for both paths.

## Consequences

- Any comment run that emits the marker now pauses. The shipped comment skills and
  `buildCommentReviewPrompt` do not yet tell Claude it may; that is a follow-up. Until
  then this is reachable via an operator-authored `comment_prompt`/skill.
- A human reply posted while the run is in flight predates the pause and does not resume
  it (the ADR-1813 trade-off).
- Seam: `SetCommentBlockedOnInputDisabledForTest`; sim twin
  `tests/sim/comment_blocked_on_input_test.go`.
