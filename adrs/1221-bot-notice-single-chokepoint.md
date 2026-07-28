# ADR 1221: Single Chokepoint for Bot Service-Notice Exclusion in processComments

**Date**: 2026-07-28
**Status**: Accepted
**Issue**: #1221 — Bot service notices bypass `isBotServiceNotice` via review threads and reinvoke
dispatchers

## Context

ADR-070 (#1088) introduced `isBotServiceNotice` and wired it into `findNewComments` as a
pre-admission filter over `item.Comments`, closing the #1083 runaway-loop incident for the
conversation-thread case. `docs/state-machine.md:902` documented the result as an unqualified
guarantee: "a quota/rate-limit bot notice never reaches `processComments()` via any path." That
claim was false for two independent reasons:

1. **Three dispatchers bypass `findNewComments` entirely.** `dispatchReviewReinvoke`,
   `dispatchCIFixReinvoke`, and `dispatchRebaseReinvoke` (`engine/reinvoke.go`) call
   `processComments` with a comment slice they build themselves — `buildReviewThreadComments()`
   for the review-reinvoke path, a single synthetic comment for the other two. None of these
   builders route through `findNewComments`, so none apply `isBotServiceNotice`.
   `buildReviewThreadComments` in particular filters only ROCKET reaction and `CommentProcessed`
   — a bot service notice sitting in an unresolved review thread passes straight through.
2. **`processComments` itself unconditionally merges `LinkedPRReviewThreadComments`.** Regardless
   of which caller invoked it, `processComments` (`engine/comments.go`, then lines 162–181) merged
   every unresolved `LinkedPRReviewThreadComments` entry into its working slice, gated only on
   ID-dedup / ROCKET / `CommentProcessed` — again, no `isBotServiceNotice` check. This affected the
   two normal `item.go` call sites too, not only the reinvoke dispatchers.

Review-thread comments are the highest-risk carrier: bot reviewers (Gemini, CodeRabbit) do post
non-actionable service notices via inline review comments, not only via issue comments — the
premise ADR-070 rested on for excluding `LinkedPRReviewThreadComments` from classification (see
Rationale below) turned out to be wrong. A bot service notice consumed as user input produces a
reply, which produces another notice — the same #1083/#1088 pump, entered through an
unclassified door.

## Decision

Add one function, `filterBotServiceNotices(comments []gh.Comment) []gh.Comment`
(`engine/comments.go`), that drops entries where `isBotServiceNotice(c)` is true, preserving
order. Call it exactly once, inside `processComments`, immediately after the
`LinkedPRReviewThreadComments` merge block and before any reaction, label, worktree, or invocation
side effect. If the filtered result is empty, `processComments` returns immediately (mirroring the
existing `claudeSuspendedUntilTime` early-return earlier in the same function).

Because every entry point into comment processing — the three `processItem` paths (via
`findNewComments`) and the three reinvoke dispatchers (via their own `build()` closures) —
converges on `processComments`, this single filter pass covers all of them structurally. No
changes were needed inside `engine/reinvoke.go`'s three dispatchers, `buildReviewThreadComments`,
or `findNewComments` itself: coverage of the reinvoke dispatchers is achieved by the fact that they
all hand their output to `processComments`, not by adding a second filter call at each call site.
This satisfies FR-3 ("one chokepoint, not duplicated per caller") literally.

`findNewComments`'s own filter is unchanged and still necessary — see Rationale.

## Rationale

### Why not filter inside each of the three reinvoke dispatchers instead?

That is exactly the duplication FR-3 forbids, and it's how this bug arose in the first place:
`isBotServiceNotice` was wired into `findNewComments` for the conversation-thread case, and every
other path into `processComments` was added later without anyone re-deriving that the same
classifier needed to apply there too. A single chokepoint inside `processComments` — the one
function every caller already shares — cannot silently diverge per-caller the way three
independent call-site filters can.

### Why keep `findNewComments`'s filter instead of relying solely on the new chokepoint?

`findNewComments`'s filter and the new chokepoint serve different purposes. `itemNeedsWork` treats
"zero new comments" as "nothing to do" — if `findNewComments` stopped excluding bot notices, a
notice-only `item.Comments` backlog would once again cause a worker to dispatch (editing label,
worktree setup, `JobStartedEvent`) even though the new chokepoint would immediately no-op the
resulting `processComments` call. Removing `findNewComments`'s filter would regress ADR-070's core
fix — the *dispatch-avoidance* half of it — while only this issue's *processing* half would remain
fixed. The two filters are complementary, not redundant: `findNewComments` decides whether a
worker runs at all for `item.Comments`; the new chokepoint decides whether a comment — from any
source — is ever handed to Claude.

### Why does this reverse ADR-070's `item.Comments`-only scoping?

ADR-070's Rationale section justified excluding `LinkedPRReviewThreadComments` from classification
explicitly: "The incident and every acceptance criterion concern conversation-thread bot replies,
not inline PR review comments. Inline review comments are substantive-by-construction... and
excluding them from this filter is the conservative choice." That assumption is what this issue
disproves: bot vendors do post non-actionable service notices via review threads, not only via
issue comments. This ADR does not change `isBotServiceNotice`'s classifier (the pattern list and
bot-login check are untouched, and are explicitly out of scope per the issue) — it changes *where*
that classifier is applied, extending its reach to a collection ADR-070 had deliberately carved
out. ADR-070's own text is left as the historical record of the prior (now-superseded) reasoning;
its Status line is updated to point here rather than being edited in place.

### Why is the dispatch-gating / cycle-count consumption left unfixed?

`dispatchReviewReinvoke`'s precheck and the catch-up loop's `ReviewCycles` increment
(`catch_up_handlers.go`) still use the unfiltered `buildReviewThreadComments(item)` count to decide
*whether to dispatch* and to bump the cycle counter — they do not consult the new chokepoint,
which only runs once a dispatch has already happened. A review thread containing only bot notices
therefore still causes a worker to dispatch every poll (immediately no-op'd once
`processComments`'s filter empties its slice) and still consumes a `MaxReviewCycles` slot, so it
will eventually self-limit via `pauseForReviewCycleLimit` rather than loop forever. This is the
same trade-off the issue text itself accepts for the adjacent watermarking question (extending
`settleBotServiceNoticesForItem` to `LinkedPRReviewThreadComments` is out of scope): the issue's
acceptance criteria are about a notice never being *processed as input*, not about dispatch
efficiency. Fixing the dispatch/cycle-count side would require touching
`buildReviewThreadComments`'s callers directly — a distinct concern, deliberately deferred as a
follow-up rather than silently left unexplained.

### Why an early return on an empty filtered slice, rather than proceeding with zero comments?

Before this issue, no caller ever invoked `processComments` with an empty `comments` slice — a
review-reinvoke proceeding past the merge/filter with zero comments would be new, untested
territory for `InvokeForComments`'s prompt-building. Returning immediately, in the same place and
style as the existing `claudeSuspendedUntilTime` guard, avoids exercising that untested path and
avoids unnecessary side effects (👀/🚀 reactions, `fabrik:editing` churn, worktree setup,
`JobStartedEvent`/`JobCompletedEvent`) for an invocation that has no actual work to do.

## Consequences

**Positive:**
- A bot service notice arriving via `item.LinkedPRReviewThreadComments`, or via any of the three
  reinvoke dispatchers, is now excluded before it ever reaches Claude — closing the highest-risk
  carrier identified in #1221 (review threads, where Gemini/CodeRabbit actually post).
- One chokepoint, not three (or four) duplicated filters — the failure mode that caused this gap
  (a fix applied at one call site, silently not re-derived at siblings added later) cannot recur
  here, since every path already converges on `processComments`.
- No change to `isBotServiceNotice`'s classifier, `findNewComments`, or any of the three
  dispatchers' `build()` closures — low blast radius, confined to one new function and one new
  call site plus an early return inside `engine/comments.go`.

**Negative / Trade-offs:**
- **Un-watermarked, notice-only review threads still consume dispatch/`ReviewCycles` budget** —
  self-limiting via `MaxReviewCycles`, not free. Extending `settleBotServiceNoticesForItem` to
  `LinkedPRReviewThreadComments` would close this residual cost but is explicitly out of scope for
  this issue (see Rationale).
- **The empty-slice early return is new terrain** for `processComments`, mitigated by placing it
  identically to the existing suspension-gate early return and by dedicated test coverage
  asserting zero side effects when a batch is entirely bot notices.

## Related Work

- ADR-070 (#1088) — origin of `isBotServiceNotice` and the `findNewComments` pre-admission filter;
  partially superseded by this ADR (see its updated Status line).
- #1083 — the original runaway-loop incident this whole remediation series traces back to.
- #1089/#1090 — sibling fixes in the same remediation series (comment-processing circuit breaker;
  decoupling Fabrik's own writes from cache invalidation/re-poll).

**References:** [docs/state-machine.md §4.1, §4.4, §6.2](../docs/state-machine.md)
