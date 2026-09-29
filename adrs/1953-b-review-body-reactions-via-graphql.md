# ADR 1953-b: React to review bodies through GraphQL `addReaction`

**Status:** Accepted
**Date:** 2026-09-29
**Issue:** [#1953](https://github.com/handarbeit/fabrik/issues/1953) (R8)
**Supersedes in part:** ADR-026's statement that synthetic `DatabaseID: 0` comments skip reaction calls.

## Context

Fabrik reacts 👀 at the start and 🚀 at the end of processing a comment, but only through REST (`/issues/comments/{id}/reactions`, `/pulls/comments/{id}/reactions`), and REST has no reactions endpoint for a pull-request review. A review body reaches the engine as a synthetic comment (`review-body:<reviewDatabaseID>`) with no `DatabaseID`, so both reaction sites skipped it (`skipping … reaction for synthetic comment … (no DatabaseID)`). A bare review body therefore looked identical on the PR whether it was handled or ignored — on #616 that made an unprocessed review indistinguishable from a processed one. `PullRequestReview` is `Reactable` in GraphQL.

## Decision

- `PRReview.NodeID` carries the review's GraphQL node ID, from `latestReviews { id }` (GraphQL path) and `node_id` (REST `FetchPRReviews`, the only source on `base:<branch>` items). The synthetic review-body comment carries it as `Comment.ReactionNodeID`; no other comment sets it.
- `GitHubClient.AddReviewReaction(subjectNodeID, content)` calls `addReaction` with the uppercase `ReactionContent` enum (`eyes`→`EYES`, `rocket`→`ROCKET`); an unknown content is an error.
- `acknowledgeComments` (👀) and `finalizeComments` (🚀) branch on "no `DatabaseID` but a `ReactionNodeID`" — the same two sites, same conditions, no separate lifecycle. Synthetics with neither (CI-fix, rebase) still skip. A failed reaction is a logged warning, never an error.
- **The durable "addressed" state stays the #1555 `review-ids-addressed` marker** (plus the in-memory watermark). The reaction is only the human-visible signal: reactions can be added or removed by anyone, so dedup never depends on it.
- **Wire contract.** `addReactionMutation` is in `wireContractRegistry` and validated against the vendored schema, and `review_reaction_test.go` pins the exact variables sent (`subjectId`, uppercase `content`). `scripts/wire-contract/record-fixtures.sh` has an `add_review_reaction` step. The **recorded response fixture has not been captured**: it needs sandbox credentials (`handarbeit/fabrik-test-alpha`) an automated stage lacks, and none was fabricated. Until a maintainer runs the script, the request-shape test's response literal is hand-authored (noted in `github/testdata/README.md`).
- **GitHub App auth.** `pull_requests: write` is already requested; whether it covers reactions on a review was not verifiable from the schema and must be checked against a real installation. A refused reaction is non-fatal by construction.

## Consequences

- One new mutation subject the sim cannot validate on the wire; the schema check and the request-shape test are the pre-release guards, the recording the remaining gap.
- `GitHubClient` gains a method (fanned out to the mock, `cmd` test client, `simgh.Sim` and `Instrumented`); the sim stores reactions per review node ID.
