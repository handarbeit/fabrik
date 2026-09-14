# ADR 1631: A local, in-process review tracker as an independent backstop against re-review loops

**Status:** Accepted
**Date:** 2026-09-14
**Issue:** [#1631](https://github.com/handarbeit/fabrik/issues/1631)

## Context

Pruefer's `alreadyReviewedAtHead` guard (`pruefer/select.go`) exists to stop it from reviewing the same PR head SHA more than once. During a real GitHub outage, it did not: PR #1623 received 14 Pruefer reviews on one unchanged commit (11 of them in 76 minutes), and PR #1629 accumulated 25 while the outage was ongoing. Downstream, the repeated "no defects found" comments tripped Fabrik's no-op comment-processing circuit breaker (#1555) and paused a healthy, already-`stage:Validate:complete` issue for human intervention. Roughly 35 spurious reviews were incurred across the two PRs, at an estimated ~$0.67 each.

A controlled experiment (documented in the issue) ruled out a bug in the guard itself: with GitHub back to all-operational, Pruefer was restarted against PR #1629 (25 existing reviews, head unchanged) with Fabrik's own daemon deliberately left down, and the review count held stable at 25 across 7 checks over 14 minutes while Pruefer's own polling was confirmed active. **`alreadyReviewedAtHead` is correct.**

The actual mechanism: `review.go`'s existing fail-closed handling (`ReviewOutcome{Err: ...}`) only fires on a *hard* `FetchPRReviews` error. During GitHub's degradation, some requests instead returned HTTP 200 with a review list that omitted the bot's own latest review at the current head — a **successful but partial** response. That is indistinguishable, from the guard's point of view, from "no prior review exists," so it fails **open** and re-reviews. This repeats on every poll for as long as the degradation lasts, with no bound on consecutive re-reviews.

`/pulls/{n}/reviews` is a bare-array REST endpoint with no `totalCount` (or any other independent count signal) to compare a short/incomplete page against — unlike the GraphQL project-board fetch (`github/project.go`'s `retryOnEmpty`), which already solved an analogous "successful but incomplete during indexer degradation" problem for a *different* endpoint by cross-checking the raw node count against the server's own `totalCount`. That pattern does not transfer here: there is no ground truth in the response to detect a truncated read against.

## Decision

Add `ReviewTracker` (`pruefer/review_tracker.go`): an in-memory, mutex-guarded, process-lifetime record of exactly which `(owner, repo, PR number, head SHA)` tuples this Pruefer process has itself successfully submitted a review for.

- **Checked before `FetchPRReviews` is even called**, immediately after the existing `PendingForceReview` check in `ReviewPR` (`pruefer/review.go`). A confirmed-duplicate head costs zero further GitHub API calls — a direct improvement to API load during exactly the kind of degraded-availability window this issue documents.
- **Recorded immediately after a successful `SubmitPRReview`.** The fact "I just reviewed this head" is produced by Pruefer itself; it does not need GitHub to confirm it back, and a subsequent degraded-but-200 `FetchPRReviews` read can never contradict it, because it's never consulted once the tracker already knows.
- **Bypassed by `forceReview`** (`/pruefer review`), exactly mirroring `alreadyReviewedAtHead`'s own bypass — a human-requested fresh review of an unchanged head must not be silently suppressed by Pruefer's own memory of a prior one.
- **Cap fixed at 1 ("reviewed this head, ever, this process")** — not a time window or an operator-configurable count. The issue's requirements accepted either form; the simpler one is sufficient for every stated acceptance criterion, and no config knob was requested. `ReviewTracker` is deliberately not exposed as a `Config` field.
- **`ReviewPR` gains a trailing `tracker *ReviewTracker` parameter**, nil-safe on both its `Recall`/`Record` methods — a nil tracker (every pre-#1631 test call site, unmodified except for the added `nil` argument) is a pure no-op, so this is an additive dependency, not a required one. `Daemon` (`pruefer/daemon.go`) owns a single shared `*ReviewTracker`, constructed by `NewDaemon` and threaded through `executeReview`'s `ReviewPR` call — the same per-`Daemon` sharing `prGates` already uses for its own concurrency-safe per-PR state.

This mirrors ADR-1615's principle for merge-train identity: assert state from a fact the system itself controls, never from external data that can independently be incomplete. It also mirrors the shape of `github/rest.go`'s `paginateREST`/#1539 precedent (GitHub REST list endpoints can silently degrade; the fix must refuse to trust an incomplete result) one layer up — here, the fix is not detecting incompleteness in the response at all, but no longer needing to.

### Relationship to ADR-1113 ("review state is not stored locally")

ADR-1113 and `cmd/pruefer/README.md` document, as a deliberate architectural property, that Pruefer's review state is derived entirely from GitHub and never persisted locally — "a restart never causes a review storm." `ReviewTracker` is an **addition to** that guarantee, not a **contradiction** of it:

- It is in-memory only; nothing is written to disk. A process restart clears it completely.
- On restart, Pruefer falls back to exactly its pre-#1631 behavior — the GitHub-derived check alone. This is never worse than before this issue; it is only better within one process's uptime.
- The residual gap this leaves — a restart mid-outage still costs one wasted review per affected head, before the tracker relearns state from a fresh successful submission — is accepted explicitly (see Consequences) rather than engineered away, because it bounds the cost to "one," which is the entire point: a provider outage should cost one wasted review, not thirty-five.

`cmd/pruefer/README.md` is updated in the same change to describe this addition explicitly, rather than leaving the original "not stored locally" sentence to read as a now-inaccurate absolute.

## Alternatives Considered

- **A `retryOnEmpty`-style truncation-detection heuristic on `FetchPRReviews`.** Rejected: there is no `totalCount` or other independent ground-truth signal on `/pulls/{n}/reviews` to detect a short/incomplete page against — a truncated-but-200 response is structurally indistinguishable from a genuinely empty one at the wire level. This is precisely why the GraphQL project-board precedent doesn't transfer.
- **A numeric or time-windowed cap greater than 1** (e.g. "at most N re-reviews per hour"). Rejected as unneeded complexity: the issue's stated acceptance criteria are satisfied by the simpler "reviewed this head, ever, this process" cap, and no operator-facing requirement asked for a window or a knob.
- **Gating dispatch from `Daemon` externally, rather than adding a parameter to `ReviewPR`.** Rejected: `ReviewPR` is the existing single, directly unit-testable entry point that all ~45 pre-#1631 tests exercise (matching the existing `client`/`claude`/`clone` dependency-injection convention); moving the check outside it would make it untestable at that level and inconsistent with how every other eligibility check in this pipeline is expressed.
- **Merging the tracker check into `select.go`'s `Eligible`/`alreadyReviewedAtHead`.** Rejected: `alreadyReviewedAtHead` is a pure function proven correct by the issue's own live experiment; folding a stateful, GitHub-independent mechanism into it would conflate two genuinely distinct signals (what GitHub reports vs. what Pruefer itself remembers) instead of keeping them as two independent checks that can each be evaluated, and tested, on their own.

## Consequences

- A Pruefer restart occurring during a live GitHub degradation window still costs exactly one wasted review per affected head, the moment before the tracker relearns state from a fresh successful submission. Accepted: bounded to "one," not "thirty-five," which is the entire improvement this ADR delivers.
- The tracker's map grows unboundedly over a long-running daemon's lifetime, with no eviction or expiry. Accepted as a non-issue at Pruefer's realistic review volume (each entry is one small fixed-size struct key); revisit if this daemon is ever run at a scale where that stops being true.
- `ReviewPR`'s signature widened by one trailing parameter, requiring a mechanical update to every existing test call site (a script-friendly, fails-loud-on-compile-error change, not a design risk).
