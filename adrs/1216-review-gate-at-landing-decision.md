# ADR 1216: Review gate checked at the landing decision

**Status:** Accepted
**Date:** 2026-07-28
**Issue:** [#1216](https://github.com/handarbeit/fabrik/issues/1216)

## Context

`wait_for_reviews` was silently unenforced whenever `wait_for_ci` deferred the same
stage's completion. A PR could merge with an outstanding requested reviewer and no
approving review, on a stage configured with **both** gates enabled. Observed against
`dev(53f9de9)`: `handarbeit/fabrik-test-alpha` PR #3747 merged with
`requested_reviewers: ["verveguy"]` still outstanding and no approval.

Two code paths each assumed the other owned the gate.

**Path 1 — `handleStageComplete` (`engine/stages.go`).** When `wait_for_ci: true`, it
deliberately does not seed `fabrik:awaiting-review`, delegating to the catch-up loop
after the CI gate clears (#617).

**Path 2 — `handleReviewGate` (`engine/catch_up_handlers.go`).** It guards on
`pctx.hasComplete` and returns immediately while that is false — also #617, to avoid
spurious `fabrik:awaiting-review` re-application during the CI-await window.

The two do not compose, because there is no poll boundary between them:

- `pctx.hasComplete` is computed once, before the Phase 1 handler chain runs.
- `catchUpPhase1Handlers` orders `reviewGate` **ahead of** `mergeAndCIGates`.
- On the pass where CI turns green, `reviewGate` has therefore already skipped itself
  (the snapshot predates the completion label), `mergeAndCIGates` then adds
  `stage:X:complete` and returns unclaimed, and Phase 2 calls `attemptMergeOnValidate`
  **in that same loop iteration**.

There is no poll pass in which the item sits at Validate with `hasComplete == true`
and `reviewGate` has not already skipped. The gate could never arm. Since CI is
essentially never already green at the instant `FABRIK_STAGE_COMPLETE` is emitted, the
conjunctive CI+review gate (#895) degenerated to CI-only on every real PR.

A **second, `wait_for_ci`-independent instance** of the same defect exists:
`handleStageComplete` calls `attemptMergeOnValidate` whenever `yoloActive && !waitForCI`
— *before* its own `wait_for_reviews` seeding block runs, so that block can never
prevent a merge or Queued-advance that already happened.

Severity differs by mode but the guard does not. Under `merge_train: off`, GitHub's
native auto-merge honours branch protection, so a repo requiring approvals is masked by
defence in depth. Under `merge_train: on`, Fabrik *is* the merge authority: the member
PR's outstanding reviewer request is simply dropped. A repo that is safe today becomes
unsafe purely by turning the train on.

## Decision

**Enforce the review gate inside `attemptMergeOnValidate` itself**, via a new
`reviewGateBlocksLanding` (`engine/reviews.go`), positioned after the cruise and
`fabrik:auto-merge-enabled` early-returns and **before** the `merge_train` fork.

`attemptMergeOnValidate` is the single landing-decision owner for both `merge_train`
modes (ADR-058/ADR-059, "invoke, don't relocate"), and both defect instances funnel
through it. One check there covers auto-merge enable, merge-queue enqueue, direct
merge, and advance-to-`Queued`, from both call sites, in both modes.

Supporting choices:

- **Live re-fetch, never the item snapshot.** `FetchPRReviews` + `FetchPRReviewRequests`
  keyed on a PR number, rather than `item.LinkedPRReviewRequests`/`LinkedPRReviews`.
  The two callers have different freshness guarantees — `handleStageComplete`'s `item`
  is the pre-stage snapshot, stale *by design* because reviewer assignment happens
  inside `MarkPRReady` — and a reviewer requested during the CI-await window must still
  block. Same "don't trust a snapshot for a gate decision" principle as
  [ADR-957](957-live-reread-at-decision-point-for-cache-staleness.md).
- **PR number from `item.LinkedPRNumber`, with a `FetchLinkedPR` fallback only when it
  is 0**, and only when `wait_for_reviews: true`. The fallback exists because
  `closedByPullRequestsReferences` is structurally empty on a `base:<branch>` repo.
  Gating on the opt-in first means stages that don't use the gate make zero extra API
  calls, and the `merge_train: on` path does not acquire a mandatory PR fetch it never
  had.
- **Absent PR ⇒ do not block; unreadable PR ⇒ block.** No PR means no reviewer requests,
  so FR-1 is vacuously satisfied and blocking would strand items and duplicate
  `handleBrokenReviewLinkage`, which already owns the broken-linkage pause. But a
  `FetchLinkedPR` *error* is unknown state, not an absent PR: on a `base:<branch>` repo
  that fallback is the only PR-resolution route, so letting a transient failure through
  would land the item with the gate never evaluated at all — the exact FR-1 failure this
  ADR exists to close.
- **Fetch error ⇒ block conservatively, discarding *both* slices.** Trusting whichever
  call succeeded risks a false `len(outstanding) == 0 && hasReviews` read that clears
  the gate on unknown state. Mirrors `checkReviewGate`'s existing `base:<branch>`
  fallback. Every block path routes through one exit (`holdLandingForReview`) so none
  can forget the label, and is bounded by the `fabrik:awaiting-review` timeout — the
  label it applies is the `FetchLabelAppliedAt` anchor `checkReviewGate` reads — so a
  sustained outage pauses for a human rather than hanging.
- **Reuse `reviewGateOutstanding` and its exact clearing condition.** The landing gate
  and the catch-up gate cannot disagree on "outstanding" because both call the same
  pure function.
- **Blocks and labels only; no escalation.** `checkReviewGate` remains the sole owner of
  the bot re-prompt ladder and the wait timeout. Once the landing gate blocks,
  `stage:X:complete` is present, so `handleReviewGate` claims the item with
  `hasComplete == true` on the next poll and takes over. This also bounds the live
  fetch's cost to the pass where CI clears.

## Consequences

**FR-4 (#617 non-regression) holds without a new guard.** `attemptMergeOnValidate` is
provably unreachable while CI is genuinely still blocking — `handleMergeAndCIGates`
claims the item in that case, so Phase 2 never runs. `catchUpPhase1Handlers` ordering
and its structural test `TestCatchUpPhase1HandlersOrder` are untouched.

**Forward constraint:** any future landing path — a new merge mechanism, a new
advance-to-column route, another convergence owner — **must route through
`attemptMergeOnValidate` or re-implement this gate**. The gate is not enforced at either
call site, by design; it lives at the choke point. Likewise, any change to the clearing
condition must be made in `reviewGateOutstanding`, not at a call site, or the two gate
sites will drift apart.

**One extra pair of REST calls per landing attempt** on `wait_for_reviews` stages,
incurred on the pass where the landing decision is actually taken. Stages without the
gate are unaffected, pinned by
`TestAttemptMergeOnValidate_ReviewGate_OptOutCostsNothing`.

## Alternatives Considered

**Reorder `catchUpPhase1Handlers` so `reviewGate` runs after `mergeAndCIGates`.**
Rejected. It cannot fix the `handleStageComplete` `!waitForCI` instance, which is a
different function outside the catch-up loop entirely — so the fix would still have to
land in `attemptMergeOnValidate`, at which point the reorder is redundant. It would also
require un-freezing `pctx.hasComplete` (it is computed before the chain runs) and
perturbs a structurally-tested invariant for no gain.

**Read `item.LinkedPRReviewRequests`/`LinkedPRReviews` instead of re-fetching.**
Rejected: under-enforces at the `handleStageComplete` call site, whose `item` is
documented stale-by-design for exactly this data.

**Call `checkReviewGate` wholesale from the landing site.** Rejected: would duplicate
escalation and timeout side effects at a site that is always followed by a poll where
Phase 1 owns them.

**Duplicate the gate in `merge_train.go`.** Rejected: contradicts the ADR-058/ADR-059
"invoke, don't relocate" pattern, and the merge-train landing machinery has zero review
awareness by design — once an item reaches `Queued`, nothing downstream re-checks, so
the gate belongs strictly before `advanceToQueued`.

## References

- [#895 conjunctive CI+review gate](../specs/895-conjunctive-ci-review-gate/) — the
  feature this issue found broken. Its own scope excluded engine unit tests, which is
  why the gap shipped uncovered.
- #617 — the origin of the `hasComplete` guard on `handleReviewGate`.
- [ADR-032: CI gate conjunctive completion label](032-ci-gate-conjunctive-completion-label.md)
- [ADR-056: Consolidate convergence gate recovery](056-consolidate-convergence-gate-recovery.md)
- [ADR-058: Merge queue integration](058-merge-queue-integration.md)
- [ADR-059: Internal merge train](059-internal-merge-train.md)
- [ADR-957: Live re-read at decision point](957-live-reread-at-decision-point-for-cache-staleness.md)
- `docs/state-machine.md` §6.6.6 — as-built description of the joint-clearing sequence.
