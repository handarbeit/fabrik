# ADR 2160: The feedback gate repairs a dispatcher that cannot see what it holds on

## Status

Accepted. Amends ADR 1953-a.

## Context

The feedback gate (`unprocessedFeedback`, ADR 1953-a) and Phase 1's review dispatcher (`handleReviewGate`) share one definition of "is this review body addressed?" — `buildReviewBodyCommentsFromReviews`, `CommentProcessed` and the durable `review-ids-addressed` marker (#1555). They do not read the same facts. The gate reads live (`e.client.FetchItemDetails` on a copy); the dispatcher reads the board cache through `e.readClient`.

The cache serves an item while the board's `updatedAt` is not after its stored baseline. `SelfWriteObserved` (ADR-044 addendum 4, #1090) moves that baseline to the local clock at every engine self-write. A review whose PR `updatedAt` is earlier than that baseline never looks new, so the cache keeps serving the pre-review copy. On #2125 the gate held for over an hour on review body 5483772616 (`holding advance — 1 unprocessed review body`) while nothing dispatched; a SIGHUP restart zeroed the baseline and the very next fetch dispatched it.

Research ruled out the other candidate. Both sides consult `CommentProcessed`, so a stale in-memory "processed" record hides a body from both and cannot make the gate hold while the dispatcher sees nothing; the only writers of `CommentProcessed` for a `review-body:` ID record the batch they were given.

#1944 fixed the same masking class for paused items' comments. Nothing covered review feedback on a non-paused, stage-complete item.

## Decision

1. **Detect.** When the gate finds review bodies or threads, it also reads the dispatcher's view — `e.readClient.FetchItemDetails` on a copy of the caller's original item, through `buildReviewFeedbackCommentsFromReviews` — and compares by comment ID. The held IDs that view lacks are `pendingFeedback.DispatcherGap`. A hold the dispatcher also sees costs a cache hit, no GraphQL. A failed view read leaves the gap empty and never loosens the hold.
2. **Repair the dispatcher's input, do not dispatch from the gate.** `repairDispatcherBlindness` applies `DeepFetchInvalidated` and records a `feedback-dispatcher-gap` cooldown of `mergeGateRecheckInterval()`. The next poll re-admits the item, performs a real fetch, and Phase 1 dispatches the ordinary `review-reinvoke` under `dispatchWithCycleLimit`: the in-flight and landed-PR bails and the `MaxReviewCycles`/`ReviewBlockedCycles` charging are all unchanged. The gate has three call sites, one (`attemptMergeOnValidate`) with no `phase1Ctx` and reachable from workers and merge paths; a direct dispatch would need machinery it cannot always build and could race a worker dispatched in the same poll.
3. **Bound it.** At most two repairs per `(item, exact gap IDs)` key, in memory. Past that the gate keeps holding (fail-closed) and does not refetch; a permanent disagreement costs two extra fetches and one `last repair` log line.
4. **Do not narrow `SelfWriteObserved`.** It exists to stop self-write refetch loops (#1090). The backstop follows the #1944 / ADR 2096 pattern instead: a conditional, bounded invalidation.

R1 ("one answer") is met by the shared builders plus the comparison: a hold the dispatcher cannot reproduce is detected and repaired within one poll. R2 needed no code change — `finalizeComments` records only the IDs in the batch it was given.

## Alternatives rejected

- **A live read in `handleReviewGate`.** One extra GraphQL call per stage-complete item per poll; defeats the cache.
- **A same-poll dispatch from the gate.** Needs a `phase1Ctx` the call sites do not all have, and risks bypassing the cycle caps.
- **Moving to a single shared "pending feedback" function.** It still needs the live input, and the builders are already shared.

## Consequences

- A masked review reaches the dispatcher one poll after the gate first holds on it, with no restart.
- The gate can now issue one extra cache-served read per hold with review feedback, and at most two real refetches per distinct gap.
- **Known wider class, out of scope:** the cache masking also affects CI status, thread comments and human comments on non-paused items. This ADR covers review bodies and threads only.
- **Residual:** if a real refetch still returned a cache without the review (the question left open about #2125's 15:21:37Z fetch), the invalidation does not help; the bound and the `last repair` log line make that visible.
- `latestReviews` is one review per author, so the older of two bodies from one author stays covered by the durable marker only.
- Test seam: `Engine.MaskReviewFromCacheForTest` (tests/sim) holds a review out of the cached view until the gate has repaired the item; the sim twin is `tests/sim/feedback_gate_dispatcher_gap_test.go`.
