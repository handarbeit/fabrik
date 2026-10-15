# ADR 2295: Sync an existing issue branch from origin and push with a verified explicit lease

## Status

Accepted. Amends ADR 2089 (decision 7 "no resync, no lease SHA" and its residual-gap bullet); builds on ADR 2044/2150 (`PrepareCatchUp`, `localIsPureRebase`, `PushCatchUp`) and keeps ADR 2299's merged-PR push skip ahead of everything new.

## Context

Report #2058: a person pushed a conflict resolution to `fabrik/issue-N`. The engine's existing worktree still held the stale local branch, rebased it onto base and pushed it over the remote branch; GitHub closed the PR. ADR 2089 added a zero-ahead guard and explicitly deferred the underlying fix. Two defects remained:

1. On the existing-worktree path `EnsureWorktree` never looked at `origin/fabrik/issue-N` (only the local-branch-missing path adopts it).
2. `PushBranch` ran a bare `--force-with-lease`, which compares against `refs/remotes/origin/fabrik/issue-N` in the bare clone shared by every worktree of the repo. `FetchOrigin`, `ensureBareCloneAs`, the targeted fetches and any worker `git fetch` refresh that ref, after which the lease matches whatever the remote holds and protects nothing.

## Decisions

1. **Sync before work.** `EnsureWorktree` calls `syncIssueBranch` on an existing, on-branch worktree before any rebase onto base (and after `worktree add` when a stale local branch is reused). One `git ls-remote` read decides: remote absent or head equal to local needs no fetch. Otherwise one targeted fetch of branch and base (a stale base ref would turn base-merged commits into branch-only patches), then classify: equal, local strictly behind (fast-forward), local holds nothing the remote lacks (reset; the `localIsPureRebase` test generalised to `holdsNothingBeyond`), remote holds nothing local lacks (keep local), else diverged. Reset is tested before keep because a pure unpushed rebase satisfies both. Mutation runs only on a clean, idle tree (`worktreeBusyReason`, extracted from `PrepareCatchUp`); any doubt (busy tree, `ls-remote` or fetch error) skips the sync with a log line. A read failure is never read as "remote absent". The sync never pushes, so it cannot eject a merge-queue entry.
2. **Divergence is a pause, not a merge.** `EnsureWorktree` returns a typed `*BranchDivergedError`; the engine caller (`WorktreeManager` has no pause capability) pauses once per episode (`hasPauseComment` dedup, a fragment distinct from the zero-ahead one). The comment path returns a new did-not-run kind, so its cycle is refunded, the comment breakers are bypassed and the comment stays unprocessed for the human-comment resume. No automatic integration of the two sides is attempted.
3. **Explicit lease on a verified head.** `PushVerified` replaces the bare lease in all three wrappers. It reads the remote head; absent → lease requiring absence; equal to the head last reconciled → the local branch is authoritative, lease on it; otherwise fetch and classify, pushing only when the remote holds nothing local lacks (lease on the fetched head). Equal/behind/local-subsumed publish nothing, because a push would rewind the remote. A rejection re-verifies once; the second is `ErrPushLeaseRejected`, left for the next poll and never forced. Wrapper order is unchanged: merge-queue skip, merged-PR skip (ADR 2299), zero-ahead guard (ADR 2089), then the verified push.
4. **The reconciled head lives in memory** (`WorktreeManager.reconciled`, under `wm.mu`, recorded at the sync, adoption, every successful push and by `PushCatchUp`, dropped by `CleanupWorktree`). A restart empties it and the first push takes the fetch-and-classify path, which is always correct. A private ref in the bare clone would survive restarts but needs cleanup in several paths and keeps objects alive; a file under `.fabrik/state` adds a second source of truth. Neither buys correctness.
5. **Fail closed at push time.** An unreadable remote returns `ErrPushUnverified` and nothing is pushed; the next poll retries, as for any push failure. The zero-ahead guard keeps its fail-open polarity (ADR 2089 decision 4): it guards one shape and must not strand pushes on a flaky PR read, whereas this check exists to never overwrite unverified remote state.
6. **One refusal base.** `ErrPushRefused` is wrapped by `ErrPushRefusedZeroAhead` (message unchanged) and `ErrPushRefusedDiverged`, so the two consumers that must stop the completion chain (`item.go` post-stage push, `pr.go` `markPRReady`) test one sentinel. `pushBranchOnCancel` refuses without pausing, as ADR 2089 decided, because the interruption already pauses the item.
7. **#2150 hand-off.** The comment path takes its pre-rebase head from `SyncReport.PreRebaseHead` (the head after the sync, before the rebase), obtained through a `WithSyncReport` out-parameter option so the 46 existing `EnsureWorktree` call sites are untouched. `RestoreUnpushedRebase` therefore restores to the synced head and never undoes a sync.
8. **`PrepareCatchUp` opts out** (`WithoutIssueBranchSync()`), so its four outcomes (busy, remote moved, diverged, pure rebase) stay exactly its own. Items whose PR sits in a merge queue also skip the sync: no push can occur for them. A retry still syncs: the sync is local-only and gated on a clean tree.

## Worker-skill pushes

The review and validate skills run `git push --force-with-lease` from the worktree with a bare lease. That is out of scope: a skill pushing a stale branch remains protected only by the shared tracking ref, which the sync has just refreshed. After a worker push local equals remote; the next engine push finds the remote moved from the record, fetches, classifies *equal*, records it and does not pause.

## Consequences

- Out-of-band commits on a behind branch survive a stage run; the #2058 shape with local work is refused and paused instead of overwritten (the case ADR 2089 cannot catch).
- One extra `ls-remote` per `EnsureWorktree` and per push; a fetch only when heads differ.
- A person's rebase with edited conflict resolution changes patch-ids and classifies as diverged: the safe direction.
- A remote-absent push can re-create a deleted, unmerged branch (the spec's must-not-exist lease); the merged case is covered by ADR 2299's skip.

## Alternatives considered

- Restoring to the remote tip instead of classifying: loses unpushed local work.
- Persisting the reconciled head: see decision 4.
- Fetching on every push: moves the shared tracking ref for no reason in the common unchanged case.
