# ADR 2089: Zero-ahead push guard

## Status

Accepted (#2089). Report #2058 is mentioned for context.

## Context

An existing worktree's local `fabrik/issue-N` is never synced from `origin/fabrik/issue-N` (`updateWorktreeFromMain` fetches and rebases onto base only; `adoptRemoteIssueBranch` runs only when the local branch is missing). An out-of-band push (a human resolving a conflict) is therefore invisible, and `PushBranch`'s bare `--force-with-lease` compares against a tracking ref that `ensureBareCloneAs`, `FetchOrigin`, the merge train and any worker `git fetch` all refresh for every branch, so the lease protects nothing. A stale branch with no commits of its own is, after the rebase, exactly the base tip; pushing it makes the remote issue branch equal to base and GitHub closes the PR for having no diff.

The complete fix (sync the branch before work, push with an explicit lease on the synced SHA, as `PrepareCatchUp` does) is larger and separate. This ADR records the cheap guard that stops the destructive outcome meanwhile.

## Decision

1. **One choke point.** The guard lives in `pushBranchUnlessQueued`, after the ADR-058 in-queue skip. Every production push goes through it, so covered callers need no signature change. The base is resolved internally with `baseBranchForItem` because `markPRReady` has no base in scope and a single point cannot drift from its callers.
2. **Literal count.** `WorktreeManager.CommitsAheadOfRef` runs `git rev-list --count origin/<base>..HEAD`. It does not reuse `commitsAheadOfBase`, which drops spec-only commits for the #921 coordinator rule: a refusal must be conservative, so a branch with only a spec commit still pushes.
3. **Open PR, read live, only when the count is zero.** The cached `LinkedPRNumber` may be zero on a fresh item and would silently disable the guard. The common path costs one local git command and no API call (ADR-957).
4. **Fail open.** An unresolvable base, a git error and a PR read error all let the push proceed, with a logged warning. This deliberately overrides the spec's lean toward refusing on unreadable state: the guard is a net for one known shape, and refusing on a transient API error would strand legitimate pushes.
5. **Refuse = pause + sentinel.** The guard pauses through `pauseIssue` (`fabrik:paused` + `fabrik:awaiting-input`, header `🏭 **Fabrik — push refused: local branch has no commits**`) with #1408 episode dedup (`hasPauseComment` / `reapplyPauseLabels`), and `pushBranchUnlessQueued` returns `ErrPushRefusedZeroAhead`. The post-stage caller short-circuits before the completion chain so `handleStageComplete` cannot strip `awaiting-input` or advance a paused item; `markPRReady` skips `MarkPRReady` and reports the refusal (returns `true`); its other two callers (the R5 PR-creation retry in `item.go` and the comment-path completion in `comments.go`) skip `handleStageComplete` on it for the same reason.
6. **PR-creation callers are exempt.** `ensureDraftPR` and `processPRCreateMarker` use `pushBranchForNewPR` (in-queue skip only). They push because no open PR exists; a zero-ahead branch there is legitimate, and exempting them removes the race where GitHub auto-creates a PR between lookup and push.
7. **No resync.** The guard never fetches the issue branch, fast-forwards or resets, or sets a lease SHA (R3).
8. **Cancellation push refuses without pausing.** The R8 (#1393) WIP push of a cancelled worker uses `pushBranchOnCancel`: same in-queue skip and zero-ahead refusal (the overwrite is as destructive there), but no `pauseIssue` — the daemon-stop / TUI-stop interruption already pauses the item under `pauseIssueMu`, and a second unserialised pause from a stale snapshot could add a duplicate comment.
9. **Test seam.** `SetPushZeroAheadGuardDisabledForTest` lets the unit and sim tests show their refusal assertions fail when the guard is off.

## Consequences

- The #2058 shape (stale zero-ahead local branch, remote holding a human commit, open PR) leaves the remote untouched and pauses the item with one comment.
- **Residual gap:** a stale branch that carries its own commits can still overwrite out-of-band work. The follow-up resync plus explicit lease supersedes this guard.
- A legitimately finished branch whose commits were all absorbed into base also reads as zero-ahead; its PR has no diff anyway, and the comment avoids asserting a cause.
- `hasPauseComment` is unscoped by time, so a later separate episode on the same item reapplies the pause labels without a fresh comment (the existing #1408 trade-off); the log line still appears.
