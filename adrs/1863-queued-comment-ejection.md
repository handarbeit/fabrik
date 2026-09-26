# ADR 1863: Queued Comment Ejection

**Date**: 2026-09-26
**Status**: Accepted
**Issue**: #1863 — merge-train: an unprocessed comment on a Queued member must eject it for processing

## Context

`Queued` is a holding stage, excluded from dispatch. ADR-1208's settle scan ejects a Queued member only for
review-thread findings; an ordinary issue or PR comment posted while a member sat in `Queued` was never acted
on before it landed. A member can wait a long time (a single trial CI cycle is ~24 minutes on
`verveguy/concept-maps`), so a steering comment posted in that window was simply lost — with #1862's post-merge
guard it is now declined rather than orphaned, but the operator's change is still never made. Related report: #1832.

## Decision

### 1. Detect with the shared predicate, human-only

`settleQueuedReviewFindings` gains a second branch, `settleQueuedCommentCause`, reached only when the member has no
review-thread findings. It computes `filterHuman(findNewComments(item))` on the item the scan already
deep-fetched: `FetchItemDetails` returns the issue's own comments plus every linked PR's, with reactions, so the
detection adds no API call.

`findNewComments` is the same "unprocessed" predicate as #1862's landing gate (`commentGateBlocksLanding`) and the
non-Validate advance guard, so all three agree. It does not exclude every bot author, so the human restriction is
layered on top rather than forked into a second predicate. This is the one place the scan is narrower than the
landing gate, and only in what *triggers an eject*.

### 2. Not train churn: a new eject function, not `ejectMember`

`ejectQueuedMemberForComments` reroutes off `Queued` with `rerouteQueuedMemberOffHolding` **first** (ADR-1208 §4:
on failure nothing is posted and the next poll retries), then posts one locally composed comment
(`🏭 **Fabrik merge-train — ejected (unprocessed comment)**`). It never touches `mergeTrainEjectionCounts`, never
pauses, and does no label mutation, `EngineCyclesCleared` or `ReviewCycles` change (ADR-1208 §5). This is
`deferRedMember`'s shape (ADR-1821).

`ejectMember` was not reused: it unconditionally increments the counter and pauses at `MaxMergeTrainEjections`, its
wording is review-finding specific, and it has five wording-pinned callers. Threading a "count / don't count" flag
through it would have touched all of them.

Unlike ADR-1545's `ejectRedSingleton`, the eject need not pause: that failure was only ever seen on a synthetic
trial branch, so nothing would re-detect it. Here the unprocessed comment is the persistent signal the ordinary
comment path re-detects. The comment carries the `🏭 **Fabrik` prefix so `findNewComments` never re-flags it — in
PAT mode Fabrik posts as the operator's own login, so the prefix is the only guard.

### 3. A parallel pending-eject signal

A member inside a live worker's dispatched batch cannot be ejected from the poll goroutine (it would race the
worker's own batch state), so, exactly as ADR-1208 does for findings, the scan leaves a signal the worker consumes at
its checkpoints. The review-finding signal (`queuedReviewEjects`, ~25 pinned test call sites) is untouched; the
comment signal is a **parallel map**, `queuedCommentEjects`, guarded by the same mutex and keyed by bare `owner/repo`
per ADR-1648.

`applyPendingReviewEjects` takes both signals for each member in one pass, so a member is consumed once and cannot
be double-ejected. A review-finding signal takes precedence and drops any comment signal — the ordinary path handles
both in one invocation, and the review-finding path stays byte-identical. The return contract is unchanged
(`ejectedCount > 0` → the caller discards the trial; the rest of the batch continues). A struct-valued single map was
the alternative: more compact, but it forces edits to every pinned call.

Stale signals are dropped in two places: the scan clears an in-batch member's comment signal when it is no longer
flagged (each poll re-derives the flag from `findNewComments`), and `deferRedMember` drops it on a CI deferral
(mirroring its review-signal drop).

### 4. `ReviewCycles` and boundedness (Requirements 4 and 5)

`ReviewCycleIncremented` has exactly one production writer, `handleReviewGate`'s dispatch. The ordinary comment path
(`processItem` → `processComments`) never increments or resets it, and `EngineCyclesCleared` is reachable only from
manual-unpause and revalidate paths. A comment eject therefore neither increments nor resets `ReviewCycles`, and —
unlike ADR-1208's findings case, which composes with `MaxReviewCycles` through `handleReviewGate` — `MaxReviewCycles`
does not bound this loop. With the `MaxMergeTrainEjections` bound also removed, something else must, and it is
structural:

1. **The eject predicate is a subset of the landing gate's.** The member cannot re-queue while any comment is
   unprocessed (`commentGateBlocksLanding` holds `advanceToQueued`, `MergePR` and auto-merge, and blocks on a failed
   live re-read). A second eject needs a *new* human comment after re-queue; each cycle consumes a distinct comment,
   so there is no autonomous eject → Validate → re-queue → eject loop.
2. **An unprocessable comment ends in the existing comment-breaker pause.** If the ordinary path fails early,
   `markCommentsProcessed` never runs and the comment stays unprocessed — at Validate, not `Queued`, where this scan
   cannot see it. `checkNoOpCommentCycle` and `checkCommentBreaker` pause it there (`MaxNoOpCommentCycles`,
   `MaxCommentCyclesPerWindow`), and ADR-1813's `resumeAuthorised` requires a later human comment to lift it.
3. **Dispatch that never admits the member** (`fabrik:blocked`, `fabrik:editing`, a foreign lock, `CanPush:false`)
   leaves it at Validate with the eject comment and never re-queues — stuck, but not a loop, and from pre-existing
   state.

### 5. Accepted edge: integration-PR comments

A merge-train integration PR carries `Closes #N` for each member. A *human* comment on it can appear in every such
member's `item.Comments` and eject the whole batch. The comment's `FromPR` is not a reliable discriminator
(`LinkedPRNumber` is "the first linked PR"), and a second filter would fork the predicate. Accepted; #1862's
post-merge guard is the backstop.

## Scope and non-goals

- Native-merge-queue members and `fabrik:auto-merge-enabled` members are skipped, as in ADR-1208; closed and
  `fabrik:paused` members are excluded, as before.
- A comment arriving after the batch is committed to landing is out of scope — #1862's post-merge guard covers it.
- Detection is per poll and the scan runs after dispatch, so a member ejected in poll N is first dispatched in
  poll N+1.

## Consequences

- A steering comment on a Queued member is acted on instead of ignored: the member returns to Validate, the comment is
  processed, and the member re-queues once Validate completes again.
- The eject is not counted toward `MaxMergeTrainEjections` and never pauses; prior genuine ejections keep counting
  (`resetEjectionCount` fires only on landing).
- A chatty operator posting several comments in 30 minutes with no commit can trip `MaxCommentCyclesPerWindow`
  (pre-existing behavior, now reachable from a Queued member).
- If `AddComment` fails after a successful reroute the eject is silent and not retried, since the member has left
  `Queued`. This matches ADR-1208 §4's accepted posture; the member is still processed.
- `tests/sim` never wires in `boardcache.CacheImpl` or the reactive observers, so its scenarios stand in for the cache's
  status write-through with `Engine.SimulateCacheStatusWriteThroughForTest` and register the observers explicitly.

## See also

ADR-1208 (the machinery extended), ADR-1862 (the shared predicate and post-merge guard), ADR-1821 (the no-count,
no-pause precedent), ADR-1545, ADR-1270, ADR-1813, ADR-1648. `docs/state-machine.md` §6.16.
