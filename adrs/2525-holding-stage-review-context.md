# ADR 2525: A holding-stage item's reviewer context is the stage it was validated in

## Status

Accepted. Amends ADR 2489 (the clean-review skip) in its choice of stage; builds on ADR 1208 (Queued review-finding ejection, `stageBeforeHolding` as the reroute target). Does not touch ADR 2465.

## Context

ADR 2489 drops a clean review from a declared reviewer. "Declared" is decided from the `expected_reviewers` of the stage named by the item's current board Status. When the merge train is on, a validated item moves to the holding stage (Queued), which declares no reviewers. The skip therefore stopped applying the moment an item was Queued, although the same review was correctly ignored one column earlier. "Addressed" is derived (ADR 2489 decision 4), so nothing remembered it. `settleQueuedReviewFindings` read the clean review as an unprocessed finding and ejected the member to Validate; Validate advanced it straight back; after `MaxMergeTrainEjections` the member paused. Observed on #2491 / PR #2524, and it affects every yolo item on a train repository whose landing review is clean.

## Decisions

1. **One stage context, resolved in one place.** `Engine.reviewContextStage(status)` returns the stage whose `expected_reviewers` apply. For a non-holding status it is the stage itself. For a holding stage it is `stageBeforeHolding` — the stage the member was validated in, and the same stage the Queued ejection reroutes to (ADR 1208), so the skip and the reroute cannot disagree. It is derived by `Order`, not hard-coded to "Validate".
2. **The fix lives inside `cleanReviewSkipper`.** All readers of the review-body funnel (the live builder, `cachedPendingFeedbackOf`, `cachedFeedbackArrivedSince`, `dispatcherVisibleIDs`, `reviewerVerdicts`) and `queuedReviewFindings` already pass `(status, number, labels)`; none changes, so they agree by construction (`TestCleanReviewAgreesAcrossReaders_Queued`). `reviewerVerdicts`' display-only standings lookup uses the same helper.
3. **Fail safe.** A status with no configured stage, or a holding stage with no earlier stage, resolves to nil: nothing declared, nothing skipped — the pre-fix ejecting behaviour.
4. **The holding stage's own `expected_reviewers` is ignored** for the clean skip. The `expected-reviewers:<mode>` label override keeps its precedence through `effectiveExpectedReviewers`.
5. **No change to ejection.** Threads, `CHANGES_REQUESTED`, marker-less bodies, non-clean markers and undeclared authors never pass the skipper and eject as before; counting, rerouting and pausing are untouched. No marker or label is written (ADR 2489 decision 4 stands).
6. **Seam.** `SetHoldingStageReviewContextDisabledForTest` (per-Engine) restores the holding-stage lookup, so the sim twin can show the loop's first ejection without disabling the whole #2489 skip.

## Audit of other current-Status stage lookups

Reachable by a Queued item on a review/feedback path: `cleanReviewSkipper` and `reviewerVerdicts` (fixed). `checkReviewGate`, `reviewGateHold`, `pauseForReviewTimeout`, the fast-advance and `review_decline.go` take an explicit stage from their caller, and a holding-stage item is never admitted to them (`itemMayNeedWork` excludes it; landing runs at Validate). `overseer_advance.go` refuses a holding stage up front. Autonomy / decision-pending / situation / downtime lookups read `AutoAdvances`, `Unmanaged`, `CleanupWorktree` and similar, not review facts. No `effectiveReviewAuthority` caller is reachable from the Queued scan.

## Consequences

A reviewer declared only on the holding stage no longer counts for the clean skip; declare it on the validating stage. A custom config whose stage before the holding stage is not where the member was validated is judged against that stage, matching the reroute.
