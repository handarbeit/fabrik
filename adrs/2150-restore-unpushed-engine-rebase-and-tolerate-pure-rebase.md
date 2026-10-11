# ADR 2150: Restore the engine's unpushed rebase, and let the singleton catch-up tolerate a pure one

## Status

Accepted (#2150). Amends the ownership rule of ADR-2044.

## Context

A comment-processing invocation (ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke`) on an item with an open PR rebases the worktree onto `origin/<base>` before Claude runs (`EnsureWorktree` → `updateWorktreeFromMain`). When the invocation commits nothing, nothing is pushed, and the local `fabrik/issue-N` stays a rebased copy of its remote: the same patches on a newer base, new SHAs. Git sees the two as diverged although neither carries work the other lacks.

The merge train's singleton catch-up (ADR-2044) treats a local branch ahead of or diverged from its remote as unpushed work owned by someone else (`ErrCatchUpLocalDiverged`) and defers to the full trial — a trial PR and about eight minutes of CI. Observed on #2097 and #2096 on 2026-10-11. The unpushed rebase also leaves the worktree on a head nobody has CI for, so the `HEAD == head_sha` rule of ADR-1997 refuses.

Two defects: the engine creates a divergence it never resolves, and the catch-up cannot tell a harmless pure rebase from real unpushed work.

## Decision

### R1: restore to the pre-rebase head (candidate (a))

`processCommentsClassified` captures HEAD before `EnsureWorktree`. After the extension loop, when no commit was made and HEAD moved only because of the rebase, `WorktreeManager.RestoreUnpushedRebase` resets the branch to the captured head. It acts only when all hold, and otherwise leaves the branch alone and logs why:

- worktree on the issue branch, HEAD still exactly the post-rebase head (no Claude commit, reset or amend);
- clean tree, no merge/rebase/cherry-pick in progress;
- a fresh fetch of the issue branch succeeds and the rebased head is **not** already on the remote;
- the rebased head carries no patch (patch-id, base history excluded) and no merge commit that was not on the pre-rebase head. This is the check that stops the engine's own `chore: remove stale .fabrik-context files` commit, or anything unexpected, being discarded.

Rejected alternatives:

- **(b) skip the rebase.** It needs a "CI gate is waiting" predicate that is not defined for the `review-reinvoke` path, and it changes behaviour for passes that do commit, which previously worked on a fresh base.
- **(c) push the rebase.** It re-runs CI and moves the head the gate recorded.
- **Restore to the remote tip instead of the pre-rebase head.** It would also heal earlier staleness but relies on patch-id equivalence for losslessness and on remote freshness. Restoring the exact pre-rebase SHA is lossless by construction; a branch that was already diverged before the pass is R2's job.

**Placement.** The restore sits right after `headChanged` is computed: before the `err && !completed` exit and `finalizeComments`, so every exit is covered; while `fabrik:editing` is still held, so the catch-up's ownership check cannot run against a worktree mid-restore; and before `markPRReady`'s push, so a completing pass pushes the head CI saw. Merge-queue items never rebase (ADR-058), so the pre-rebase and post-rebase heads are equal and nothing runs.

### R2: the catch-up accepts a pure local rebase

When the local branch is neither equal to nor behind the remote head, `PrepareCatchUp` fetches the base branch and asks whether the local branch is a pure rebase of the remote: zero local-only non-merge patches and zero local-only merge commits.

- `git rev-list --right-only --cherry-pick --no-merges --count <remote>...HEAD ^origin/<base>` must be 0. **Plain `git cherry` cannot be used:** base commits new to the local branch print as `+`, so every rebase onto an advanced base would read as impure. Excluding the base's history removes them. A conflict-edited rebase changes the patch-id and so reads as local-only work.
- `git rev-list --merges --count HEAD ^<remote> ^origin/<base>` must be 0. Merge commits are invisible to patch-ids.

If pure, the branch is reset to the remote head (the head CI validated) and the catch-up proceeds from it; the local rebase is never pushed. Anything else — a local-only patch or merge, an unfetchable base, a git error — keeps `ErrCatchUpLocalDiverged`. Every failure mode makes the helper report "not pure", so the cost is a trial as today, never a lost commit. R2 is independent of R1 and covers worktrees already in the state, including ones from before R1 shipped.

### R3: ADR-2089 untouched

Neither mechanism adds a push, so the zero-ahead guard is neither reached nor changed.

## Consequences

- A no-commit comment pass leaves `fabrik/issue-N` equal to its remote; the lone Queued member behind main takes catch-up and the singleton fast path instead of a trial.
- **Behaviour change in the reinvoke hooks.** `dispatchReviewReinvoke` and `dispatchCIFixReinvoke` capture `headBefore` before the rebase. A rebased no-commit pass previously made `headAfter != headBefore`, hiding the no-op; after the restore they compare equal, so the review-cycle refund (`ReviewCycleDecremented`, #1045) and `CIFixNoOpRecorded` (#958) fire where they silently did not, and the latter no longer records an unpushed rebased SHA as the PR head. This is a latent fix.
- If the fetch of the issue branch fails, or the branch has local-only content, the rebase is left in place and logged; R2 is the backstop.
- Each no-commit pass re-rebases and restores; the cost is local git only.
- The engine's `chore: remove stale .fabrik-context files` commit (only for branches that tracked `.fabrik-context`) makes R1 leave the branch alone by design.
- Test seams: `SetCommentRebaseRestoreDisabledForTest` (R1) and `SetCatchUpPureRebaseDisabledForTest` (R2); the sim scenarios in `tests/sim/mergetrain_singleton_catchup_rebase_test.go` fail with either removed.
