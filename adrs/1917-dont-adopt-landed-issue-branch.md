# ADR 1917: Don't adopt an already-landed origin issue branch

## Status

Accepted (#1917). Amends the adoption rule of #1916 (`adoptRemoteIssueBranch`).

## Context

`EnsureWorktree` creates a missing local `fabrik/issue-N` from `origin/fabrik/issue-N` when that branch exists on origin, so a branch with unmerged work whose local copy was lost keeps its commits and its pushes keep a valid lease. The check was existence only. A branch that has already **landed** (merged by merge commit, or squash/rebase-merged with GitHub's head-branch auto-delete off, the default) exists on origin too, so `fabrik:revalidate`, a reopen or a landing-verification failure adopted merged history and every later stage worked against content already on base.

## Decision

1. **Where.** The decision stays in `adoptRemoteIssueBranch`, after the existing single-branch fetch and before it returns the remote ref. The fetch stays ahead of the decision on purpose: if adoption is declined, `refs/remotes/origin/fabrik/issue-N` is still current, so the next `--force-with-lease` push passes its lease and the stale-lease loop #1916 fixed cannot return.
2. **R1, ancestry.** `git merge-base --is-ancestor <origin branch> <baseRef>` (the `baseRef` `EnsureWorktree` already resolved). Exit 0 = every commit is on base: decline, fork from base, one log line (worded "contained in", since an empty branch equal to base is not "landed"). Lossless by construction.
3. **R2, merged PR head.** A squash/rebase merge leaves a tip that is not an ancestor. `WorktreeManager` makes no GitHub call, so callers pass the facts through the variadic `EnsureWorktreeOption` `WithLandedPRHeads`; a tip equal to a supplied merged-PR head declines adoption. Unknown or empty falls back to R1 only.
4. **R3, base mismatch.** `WithPRBaseProbe` supplies the base of the item's open PR; when it differs from the resolved base a loud `warn: base mismatch` line naming both is logged. Adoption is unchanged (refusing would lose unmerged work; rebasing is the stage's job).
5. **R5 polarity: doubt means adopt.** Any error in the ancestry check (exit other than 0/1, exec error, unresolvable ref) adopts. This is the opposite of the fetch-error path in the same function, which forks from base. Adoption is lossless; forking on doubt reintroduces the stale-lease loop. A comment at the function says so.
6. **Lazy options, variadic.** The callbacks run only when adoption is about to happen, under `wm.mu`. The engine side (`worktreeAdoptionOptions`, `engine/worktree_landed.go`) reads the cached `LinkedPRState` (merged + head) and makes at most one live `FetchPRDetails` for the `fabrik:credited-pr:<N>` PR and one for the PR base; every failure is "unknown". Cache lag can only make R2 miss, which degrades to R1. A variadic parameter keeps ~40 positional test call sites untouched. `PrepareCatchUp` passes no options.
7. **Seams.** `SetLandedBranchCheckDisabledForTest` (R1 and R2 only) and `SetIsAncestorFnForTest`, so each test is shown to fail without the check.

## Rejected alternatives

- **Eager facts:** would cost a PR read on every call, including the common path that adopts nothing.
- **Fetching `origin/<base>` before R1:** adds network under the mutex; a stale base can only turn "landed" into "not an ancestor", i.e. adopt.
- **A new struct parameter:** touches every caller for no behavioural reason.
- **Forking on doubt:** see 5.

## Consequences

A re-entered item whose branch has landed starts from current base. After the fresh fork the next push replaces the landed origin branch; that is intended. A squash-merged branch whose merge is not yet in the cache stays adopted until the cache refreshes (a missed skip, not data loss).
