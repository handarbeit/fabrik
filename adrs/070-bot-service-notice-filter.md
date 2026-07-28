# ADR 070: Bot Service-Notice Filter and No-Work-Needed Reply Suppression

**Date**: 2026-07-26
**Status**: Superseded in part by ADR-1221 (the item.Comments-only scoping rationale is reversed; the pre-admission filter in findNewComments and the watermarking settle scan are unchanged)
**Issue**: #1088 — Runaway-loop fix (2/4): don't spawn a worker or reply for non-actionable bot service-notices

## Context

Incident #1083: `gemini-code-assist[bot]` ran out of daily quota and auto-replied to thread activity with a `[!WARNING] You have reached your daily quota limit.` banner. Each such comment (a new ID every time) was indistinguishable from any other bot comment to `findNewComments` — it admitted the comment, `processComments` spawned a full comment-processing worker (~506k cache-read tokens/iteration), and Fabrik posted a "not actionable" reply. That reply then (a) externally re-triggered the bot to reply again, restarting the cycle, and (b) bumped the issue's `updatedAt`, forcing an immediate re-poll. The loop ran ~995 times for roughly $210 before it was noticed. ADR-069 (fix 1/4, #1087) closed the ability of this pattern to silently defeat an operator's `fabrik:paused`, but did nothing to stop the loop from running in the first place in the default (non-paused/cruise) case — that is this issue's scope.

## Decision

Two independent, complementary mechanisms:

**1. Pre-admission filter.** `isBotServiceNotice(c gh.Comment) bool` (`engine/comments.go`) classifies a comment as non-actionable bot noise when `gh.IsBotLogin(c.Author)` is true **and** the lowercased body contains one of a small, literal pattern list (`botServiceNoticePatterns`): `"daily quota limit"`, `"you have reached your daily quota"`, `"rate limit exceeded"`, `"you have reached your rate limit"`, `"you have exceeded your rate limit"`, `"api rate limit"`. This is wired in as a fourth exclusion inside `findNewComments`, alongside the existing `CommentProcessed`/Fabrik-prefix/rocket-reaction checks. Because `findNewComments` is the single function underlying all comment-processing entry points (normal dispatch, the paused/awaiting-input resume decision via `humanNewComments`, and the catch-up stage-advance check in `poll.go`), adding the check there gives "no worker spawned, no reply" uniformly, with no per-call-site duplication.

**2. Watermarking via a settle scan, not inline.** A bot-notice comment excluded by `findNewComments` never reaches `processComments`, so nothing gated on dispatch (👀 reaction, `fabrik:editing`, the eventual 🚀ᅠreaction + `CommentProcessed` write) ever runs for it — left alone, the comment would re-qualify as "bot-notice-shaped" on every future poll forever (harmless, since it's still excluded, but never actually watermarked). `settleBotServiceNotices` (`engine/bot_notice_settle.go`) runs unconditionally every poll over the raw `board.Items` snapshot — mirroring `settleMergeTrainMemberCloses`'s (ADR-061) precedent for scanning outside `deepFetchCandidates`/dispatch-gated state — and applies the same 🚀 reaction + `CommentProcessed` watermark `finalizeComments` would have applied, for every comment matching `isBotServiceNotice` that isn't already watermarked. Unlike the merge-train scan, it carries no retry/escalate/label machinery: a missed reaction this poll is harmless (the comment stays both unprocessed and unreacted) and simply retries next poll for free.

**3. Reply suppression on `FABRIK_NO_WORK_NEEDED`.** Independent of the pre-admission filter — this covers the case where a comment *is* admitted and processed, but Claude's verdict is "no action needed." `publishCommentOutput` (`engine/comments.go`) now captures `noWorkNeeded := CheckNoWorkNeeded(output)` immediately after computing `summary`, before any markers are stripped (`CheckNoWorkNeeded` matches the raw marker line). All three existing reply-post branches — the issue stage comment create/rewrite, the `post_to_pr` issue comment, and the review-reinvoke PR summary — are gated on `!noWorkNeeded`. The issue-body update (`FABRIK_ISSUE_UPDATE`), `fabrik:editing` removal, 🚀 reactions, `CommentProcessed` watermarking, and `FABRIK_STAGE_COMPLETE`-driven completion handling in `finalizeComments` are all unaffected — only the reply post itself is suppressed. This directly targets the incident's second pump: posting *any* reply, even a "not actionable" one, is what re-triggered the subscribed bot.

## Rationale

### Why can't the watermark side-effect live inside `findNewComments`?

`findNewComments` is a deliberately pure, `e.client`-free function reused by 7+ call sites and constructed directly (`&Engine{cfg, store}`, no client) by several existing unit tests (`TestFindNewCommentsFiltering`, `TestHumanNewComments*`). Adding a network call (`AddCommentReaction`) or store write inside it would nil-panic those tests and, even if guarded, would fire redundantly — `findNewComments` runs 2–4× per item per poll (the dispatch gate, `processItem`'s own check, the catch-up loop, plus the pause/awaiting-input branches), so the same comment would be reacted-to and watermarked multiple times in a single poll pass.

### Why can't the watermark side-effect live inside `processComments` instead?

Once `findNewComments` excludes bot-notice comments, `itemNeedsWork` sees zero new comments for a bot-notice-only backlog and never dispatches `processItem` at all — there is no invocation of `processComments` for the watermark step to live inside. A settle scan sourced from the raw board snapshot is the only place that reaches these comments regardless of dispatch outcome.

### Why a narrow, literal pattern list instead of a bare `"quota"`/`"rate"` substring?

Two reasons, both concrete rather than hypothetical. First, the issue's own false-positive risk: a broad substring could catch genuine bot review prose that happens to discuss rate limiting (e.g. a review comment suggesting a rate-limit fix). Second, `engine/blocked_on_input_test.go` has five existing fixtures using the literal bot-authored body `"quota notice"` (ADR-069's own test suite, exercising the human-only resume gate) that must **not** be caught by this filter — they test a different concern (bot chatter must still be admitted and processed together with a resuming human comment) and a collision would silently break that coverage. None of the six chosen patterns match `"quota notice"`, verified both by inspection and by a dedicated unit test case (`TestIsBotServiceNotice`'s "existing quota notice fixture body must not collide") and by running the full existing suite unchanged.

### Why scope classification to `item.Comments` only, not `item.LinkedPRReviewThreadComments`?

The incident and every acceptance criterion concern conversation-thread bot replies, not inline PR review comments. Inline review comments are substantive-by-construction (they always carry a specific finding tied to a diff location) and excluding them from this filter is the conservative choice the issue's "must not skip genuine review content" requirement calls for.

### Why capture `noWorkNeeded` before stripping markers, not after?

`CheckNoWorkNeeded` matches `^FABRIK_NO_WORK_NEEDED$` against the raw output. `publishCommentOutput` strips that exact line a few statements later (to keep it out of the posted comment even when a reply *is* posted, e.g. on the stage-invocation path elsewhere in the engine). Checking after stripping would always observe `false`.

### Why does the stage-invocation meaning of `FABRIK_NO_WORK_NEEDED` (advance to Done) stay untouched?

This ADR only extends the marker's handling on the **comment-processing** path — a different code path (`processComments`/`publishCommentOutput`) than the stage-invocation path (`processItem`'s stage-run branch, ADR-045/ADR-060) that treats the same marker as "skip remaining stages, move to Done." The issue scopes this explicitly, and the two paths don't share code that would make this ambiguous — `publishCommentOutput` is only ever called from `processComments`.

## Consequences

**Positive:**
- A gemini quota-notice comment (or any future bot service-notice matching the pattern list) now spawns no worker, posts no reply, and is watermarked within one poll cycle — the two halves of the #1083 pump (worker cost, reply-triggered bot re-ping) are both closed for this specific comment shape.
- The two mechanisms are independent and additive: the pre-admission filter stops the *known* notice shapes before any cost is incurred; the reply-suppression mechanism is a second line of defense for *any* comment (bot-notice or otherwise) where Claude itself concludes no action is needed, closing the loop even for notice phrasings the pattern list doesn't yet cover.
- No new labels, no new config/YAML surface, no change to `github.IsBotLogin` — purely additive, low-blast-radius changes confined to `engine/comments.go`, a new `engine/bot_notice_settle.go`, and one new call in the poll loop.

**Negative / Trade-offs:**
- **False negatives are accepted by design.** The pattern list is deliberately narrow; a differently-worded quota/rate-limit message from a different bot will not be caught by the pre-admission filter (though it may still be caught by the reply-suppression mechanism if Claude classifies it as no-action-needed during processing — at the cost of one worker invocation instead of zero). Extending the pattern list is a follow-up, not a blocker.
- **The settle scan is a second unconditional per-poll pass over all board items**, alongside `settleNoWorkNeededScan`, `settleRevalidateScan`, and `settleMergeTrainMemberCloses`. The cost is in-memory string matching over already-fetched data (no additional GitHub API calls except the reaction/watermark writes themselves, which only fire once per matching comment) — the same justification ADR-061 already established for its sibling scan.
- **A bot-notice comment that arrives during a pause is now silently dropped from the "process together on resume" backlog** (ADR-069) rather than included — this is the correct behavior (it's non-actionable regardless of pause state) but is a deliberate behavior change from ADR-069's "all accumulated bot chatter is processed together with the resuming human comment" framing for this one comment shape.

## Related Work

- Incident #1083 (human incident report — not modified by this ADR or its issue).
- ADR-069 / #1087 — fix 1/4, the direct predecessor; establishes `github.IsBotLogin` as load-bearing for comment-author filtering and the precedent this issue's pattern-matching layers on top of.
- #1089, #1090 — fixes 3/4 and 4/4 of the same runaway-loop remediation series (comment-processing circuit breaker; decoupling Fabrik's own writes from cache invalidation/re-poll).

**References:** [docs/state-machine.md §4.1, §4.2, §4.4](../docs/state-machine.md)
