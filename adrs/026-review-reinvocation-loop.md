# ADR 026: Three-Phase Review Gate with Re-invocation Loop

**Status**: Accepted  
**Date**: 2026-04-14

## Context

When Fabrik auto-advances from Implement to Review in `yolo` or `cruise` mode, bots such as GitHub Copilot and Gemini Code Assist may not have completed their automated reviews yet. The prior two-phase review gate (ADR 017) blocked advancement until all requested reviewers submitted, but it only waited — it did not actually address the submitted feedback.

The result: Review runs, signals `FABRIK_STAGE_COMPLETE`, `fabrik:awaiting-review` fires, reviewers submit, gate clears... and the issue advances to Validate with the review feedback unaddressed.

The problem is compounded because Review itself has `mark_pr_ready_on_complete: true`, so each commit from a re-invocation triggers bots again, potentially creating a cycle.

## Decision

Extend the two-phase gate into a **three-phase loop**:

1. **Phase 1 (unchanged — always-gate):** `handleStageComplete` immediately applies `fabrik:awaiting-review` whenever `wait_for_reviews: true`, before reviewer assignments have propagated.

2. **Phase 2 (extended — gate evaluation):** The catch-up loop calls `checkReviewGate` with fresh data. It now returns `(blocked, timedOut bool)` instead of a single `bool`:
   - `blocked=true` → wait another poll cycle
   - `timedOut=true` → **pause** with `fabrik:awaiting-input` (was: advance with warning)
   - `blocked=false, timedOut=false` → gate cleared naturally; proceed to Phase 3

3. **Phase 3 (new — re-invocation):** When the gate clears with submitted reviews present, the catch-up loop increments a per-issue cycle counter and dispatches a goroutine that calls `processComments` with synthetic `gh.Comment` objects built from the PR review bodies. The `fabrik-review-comment` skill reads the feedback, applies fixes, commits, pushes, and outputs `FABRIK_STAGE_COMPLETE`. This re-triggers Phase 1, and the loop continues until no reviewers remain.

When no submitted review bodies are present (gate cleared with empty `LinkedPRReviews`), re-invocation is skipped and the issue advances normally.

### Synthetic Comments

PR review bodies are delivered to `processComments` as synthetic `gh.Comment` objects with `DatabaseID: 0`. This reuses the existing comment-processing pipeline — prompt building, skill invocation, and `FABRIK_STAGE_COMPLETE` detection — without adding a new invocation path.

The `DatabaseID: 0` sentinel is guarded in `processComments`: reaction calls (`AddCommentReaction` for 👀 and 🚀) are skipped for synthetic comments to avoid invalid REST API calls. A debug log entry is emitted instead.

### Cycle Cap

A per-issue, in-memory `reviewCycleCount` (similar to `retryCount`) caps re-invocations at `FABRIK_MAX_REVIEW_CYCLES` (default 5). When the cap is reached, Fabrik pauses with `fabrik:awaiting-input` and posts an explanatory comment. The count resets on engine restart, which is acceptable — the cap's purpose is protecting against tight loops within a session.

### Timeout Behavior Change

The prior timeout behavior (advance with warning) is replaced: timeout now pauses with `fabrik:awaiting-input`. This is a deliberate behavior change — a timed-out review wait indicates something unexpected has happened and warrants human review, not silent advancement.

### YAML Defaults

`wait_for_reviews: true` is added explicitly to `stages/examples/review.yaml`, `stages/examples/validate.yaml`, and both corresponding live configs. The `stages/stages.go` `WaitForReviews *bool` field retains nil-means-false semantics; no global default is introduced. Only Review and Validate have ready PRs, making them the only stages where this gate is meaningful.

## Alternatives Considered

### Context file instead of synthetic comments

Writing review content to `.fabrik-context/pr-reviews.md` and passing a trigger comment would avoid the `DatabaseID` guard. Rejected because it requires skill-side coordination to read the file and bypasses the natural `FABRIK_STAGE_COMPLETE` detection already in `processComments`.

### Clearing `stage:Review:complete` label for re-run

Clearing the complete label would cause the normal dispatch loop to re-run the full Review stage. Rejected because it re-runs the entire Review prompt rather than just the targeted comment-processing path, wasting turns and context.

### `pendingReviewReinvoke` map to dispatch loop

A map flag in the catch-up loop checked by the dispatch loop on the same cycle is cleaner for items normally skipped by `itemNeedsWork`. Rejected in favor of direct goroutine dispatch from the catch-up loop because items with `stage:Review:complete` are already skipped by `itemNeedsWork`, so the normal dispatch would not pick them up regardless.

## Consequences

**Positive**:
- Bot and human review feedback is now actually addressed before advancement — not just waited for
- The review loop is self-limiting (cycle cap) and observable (`fabrik:awaiting-review` label, log messages, pause comments)
- No new pipeline stages required; the existing `comment_prompt` / `comment_skill` path is reused

**Negative / Trade-offs**:
- Each re-invocation cycle adds one Claude invocation (minutes of latency) and several GitHub API calls
- The cycle count is in-memory; a restarted engine resets the count, potentially allowing more cycles than the configured cap across restarts
- Timeout now pauses instead of advancing — operators must monitor for `fabrik:awaiting-input` on review stages

## Files Changed

- `github/types.go` — `PRReview` gains `Body string` and `DatabaseID int`
- `github/project.go` — `latestReviews` GraphQL extended with `databaseId` and `body`
- `engine/reviews.go` — `checkReviewGate` returns `(blocked, timedOut bool)`; new helpers `buildSyntheticReviewComments`, `dispatchReviewReinvoke`, `pauseForReviewTimeout`, `pauseForReviewCycleLimit`
- `engine/engine.go` — `Config.MaxReviewCycles`; `Engine.reviewCycleCount`
- `engine/comments.go` — `DatabaseID == 0` guard in 👀 and 🚀 reaction loops
- `engine/poll.go` — catch-up loop extended with Phase 3 re-invocation dispatch
- `cmd/root.go` — `FABRIK_MAX_REVIEW_CYCLES` env var parsing
- `stages/examples/review.yaml`, `stages/examples/validate.yaml`, `.fabrik/stages/review.yaml`, `.fabrik/stages/validate.yaml` — `wait_for_reviews: true` added
- `.fabrik/plugin/skills/fabrik-review-comment/skill.md` — extended to handle engine re-invocation mode
