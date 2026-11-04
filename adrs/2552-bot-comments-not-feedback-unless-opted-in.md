# ADR 2552: Bot-authored conversation comments are not feedback unless the bot is opted in

## Status

Accepted. Applies the idea of ADR 2489 (a clean review from an expected reviewer is not actionable) to conversation comments. Uses the holding-stage context of ADR 2525. Does not touch review threads, review bodies or the review-gate verdict (ADR 2465).

## Context

The linked PR's conversation comments are merged into `item.Comments`. `filterNewComments` is the one funnel that decides which of them are unprocessed feedback. It dropped a bot comment only when it matched a known service-notice phrasing. Any other bot comment, such as Vercel's deploy notice on every `fabrik/*` PR, was feedback: it started a paid comment-review pass that found nothing to do and held the feedback gate until then (about $1.40–$3.20 per issue on 0.0.89, community report #2497).

## Decisions

1. **The rule lives in the shared funnel.** `filterNewComments` gains a required `botOptedIn func(author string) bool` parameter. A comment whose author is a bot login (`gh.IsBotLogin`, unchanged) is dropped unless `botOptedIn(author)` is true. A nil predicate opts no bot in, so a caller that forgets it fails safe against the cost leak and shows up in the agreement test. The four direct callers (`findNewComments`, `cachedPendingFeedbackOf`, `cachedFeedbackArrivedSince`, `markCommentsSeenByStage`) all pass `Engine.botCommentOptIn(status, number, labels)`; `TestFilterNewCommentsCallersPassTheOptIn` scans the sources so a new caller must too, and `TestBotCommentOptInAgreesAcrossReaders` pins the verdicts (undeclared, declared, label-overridden, Queued). The rule applies to both comment sources (issue and PR), since both come through the funnel.

2. **Opt-in is the union of the effective `expected_reviewers` and a new stage key `feedback_bots`.** Reusing `expected_reviewers` alone is too narrow for the bot class this issue is about: a declared reviewer counts as outstanding until it submits a formal review (`declaredReviewersOutstanding`), so a comment-only bot declared there would hold the review gate, run the re-prompt ladder and pause at timeout. `feedback_bots` opts a bot in for comments without touching the gate. A declared reviewer is also opted in, so a bot that posts both reviews and comments needs no config change. Identities use the `expected_reviewers` form (bare handle, no `@`, no `[bot]`, no duplicates), matched with `reviewerIdentityMatches`. `feedback_bots` is stage-level only; it has no label override, while the `expected-reviewers:` label still flows through the effective declaration. Rejected: `expected_reviewers` only (the gate-wait trap), `feedback_bots` only (it would ignore a declared reviewer's comments, which is surprising).

3. **The stage evaluated is the item's current Status, resolved through `reviewContextStage`.** Same as `cleanReviewSkipper`: a PR comment posted at Implement and pending at Validate is judged against Validate's declaration, and a Queued member decides as the stage it was validated in (ADR 2525).

4. **Skipping is derived state; two writers stop touching skipped bots (R5).** The funnel is pure. `markCommentsSeenByStage` used to 🚀-react and watermark every pre-stage comment the funnel did not report as pending; `settleBotServiceNoticesForItem` did the same for known notices. Both now skip a bot that is not opted in (`skipsBotComment`), so a skipped comment gets no reaction, no `CommentProcessed` marker, no comment and no label. Because it is never watermarked, nothing re-admits the item for it: `itemNeedsWork` and `postRunFeedbackAdmission` read through the same funnel (`TestUndeclaredBotComment_NoReadmission`). For an opted-in bot both writers behave as before and the service-notice exclusion still applies (FR-003). `filterBotServiceNotices` in `processCommentsClassified` is unchanged (R4: thread and reinvoke handling).

5. **`IsBotLogin` is not redefined.** Any login ending in `-bot` is a bot, so a human whose login ends that way has their comments dropped. This is consistent with `filterHuman` (pause resume already ignores such logins) and the exposure is accepted.

6. **Accepted behaviour change.** An undeclared bot whose comments carried findings (for example a bot summary comment) goes quiet until it is added to `feedback_bots` or `expected_reviewers`. This is documented in the user guide and the state-machine page.

7. **Unchanged.** Review threads, review bodies, `CHANGES_REQUESTED`, the review gate, `filterHuman`, and the pause-resume predicate (`resumeAuthorised`, human comments only).

## Consequences

- An issue whose PR gets only undeclared-bot conversation comments dispatches no comment-review worker for them, holds no gate and spends no tokens.
- New stage key `feedback_bots`; no new label, flag or env var.
- Seam: `Engine.SetBotCommentRuleDisabledForTest` makes `botCommentOptIn` opt every bot in, in every reader and writer at once, so the control run of the sim twin `tests/sim/bot_comment_feedback_test.go` can only differ because of the rule. New sim fixture `simgh.SeedPRComment` seeds a PR conversation comment from an arbitrary author.
- Mentioned for context: community report #2497, #2489, #2414.
