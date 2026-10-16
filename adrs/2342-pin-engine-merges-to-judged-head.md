# ADR 2342: Pin every engine merge, auto-merge and enqueue to the head the gates judged

**Date**: 2026-10-15
**Status**: Accepted
**Issue**: #2342 — the ordinary landing merged a PR head nobody judged (engine state audit, 2026-10-15, §1.2)
**Amends**: ADR-1644 (premise about the ordinary path), ADR-058 FR-1 (source of the enqueue head)

## Context

The CI gate reads its head from the cached `FetchLinkedPR` (up to `linkedPRCacheTTL`, 45 s old).
`attemptMergeOnValidate` then re-reads the item live and judges the review, thread and feedback gates
against `item.LinkedPRHeadSHA`; `landValidated` read the PR a third time and called
`EnablePullRequestAutoMerge` or `MergePR` naming no head at all. ADR-1644 introduced
`MergePRAtHeadSHA` for the merge-train singleton fast path and justified leaving every other caller
unpinned because they "merge a Fabrik-owned branch nobody else pushes to". That is wrong for the
ordinary landing: its branch is the PR's `fabrik/issue-N` head, writable by anyone with push access.
On a repository with no required checks a push between the gates and the merge request makes
auto-merge enablement fail as already clean, the engine falls back to a direct merge, and an unchecked
commit reaches the base branch. `settleSHAInvalidationScan` runs after the landing and cannot stop it.

## Decision

1. **One judged head per landing decision.** `attemptMergeOnValidate` takes
   `judgedHead = item.LinkedPRHeadSHA` once, right after its live `FetchItemDetails` re-read — the
   value the thread, review and feedback gates were judged against — and passes it to `landValidated`.
   The landing makes no head read of its own; its `FetchLinkedPR` supplies the PR number and the
   merge-queue flag only.
2. **Everything is pinned.** The enqueue (`EnqueuePullRequest`), `EnablePullRequestAutoMerge` (new
   required `expectedHeadOID` parameter on the existing method — no pinned sibling, so no unpinned
   caller can exist) and the direct-merge fallback (`MergePRAtHeadSHA`) all name `judgedHead`.
   `MergePR` is removed from `engine.GitHubClient`; the client method remains for tooling and tests.
   An empty `judgedHead` defers with a log line and issues no write (FR-008).
3. **The CI gate's head is compared, not re-read.** `handleMergeAndCIGates` records the head it
   cleared for a `wait_for_ci` stage (`noteCIJudgedHead`, in memory, 2-minute TTL, consumed by
   `takeCIJudgedHead`). A recorded head that differs from `judgedHead` defers the landing: the CI
   verdict is for a superseded commit. No new GitHub read; with no record (no `wait_for_ci`, restart)
   there is no CI claim to compare and the pin to `judgedHead` still applies.
4. **`gh.ErrHeadMoved`, classified from the live head.** REST: `mergePR` reads `head.sha` with the
   mergeable status and refuses before the PUT when it differs from the pin; after a 409 it re-reads
   the head, and only a head that positively differs yields `ErrHeadMoved` (wrapping `ErrConflict`, so
   the singleton fast path behaves as before) — a 409 on an unmoved head stays a plain `ErrConflict`
   and is never deferred silently. GraphQL (enable, enqueue): GitHub's refusal text is undocumented, so
   after any refusal other than `ErrAutoMergeNotEnabled` the client reads the PR head once; only a
   head that differs from the pin is `ErrHeadMoved`. That read classifies a refusal that already
   happened; it is not a head read for a merge. A refusal for another reason keeps its handling.
5. **A head mismatch is a silent deferral.** `(enabled=false, deferred=true, err=nil)` — no label,
   comment, pause, retry/cycle/ejection counter, and no fallback to a direct merge — plus one log line
   and the existing `merge-unsettled` admission cooldown so the next poll re-admits the item and
   judges the new head. Repeated pushes defer each poll with no accumulated state.
6. **Other ordinary-path sites.** `reenableAutoMergeAfterRebase` (re-arm after a rebase push) pins its
   enable and its already-clean direct merge to the one head it reads at entry; no gate judged that
   post-rebase head, so this only narrows the window, and a moved head is left to the next poll's
   convergence pass. `reEnqueueOrPause` and `routeQueuedGroup` already pass a head from poll-native
   state; a stale pin there fails safe (the enqueue is refused). ADR-058 FR-1's "head from poll-native
   state" rule still holds for those two; the landing decision's enqueue uses `judgedHead`, itself taken
   from the same live `FetchItemDetails` read the decision's gates use.
7. **The merge train (R4).** The audit found one unpinned train merge: `settleAndMerge`'s `MergePR`.
   It now calls `MergePRAtHeadSHA` on the head `classifyLandingCI` just judged (`pr.HeadSHA`, else the
   head adopted at the first read; with neither it does not merge) and maps `ErrHeadMoved` to the
   existing `outcomeHeadMoved`, which already charges no ejection, no `recordTrial` and no bisection and
   leaves members Queued. The landing paths do not additionally thread the trial's recorded head into
   `settleAndMerge`: the landing re-judges CI on the PR's own head each iteration and pins to exactly
   that head, so a commit pushed after trial validation is merged only if its own CI is green. The
   singleton fast path is unchanged.

## Consequences

- With an unmoved head the sequence of GitHub writes is unchanged apart from the added binding.
- A burst of pushes costs a deferral per poll, bounded by the poll interval and the `merge-unsettled`
  cooldown; the CI-head cache lag (up to 45 s) can add a few extra silent deferrals.
- GitHub's real error text for the two GraphQL refusals stays unverified in CI. The sim models the
  refusal as `ErrHeadMoved`; the live `TestLandingHeadPinRefusedOnMovedHead` logs the real text for
  capture as a wire-contract fixture. The live suite cannot race a push inside one engine poll, so it
  replays the precondition (head A, push B, pinned request at A) through the client methods the engine
  uses; the engine flow is covered by the sim twins using `FaultTable.BeforeCall`.
- A pre-existing auto-merge enabled before a push is GitHub's and the post-landing SHA-invalidation
  scan's concern, unchanged.

## Alternatives rejected

- Re-running the CI gate inside the landing: a second CI evaluation and new reads.
- Threading Phase 1's settle result through `phase1Ctx`: touches every handler for one value.
- A pinned sibling `EnablePullRequestAutoMergePinned`: leaves an unpinned method callable.
- Matching GitHub's error text alone: unverified, and the failure direction (deferring a different
  refusal forever) is the unsafe one.
