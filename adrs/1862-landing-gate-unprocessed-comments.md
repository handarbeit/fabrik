# ADR 1862: Landing gate on unprocessed comments, and a post-merge comment guard

**Status:** Accepted
**Date:** 2026-09-26
**Issue:** [#1862](https://github.com/handarbeit/fabrik/issues/1862)
**Origin:** report #1832

## Context

Fabrik landed a Validate-complete item while an unprocessed comment was already
known, then processed that comment after the merge. The worker committed the
requested change to the merged PR's branch, where it was orphaned, and the comment
still got 👀 → 🚀, so it looked handled when it was not (#1832, `merge_train: off`):

```
11:11:57  maintainer posts a steering comment on the PR
11:12:02  maintainer swaps fabrik:cruise for fabrik:yolo
11:12:53  [merge] PR #885: merged using method "merge"
11:12:56  [#868 comments] processing 1 new comment(s) — stage: Validate
11:21:21  [#868 done] comment processing complete     # commit pushed to the merged branch
```

Within one poll, the catch-up loop (Phase 1 handlers, then Phase 2) runs **before**
`dispatchCandidates`, and both work from the same deep-fetched snapshot. Phase 2's
Validate branch calls `attemptMergeOnValidate` and returns before the
`findNewComments` guard every other stage's advance has; dispatch then processes the
comment against a merge that has just happened. With `merge_train: on` the same
sequence pushed the item into `Queued` with its comment unanswered.

`attemptMergeOnValidate` is the single landing-decision owner for all three landing
paths (auto-merge / enqueue, the direct-merge fallback, `advanceToQueued`) and is
called from both `runCatchUpPhase2` and `handleStageComplete`. It had a review gate
(`reviewGateBlocksLanding`, ADR-1216) but no comment check.

## Decision

Two independent mechanisms.

### A. Comment gate at the landing decision

`commentGateBlocksLanding` (`engine/comment_landing_gate.go`) is called inside
`attemptMergeOnValidate`, after the dependency guard's live `FetchItemDetails`
re-read and immediately before `reviewGateBlocksLanding`, so it sits ahead of the
`merge_train` fork and both modes are gated identically. The direct-merge fallback
repeats the check against its own re-read just before `MergePR`.

- **Predicate: `findNewComments`, unchanged.** It is the predicate the non-Validate
  advance guard in `runCatchUpPhase2` uses, so the two can never disagree about what
  is unprocessed. It also consults the in-memory `CommentProcessed` store, so a
  comment processed in the same `finalizeComments` cycle reads as processed before
  GraphQL reflects its 🚀.
- **Live read.** The gate reads the `item.Comments` the dependency guard's re-read
  just refreshed, so the stage-completion call site — which holds a pre-invocation
  snapshot that can be tens of minutes old — sees current comments. If that re-read
  fails, the gate **holds** (conservatively, as ADR-1216 does for review state); the
  dependency guard's own fail-open is unchanged.
- **Return contract: `deferred=true`.** A hold returns `(false, true, nil)` — the
  "landing deferred" return guard 1 already uses. `handleStageComplete` treats it as
  "do not advance", so a `wait_for_ci: false` + yolo item cannot fall through to
  `advanceToNextStage` unmerged. (The completion label is still applied, exactly as
  for guard 1, so Phase 2 re-enters the decision.)
- **Process, then re-evaluate — no new plumbing.** The held item keeps
  `stage:Validate:complete`; dispatch processes the comment through the normal path;
  any commit changes the PR head SHA, which `settleSHAInvalidationScan` turns into a
  Validate re-run so the CI gate verifies the new head before any merge. With no
  commit, the next poll finds the comment 🚀'd and lands.
- **Re-admission.** A hold records `CooldownRecorded{Reason: "comment-pending"}`,
  mirroring `handleReviewGate`'s `review-blocked`, so `itemMayNeedWork`'s expiry path
  re-evaluates the held item on a timer rather than depending on incidental
  `updatedAt` movement. The plain new-comment dispatch path has no cooldown gate, so
  this never suppresses processing the comment.

### B. Post-merge guard at the comment-processing chokepoint

`postMergeCommentGuard` (`engine/post_merge_comments.go`) runs in
`processCommentsClassified` — through which every comment-processing invocation
passes (the three `processItem` routes and the review, CI-fix and rebase reinvokes) —
after `filterBotServiceNotices` and **before** `recordCommentBreakerInvocation`, 👀,
`fabrik:editing`, the worktree and Claude. If the item's work already landed it:

- invokes **no** worker and pushes nothing;
- posts one reply (issue and linked PR) saying the work already landed, the change
  was **not** applied, and it needs a new issue — only when the batch contains a
  human-authored comment with a `DatabaseID` (bot-only and synthetic reinvoke
  batches have no commenter to tell);
- adds **no** 🚀 to the triggering comment;
- returns the new `didNotRunPostMerge` kind with a nil error (the
  `didNotRunSuspended` precedent), so reinvoke dispatchers refund their cycle
  counters.

Because it runs before the breaker record, a guarded cycle never feeds the comment
breakers and is unaffected by `resetCommentBreaker`.

**"Already landed" (`itemPRAlreadyLanded`)** acts only on positive evidence:
`fabrik:credited-pr:<N>` or `fabrik:awaiting-landing-verification` (the merge-train
case, where the member's own PR is closed-not-merged, ADR-1616), or a linked PR that
is merged — for a closed-not-`Merged` PR confirmed with `FetchPRMerged`, because the
list endpoint's flag lags a merge by seconds (as `advanceValidateTerminalItem` does).
It reads `e.client`, never the boardcache-backed `readClient`. A read error, no PR, an
open PR, or a human-closed PR with no landing label all mean "not landed" and fall
through to normal processing; human-closed PRs stay on `pauseForPRClosedNotMerged`.

**Durable reply dedupe.** Leaving the comment without 🚀 means it stays "new", so
without a durable record every poll on a merged-but-open item would re-dispatch and
re-reply. The reply embeds `<!-- fabrik:post-merge-comment:<commentID> -->` and the
guard skips comment IDs whose marker is already present in `item.Comments` (the
`hasPauseComment` precedent). An in-memory `CommentProcessed` watermark was rejected:
it would hide the comment until a restart and answer it a second time after one.

This deliberately breaks "processed ⇒ 🚀" for one class of comment (ADR-009): a
comment that was answered but not applied must not look applied.

## Deadlock analysis

The gate counts exactly what dispatch will process, so a comment it holds on is one
dispatch acts on. Every case where a counted comment is not processed ends in either
a transient wait or the existing pause — never a silent, indefinite block.

| Situation | Outcome |
|---|---|
| Engine's own comments (`🏭` prefix), bot quota/sunset notices | Excluded by `findNewComments`. Cannot block. |
| Pruefer review summaries and inline findings | Live in `LinkedPRReviews`/`LinkedPRReviewThreadComments`, not `item.Comments`. Cannot block. |
| Other bot issue/PR comments (Copilot/Gemini/CodeRabbit summaries) | **Counted.** Dispatch processes and 🚀's them, so the hold is one comment cycle. |
| Paused / awaiting-input item | The catch-up loop skips paused items; nothing lands. Comment-breaker, no-op-breaker trips and tools-denied at its bound all land here — the existing pause. |
| Unprocessable comment after a manual unpause | Still "new", so dispatch retries and the breaker re-trips; the pause is visible each time (ADR-1813). A user 🚀 is a manual escape hatch. |
| Worker in flight / `fabrik:editing` | Transient hold; the comment is being processed. |
| Claude usage-limit suspension | Holds for the suspension (up to 8 days); dispatch is skipped identically. |
| `fabrik:locked:<other-user>` | Dispatch skips it and Phase 2 has no lock check, so the non-owning instance holds. Accepted pre-existing multi-instance behavior. |
| Closed item at Validate | Never reaches Phase 2 (ADR-1387). |

## Alternatives considered

- **Gate at the call sites.** A guard before Phase 2's Validate branch leaves the
  stage-completion and merge-train paths ungated. Rejected; `attemptMergeOnValidate`
  is the only place all three landing paths and both call sites converge.
- **A human-only predicate** (`filterHuman(findNewComments)`, or a shared
  "actionable" predicate used by both this gate and the non-Validate guard).
  Reading AC3 literally for *all* bots would need it, but it changes the existing
  non-Validate guard and redefines "unprocessed", which the spec forbids. A
  third-party bot comment costs one dispatch cycle and cannot deadlock, so the
  existing predicate is kept. If that cost proves material, a shared predicate is the
  follow-up.
- **A fourth return value** instead of `deferred=true`. Rejected: the existing
  `reviewThreadDeferred` handling in `handleStageComplete` is exactly the required
  behavior.

## Accepted residual races

- A comment posted between the gate's read and the merge is caught only on the
  direct-merge path (its second read). The enable-auto-merge and enqueue paths have no
  second read; GitHub merges later. The post-merge guard is the backstop for the
  direct-merge outcome but cannot undo a merge GitHub performs afterwards.
- Comments arriving after `fabrik:auto-merge-enabled` is set are not gated —
  `attemptMergeOnValidate` returns at that label and Phase 2 returns before the
  guard. Only the post-merge guard protects that convergence window (ADR-1216's
  guard 2 is the precedent if it needs closing).
- Comments on a member already in `Queued` are out of scope here; they are handled by the Queued settle scan's comment cause (ADR-1863).

## Consequences

- A comment on a Validate-complete yolo or train item holds the landing until it is
  processed; the cruise → yolo swap after commenting is no longer a race.
- A comment that arrives after the work landed is answered with a "not applied" reply
  instead of being applied to a dead branch and 🚀'd.
- Not changed here, but observed while writing the sim scenarios: with
  `merge_train: on` and Validate `wait_for_ci: false`, a successful `advanceToQueued`
  returns `(false, false, nil)` into `handleStageComplete`'s ordinary advance. That
  pre-dates this change and is not part of the shipped default (`validate.yaml` sets
  `wait_for_ci: true`).

## References

ADR-009 (comment processing), ADR-1089/1413/1555 (comment breakers), ADR-1216 (the
landing-decision review gate this mirrors), ADR-1387 (closed items), ADR-1616
(landing verification and `fabrik:credited-pr:<N>`), ADR-1802 (stage rework
re-entry), ADR-1813 (pause resume). `docs/state-machine.md` §2.2, §6.6.6.
