# ADR 2517: Replay equivalence for branch subsumption

## Status

Accepted. Amends [ADR 2150](2150-restore-unpushed-engine-rebase-and-tolerate-pure-rebase.md) (the patch-id test in the restore and the catch-up pure-rebase tolerance) and [ADR 2295](2295-worktree-sync-and-verified-lease-push.md) (decision 1, the "holds nothing the remote lacks" test).

## Context

On #2498 the engine rebased the issue branch onto a moved `main` before a comment pass. The rebase was clean and Claude made no commit, so nothing pushed it. The restore step (ADR 2150) declined to undo the rebase, and the next stage dispatch classified the local branch against the remote as diverged and paused the item.

Both safeguards decide "does local hold anything the remote lacks?" by patch-id. A patch-id hashes a hunk's context lines. One of the branch's commits edited `CLAUDE.md` next to lines the base had just changed, so the rebase changed its context and so its patch-id. The commit read as new work although the branch held nothing new. The check failed closed, which is safe, but it paused an item for no reason, and `CLAUDE.md`, `docs/state-machine.md` and `docs/llms-full.txt` are edited by nearly every PR.

## Decision

1. **A content-based fallback follows a patch-id miss.** `replayEquivalent(wtDir, subject, other, baseRef)` holds when replaying `other`'s net change onto `subject`'s own base gives `subject`'s tree, and `subject` has no merge commit that `other` lacks:

   ```
   B_s = merge-base(subject, baseRef)    B_o = merge-base(other, baseRef)
   git merge-tree --write-tree --merge-base=B_o B_s other   ==   subject^{tree}
   ```

   It is one git call with no worktree. It writes only unreferenced tree objects. The base ref is the fixed point, so a branch rebased several times onto successively newer bases still matches.

2. **Where it applies.** Three sites, each after the existing patch-id test has said "not subsumed":
   - `RestoreUnpushedRebase` (post against pre): the content check lets the restore undo a clean rebase that shifted context. The engine's own `chore: remove stale .fabrik-context files` commit changes the tree and so is still left alone.
   - `classifyBranch` (HEAD against the remote): a match is `relLocalSubsumed`, so the sync resets to the remote through its existing path. `verifyForPush` shares this function; a content-equivalent local is therefore "nothing to publish, push skipped" there rather than "diverged, refused". A push would rewind the remote, so skipping is the right outcome.
   - `localIsPureRebase`: `PrepareCatchUp` accepts the branch and resets it to the remote head.

   The remote-subsumed mirror test is unchanged.

3. **Every error is "not equivalent".** A missing merge-base, a conflict (exit 1), an unsupported `--merge-base` on old git (needs git 2.40), or any other failure returns false with a reason. The reason never contains the word "failed": callers read that word in `holdsNothingBeyond`'s reason as git doubt and skip the sync, which would turn today's pause into a silent skip. The fallback therefore can only move an outcome from "diverged / declined" to "reset / restored", never the other way, and on old git it silently reverts to the earlier behaviour.

4. **No commit-count requirement.** Requiring the same number of commits would pause a branch from which git dropped a commit the base made empty. Equal trees mean the reset discards no content, only SHAs and messages the remote carries in equivalent form.

5. **Nothing is pushed and nothing is lost.** The reset points are `reset --hard <remote>` on a tree the sync and the restore already require to be clean and idle. A local-only commit, a local-only merge and a conflict-edited rebase (a changed tree) all keep their previous outcome.

6. **Visibility and seams.** Each new reset logs the matched rule (`replay-equivalent (patch-ids differ, net content identical)`; the existing reset lines gain `(rule patch-id)`). The pause comment and the `REFUSING to work on…` line are unchanged. Two independent per-Engine seams neutralise the fixes: `SetCommentRebaseRestoreContentCheckDisabledForTest` (restore) and `SetReplayEquivalenceDisabledForTest` (sync, push-time verification and catch-up).

## Consequences

- The #2498 sequence ends with no pause, with either fix alone.
- A stale or wrong base ref can only produce a false "not equivalent" (fail closed).
- The net-diff form tests the final content, not each intermediate commit. That is lossless for a reset to a remote holding the same final content.
