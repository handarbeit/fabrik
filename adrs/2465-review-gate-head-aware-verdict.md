# ADR 2465: The authoritative review gate judges each reviewer at the head commit

Status: accepted (#2465). Amends [ADR-1250](1250-review-authority-orthogonal-to-autonomy.md) (the no-branch-protection fallback) and extends [ADR-1283](1283-declared-unrequested-reviewers.md) (`expected_reviewers` is the boundary for a bot's clean pass). The collapse it reconciles is documented in [ADR 2348](2348-observed-at-ordering-rule.md). The as-built description is `docs/state-machine.md` §6.1.1.

## Context

With `review_authority: authoritative` and no branch-protection review requirement, `reviewGateAuthorityVerdict` blocked on **any** `CHANGES_REQUESTED` in the review list it was handed, whatever commit that review was given on. The list came from `FetchPRReviews`, which collapses the history to one entry per author and never lets a `COMMENTED` overwrite a formal verdict (GitHub's `reviewDecision` semantics, deliberate). A self-submitting bot with no approve mode (Pruefer) that requested changes once could therefore never clear its own request, even by reviewing the fixed head cleanly: the gate repeated `<bot> requested changes` until its timeout and then needed a human. The report (community thread #2453) counted nine such timeouts across six issues.

The same history was read three ways: the gate (sticky formal verdict, `CommitID` ignored), the Store (`latestReviewPerAuthor`, latest by `(SubmittedAt, DatabaseID)` whatever the state) and `reviewerVerdicts` behind `fabrik_status` (judged at `AgreedHead()`, so it said `clean_pass` on the very PR the gate was blocking).

## Decision

1. **Head-aware verdicts (R1).** In the fallback, a formal verdict on a commit other than the head is not a verdict on the head. The reviewer is *awaiting re-review*: the gate keeps waiting, and the reason says so instead of "requested changes". A stale `APPROVED` stops counting too (GitHub's "dismiss stale reviews" is the precedent). A human's objection on the current code is untouched.
2. **A declared bot's clean review at head supersedes its own request (R2).** For a reviewer named in the effective `expected_reviewers` (`effectiveExpectedReviewers`, so the `expected-reviewers:<mode>` label applies; matched with `reviewerIdentityMatches`), a `COMMENTED` or `APPROVED` at head supersedes that reviewer's earlier `CHANGES_REQUESTED`. For every other reviewer a comment never withdraws a change request.
3. **One definition (R3).** `reviewerStandings` (`engine/review_standing.go`) is a pure helper over `(history, head, declared)` returning *no verdict / clean / standing change request / awaiting re-review* per reviewer. `checkReviewGate`, `reviewGateBlocksLanding`, `pauseForReviewTimeout`'s authority line (through `Engine.authorityVerdict`) and `reviewerVerdicts` all use it. The timeout comment is included because it would otherwise keep saying "requested changes" (SC-004).
4. **A new uncollapsed read, not a changed one.** `FetchPRReviewHistory` returns every eligible submission (`PENDING` excluded, `DISMISSED` kept). `FetchPRReviews` is now the collapse of that list with byte-identical output, so Pruefer's eligibility and patch-skip decisions and the feedback paths do not move (the same shape caused the #1539 re-review loop). `FetchPRReviewDecision` and its simulation stay on the sticky collapse: it models GitHub's own value, which R4 keeps authoritative.
5. **The gate always reads history in the fallback.** The collapsed list in hand (GraphQL `latestReviews`, the Store, or the REST collapse) is not a safe input: a GraphQL-shaped `COMMENTED` at head would hide an undeclared reviewer's standing request. A non-empty `reviewDecision` short-circuits before any read (R4, and no extra call under branch protection). The head is read live next to the history so a push cannot make a stale review look current.
6. **Fail closed (R5).** An unreadable history blocks. An unknown head, or a verdict with no commit id, cannot be shown stale, so a `CHANGES_REQUESTED` keeps blocking and `APPROVED` stays non-blocking, exactly as before. A head read that fails does not fail the gate on its own (no review is then newly blocked or cleared).
7. **The Store carries the evidence the view needs.** `LinkedPRState.StandingReviews` holds each reviewer's latest formal verdict and a later `COMMENTED` (at most two per reviewer), written by the gate's live history read (`PRStandingReviewsRead`: unordered, wakes nothing). `reviewerVerdicts` judges `Reviews ∪ StandingReviews`. `latestReviewPerAuthor`/`mergeReviews` stay as they are and are documented as the display/feedback collapse, not a verdict.

## Consequences

- The reported sequence (declared bot `CHANGES_REQUESTED` on A, push B, `COMMENTED` on B) clears the gate with no operator action; the same sequence by an undeclared reviewer still blocks, now as *awaiting re-review*.
- A stale `APPROVED` newly holds the gate in the no-branch-protection fallback until the reviewer re-reviews or the review-wait timeout runs. This is the spec's R1 and is visible in the reason text.
- Authoritative mode with an empty `reviewDecision` pays one extra REST history read and one PR read when the gate would otherwise clear.
- `fabrik_status` can lag a *webhook-only* dismissal of an undeclared reviewer's earlier request, and before the gate's first live read cannot see one at all (the Store keeps one review per author). This is a display staleness; the gate does not read the Store's evidence.
- Not addressed (siblings): a `CHANGES_REQUESTED` on the current head that nothing supersedes (the declined-finding deadlock — addressed by ADR 2466), the review-wait clock, advisory mode, and Pruefer's own behaviour.

## Verification

`engine/review_standing_test.go` (table over history × head × declared set, both gate sites, the timeout line, R4/R5 and the gate/view agreement test), `github/prs_test.go` (history read), `internal/itemstate/live_read_test.go`, and the sim twin `tests/sim/review_authority_head_test.go`, each scenario also run under `SetReviewStandingDisabledForTest` (the pre-change rule) to show it flips.
