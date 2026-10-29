# ADR 2466: A declined at-head bot review finding is an immediate decision pause

## Status

Accepted

## Context

Review can answer an authoritative reviewer's `CHANGES_REQUESTED` with a deliberate no-change. A bot that reviews only on push (Pruefer triggers on `opened`, `reopened`, `synchronize`, `ready_for_review` and skips a head it has reviewed) never reviews again, so the request stays on the head. ADR 2465 only discounts verdicts on older commits. The gate waited for a review that could not come and paused on `review wait timeout`; every unpause repeated it (#2453, chronicle#529).

## Decision

1. **Level-triggered at the tail of `handleReviewGate`**, not in `dispatchReviewReinvoke`'s `after` hook. The hook fires once, in the worker goroutine, on the dispatch-time snapshot, and is lost on restart; after a resume nothing is dispatched, so it could not meet the requirement that a resume with unchanged state re-surfaces the decision without a timeout. The same fact is detected on the next poll (the worker exit already wakes the poll) from durable inputs: the uncollapsed review history, the live head, and the processed marker of the review body. The check sits ahead of the `MaxReviewCycles` terminal check and `pauseForReviewTimeout`, so the more specific diagnosis wins and nothing double-posts.
2. **"Declined" = a declared reviewer's `CHANGES_REQUESTED` exactly at the known live head, with a body and database ID, whose `review-body:<id>` is already processed**, in authoritative mode, with no worker in flight. A reinvoke that pushed would have made the verdict stale; an interrupted or failed one does not mark the body processed. Every doubt (unreadable history, unknown head, no body/ID, undeclared stage) falls through to today's behaviour; the failure polarity is "old timeout", never "cleared".
3. **Cause `question`** — a decision is put to a human. `review-timeout` contradicts the requirement that it not read as a timeout; a new vocabulary member would widen the closed set, the attention meanings and the scans for no behavioural gain. `fabrik:awaiting-input` stays on the pause as on every engine pause (ADR 2487).
4. **`EnginePaused` is applied on both the fresh and the reapply branch**, like `pauseForReviewCycleLimit` (#1460), so a label-removal resume zeroes the review counters rather than being sent straight to the cycle-limit pause by stale `ReviewBlockedCycles`.
5. **R4: the worker's reply never answers the finding.** The only writes are the pause pair, the cause label and `EnginePaused`. The standing request stays, so the gate still blocks after a resume; a new review, a dismissal, a new head or `review-authority:advisory` changes the inputs and ends detection.
6. **Dedup** by a stable first-line fragment naming the head and review IDs (`hasPauseComment`): a repeat on the same episode reapplies the pause without a comment; a new head or review ID is a new episode.
7. **Reviewer text is untrusted**: the excerpt is one line, ≤400 runes, backticks replaced, `@` mentions defused with a zero-width break and `<!--` neutralised; it is posted only through `pauseIssue`.
8. **Branch-protection repos are included** (`reviewDecision == CHANGES_REQUESTED` is the same stuck state); the check makes its own `liveReviewHistory` read, but only on the rare path where the gate is blocked or timed out, authoritative, declared reviewers exist and nothing is actionable.

## R3 (re-request the review) is deferred

Fabrik already re-requests in the timeout re-prompt (`AddReviewRequest`), and that has never been shown to re-run a self-submitting bot. Pruefer has no `review_requested` trigger and skips a head it already reviewed; GitHub cannot request a review from an App that is not a user-type collaborator; it could not be verified for other bots. Only the decision pause ships; re-requesting remains a follow-up if a reviewer is shown to re-run on request.

## Consequences

Operators get an immediate, named decision instead of one or more wasted timeouts. Operators who never declared the bot under `expected_reviewers` keep today's behaviour. Seam: `SetDeclinedReviewPauseDisabledForTest`. Tests: `engine/review_decline_test.go`, sim twin `tests/sim/review_declined_finding_test.go`.

Related: ADR 1250, 1283, 1045, 1518, 2465, 2467, 2487; `docs/state-machine.md` §6.1.1.
