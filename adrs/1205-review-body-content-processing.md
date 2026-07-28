# ADR 1205: Review Body Content Processing

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1205 — feed PR review body text into the same comment-processing path that consumes review threads, so findings a reviewer couldn't anchor to a diff line aren't silently dropped

## Context

Fabrik fetches PR review bodies (`LinkedPRReviews[].Body`, `github/project.go`) but only ever reads `.State` from them — to decide whether the `wait_for_reviews` gate clears. `buildReviewThreadComments` (`engine/reviews.go`), the sole review-reinvoke content source, consumes only inline per-line comments (`LinkedPRReviewThreadComments`). A reviewer that puts a finding in the review *body* clears the gate and produces zero auto-fix work — the finding is dropped, silently, for every reviewer (Gemini, CodeRabbit, `claude-review.yml`, Pruefer).

This became urgent with ADR-1189: Pruefer now demotes findings it cannot anchor to a diff line/file into the body under a `## Additional findings (could not anchor to diff)` heading. Those are exactly the findings this gap eats. PR #1201 showed both surfaces active on one review: inline comments reached Fabrik, the demoted body findings did not.

The engine cannot tell whether a body finding merely restates an inline one or is genuinely additional — that is a semantic read only a worker (the comment-processing skill) can make. So the split is: **engine does mechanical admission only** (deliver the content, filter out pure no-op verdicts), **skills do semantic judgment** (reconcile overlap, address the union).

## Decisions

### 1. A review body becomes a `gh.Comment` with a new `ReviewID` discriminator — not a parallel type

Mirrors the existing `ReviewThreadID` convention (inline thread comments) rather than threading a second `[]gh.PRReview` parameter through `buildReviewThreadComments`/`processComments`/`buildCommentReviewPrompt`. Reusing `gh.Comment` means the circuit breaker (#1089), outbound mention neutralization (#1141), `MaxReviewCycles`, and paused/awaiting-input dispatch gating (#1087) — all of which are already generic over `[]gh.Comment` — apply to review-body-sourced content with zero new wiring. The alternative (a parallel type) would have roughly doubled the "is this a thread comment, a review body, or a plain comment" branch count across `comments.go`/`claude.go`/`reviews.go` for no offsetting benefit.

`Comment.ReviewID` is mutually exclusive with `ReviewThreadID` and `Path` — a review body has no thread and no file/line anchor. A display-only `Comment.ReviewState` field (the review's overall verdict — `APPROVED`/`CHANGES_REQUESTED`/`COMMENTED`) rides along for prompt formatting; gate/dedup logic never reads it, and the non-actionable-body classifier inspects only `Body`, deliberately unprefixed, so its exact-match phrase comparison stays reliable.

### 2. `buildReviewBodyComments` mirrors `buildReviewThreadComments` structurally

New function in `engine/reviews.go`, filtering `item.LinkedPRReviews` down to bodies that are: non-empty `PRReview.ID` (a GraphQL node ID to watermark against — see Decision 4), non-`DISMISSED`, not already ROCKET-reacted, not in `snap.CommentProcessed`, and not classified non-actionable (Decision 3). `dispatchReviewReinvoke`'s `precheck`/`build` and `handleReviewGate`'s emptiness check (`engine/catch_up_handlers.go`) OR the two builders together — either source alone is sufficient to trigger a reinvoke, and `dispatchReviewReinvoke`'s batch concatenates both, so a review carrying inline comments *and* additional body findings is addressed in one invocation, not split across two reinvoke cycles.

`processComments`'s same-poll merge (previously duplicating `buildReviewThreadComments`'s filter logic inline) now calls both builders directly instead — a single source of truth for "what's outstanding," extended to cover bodies with no risk of the merge and the dispatch-time check drifting apart.

### 3. The non-actionable-body filter is a new, deliberately exact-match predicate — not a reuse of `isBotServiceNotice`'s substring matching

`isBotServiceNotice` (#1088) is reused unmodified for what it already covers (quota/rate-limit service notices from a bot author) — it already operates on any `gh.Comment`. It is not, on its own, the right shape for "this review body has nothing to act on": its patterns target service-status prose, not verdict-only content ("LGTM", "Approved").

`isNonActionableReviewBody` (`engine/comments.go`) adds a second, narrower check: an empty/whitespace-only body, or a **trimmed, case-insensitive, exact match** against a short curated approval-phrase list (`lgtm`, `lgtm!`, `looks good`, `looks good!`, `looks good to me`, `looks good to me!`, `approved`, `👍`). Exact-match, not substring, is the load-bearing choice: a substring scan risks dropping a real finding in a body that happens to *open* with "LGTM" before diving into actual feedback — precisely the failure mode this issue exists to fix. A filter that recreates its own bug in miniature is worse than no filter.

### 4. The durable watermark for a review body must go through GraphQL `addReaction`, not REST

GitHub's REST API exposes no reactions endpoint for `PullRequestReview` itself (only issue comments, PR review *comments*, commit comments, issues, releases, discussions). `AddReviewReaction(reviewID, content string)` (`github/comments.go`) is the first `addReaction` GraphQL mutation call site in this codebase, keyed on the review's node ID (`PRReview.ID`, surfaced on `Comment.ReviewID`), not `DatabaseID`.

`acknowledgeComments`/`finalizeComments` (`engine/comments.go`) gain a third reaction-dispatch branch, checked *before* the existing `DatabaseID == 0` synthetic-comment guard (a review's `DatabaseID`, if any, is unrelated to its reactability via this GraphQL path):

1. `c.ReviewID != ""` → `AddReviewReaction` (GraphQL, review bodies)
2. `c.ReviewThreadID != ""` → `AddPRReviewCommentReaction` (REST, inline threads)
3. else → `AddCommentReaction` (REST, plain issue/PR comments)

Step 10's `ResolveReviewThread` side effect stays scoped to branch 2 — a review body has no thread to resolve.

`isReviewReinvoke` (gates PR-summary posting) is redefined from "every comment has `ReviewThreadID != ""`" to "every comment has `ReviewThreadID != "" || ReviewID != ""`" — a batch mixing thread comments and a review body, or sourced purely from bodies, is still review-originated and should still get the "review feedback addressed" PR summary. `formatReviewFeedbackComment` reports resolved-thread and review-body-finding counts separately in that summary (`countReviewBodies`), since a review body contributes no per-thread footer entry (no path/line).

### 5. Review-thread-comment watermarking was already correct — Plan corrected Research's finding here

Research's writeup (sourced from the issue's own prior-discussion comment) claimed `acknowledgeComments`/`finalizeComments` never write the ROCKET reaction for review-thread comments, leaving them restart-unsafe. Direct code reading during Plan showed this had already been fixed by commit `7ea7572` ("Fix review re-invocation loop: use real PR review thread comments"), well before this issue was filed — both functions already branch on `ReviewThreadID != ""` and call `AddPRReviewCommentReaction` for both 👀 and 🚀. No production fix was needed; only a regression test (`TestBuildReviewThreadComments_RocketReactionSurvivesRestart`, `engine/reviews_test.go`) confirming a ROCKET-reacted thread comment is excluded by `buildReviewThreadComments` even on a fresh `Engine` with empty in-memory state — proving the durable half of the watermark holds independent of `itemstate.Store`, which has no persistence.

### 6. The webhook delta path also needed the review's node ID — a second producer Research missed

`boardcache/delta.go`'s `applyPullRequestReviewDelta` upserts `item.LinkedPRReviews` from `pull_request_review` webhook payloads (`submitted`/`edited`/`dismissed`), independent of the GraphQL `FetchItemDetails` deep-fetch path. `pullRequestReviewPayload` gained `NodeID`/`SubmittedAt` fields, and the delta now looks up the existing review by `DatabaseID` before upserting, preserving its `Reactions` — an `edited` webhook must not silently erase a ROCKET watermark recorded since the last deep-fetch, or a fully-processed review body would look unprocessed again on the very next webhook-driven update.

### 7. `base:<branch>` repos are out of scope for review-body processing — no REST plumbing added

`closedByPullRequestsReferences`, and everything nested inside it including `latestReviews`, is structurally empty for PRs targeting a non-default branch (#1046/#1047/#1050) — `item.LinkedPRReviews` is already always empty there, so `buildReviewBodyComments` needs no special-casing to inherit the carve-out. This mirrors the pre-existing `LinkedPRReviewThreadComments` "out of scope" note. The REST fallback `checkReviewGate` already uses for `base:<branch>` gate evaluation (`FetchPRReviews`) carries no GraphQL node ID or reaction summary from GitHub's REST reviews endpoint, so even if body content were plumbed through it, the durable `AddReviewReaction` watermark (Decision 4) would have nothing to key on. Extending REST support is left as an explicit follow-up, not silently attempted here.

## Consequences

**Positive:**
- A finding present only in a review body now reaches a worker and is acted on — the gap #1205 exists to close, and the specific gap ADR-1189's demote-to-body behavior made urgent.
- A review carrying both inline comments and additional body findings is addressed together, in one invocation (Decision 2) — not split across two reinvoke cycles, and not double-charged against `MaxReviewCycles`.
- Nearly all loop-safety machinery (circuit breaker, mention neutralization, cycle limits, human-only pause) applies with zero new wiring, confirmed by dedicated review-body-sourced tests (`engine/review_body_loop_test.go`) rather than assumed by inspection alone.
- The review-thread-comment watermarking gap the issue's prior discussion worried about was found, on direct inspection, to already be closed — corrected in Plan rather than carried forward as unnecessary rework.

**Negative / Trade-offs:**
- Steady-state review-reinvoke volume increases: a review with only a body (previously zero reinvoke cycles) now consumes one, bounded by `MaxReviewCycles` (default 5) — intended, but worth watching on repos where reviewers habitually write body-only reviews.
- The non-actionable-body filter (Decision 3) is a new curated phrase list with the same class of risk `isBotServiceNotice` already carries: a phrase drifts, a reviewer's genuine one-word verdict doesn't match any curated entry, and the body gets processed as if it had findings (harmless — the skill will see there's nothing to do — but a wasted invocation). The exact-match choice bounds the more dangerous direction of that risk (false negative on a real finding) at the cost of not catching every no-op phrasing.
- `AddReviewReaction` is the first GraphQL `addReaction` call in this codebase, verified only against the issue author's stated investigation and this PR's own tests — not against a recorded live-API fixture.

## Related Work

- ADR-1189 (`adrs/1189-pruefer-inline-review-comments.md`) — establishes the demote-to-body behavior that made this gap urgent; this issue is the consumer-side fix ADR-1189's own "Positive consequences" section anticipated but didn't implement.
- ADR-070 (`adrs/070-bot-service-notice-filter.md`, #1088) — `isBotServiceNotice`, reused unmodified (Decision 3) for the bot-service-notice half of review-body filtering.
- ADR-069 (`adrs/069-human-only-pause-resume-trigger.md`, #1087) — confirmed not directly applicable to the review-reinvoke path: `poll.go` already excludes paused items from Phase 1 before any reviewer-content check runs, so a bot review body has no unpause vector to defend against on this surface.
- ADR-073 (`adrs/073-outbound-bot-mention-neutralization.md`, #1141) — confirmed generically applicable; operates on output-formatting call sites, not comment source.
- ADR-1089 (`adrs/1089-comment-processing-circuit-breaker.md`) — confirmed generically applicable; the review-body-sourced trip test (`TestCommentBreaker_TripsAfterThreshold_ReviewBodySourced`) is the new evidence this issue's Definition of Done asked for.
- `engine/reviews.go` (`buildReviewThreadComments`) — the sibling this issue's `buildReviewBodyComments` mirrors structurally.
- `docs/state-machine.md` §2.4, §4.1, §4.2, §6.1–§6.3 — as-built documentation updated in the same change set per `CLAUDE.md`'s canonical-docs rule.
- PR #1201 — the live review that demonstrated both surfaces active on one review, with only the inline half reaching Fabrik before this issue.
