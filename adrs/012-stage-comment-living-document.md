# ADR 012: Stage Comment as Living Document

## Status

Accepted

## Context

When a user comments on an issue during a stage (e.g., answering a Specify-stage question), Fabrik's `processComments` invokes Claude to incorporate the input and posts the result as a new comment. This created two problems:

1. **Thread noise:** Each comment processing run adds a new comment to the issue. An issue with multiple rounds of user interaction accumulates a long comment thread of near-duplicate stage outputs.

2. **Identity confusion:** The "comment review" comment uses a variant header (`🏭 **Fabrik — stage: Research (comment review)**`) that is distinct from the stage's own output comment. This makes it harder to answer "what is the current Research output?" — you have to look at both.

## Decision

When `processComments` completes, instead of always posting a new comment:

1. `findStageComment()` scans `item.Comments` for the most recent comment whose body starts with `🏭 **Fabrik — stage: {stageName}**` (the base stage name, no variant suffix).
2. If found: call `UpdateComment()` to replace its body with Claude's new output.
3. If not found: call `AddComment()` to create a new comment using the base stage name header.

The `(comment review)` variant header is retired. Comment processing output is now the stage's authoritative output — it updates the same comment, under the same header.

**Exceptions:**
- `post_to_pr` stages: skip the rewrite and post new comment processing output on the issue as a new comment. The stage output lives on the PR; rewriting a PR comment from an issue comment flow adds complexity for unclear benefit.
- Empty output: if Claude produces no output, no comment is created or updated.

**No acknowledgement comment:** The rocket reaction (🚀) on the user's comment is sufficient signal that input was processed. Acknowledgement comments add noise to the thread, which this ADR is trying to reduce.

## Alternatives Considered

**Always post a new comment:** The previous behavior. Simple but noisy. Abandoned because it creates a growing thread of near-duplicate outputs.

**Keep the `(comment review)` variant header:** Preserves backward compatibility with existing comments but requires `findStageComment` to handle both variants, adds complexity, and perpetuates the identity confusion.

**Delete old comment and post new one:** Technically equivalent to rewriting, but deletion + creation creates two events in the GitHub API and a brief gap where no comment exists. Update-in-place is cleaner.

## Consequences

- The issue comment thread stays clean: each stage has at most one living output comment.
- The stage comment becomes the authoritative, up-to-date output regardless of whether it was last touched by a stage run or comment processing.
- Existing `(comment review)` comments in live issues will not be matched by `findStageComment` (different header prefix). The first post-deploy comment processing run creates a new base-name comment alongside the old variant. The old one stays as historical record; the new one becomes the living document going forward.
- `findStageComment` must stay in sync with `formatOutputComment` in `engine/pr.go`. Both live in the engine package; `findStageComment` is in `engine/context.go` with a comment noting the coupling.
- `UpdateComment` is a new method on `GitHubClient` backed by `PATCH /repos/{owner}/{repo}/issues/comments/{id}`.
