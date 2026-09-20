# ADR 1813: A Pause Is Resumed Only by a Comment That Postdates It

**Date**: 2026-09-20
**Status**: Accepted
**Issue**: #1813 — the paused-resume gate re-fires on the same unprocessed comment every poll, so a circuit-breaker pause can never hold

## Context

ADR-069 (#1083) restricted the paused / awaiting-input resume trigger to human comments, but it still authorised the resume from a *state*: "an unprocessed human comment exists" (`filterHuman(findNewComments(item))`). "New" means *not yet marked processed*, and a comment only becomes processed when an invocation succeeds. When processing can never succeed (a session limit misreported as `api_error`, a poisoned session, a comment that reliably crashes the stage) the comment stays "new" forever, so the gate lifted the pause every poll. The resume also ran `clearFailedStage` / `unblockAwaitingInput`, zeroing the breaker counters, so each trip re-armed from zero. On #1752 the comment-processing breaker tripped and was overridden ten times in ten minutes (139 invocations in 20 minutes), and ADR-069's "honorable pause" guarantee did not hold for exactly the issues most likely to need it.

Four call sites carried this shape: `itemNeedsWork`'s awaiting-input and paused branches, and `processItem`'s awaiting-input block and paused loop.

## Decision

Authorise a resume from an *event*: a human comment created at or after the moment the current pause began. One shared predicate, `(*Engine).resumeAuthorised(item) (authorised bool, raw []gh.Comment, refused int)` in `engine/comments.go`, replaces the four inline `filterHuman(findNewComments)` tests (and the dead `humanNewComments`).

- `raw` is the unchanged `findNewComments` set, so an authorised resume still hands the whole backlog, including bot chatter, to `processComments` in the same pass (R5). Only the boolean changes.
- **Anchor = the latest `labeled` event for `fabrik:paused`**, read with `client.FetchLabelAppliedAt`. `isAwaitingInput` implies paused, and `pauseIssue` / `reapplyPauseLabels` apply both labels within seconds, so the single label covers both gates (R4). Using `fabrik:paused` alone also means any gap between the two events resolves toward resume.
- The anchor is read **directly, not through `e.labelAppliedAt`'s record-on-write cache** (ADR-1314). That cache skips writing when an entry exists, so a human removing the label in the GitHub UI leaves a stale early timestamp that a later pause would inherit, silently re-arming the loop. The cost is one paged REST call, incurred only when a paused item already has an unprocessed human comment.
- A comment refuses the resume only when it **strictly predates** the anchor.

### R6 tie-break: every indeterminate input resumes

An anchor fetch error, no `labeled` event found (zero time), a human comment with a zero `CreatedAt`, or a comment timestamp equal to the anchor (both have one-second resolution, so equality proves nothing) all authorise the resume. Only the positive finding "every human comment strictly predates the pause" refuses. An extra resume costs a wasted cycle; a pause nobody can lift is an operator emergency.

### Alternatives considered

| Option | Verdict |
|---|---|
| (a1) latest `fabrik:paused` labeled event | **Chosen.** Produced by every pause producer, including an operator's manual label and the direct `AddLabelToIssue`/`addLabel` paths in `merge_train.go`, `settle.go`, `spawn.go`; durable in GitHub's event log (R3). |
| (a2) Fabrik pause comment's `created_at` | Rejected. Pause comment text differs per domain (no single signature to scan for, unlike `hasCIGatePauseComment`'s per-domain fragments) and several pauses, including an operator's, post none. |
| (b) 🚀-react the triggering comment at pause time | Rejected. Misreports processing state and would permanently hide the comment from a later legitimate invocation. |
| (c) watermark label | Rejected. New label vocabulary and cleanup for no benefit over (a1). |

## Consequences

- One comment authorises at most one resume (R1): after it, any re-pause writes a newer `labeled` event and the comment predates it. A comment arriving after the pause still resumes on the next poll (R2), even beside an older unprocessable one.
- No in-memory state is consulted for the decision (R3); the anchor lives in GitHub.
- R7 follows without a code change to the resets: `clearFailedStage`, `unblockAwaitingInput`, `CommentBreakerReset` and `EngineCyclesCleared` simply stop running when the gate refuses. Tests assert the counters are untouched.
- **Accepted trade-off:** a human comment posted *during* an invocation that then pauses (`blockOnInput`, a breaker trip) predates that pause and does not resume it; the human must comment again.
- Future pause producers must generate a `labeled` event for `fabrik:paused`, which any label application does inherently.
- Refusals log a distinct line ("N human comment(s) predate the pause") so operators can tell them from the bot-only "none human-authored" case.

## See Also

- ADR-069 (supplemented), ADR-1089 and ADR-1555 (comment breakers whose resets now follow only a genuine resume), ADR-1314 (label-applied-at cache, bypassed here).
