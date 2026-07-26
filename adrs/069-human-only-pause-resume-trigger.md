# ADR 069: Human-Only Pause/Awaiting-Input Resume Trigger

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1087 — Runaway-loop fix (1/4): only human comments lift fabrik:paused / awaiting-input

## Context

Incident #1083: a bot quota-notice ping-pong drove Fabrik into an unbounded, unpausable comment-processing loop (995 worker invocations, roughly $210, over 12 hours). `itemNeedsWork` and `processItem` (`engine/item.go`) treated *any* new comment on a `fabrik:paused` or `fabrik:awaiting-input` issue as an implicit "the user wants to resume" — stripping the pause and dispatching comment processing without checking whether the comment came from a human, a bot (`gemini-code-assist`, `*[bot]`, `dependabot`, `copilot-*`), or Fabrik's own output. During #1083 a bot re-commented on the thread every cycle, so an operator-applied `fabrik:paused` was silently stripped on the very next poll — leaving no in-band way to halt the loop short of `kill -9`. `fabrik:paused` is meant to be the operator's kill switch; a mechanism that lets bot chatter silently override it defeats that guarantee.

## Decision

Restrict the paused/awaiting-input resume trigger to human-authored comments. A new method, `humanNewComments` (`engine/comments.go`, immediately after `findNewComments`), calls `e.findNewComments(item)` and filters out any comment whose `Author` is a recognized bot login (`github.IsBotLogin`, already covering `gemini-code-assist`, `*[bot]`, `*-bot`, `copilot-*`, `dependabot`). It composes on top of `findNewComments` rather than reimplementing its dedup/rocket-reaction/Fabrik-prefix logic, so there remains a single source of truth for "is this a new comment at all."

`humanNewComments` replaces `findNewComments` at exactly the four resume-decision sites in `engine/item.go`:
- `itemNeedsWork`'s awaiting-input check
- `itemNeedsWork`'s paused check
- `processItem`'s awaiting-input resume block
- `processItem`'s paused unpause loop

Both the gate (`itemNeedsWork`) and the action (`processItem`) call the same shared method, so they cannot drift into disagreement — a scenario where the gate would admit an item for dispatch and the action would then silently no-op, leaving a stuck-but-not-obviously-so state.

In `processItem`, `humanNewComments` is used only to decide *whether* to resume. Once a human comment authorizes it, both the awaiting-input and the paused branches hand the full raw comment set (`e.findNewComments(item)`) to `processComments`, in the same invocation — not just the human comment that triggered the resume. This matters because during a pause a bot may have posted several comments that were never consumed (no reaction, no `CommentProcessed` record, since `humanNewComments` filtered them out of the *decision* but `findNewComments` still returns them for *processing*). Passing the raw set once resumed ensures that backlog is processed together with the resuming comment in one pass, rather than being picked up as a second, separate comment-processing invocation on the next poll.

`findNewComments` itself, and its one remaining raw-form caller — the non-paused new-comment dispatch check (`engine/item.go`, `newComments > 0 ⇒ dispatch`) — are unchanged. Bot-authored comments and reviews continue to trigger comment processing on a non-paused issue exactly as before; only the ability of a bot comment to silently resume a *deliberate pause* is removed. The `fabrik:awaiting-review` gate (`engine/reviews.go`) has no dependency on `findNewComments` at all and is unaffected — it is cleared by a formally submitted PR review, independent of the comment-based resume path.

## Rationale

### Why a new method rather than inlining the author check at each site?

Four call sites independently re-deriving the same "is this comment human" predicate is the exact drift risk this ADR is designed to close: if one site's inline check diverged even slightly from another's, the gate and the action could reach different conclusions about the same comment, producing an item admitted into dispatch that then does nothing. A single shared method makes that class of bug structurally impossible — there is only one place the predicate is defined.

### Why filter by author rather than by comment content or a new marker?

The author is already reliably available (`gh.Comment.Author`, populated by the deep fetch) and `github.IsBotLogin` already exists, is exported, and already covers every bot pattern named in the incident. No new GitHub-side primitive (label, reaction, marker) is needed, and no heuristic on comment body text is required or desired — body-text heuristics are fragile and this fix does not need one.

### Why not also exclude `e.cfg.User` (Fabrik's operator identity)?

An earlier revision of `humanNewComments` also excluded any comment whose author matched `e.cfg.User`, reasoning that this was "Fabrik's own identity." That reasoning was wrong and the exclusion was removed during review (caught by an automated PR reviewer). `e.cfg.User` is the **operator's** GitHub login (`--user` / `FABRIK_USER`) — the account `blockOnInput` explicitly @-mentions when posting "awaiting your input … Reply on this issue to resume" (`docs/USER_GUIDE.md` §"Stages Waiting for Input"). It is not a bot or service-account identity, and in the common (currently only supported — `--filter-user`, #671, is future work) single-account deployment, Fabrik itself posts under that same account. Excluding `c.Author == e.cfg.User` would therefore have filtered out the operator's own resume reply in exactly that deployment shape — recreating this issue's own bug (an unpausable pause), just triggered by the human instead of a bot. It also bought nothing: every Fabrik-authored comment already carries the `🏭 **Fabrik` prefix and is unconditionally excluded further upstream, in `findNewComments`, independent of author. `humanNewComments` therefore filters on bot login only.

### Why not also gate the non-paused dispatch path?

Out of scope, and deliberately so. The incident's root cause was specifically that a bot comment could defeat an *operator's explicit pause*. Bot-authored comments and PR review comments on a non-paused issue are legitimate signals Fabrik should keep acting on (e.g., "please address Copilot feedback" nudges, automated review threads) — narrowing that path too would regress real functionality the incident did not implicate.

### Why is this only fix 1 of 4?

This ADR intentionally addresses only the resume-trigger authorship gap. Sibling issues (#1088, #1089, #1090) cover bot service-notice handling, a circuit breaker, and self-write/cache decoupling — the other contributing factors identified in the #1083 postmortem. Each is independently scoped and independently landable; none is blocked by or blocks this one at the code level.

## Consequences

**Positive:**
- `fabrik:paused` and `fabrik:awaiting-input` are now an effective operator kill-switch: bot chatter can no longer silently strip either label.
- The fix is small and additive — one new filtering method, four call-site substitutions, no new labels, no new config surface, no change to `findNewComments`'s own semantics.
- Gate/action symmetry is structurally guaranteed by sharing one method, eliminating an entire class of stuck-item bug.

**Negative / Trade-offs:**
- An operator who *wants* a bot comment to resume a paused issue (e.g., manually re-triggering after reviewing a bot's suggestion) must now leave a human comment instead; this is the intended behavior change, not a regression.
- Bot chatter accumulated during a pause/awaiting-input wait is processed together with the resuming human comment in the same `processComments` invocation (see Decision) — not silently dropped, and not deferred to a later poll.

**Fixed during review — test fixtures modeling the operator's own reply:** an intermediate revision of this fix (before the `e.cfg.User` exclusion above was identified and removed) briefly renamed three fixtures' comment author from `"testuser"` to `"humanuser"` in `engine/blocked_on_input_test.go`. Those fixtures were not incidental — `Author: "testuser"` modeled the operator answering the awaiting-input question under the same account `testEngine`'s harness configures as `cfg.User`, matching `buildAwaitingInputComment`'s `@testuser` mention. Renaming them was a workaround that made the suite pass by testing a different (now-unaffected) scenario instead of catching the regression. They have been reverted to `Author: "testuser"`, restoring coverage for the actual deployment shape the `e.cfg.User` exclusion would have broken.

## Related Work

- Incident #1083 (human incident report — not modified by this ADR or its issue).
- #1088, #1089, #1090 — fixes 2–4 of the same runaway-loop remediation series.

**References:** [docs/state-machine.md §1.1, §1.3, §2.2, §4.4, §8.1, Appendix C](../docs/state-machine.md)
