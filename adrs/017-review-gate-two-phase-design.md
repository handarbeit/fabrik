# ADR 017: Review Gate Two-Phase Design

## Status

Accepted

## Context

Issue #227 requires that in yolo/auto-advance mode, Fabrik holds an issue in place after a stage completes if there are outstanding PR reviewer requests. The motivating use case is GitHub Copilot code review: when the Implement stage marks a PR ready-for-review, Copilot is added as a reviewer and submits feedback minutes later. Without this gate, the issue advances to Review before Copilot's feedback is available.

Two architectural questions drive the design:

1. **When `handleStageComplete` runs, the linked PR's reviewer list is always empty** because reviewers are only added by GitHub after `MarkPRReady` is called — which happens inside the stage, before `handleStageComplete` returns. A naive check would see zero pending reviewers and advance immediately.

2. **Timeout tracking must survive engine restarts.** An in-memory timestamp resets on restart, potentially allowing issues to wait indefinitely after a restart. The GitHub issue events API (`GET /repos/{owner}/{repo}/issues/{number}/events`) returns a `created_at` timestamp for each `labeled` event, giving us a restart-persistent record of when `fabrik:awaiting-review` was first applied.

## Decision

### Two-Phase Advance Path

The review gate is split across two code paths:

**Path 1 — `handleStageComplete` (always-gate)**: When a stage with `wait_for_reviews: true` completes and `shouldAdvance` is true, `handleStageComplete` applies `fabrik:awaiting-review` immediately and returns without advancing. It does not inspect `LinkedPRReviewRequests` because that data is always stale at this point (reviewers haven't been assigned yet).

**Path 2 — catch-up loop in `poll.go` (evaluate-gate)**: On subsequent poll cycles, `itemMayNeedWork` detects `fabrik:awaiting-review` and forces a deep-fetch via `FetchItemDetails`. The catch-up loop then calls `checkReviewGate`, which has fresh `LinkedPRReviewRequests` data and makes the real gate decision. When all reviewers have submitted (or the timeout has elapsed), it removes `fabrik:awaiting-review` and advances.

This approach trades one extra poll cycle of delay (when no reviewers are actually assigned) for correctness and simplicity — no GraphQL re-fetch in `handleStageComplete`.

### Gate Logic

`checkReviewGate` returns `true` (blocking) when:
- `stage.WaitForReviews` is `*true`, AND
- `item.LinkedPRReviewRequests` is non-empty (at least one reviewer still outstanding), AND
- The timeout has not yet elapsed.

"Outstanding" is defined by presence in `reviewRequests` — GitHub removes a reviewer from this list when they submit any review (APPROVED, CHANGES_REQUESTED, or COMMENTED). If a review is dismissed and the reviewer is re-requested, they re-appear in `reviewRequests` and re-block the gate. This naturally implements the dismissed-reviewer requirement without special casing.

### Timeout Persistence via Issue Events API

The timeout start time is fetched from `GET /repos/{owner}/{repo}/issues/{number}/events`, filtering for `event == "labeled"` with `event.label.name == "fabrik:awaiting-review"`. The implementation pages through all events to find the most recent application timestamp. If not found (fail-open), the timeout never fires — preferable to incorrectly timing out.

### `itemMayNeedWork` Extension

Items with `fabrik:awaiting-review` are force-included in the deep-fetch phase even if their `updatedAt` timestamp hasn't changed. A reviewer submitting a PR review does not bump the issue's `updatedAt`, so without this bypass, idle awaiting-review items would be skipped permanently.

## Alternatives Considered

**Re-fetch in `handleStageComplete`**: Call `FetchItemDetails` again before `checkReviewGate` when `wait_for_reviews: true`. This eliminates the extra poll cycle but adds a GraphQL round-trip at the critical path (stage completion) and still has a race window if GitHub hasn't propagated the reviewer assignment yet.

**In-memory timestamp**: Store `awaitingReviewSince map[string]time.Time` in the engine. Simpler, no REST API call, but resets on restart — an issue could be held indefinitely if the engine restarts repeatedly.

## Consequences

- One extra poll cycle delay on stage completion when `wait_for_reviews: true` (even if no reviewers are assigned). This is acceptable given the purpose of the gate.
- An additional REST API call per poll cycle per awaiting-review item (issue events API for timeout check). Acceptable for normal pipeline use.
- `FetchItemDetails` GraphQL query cost increases slightly (adding `reviewRequests` and `latestReviews` fields per linked PR node). Fail-open on GHES if fields are unavailable.
- The `GitHubClient` interface gains `FetchLabelAppliedAt` — all implementations (real client, mock, test stubs) must implement it.
- Stage opt-in is consistent with all other stage YAML flags: `wait_for_reviews: false` by default.
