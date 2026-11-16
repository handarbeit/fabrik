# ADR 2582: A dismissed review does not hide the same author's later COMMENTED review

Status: accepted (#2588, work issue for community report #2582). Amends [ADR 2465](2465-review-gate-head-aware-verdict.md) decision 4 (its "`FetchPRReviews` … byte-identical output" claim) and leaves [ADR 2467](2467-review-wait-anchor-on-current-head.md), [ADR 1250](1250-review-authority-orthogonal-to-autonomy.md) and [ADR 1283](1283-declared-unrequested-reviewers.md) unchanged. As-built: `docs/state-machine.md` §6.6 and §6.1.1.

## Context

Community report #2582: a `wait_for_reviews` gate with a declared `expected_reviewers` bot timed out (`fabrik:pause:review-timeout`, "did not respond") although the bot had reviewed the current head with a clean `COMMENTED` review. It happened on three or more PRs, and unpausing re-ran the same wait, because the bot never re-reviews an unchanged head.

`github.(*Client).FetchPRReviews` collapses the review history to one entry per author and skipped a `COMMENTED` whenever the stored entry was a "formal verdict", a set that included `DISMISSED`. Once any review by an author had been dismissed, every later `COMMENTED` by that author was dropped and the collapsed entry stayed `DISMISSED`. `declaredReviewersOutstanding` and `reviewGateOutstanding.hasReviews` skip `DISMISSED`, so the bot read as silent and the re-prompt ladder ran into the timeout pause. The Store's collapse (`itemstate.latestReviewPerAuthor`: latest whatever the state) and `reviewerVerdicts` (head-aware standings) said the opposite, which is why `fabrik_status` showed `clean_pass: true` for the PR the gate was blocking: the cached view cleared the gate and the live confirmation read (ADR 2350) blocked it.

## Decision

1. **A dismissal ends a verdict; it is not one.** In `FetchPRReviews`' collapse only a stored `APPROVED` or `CHANGES_REQUESTED` resists a later `COMMENTED` (new predicate `isStandingReviewVerdict`). A stored `DISMISSED` or `COMMENTED` is superseded by a later `COMMENTED`. APPROVED/CHANGES_REQUESTED stay sticky (GitHub's `reviewDecision` semantics, R2), and a `DISMISSED` that follows a verdict still supersedes it.
2. **`isFormalReviewVerdict` is unchanged.** It still includes `DISMISSED` because `canEstablishOrSupersedeReview` uses it to decide which states reach `FetchPRReviewHistory`; changing it would change the uncollapsed history ADR 2465's head-aware verdict depends on. The collapse gets its own narrower predicate.
3. **The fix is in the shared collapse, not in the engine.** All four gate readers (`checkReviewGate`, `reviewGateHoldFor` for landing and `fabrik_advance`, `pauseForReviewTimeout`'s status text, and the `liveReviews` Store write-through) go through `FetchPRReviews`, so one change fixes each and brings the live read in line with the cached view (R1, R3). The re-prompt ladder, the review-wait anchor (#2467) and the timeouts are untouched (R6); a bot that reviewed the head is simply no longer outstanding.
4. **The sim's collapse is the same rule** (`simgh.latestReviewsByAuthor`, `isStandingVerdict`), still one shared reduction behind `FetchPRReviews`, `FetchPRReviewDecision` and the board projection. `Sim.SetStickyDismissedForTest` (per Sim) restores the old rule as the non-vacuity control of `tests/sim/review_dismissed_then_commented_test.go`; `FIDELITY.md` is updated (R5).

Edge cases: lone `DISMISSED` → still "did not respond"; `DISMISSED → COMMENTED → CHANGES_REQUESTED` → `CHANGES_REQUESTED`; `CHANGES_REQUESTED → DISMISSED → COMMENTED` → `COMMENTED`; `COMMENTED → DISMISSED` → `DISMISSED`. A `COMMENTED` after a dismissal on an older head counts as a response to the *collapse*; whether it satisfies the *current head* in the authoritative fallback is still `reviewerStandings`' decision (ADR 2465), unchanged.

## R4 audit: every `FetchPRReviews` consumer

| Consumer | Affected? |
|---|---|
| `checkReviewGate` (`engine/reviews.go`, via `liveReviews`, advisory and authoritative) | **Yes, intended.** `outstanding`, `declaredReviewersOutstanding` and `hasReviews` see the later `COMMENTED`. |
| `reviewGateHoldFor` / `reviewGateBlocksLanding` and `fabrik_advance` | **Yes, intended.** Same read, same fix. |
| `pauseForReviewTimeout` (`resolvedReviewData`, authority line) | **Yes, intended.** Status text and `hasReviews` agree with the gate. |
| `liveReviews` Store write (`engine/live_read.go`) | **Yes, positive.** The Store and the live read now agree for DISMISSED→COMMENTED, so a live read no longer rewrites the cache to the wrong answer. |
| `buildReviewBodyComments` REST fallback (`base:<branch>` and missing-link items) | **Yes, accepted.** After a dismissal the bot's later `COMMENTED` body becomes a review-body candidate on this path, as it already is on the GraphQL path. `cleanReviewSkipper` (ADR 2489) still drops a clean (`inline=0 body=0`) review of a declared reviewer, so no spurious `review-reinvoke`. Not separately tested. |
| `authorityVerdict` / `reviewerStandings` (ADR 2465) | **No.** They read `FetchPRReviewHistory`, which is unchanged, and already treat `DISMISSED` as no verdict. |
| Pruefer `Eligible` (`pruefer/review.go`, `select.go`: `alreadyReviewedAtHead`, `alreadyReviewedAtAll`) | **Only toward correct.** If Pruefer's earlier review was dismissed and it later reviewed the head, the old collapse stayed `DISMISSED` at the old commit, so the head looked unreviewed; it now reports the real latest review. `alreadyReviewedAtAll` (CadenceOnce) is unchanged (any bot entry counts). Pinned by `pruefer/dismissed_review_collapse_test.go`. |
| Pruefer `catchUpSkip` / `lastBotReview` (`patchskip.go`) and `trialpr.go` | **Only toward correct,** same reason; Pruefer posts only `COMMENTED` reviews so the changed shape is rare. No Pruefer test references `DISMISSED` and none changed. |

No consumer needs the old collapse, so no per-caller variant is kept.

## Tests

`github/prs_test.go`: `TestFetchPRReviews_CollapseShapes` (new; DISMISSED→COMMENTED collapses to COMMENTED, CHANGES_REQUESTED/APPROVED→COMMENTED stay) and `TestFetchPRReviews_CommentedNeverSupersedesVerdict` (its `DISMISSED` subtest removed: it asserted the bug, a COMMENTED not overwriting a DISMISSED). `tests/sim/simgh/reviews_collapse_test.go` (parity table, both seam settings), `tests/sim/review_dismissed_then_commented_test.go` (advisory and authoritative, shipped and neutralised; no `fabrik:bot-reprompted`, no pause), and two `nonvacuity.sh` entries.

## Consequences

A declared bot whose review was dismissed and that later reviews the head clears the gate instead of timing out. A bot that is dismissed and never re-reviews still reads as not responding (out of scope). GraphQL `latestReviews` semantics and human-review counting are unchanged.
