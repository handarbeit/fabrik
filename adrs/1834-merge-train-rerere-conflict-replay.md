# ADR 1834: Merge-Train Conflict Resolution Replay via `git rerere`

**Date**: 2026-09-24
**Status**: Accepted
**Issue**: #1834 — merge-train: conflict resolutions die with their trial — re-assembly re-pays
Claude for identical conflicts (use git rerere)

## Context

A merge-train conflict resolution lives only on its trial branch. `cleanupTrialArtifacts` deletes
the trial worktree and branch after every trial — including bisection sub-trials — regardless of
outcome. When a trial is abandoned (red and bisected, re-formed after an ejection, or rebuilt after
the base moves), the resolution Claude produced is deleted with it, and the next assembly pays a
fresh Claude invocation for the byte-identical conflict.

The motivating incident (`verveguy/concept-maps`, 2026-09-20) resolved the same file's conflict
twice in 80 minutes across two trials for the same two members, and the operator's own record lists
this first among the reasons the train was turned off for that repo. On a repo where conflicts are
the common case (shared index files), this is most of the train's cost.

`git rerere` ("reuse recorded resolution") is a standard git feature for exactly this: it records a
completed conflict resolution keyed on the conflict's own three-way content, and replays it
automatically the next time the identical conflict recurs, independent of branch or worktree
identity. Nothing in the codebase enabled it before this issue.

## Decision

### 1. Enable `rerere.enabled`/`rerere.autoupdate` repo-wide, not worktree-scoped

**This corrects the plan's original decision, made during implementation.** The plan called for
scoping enablement narrowly to merge-train trial worktrees via Git's per-worktree config extension
(`extensions.worktreeConfig` + `git config --worktree`), reasoning that a regular per-issue
worktree off the same bare clone should be unaffected, and that `extensions.worktreeConfig` was
"more precisely targeted" than a repo-wide setting.

That approach was implemented, then reverted after direct reproduction: turning on
`extensions.worktreeConfig` on a bare clone makes `core.bare` — `true`, a bare repo's own top-level
config value — become the extension's shared/common value, and **any** linked worktree that
doesn't explicitly override `core.bare` in its own `config.worktree` inherits `true` and fails
every ordinary git operation with `fatal: this operation must be run in a work tree`. This is not
limited to the newly-created trial worktree; it breaks **every** worktree already checked out from
that bare clone, and every one created afterward, unless each one is individually patched. Since
Fabrik's worktrees for a given repo all share one bare clone (`NewWorktreeManagerForRepo`) — every
open issue's worktree, not just merge-train trials — enabling the extension at trial-worktree
creation time would have broken every other open issue's worktree for that repo the instant the
first merge-train trial worktree was created.

Given that finding, the plan's own Technical Question 1 alternative — unconditional repo-wide
enablement — is not merely simpler, it is the only one of the two options that is actually safe.
`enableRerere` (`engine/worktree.go`) sets `rerere.enabled`/`rerere.autoupdate` on the bare clone,
called from `ensureBareClone` immediately alongside the existing `setCommitterIdentity` call
(same idempotent "set if unset" shape, same call site, same two production+test coverage
implications). The risk this scoping was meant to avoid — silently changing rerere behavior for
regular per-issue worktrees — is real but low-severity: a rerere lookup with no recorded resolution
is a pure no-op, so at most a Validate-stage rebase conflict that happens to exactly match a
merge-train-recorded resolution is itself replayed for free. That is a strict improvement, not a
regression.

### 2. Replay-aware detection via a check ahead of classification, not inside `unmergedPaths`

`git merge` always exits non-zero on a conflict, even when rerere's `autoupdate` fully replays and
stages every hunk — git never auto-commits on the caller's behalf just because a recorded
resolution matched. So `mergeErr != nil` in `assembleTrialBranch` is uninformative on its own about
whether there is still anything for Claude to do.

`resolveTrainConflict` checks `unmergedPaths` immediately: an empty result, following a merge
failure, is exactly "rerere replayed everything" — `commitRerereReplayedMerge` finalizes directly
(a plain `git add -A` + `git commit`, mirroring `resolveConflictWithClaude`'s own finalization
commit), and Claude is never dispatched. A non-empty result falls through to the existing
classification logic unchanged: `buildTrainConflictComment` was never handed a precomputed path
list for the plain (non-generated) case, so a *partially* replayed conflict already resolves
correctly with no code change — Claude's own `git status` naturally shows only what rerere left
behind.

### 3. The ADR-1235 interaction: recover original conflict membership from `mergeOut`, not from `unmergedPaths`

`git rerere` has no awareness that a path is a declared generated file (ADR-1235). If a conflict
confined to a declared generated path (e.g. `docs/llms-full.txt`) was already resolved via
`regenerateAndCommit` once, and the identical conflict recurs, `rerere.autoupdate` would silently
replay and re-stage that old committed content before `resolveTrainConflict`'s
`unmergedPaths`-based classification ever ran. `classifyConflictedPaths` would then see an empty
`paths` slice, `matched` would come back empty, and the declared regeneration command would never
re-run — landing stale replayed content instead of ADR-1235's guaranteed fresh regeneration of the
trial's actual current sources.

The fix does not introspect `rr-cache` or force-regenerate every declared path on any conflict
(both considered and rejected — see below). Instead, `conflictedGeneratedSpecsFromMergeOutput`
(`engine/generated_files.go`) recovers the conflict's *original* membership from the merge's own
output: git always prints a `CONFLICT (...)` line naming every originally-conflicted path,
independent of whether rerere then silently resolved it. A substring check of each declared
generated path against those lines is simple and robust across conflict-kind variants (content,
add/add, and modify/delete conflicts all name the path directly in that line's text) — it needs no
structured parsing of `rr-cache` internals. `resolveTrainConflict` unions that recovered set with
whatever `classifyConflictedPaths` finds in the current (post-rerere) unmerged set via
`unionGeneratedSpecsByPath`, before any dispatch decision, so a declared generated path is always
force-regenerated whether or not it is still genuinely unmerged by the time the check runs.

### 4. Red-trial hygiene: a throwaway solo re-merge plus `git rerere forget`, not `rr-cache` directory bookkeeping

Bisection sub-trial worktrees are destroyed (`cleanupTrialArtifacts`) immediately after each
sub-trial's own CI result is known — before the poisoner is identified, since bisection only learns
the poisoner once every sub-trial has run. The porcelain `git rerere forget <path>` command
requires a *live* conflicted working tree to re-derive the conflict's hash from; it cannot be
pointed at an arbitrary historical hash from outside a live conflict. By the time a poisoner is
known, every worktree that ever observed its conflict is already gone.

The plan's original alternative — diffing `rr-cache`'s directory listing immediately before and
after each member's own resolution step, threading a `map[int][]string` (member number → newly-
touched hash directories) through the fully recursive `bisect` call tree, and retaining it for the
lifetime of a red-batch episode — was considered and rejected as substantially more code and much
harder to test reliably for a hygiene feature that only needs to fire once per ejected poisoner.

Instead, `forgetPoisonerResolutions` (`engine/merge_train.go`, called from `handleRedBatch`
immediately after the poisoner is ejected) reconstructs a fresh live conflict cheaply: a disposable
trial worktree forked off the same pinned `p.baseSHA` every trial in the episode used, a solo
`git merge --no-ff --no-edit <poisoner.headSHA>`, and — if that conflicts — `git rerere forget` for
every path named in *that* merge's own output. Paths are recovered via
`conflictedPathsFromMergeOutput`, the same `mergeOut`-derived technique as the ADR-1235 guard above,
deliberately **not** `unmergedPaths`: direct testing confirmed that once rerere's `autoupdate` has
replayed and staged a path, `git status` no longer reports it as unmerged even though `git rerere
forget` still has a genuine, live resolution to forget there — the identical structural gap
Decision 2 closes for the ordinary assembly path.

If the solo merge doesn't conflict at all, the batch's redness was a non-isolable interaction
(ADR-059 D-e) unrelated to any conflict resolution, and nothing is forgotten — silently, with no
warning logged, since this is the expected, non-exceptional case for that kind of redness. Entirely
best-effort throughout: every git failure here is logged and none of them affect the eject that
already happened. Forgetting is unconditional for every path in the poisoner's own solo conflict —
not narrowed to "only resolutions that involved a specific cross-PR interaction" — since a bad
resolution recorded anywhere in the poisoner's own conflict history is exactly the class of thing
this hygiene step exists to stop replaying.

### 5. `gc.rerereResolved`/`gc.rerereUnresolved` defaults accepted as-is; `git rerere gc` is now actually invoked

Nothing in the codebase called `git rerere gc` before this issue, making its governing config
(`gc.rerereResolved` default 60 days, `gc.rerereUnresolved` default 15 days) moot — without ever
running `gc`, `rr-cache` would grow unbounded regardless of what those values said.
`runMergeTrainWorker` now runs `git rerere gc` once per worker invocation, immediately after
`prepareTrainWorker` succeeds, best-effort (logged, never blocking). The defaults themselves need
no override: `rr-cache` entries are small text blobs, and the merge-train's own observed reuse
window (minutes to weeks, per the motivating incident) comfortably fits inside 60 days.

### 6. No new configuration toggle

The issue's own requirements never mention one. Rerere is unconditionally enabled whenever a bare
clone is created or repaired — there is no `merge_train_rerere`-style opt-out, and no
`docs/USER_GUIDE.md` flag-table entry was added for one.

## Consequences

- Enabling rerere is now repo-wide, not merge-train-specific — a behavior change beyond the issue's
  literally-stated scope ("merge-train assembly"), accepted because the narrower alternative is
  unsafe (Decision 1) and the repo-wide behavior change carries no downside beyond an occasional,
  harmless free replay outside the merge train.
- `resolveTrainConflict` and `forgetPoisonerResolutions` both depend on parsing `git merge`'s
  combined output for conflict membership, rather than only ever consulting `unmergedPaths`/`git
  status`. This is a second, independent load-bearing use of the same "recover original membership
  from `mergeOut`, not from current index state" technique — a maintainer changing conflict-related
  code should expect both code paths to need the same care if git's conflict-line wording ever
  changes across a future git version.
- Two merge-train workers for the same repo but different `(repo, base)` partitions (ADR-1648) can
  run concurrently, each in its own trial worktree off the same shared bare clone, both writing
  into the same `rr-cache` with no mutex serializing the `git merge`/`git commit` sequence between
  them. This is **accepted, not mitigated**: git's `rr-cache` writes are per-hash-directory, so two
  *different* conflicts recorded concurrently don't collide; the only real race is two workers
  resolving the *identical* conflict hash at the same moment — narrow, low-probability, and
  low-severity (worst case, one valid resolution's postimage is overwritten by another, still
  gated by the unchanged CI trust boundary below).
- The trust boundary for a conflict resolution is unchanged (Requirement 4): a replayed rerere
  resolution is committed and pushed exactly like a fresh Claude resolution, then validated by the
  trial's own combined CI. No code path treats a replay as more trusted than a fresh resolution.
- `docs/state-machine.md` §6.26 and `docs/USER_GUIDE.md`'s "Merge Train / Queued" section are
  updated to describe the mechanism and the user-visible cost change.

## Rejected Alternatives

- **Worktree-scoped enablement via `extensions.worktreeConfig`.** The plan's original decision —
  see Decision 1 for the concrete failure mode discovered during implementation.
- **Force-regenerating every declared generated path on any conflict in the trial**, regardless of
  whether that path was ever part of the conflict. Rejected as wasteful and imprecise compared to
  recovering the conflict's actual original membership from `mergeOut` (Decision 3).
- **Introspecting `rr-cache` directly** (either for the ADR-1235 guard or for red-trial hygiene) —
  rejected in both cases in favor of the simpler, already-available `mergeOut` text, which needs no
  parsing of `rr-cache`'s internal hash-directory format and is already proven correct by git's own
  documented conflict-line output.
- **`rr-cache` directory diffing plus a member→hash-directory map threaded through `bisect`** for
  red-trial hygiene — see Decision 4. Rejected as substantially more code and much harder to test
  reliably than a throwaway solo re-merge, for a hygiene feature that only needs to trigger once
  per ejected poisoner.
- **A `merge_train_rerere` configuration toggle.** Rejected: the issue's requirements never mention
  one, and rerere's downside when it has nothing recorded is zero — there is no scenario where an
  operator would want it off.
