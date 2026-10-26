# Feature Specification: A webhook-resolved review thread leaves no stale feedback in the Store, even for comments added by webhook

**Feature Branch**: `fabrik/issue-2444`
**Created**: 2026-10-25
**Status**: Specified
**Input**: User description: "## Problem

A review-thread comment that reaches the Store by webhook (`pull_request_review_comment` `created`) is stored with no `ReviewThreadID`: the payload carries none, so `boardcache/delta.go` applies `ReviewThreadCommentAdded` and invalidates the deep cache so the next deep fetch fills it in. Until that fetch, a `pull_request_review_thread` `resolved` delivery for the same thread finds nothing to drop (`ReviewThreadResolved` matches on `ReviewThreadID`), so the resolved thread stays counted in the Store's feedback view.

Seen live in the 0.0.90 gate (bed B, `app/on/webhook`, `TestWebhookReviewThreadFacts`, `fabrik-test-gamma#875`): with polls held after the comment arrived by webhook, the resolution delivery was applied and the reviewer's outstanding count did not move.

Impact is bounded: the next poll's deep fetch replaces the thread set, and every landing and advance decision reads feedback live (`feedbackGateBlocks`, ADR 2350), so nothing merges or advances on the stale view. What is wrong for up to one poll interval is the cache-only surfaces — `fabrik_status` `pr.reviewers.outstanding`, `validate-settled`'s `meta.reviewers`, the attention classifier's cached-feedback inputs and any channel event built from them — and any dispatch decision taken from the cached view in that window (a review-reinvoke for an already-resolved thread).

## Requirements

- A thread resolved by webhook is dropped from the Store whether its comments came from a deep fetch or from a webhook, with no GitHub call in the delta path (ADR 2352's rule).
- Candidate approach (Research to confirm): the `pull_request_review_thread` payload lists the thread's comments (`thread.comments[].id`); match on the comment database IDs when `ReviewThreadID` is empty, or record the thread ID on the stored comments from that payload.

## Acceptance

- A sim twin: a review comment added by webhook, then its thread resolved by webhook before any deep fetch, leaves the Store with no comment from that thread; neutralising the fix makes it fail.
- A `boardcache` delta test over a verbatim captured `pull_request_review_thread` `resolved` payload.

## Out of scope

- The live test itself (`TestWebhookReviewThreadFacts` waits one poll interval before resolving since PR #2443; once this lands it could drop that wait)."

## Background

The engine keeps a cached view of a pull request's review feedback (unresolved review-thread comments and a resolved-thread count). When a reviewer adds an inline comment, the webhook for that comment (`pull_request_review_comment` `created`) does not say which review thread the comment belongs to, so the comment is stored with no thread identity and the cache is marked so the next deep fetch fills the identity in. When the thread is later resolved, the `pull_request_review_thread` `resolved` delivery is applied by matching the thread's identity against stored comments. A comment stored without a thread identity can never match, so the resolution is silently a no-op until the next deep fetch replaces the thread set.

This was observed live in the 0.0.90 release gate: with polls held after the comment arrived by webhook, the resolution delivery was applied and the reviewer's outstanding count did not move.

The impact is bounded. The next poll's deep fetch corrects the view, and every landing and advance decision reads feedback live, so nothing merges or advances on the stale view. For up to one poll interval, however, the cache-only surfaces report a resolved thread as still outstanding: the status tool's per-reviewer outstanding count, the reviewer summary on the Validate-settled event, the attention classifier's cached-feedback inputs and any channel event derived from them. A dispatch decision taken from the cached view in that window (for example a review-reinvoke for an already-resolved thread) is also wrong.

The webhook-event design for facts the engine uses (ADR 2352) requires that delta handlers make no GitHub call, so the fix must work from information in the deliveries themselves. The `pull_request_review_thread` payload lists the thread's comments, which is the information needed to relate a resolution to comments that were stored without a thread identity.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Resolving a thread right after its comment arrived by webhook clears the cached feedback (Priority: P1)

A reviewer comments on a pull request line and then resolves the thread. Both events reach the engine by webhook, with no deep fetch in between. The cached feedback view must reflect the resolution immediately.

**Why this priority**: This is the defect. The cache-only surfaces show feedback as outstanding that is already resolved, and a dispatch decision from the cached view could act on it.

**Independent Test**: With a pull request in the Store, apply a review-comment-created delivery and then a review-thread-resolved delivery for the same thread with no deep fetch in between; the Store holds no comment from that thread and the resolved-thread count has increased by one.

**Acceptance Scenarios**:

1. **Given** a review-thread comment stored from a webhook (no thread identity), **When** the matching thread's `resolved` delivery is applied before any deep fetch, **Then** that comment is no longer in the Store's feedback view and the resolved-thread count increases by one.
2. **Given** a thread with several comments, some stored from webhooks and some from a deep fetch, **When** the thread's `resolved` delivery is applied, **Then** all of that thread's comments leave the Store's feedback view.
3. **Given** the same state, **When** the cache-only surfaces are read after the resolution, **Then** the reviewer's outstanding count no longer includes the resolved thread.

---

### User Story 2 - Existing resolution behaviour is unchanged (Priority: P2)

Resolutions of threads whose comments came from a deep fetch, replayed or duplicate resolutions, and resolutions of threads the Store has never seen continue to behave as they do today.

**Why this priority**: The fix must not regress the correct paths that ADR 2352 established.

**Independent Test**: The existing resolution tests (deep-fetched thread, replay, unknown thread) pass unmodified.

**Acceptance Scenarios**:

1. **Given** a thread whose comments carry a thread identity from a deep fetch, **When** its `resolved` delivery is applied, **Then** the comments are dropped exactly as before.
2. **Given** a `resolved` delivery for a thread with no comments in the Store, **When** it is applied, **Then** nothing changes and the resolved-thread count does not move.
3. **Given** a `resolved` delivery applied twice, **When** the second is applied, **Then** it changes nothing.

---

### Edge Cases

- A thread resolved by webhook when only some of its comments are stored (others not yet seen): the stored ones are dropped; comments arriving afterwards are not resurrected by the resolution.
- A thread comment stored by webhook that belongs to a different, still-unresolved thread on the same pull request is not dropped.
- A `resolved` delivery for a pull request that is not linked to a cached item is ignored, as today.
- A thread resolved and then unresolved before any deep fetch behaves as the existing unresolve handling already does (deep cache invalidated).
- A resolution delivery that arrives before the comment-created delivery for the same thread (out-of-order delivery) is handled by the existing ordering rule; this spec does not widen it.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Applying a `pull_request_review_thread` `resolved` delivery removes every stored comment belonging to that thread from the Store's feedback view, whether the comment was stored from a deep fetch or from a `pull_request_review_comment` `created` delivery.
- **FR-002**: Identifying which stored comments belong to the resolved thread uses only information carried in the stored state and in the delivery payload; the delta path makes no GitHub call.
- **FR-003**: The resolved-thread count increases by one only when the resolution removed at least one comment, so a replayed or unknown-thread resolution changes nothing.
- **FR-004**: A comment belonging to a different thread is never removed by another thread's resolution.
- **FR-005**: Resolutions of threads whose comments already carry a thread identity behave exactly as before this change.
- **FR-006**: The existing ordering rule for thread facts (a stale delivery does not override newer state) continues to apply to the resolution.
- **FR-007**: A sim scenario covers the sequence "review comment added by webhook, then its thread resolved by webhook, before any deep fetch", and fails when the fix is neutralised.
- **FR-008**: A delta-level test applies a verbatim captured `pull_request_review_thread` `resolved` payload (stored with the other captured webhook fixtures) to a Store containing a webhook-added comment of that thread, and asserts the comment is dropped.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a webhook-added review comment and its webhook resolution, with no deep fetch in between, the Store holds zero comments from that thread and the cache-only reviewer outstanding count excludes it, with no wait for the next poll.
- **SC-002**: The delta path performs zero GitHub calls for the scenario (verified by the existing capture-environment check that fails on any fallback fetch or read-client call).
- **SC-003**: The sim scenario passes with the fix and fails with the fix neutralised.
- **SC-004**: All pre-existing review-thread resolution tests pass unmodified.

## Assumptions

- The `pull_request_review_thread` `resolved` payload lists the thread's comments with identifiers that can be related to the identifiers of comments stored from `pull_request_review_comment` deliveries. Research confirms this against the captured payload; if it does not hold, the follow-up is a new question rather than a silent fallback.
- Choosing between "match by comment identifiers when a stored comment has no thread identity" and "record the thread identity on stored comments from the resolution payload" is a design choice for Research and Plan; either satisfies this spec as long as FR-001 to FR-006 hold.
- The next deep fetch continues to replace the thread set and remains the backstop; this change does not remove the deep-cache invalidation on comment creation.
- Landing and advance decisions keep reading feedback live; this change neither relies on nor alters that.

## Out of Scope *(optional)*

- Changing `TestWebhookReviewThreadFacts` (the live test). It waits one poll interval before resolving since PR #2443; once this lands, dropping that wait is a follow-up.
- Changing how landing and advance decisions read feedback (live reads, ADR 2350).
- Supplying the thread identity at comment-creation time by any means that needs a GitHub call.
- Other cached facts delivered by webhook with incomplete data.

## Source References *(optional)*

- ADR 2352 and `docs/state-machine.md` §D.2.2 (webhook events for facts the engine uses; no GitHub call in delta handlers)
- ADR 2350 (live reads for landing and advance decisions)
- `docs/state-machine.md` webhook table row for `pull_request_review_thread` `resolved`
- PR #2443 (live test waits one poll interval before resolving)
- 0.0.90 gate observation: bed B, `app/on/webhook`, `TestWebhookReviewThreadFacts`, `fabrik-test-gamma#875`
