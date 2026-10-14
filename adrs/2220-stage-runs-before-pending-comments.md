# ADR 2220: A stage runs before comments pending on entry

## Status

Accepted

## Context

`processItem` sent an item to `processComments` whenever `findNewComments` returned anything, ahead of the stage-run path, without asking whether the stage had run. The comment skills assume the stage already produced its output, and `finalizeComments` accepts `FABRIK_STAGE_COMPLETE` from the comment pass. A comment posted while the item sat in an earlier column was therefore waiting when it entered the next stage, and that stage's comment skill could complete it alone (seen on #2218): no spec expansion in Specify, no Plan comment, no research comment, no draft PR.

## Decision

Run the stage first and leave the pending comments for the comment path afterwards.

1. **Routing.** `processItem` and its admission mirror `itemNeedsWork` route a pending comment to `processComments` only when `stageRanOnEntry` holds. Otherwise they fall through to the ordinary stage-run checks (complete label, retry cooldown, dispatch).
2. **Definition of "ran".** Durable state only: `stage:<S>:complete`, `stage:<S>:failed`, `fabrik:reworking:<S>`, `fabrik:awaiting-ci`/`awaiting-review`/`awaiting-done`, the store's engine-pause record, the stage's own `🏭 **Fabrik — stage: <S>**` output comment, or an authorised resume from a pause in the same call (ADR-1813 resume path unchanged). Anything undeterminable counts as "not run".
3. **No watermark from the stage run.** `markCommentsSeenByStage` skips comments `findNewComments` reports as pending. Without this the stage would consume the comment and the comment pass would never happen.
4. **No-work gate.** `handleNoWorkNeeded` was the one stage-completion advance outside `feedbackGateBlocks` (ADR-1953-a). It now consults it before writing `fabrik:awaiting-done`; on a hold it adds only `stage:<S>:complete` and returns.

The advance after the stage run needs no new code: `handleStageComplete` already holds it via `feedbackGateBlocks`, the item parks at `stage:<S>:complete`, and the next poll's ordinary path runs the one comment pass under ADR-1802's rework bracket.

## Alternatives rejected

- **Column-entry time (FR-002's example, "output newer than the latest move into the column").** The engine has no durable column-entry time: `StatusEnteredAt` and `StatusMovedAt` reset on restart. A `PROJECT_V2_ITEM_STATUS_CHANGED_EVENT` read would need a new client method, simgh support and a wire fixture. The engine already treats `stage:<S>:complete` as "ran at all", whatever column the item moved through; this follows it. An item moved back into a stage that has an old output comment but no complete label gets the comment path, as it did before. A timeline read is a possible follow-up.
- **Labels only.** A stage that blocked on input and was resumed by a comment pass that did not complete it leaves no label, so its next comment would run the stage again (caught by `TestSpecifyPersistSpec_MultiRoundClarification`). The output comment covers it. Its weakness is that an incomplete run also posts partial output, so a comment arriving after that goes to the comment skill — the behaviour before this change, and what FR-003 specifies ("blocked on input or still being reworked").
- **Passing pending comments to the stage prompt as "New Comments".** Out of scope; changes prompts and bookkeeping.
- **In-memory "ran" set.** Lost on restart (FR-002).

## Consequences

- One extra comment-pass invocation when a comment was waiting.
- A stage that ends incomplete with no output and a pending comment is retried as a stage run under its normal backoff; the comment waits.
- `docs/state-machine.md` §4.4/§4.5 and `docs/stage-lifecycle.md` describe the rule. Seam: `Engine.SetStageFirstRoutingDisabledForTest`; sim twin `tests/sim/stage_first_comment_test.go`.
