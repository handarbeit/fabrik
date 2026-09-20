# ADR 1812: A reinvoke that never ran is not charged a cycle

**Status:** Accepted
**Date:** 2026-09-20
**Issue:** [#1812](https://github.com/handarbeit/fabrik/issues/1812)

**Extends:** [ADR-1458: transient `api_error` exemption](1458-transient-api-error-exemption.md)
to the reinvoke cycle counters.

**Interacts with:** [ADR-1045](1045-review-body-comment-actionability-and-noop-budget.md)
(no-op refund), [ADR-1518](1518-review-gate-non-convergence-terminal-check.md)
(`ReviewBlockedCycles`), [ADR-1555](1555-success-agnostic-comment-cycle-breaker.md)
(no-op comment breaker).

## Context

`handleReviewGate` charges `ReviewCycles` synchronously before dispatch; the
rebase and CI-fix dispatchers charge `RebaseCycles`/`CIFixCycles` the same way
(via `dispatchWithCycleLimit`, and inline in `checkAutoMergeConvergence`). The
only refund was #1045's HEAD-unchanged compensation, behind an `err != nil`
guard in `dispatchReviewReinvoke`'s `after` hook. So an invocation that exited
at turn 1 with `terminal_reason: "api_error"` (a 429 session limit), spending
$0.00, was charged a cycle it could never get back. Five in 37 seconds hit
`FABRIK_MAX_REVIEW_CYCLES` and paused a healthy issue (#1777), with a message
blaming a reviewer. It reproduced on six issues across three daemons. #1597 is
the community report behind this.

The stage-dispatch path already treats the identical exit as "the stage never
ran" (ADR-1458, `handleAPIErrorExit`) and exempts it from `max_retries`; the
reinvoke path was the outlier.

## Decisions

1. **The refund lives in `dispatchReinvoke`**, the shared goroutine scaffold,
   not `dispatchWithCycleLimit`. `checkAutoMergeConvergence` increments the
   rebase and CI-fix counters inline and never goes through
   `dispatchWithCycleLimit`; `dispatchReinvoke` is the one point every charge
   flows through. `reinvokeOpts.cycle` names the compensating mutations.

2. **The classification is a side channel, not a changed error contract.**
   `processComments` returns `nil` for a usage-limit exit and for an
   account-suspension skip, so the `after` hooks could never have seen those
   through `err`. `processCommentsClassified` returns `(didNotRunKind, error)`;
   `processComments` is a thin wrapper, so the two user-comment callers are
   byte-identical. Returning a typed error from the `nil` paths was rejected: it
   would change what those callers see, make `dispatchReinvoke` log a benign
   skip as "re-invocation failed", and risk the comment-breaker exclusion.

3. **What counts as did-not-run:** `claudeUsageLimitError`,
   `claudeAPIErrorExit` (whose classifier already refuses a run that used turns
   *and* cost — R4), the account-wide suspension gate, and
   `apiKeyHelperDetectedError`. The last is produced only by the stage-dispatch
   path today, so its arm in `classifyDidNotRun` is forward-compatibility, not
   exercised on the comment path. Everything else — turn limit, resume failure,
   tools denied, mid-run crash, publish failure — stays charged (R3). Ambiguity
   is charged: under-refunding costs an unnecessary pause, over-refunding
   removes the only bound on a genuine non-convergence loop.

4. **`ReviewBlockedCycles` is also refunded, for did-not-run exits only.** The
   issue asserted a did-not-run cycle never increments it; that holds only when
   `blocked || timedOut` is false. When the gate is blocked, a 429 storm would
   otherwise still reach `max(ReviewCycles, ReviewBlockedCycles)` and produce
   the same false pause. ADR-1518's guarantee is preserved for a run that
   *executed* and no-op'd: that never takes the did-not-run path.

5. **On a did-not-run exit `opts.after` is skipped.** Its HEAD-based logic is
   meaningless when nothing ran, and skipping it removes two accidental
   behaviors on the `nil`-return paths (a bogus CI-fix no-op-SHA debounce, a
   rebase auto-merge re-enable). It also makes the #1045 refund and this refund
   disjoint, so they cannot double-refund; store flooring is a second line of
   defence.

6. **The pause message uses a streak tally, not timestamps.**
   Refunded cycles vanish from the counters, so `DidNotRunReinvokes` is the only
   remaining evidence. It is cleared by `EngineCyclesCleared` and **reset by
   `DidNotRunReinvokesReset` whenever a reinvoke stays charged** (ran, or
   ambiguous), so it is the streak of never-ran reinvokes since the last genuine
   cycle. Without the reset (review finding on this PR), five 429s spread over
   days plus five real non-converging cycles would satisfy `tally >= cycleCount`
   and blame Claude availability for a reviewer that never converges. The store
   keeps no per-cycle timestamps; elapsed time appears only as guidance text. The
   dedup fragments (`reviewCyclePauseFragment` etc.) are unchanged so #1460's
   `hasPauseComment` still matches, and a zero tally leaves the message
   byte-identical. Because the tally is per stage, not per counter, a real
   reinvoke of any of the three kinds ends the streak — deliberately the
   conservative direction (reverts to the original wording).

## Consequences

- A did-not-run storm no longer trips `MaxReviewCycles`/`MaxCIFixCycles`/
  `MaxRebaseCycles`. It is bounded by the comment circuit breaker (§4.6,
  default 10 per 30 min) and the #1555 no-op breaker (default 10), which still
  count these exits. The false pause moves from cycle 5 to about cycle 10 and
  carries the breaker's own message. Adding backoff to the reinvoke cadence is
  out of scope; detecting the 429 upstream (sibling issue) is what prevents
  the storm. Both are wanted.
- The tally is stage-scoped, not per-counter, so the review pause message may
  cite a streak that included rebase or CI-fix reinvokes. The wording says
  "re-invocations for this stage" and is guidance, not a claim about the exact
  counter.
- Each refund is logged (`invocation did not run (<kind>) — refunding <counter>
  cycle counter … (#1812)`), so a silent refund cannot hide the next outage.
