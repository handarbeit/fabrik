# ADR 2483: Merge-train PRs are not attached to a member by the webhook auto-heal

## Status

Accepted. Amends the auto-heal description in ADR 2352; relates to ADR 2051 (the train owns its trial state) and ADR 1772 (the hydration guard in batch formation).

## Context

With a webhook transport, a `pull_request` or `check_run` delivery for a PR that is not yet in the Store's `prToKey` index goes through `CacheImpl.resolvePRLinkage`, which fetched the PR's closing issues and returned the first one that is cached. A merge-train trial or integration PR closes every member of its batch, so the first-named member always won. The heal then wrote the trial PR's number and head SHA into that member's linked-PR facts, invalidated its deep cache, and left the trial PR indexed to it, so later deliveries rewrote the same member without any heal. The invalidated member read as "not yet hydrated" and `groupQueuedByRepoAndBase` dropped it from the next batch snapshot, so the batch re-formed. Seen on the 0.0.90 gate (`TestMergeTrainQueuedDeeperThanBatchCap`, `app/on/webhook`).

Nothing in the engine reads a trial PR's facts from the Store: the train works from its persisted run record and live reads (`evalTrialCI`, `FetchCheckRuns` on the trial SHA).

## Decision

Two independent guards in `boardcache`:

1. **Head-branch gate.** A delivery whose PR head branch starts with `fabrik/merge-train/` (`boardcache.TrainBranchPrefix`) is dropped before any Store access, in the `pull_request`, `pull_request_review`, `pull_request_review_comment` and `check_run` handlers. It sits before the `prToKey` lookup, so an index already pointing at a member is not written through, and before `CheckRunCompleted`, so nothing is buffered. The branch comes from the payload (`pull_request.head.ref`; for `check_run`, `check_suite.head_branch` or `pull_requests[].head.ref`), so no GitHub call is added. The train facts are ignored rather than routed anywhere, because nothing consumes them. This is the only guard that covers a singleton landing PR, which closes one member.
2. **Multi-closing-issue refusal.** `resolvePRLinkage` counts the *cached* closing issues; with more than one it logs once and returns not found, so the PR stays unlinked (and the caller records its usual negative miss). It never picks by position. One cached issue (even if the body names uncached ones) heals as before, and a PR already in `prToKey` never reaches this code. This backstop needs no payload field, so it holds when a `check_run` carries no branch information (`pull_requests` is empty for fork PRs).

The prefix is a separate constant (`boardcache` cannot import `engine`), pinned to `engine.trainBranchPrefix` by `TestTrainBranchPrefixMatchesBoardcache`.

One per-`CacheImpl` seam, `SetPRHealGuardDisabledForTest`, disables both guards so the sim twin (`tests/sim/webhook_trainpr_heal_test.go`) can show it fails without the fix.

## Alternatives rejected

- **Read `head.ref` in `resolvePRLinkage` from the REST fetch it already makes.** One place would cover all handlers, but `ReadClient.FetchPRClosingIssues` has about six implementers and mocks, and an additive method adds a REST call per delivery against ADR 2352's rule that handlers make no GitHub call.
- **Refuse versus apply to none.** Same observable outcome; refusing reuses the not-found path.
- **Tighten `check_run`'s `prNums[0]` pick when a SHA has several PRs.** Out of scope; with the refusal in place it can no longer attach to an arbitrary member in the train case.

## Consequences

- During an open trial no delivery for the trial PR changes a member's linked PR, head SHA or hydration.
- Residual: a singleton train PR delivered as a `check_run` with no branch fields still heals onto the member being landed. It is not the observed failure and the effect is small.
- A member already mis-attached in a running daemon is not cleaned up; the Store corrects it on the next deep fetch or reconcile.
- `TestMergeTrainQueuedDeeperThanBatchCap` is registered `events: neutral`, so the sparse gate plan runs it only on poll cells; the webhook cell exercises it under `E2E_MATRIX=full`. Changing that classification is a gate-planning decision left out of this change.
