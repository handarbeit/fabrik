# ADR 2444: Resolve webhook-added thread comments by identity

## Status

Accepted. Amends ADR 2352 (resolve matching).

## Context

A review-thread comment that reaches the Store from a `pull_request_review_comment` `created` delivery is stored with no `ReviewThreadID`: the payload carries none, so the handler invalidates the deep cache and waits for the next deep fetch to fill the identity in. ADR 2352's `ReviewThreadResolved` matched stored comments on `ReviewThreadID` alone, so a `pull_request_review_thread` `resolved` delivery arriving before that fetch found nothing, was a silent no-op, and the resolved thread stayed in the cache-only feedback views (`fabrik_status` `pr.reviewers.outstanding`, `validate-settled` `meta.reviewers`, the attention classifier, derived channel events) for up to one poll interval. Landing and advance decisions read feedback live (ADR 2350), so nothing merged or advanced on the stale view. Seen in the 0.0.90 gate (`TestWebhookReviewThreadFacts`).

## Decision

1. The `resolved` payload's `thread.comments[]` (node ID and numeric ID) is decoded (lenient scalars, ADR 2383) into `ReviewThreadResolved.CommentNodeIDs` / `CommentDatabaseIDs`. No GitHub call is made (ADR 2352).
2. A stored comment is dropped when its `ReviewThreadID` equals the thread, **or** when its `ReviewThreadID` is empty and its node ID is listed or its non-zero database ID is listed.
3. Identity matching applies only to comments with an empty `ReviewThreadID`. A comment tagged with another thread is never removed by this thread's resolution, so wrong-thread removal is ruled out structurally rather than by trusting payload contents.
4. Everything else is unchanged: the observed-at order check before the drop, the order key recorded only when something was removed, `ResolvedThreadCount` bumped only when something was removed (replay and unknown threads change nothing), and the deep-cache invalidation on `created` stays as the backstop.
5. Alternative rejected: stamping the thread ID onto stored comments from the resolve payload. It mutates stored data and benefits nothing else.
6. Neutralisation seam: `Store.SetThreadResolveByIdentityDisabledForTest` (per Store) with an `Engine` passthrough, so the sim twin shows it is not vacuous and stays `t.Parallel()`-safe.

## Consequences

- A resolve before any deep fetch clears webhook-added comments immediately.
- A missing or empty `thread.comments[]` degrades to the previous behaviour.
- If only some of a thread's comments are stored, those are dropped; a later `created` for another comment of the already-resolved thread reappears until the next deep fetch (the out-of-order case, left to the existing ordering rule).
- Follow-up (not done here): `TestWebhookReviewThreadFacts` waits one poll interval before resolving (PR #2443) and could drop that wait.
