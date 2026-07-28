# ADR 1207: Yolo-Merge Review-Thread Guards

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1207 — yolo items can merge with unresolved review threads: findings arriving near Validate completion are lost

## Context

A `fabrik:yolo` item can merge with unresolved PR review threads, losing review findings that arrived shortly before Validate completed. Observed live on #1183 / PR #1202: three findings arrived and were addressed by the existing review-reinvoke loop before Validate completed, but a fourth arrived 71 seconds before GitHub's own merge completed and was never seen — the fix it would have prompted is still unaddressed on `main`.

The existing mechanisms don't cover this gap. §2.9 Review Reinvoke fires on any `stage:<X>:complete` item with unresolved threads, but it only helps while the item is *parked* at a completed stage — with `yolo`, Validate completing does not park anything; `attemptMergeOnValidate` runs in the same flow and advances immediately. §2.16 SHA-invalidation compares `snap.ValidateCompletedSHA()` against the linked PR's head SHA, but a review comment does not change the head SHA, so there is nothing for that scan to detect. The back-edge exists for "a commit arrived after Validate," not "a finding arrived after Validate." `fabrik:cruise` is unaffected — it deliberately stops at Validate for human merge, so a late finding is caught by the normal reinvoke loop before anyone merges.

#1189 made the pattern worse, not better: Pruefer now posts findings as inline comments and re-reviews on every new head SHA, and Fabrik pushes a fix per reinvoke, each producing a fresh SHA and a fresh review. The faster the fix loop runs, the more likely a review lands inside the merge window — this race is a consequence of the loop working, not a separate bug.

## Decisions

### 1. Guard both points — the initial decision and the convergence window — not just one

The issue's own Prior Art pointed at a single guard inside `checkAutoMergeConvergence` (`engine/merge_gate.go`), the function that polls repeatedly during the window between auto-merge being enabled and GitHub actually merging. Guarding only `attemptMergeOnValidate`'s one-shot decision would not have caught the #1183/PR#1202 incident: all three earlier findings were already resolved by the time Validate completed, and the fourth arrived after that one-shot decision had already run, during the pending-CI window. The exposure is an interval, not a moment, so both ends of that interval need a guard.

### 2. Guard 2's disable mutation lives inside `handleReviewGate`, not `checkAutoMergeConvergence` — deviating from the issue's own Prior Art pointer

This is the load-bearing correction Research surfaced and Plan built around. `catchUpPhase1Handlers` (ADR-056 D3) is `dependencies → reviewGate → autoMergeConvergence → mergeAndCIGates`, and each handler claims and stops the Phase 1 chain on `true`. Whenever a fresh unresolved thread appears on an item that already carries `fabrik:auto-merge-enabled`, Handler 2 (`handleReviewGate`) finds it via `buildReviewThreadComments` and dispatches a review reinvoke on that same poll — claiming the item and preventing Handler 3 (`handleAutoMergeConvergence`, which calls `checkAutoMergeConvergence`) from ever running that cycle. A guard placed only inside `checkAutoMergeConvergence`, as the issue's Prior Art literally cites, would systematically not fire in exactly the scenario it exists to catch — it would pass its own narrow unit tests without closing the race in the full poll loop.

Guard 2 instead lives inside `handleReviewGate` itself, immediately before the existing `dispatchWithCycleLimit` call: if the item carries `fabrik:auto-merge-enabled` and `currentHeadReviewThreadComments(item)` is non-empty, `disableAutoMergeForReviewThreads` runs first. No reordering of `catchUpPhase1Handlers` was used or needed — Handler 2 already runs before Handler 3 by design ("review threads addressed before any merge/CI gate evaluation"), so this is the natural site for the disable, not a workaround grafted onto the existing structure.

### 3. Full disable-and-re-enable was built, not the sanctioned fallback (loud log only)

The issue sanctioned a narrower fallback — guard `attemptMergeOnValidate` only, plus a loud log (no real disable) when threads appear during convergence — if the disable/re-enable round-trip with `fabrik:auto-merge-enabled`'s dual role (idempotency guard for guard 1 + budget-start anchor for `checkAutoMergeConvergence`) proved unsafe. It did not: `pauseForMergeGroupStall` (`engine/merge_gate.go`) already removes and later relies on `attemptMergeOnValidate` to re-apply the same label with a fresh timestamp as a resumption mechanism, and that idiom is exercised in production today. The full mechanism was built directly on that precedent, and the round-trip test (`TestGuard2RoundTrip_DisableThenResolveReenablesAutoMerge`, `engine/catch_up_handlers_test.go`) exercises disable → resolve (ROCKET reaction) → re-enable end to end.

The convergence-budget-reset-on-every-cycle behavior this implies (each disable/re-enable restarts `checkAutoMergeConvergence`'s dwell-anchor budget) is accepted as-is rather than re-engineered with a cumulative first-enabled timestamp — bounded overall by `MaxReviewCycles` (a dispatch-count bound), not wall-clock, consistent with keeping this stopgap narrow.

### 4. `isOutdated` (GitHub-computed) over manual `originalCommit { oid }` SHA comparison for stale-thread scoping

The issue's Prior Art floated adding `originalCommit { oid }` to the `reviewThreads` GraphQL comment selection and comparing it against the linked PR's head SHA in the engine. GitHub's `PullRequestReviewThread` type already exposes `isOutdated: Boolean`, computed by GitHub itself from whether the thread's commented lines still exist unchanged in the current diff. This was not previously fetched. Using it avoids adding per-comment SHA plumbing and a new comparison point in the engine — GitHub already does the work of determining "has this thread's commented lines been superseded by a later push."

`currentHeadReviewThreadComments` (`engine/reviews.go`) wraps the existing `buildReviewThreadComments` (reused, not duplicated, per the issue's explicit requirement) and filters out any comment whose thread `IsOutdated`. Both guards call this helper exclusively — there is no separate stale-SHA detection path.

### 5. Mutation before label removal in `disableAutoMergeForReviewThreads`

The GitHub-side disable (`DisablePullRequestAutoMerge` for native auto-merge, `DequeuePullRequest` for merge-queue-enabled PRs — branching exactly like the existing `reenableAutoMergeAfterRebase` does) runs first; `fabrik:auto-merge-enabled` is removed only on mutation success. If the label were removed first and the mutation then failed, Fabrik would stop watching the PR (`handleAutoMergeConvergence` is gated on the label's presence) while GitHub might still merge it underneath — recreating the exact race this issue closes. With mutation-first ordering, a mutation failure leaves the label in place and the same disable is retried on the next poll; the accepted degraded outcome on a *label-removal* failure after a successful mutation is that `checkAutoMergeConvergence`'s existing "user disabled auto-merge" pause path fires on the next poll — a safe, human-visible failure mode, not a silent merge.

### 6. `attemptMergeOnValidate`'s widened return signature: `(enabled, deferred bool, err error)`, not a boolean-overload

Guard 1 needed the caller (`handleStageComplete`) to distinguish "genuinely blocked by a real review thread — must not advance" from the two pre-existing benign `(false, nil)` cases (cruise label present, no linked PR found). Overloading the existing two-value return would force the call site to re-derive *why* it got `false` by independently re-checking cruise/idempotency conditions the function already evaluated internally — a "boolean blindness" smell. The mechanical cost was updating roughly a dozen existing two-value call sites in `engine/stages_test.go`; that is a one-line find/replace, not a design cost. `handleStageComplete` folds the new `deferred` value into `shouldAdvance` the same way it already folds `autoMergeEnabled`. `poll.go`'s Phase 2 Validate call site discards `deferred` — Phase 1's review gate already prevents reaching that call while blocked in the stock config, and the retry loop there is unconditional regardless.

### 7. No reuse of `pauseOpts.removeAutoMerge`

That existing flag is Fabrik-label-only at every current call site — it has never called a real GitHub mutation; every existing "disable auto-merge" pause path (`pauseForConvergenceFailed`, `pauseForMergeGroupStall`, the "user disabled auto-merge" branch in `checkAutoMergeConvergence`) only removes the Fabrik bookkeeping label. Guard 2 is the first place a *real* GitHub-side disable becomes necessary. Conflating it with `removeAutoMerge`'s existing weaker semantics would silently change the behavior of three existing pause call sites that were never meant to touch GitHub. `disableAutoMergeForReviewThreads` is a new, narrowly-scoped function instead.

## Consequences

**Positive:**
- Closes the specific race that produced the #1183/PR#1202 incident: an unresolved review thread on the current head now blocks both the initial Validate-completion decision and the ongoing convergence window, with the disable correctly placed to actually fire in the scenario that caused the loss (unlike the issue's own initial Prior Art pointer).
- Both guards reuse the same detection primitive (`currentHeadReviewThreadComments` → `buildReviewThreadComments`), so there is exactly one place "is this thread unresolved and current" is decided, not two that could drift apart.
- Once a blocking thread is resolved by the existing review-reinvoke loop, no new plumbing is needed for either guard to retry: guard 1 rides poll.go's existing Phase 2 retry of `attemptMergeOnValidate`, and guard 2's label removal feeds directly into that same retry path.
- `MaxReviewCycles` continues to bound and escalate the reinvoke loop unchanged at both guard points — the disable in guard 2 is additive to the existing dispatch, not a parallel gate with its own limit.
- A thread against a superseded commit (`isOutdated`) never blocks indefinitely at either guard point, using a signal GitHub already computes rather than new per-comment SHA plumbing.

**Negative / Trade-offs:**
- Residual race is bounded, not eliminated: GitHub can still complete a merge asynchronously between Fabrik poll cycles. This reduces exposure from "unbounded until the next poll or GitHub's own timing" to "bounded by `PollSeconds`," not zero — closing it fully would require GitHub-side branch-protection changes, explicitly out of scope for this issue.
- The convergence budget (`checkAutoMergeConvergence`'s dwell anchor) resets on every disable/re-enable cycle. Repeated late findings on a single item could in principle reset it indefinitely; this is accepted rather than built around, since `MaxReviewCycles` already bounds the dispatch count regardless.
- This is an explicit, narrowly-scoped stopgap for `fabrik:yolo` specifically — not a general redesign. #1071 carries a design direction (a decidable "hold a valid signature from every required signer for this SHA" check, evaluated once) expected to supersede this "have objections appeared" check, which is undecidable and needs a guard at every advancement point. No reusable "block on unresolved threads" abstraction was extracted beyond the two call sites here, deliberately, per the issue's own instruction not to invest in infrastructure meant to outlive it.
- The merge-train `Queued` blackout window (a related but structurally different race, remedied by eject-and-requeue rather than disable-and-reenable) remains open, tracked separately in #1208 and chained behind this issue.
