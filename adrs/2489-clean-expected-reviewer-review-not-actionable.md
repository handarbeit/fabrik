# ADR 2489: A clean review from an expected reviewer is not actionable

## Status

Accepted. Builds on ADR 2488 (the findings-count marker) and ADR 1045 (every non-dismissed review body is actionable). Does not touch ADR 2465 (the review-gate verdict).

## Context

Since #1045 the body of every review that is not `DISMISSED` or `PENDING` is actionable, because a bot such as Pruefer submits its whole finding set as a `COMMENTED` review. A review that found nothing is therefore also actionable: the engine dispatches a `review-reinvoke`, the worker reads it, pushes nothing and the cycle is refunded. The round still holds a worker slot and costs a session (41 such rounds in the #2453 report).

ADR 2488 gave Pruefer reviews a structural, machine-readable final line carrying the number of inline and body findings. The engine can now tell a clean review from one with findings without a worker reading prose.

## Decisions

1. **The decision lives in the shared funnel.** `reviewBodyCandidates` is the one definition of "actionable review body"; five readers re-derive it (the live builder behind Phase 1 dispatch and the feedback gate, `cachedPendingFeedbackOf`, `cachedFeedbackArrivedSince`, `dispatcherVisibleIDs`, `reviewerVerdicts`) and must agree (`TestCachedFeedbackMatchesLiveGate`, the #2160 dispatcher-gap repair). It gains a nil-safe `skip` parameter; every caller passes the same `Engine.cleanReviewSkipper`. Filtering only the dispatcher or only the gate would make the others disagree. ADR 1953-a is respected: the gate still never decides what counts as actionable.

2. **Trust model.** A review is skipped only if its author matches the stage's *effective* `expected_reviewers` (label overrides included) through `reviewerIdentityMatches`, the same matcher and set the review gate uses, and its state is `COMMENTED` or `APPROVED`. The marker is read only from `PRReview.Body`; comments never go through the funnel. Anything uncertain is actionable.

3. **Strict parse, reconciling the spec with ADR 2488.** Exactly one opener in the whole body (the producer neutralises look-alikes, so a second is a quote or a forgery), on the last non-empty line, closed by `-->`; `inline` and `body` once each as non-negative integers. Whitespace and extra trailing keys are tolerated, as ADR 2488's parser contract says. `coverage` is not consulted: a worker can act on findings, not on entries the reviewer did not open. A contract test parses Pruefer's own `testdata/review_body_*.md` so grammar drift fails.

4. **Addressed state is derived, not written (deviation from the issue's FR-007).** The `review-ids-addressed` marker (#1555) exists only inside a Fabrik review-feedback PR comment, which is posted after a worker ran. Recording it for a skipped review would need a new comment, which the issue's own assumptions rule out. Instead the predicate is pure: every poll and every restart re-excludes the same review, so it never reappears as unprocessed (SC-004). We also do not backfill `CommentProcessed`: it would outlive a change to the declared list until restart, and the cache-only readers must not write. If the author stops being declared the review becomes actionable again, which fails towards work.

5. **`CHANGES_REQUESTED` is never skipped (narrowing of the issue's edge case).** Skipping a change request would leave the authoritative verdict blocking with no worker dispatched to clear it. Pruefer never emits a change request with zero findings, so this costs nothing.

6. **Unresolved threads are independent.** A thread comment carries no review ID, so threads cannot be attributed to the review that opened them. The body predicate ignores threads and they keep dispatching and holding the gate on their own (FR-009).

7. **The verdict is untouched.** `checkReviewGate`, `authorityVerdict` and `reviewerStandings` read reviews directly, not through the funnel (FR-008, ADR 2465).

## Consequences

- A clean expected-reviewer review starts no worker, produces no `ReviewCycleDecremented` refund, is never charged to `ReviewBlockedCycles`, and does not hold an advance or landing.
- `reviewerVerdicts` (display only) stops counting such a review as outstanding.
- No new config key, label, flag or env var.
- Seam: `Engine.SetCleanReviewSkipDisabledForTest`; sim twin `tests/sim/review_clean_expected_test.go` runs the neutralised control.
