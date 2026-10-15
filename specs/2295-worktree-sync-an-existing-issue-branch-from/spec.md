# Feature Specification: Sync an existing issue branch from origin before work, and push with an explicit lease on a verified remote head

**Feature Branch**: `fabrik/issue-2295`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: the original issue body, verbatim:

~~~~text
## Problem

Report #2058: a person pushed a conflict resolution to `fabrik/issue-N`. The engine's existing worktree still held the stale local branch. The engine rebased that branch onto base, it was pushed over the remote branch, and GitHub closed the PR. #2089 (shipped in v0.0.84, `engine/push_guard.go`) refuses the push when the local branch is zero commits ahead of base while a PR is open. #2089's spec deferred the underlying fix: sync from origin before work, plus an explicit SHA lease. That follow-up is this issue. Until it lands, a stale branch that still carries its own commits can overwrite out-of-band work.

In v0.0.85:

- **No sync on the existing-worktree path.** When the worktree directory exists and is on the issue branch, `EnsureWorktree` (`engine/worktree.go:437`) calls only `updateWorktreeFromMain` (`engine/worktree.go:455`). That fetches `origin <base>` (`:998`) and runs `git rebase origin/<base>` (`:1025`). It never looks at `origin/fabrik/issue-N`. `adoptRemoteIssueBranch` (`:531`), which probes and fetches the remote issue branch, runs only when the local branch is missing (#1915, #1917).
- **The lease protects nothing.** `PushBranch` (`engine/worktree.go:620`) runs a bare `git push --force-with-lease -u origin fabrik/issue-N` (`:625`). A bare lease compares against `refs/remotes/origin/fabrik/issue-N` in the bare clone that every worktree of the repo shares. Several paths refresh that ref: `FetchOrigin` (`:972`, a full `git fetch origin` used by the merge train), `ensureBareCloneAs`, the targeted fetches in `adoptRemoteIssueBranch`, `PrepareCatchUp` and `RestoreUnpushedRebase`, and any worker that runs `git fetch`. After any of them, the lease matches whatever the remote currently holds, so the push overwrites it.
- All engine pushes funnel through `pushBranchUnlessQueued`, `pushBranchForNewPR` and `pushBranchOnCancel` (`engine/merge_queue.go:55-97`). Each ends in `wm.PushBranch`. The #2089 guard (`zeroAheadCheck`, `engine/push_guard.go:66`) runs in the first and third.
- **The precedent already exists.** The merge-train singleton catch-up does what the ordinary path lacks:
  - `PrepareCatchUp` (`engine/worktree_catchup.go:40`) checks for a clean tree and no in-progress merge, rebase or cherry-pick. It makes a targeted fetch of the issue branch, fast-forwards when the local branch is strictly behind, and resets a *pure rebase* of the remote to the remote head (`localIsPureRebase`, `:124`, #2150). Anything else it refuses.
  - `PushCatchUp` (`:172`) pushes with `--force-with-lease=refs/heads/<branch>:<expectedHead>` and then advances the tracking ref.
- **#2150 interplay.** The comment path captures `preRebaseSHA` before `EnsureWorktree` (`engine/comments.go:410-414`). After a pass with no commit, it calls `RestoreUnpushedRebase` (`engine/comments.go:494`, `engine/worktree_restore.go:28`), which resets to that head. If the sync moves the branch, that captured SHA is the stale pre-sync head:
  - `RestoreUnpushedRebase` would then refuse (the post head carries commits the pre head lacks), leaving the unpushed rebase that #2150 exists to remove.
  - Or, if its checks passed, it would undo the sync.
- **Concurrency.** `EnsureWorktree` runs from the item's own dispatch (`engine/item.go:778`, `engine/comments.go:414`) under the item's in-flight claim and `wm.mu`. `PrepareCatchUp` calls it with `skipUpdate=true` for a Queued member. `catchUpBusyReason` (`engine/merge_train_catchup.go:185`) defers the catch-up while a worker is in flight or `fabrik:editing` is live. Stage workers push the same branch themselves from the same worktree: the review and validate skills run `git push --force-with-lease` (`plugin/fabrik-workflows/skills/review/SKILL.md:83`, `validate/SKILL.md:45`).

## Requirements

**R1. Sync before work.** On the existing-worktree path of `EnsureWorktree`, before any rebase onto base, the engine makes a targeted fetch of `fabrik/issue-N` into its tracking ref and classifies the local branch against the fetched remote head:

- **remote absent** (never pushed, or deleted): no sync.
- **equal**: nothing to do.
- **local strictly behind** (an ancestor of the remote): fast-forward.
- **local holds nothing the remote lacks** (no local-only patch by patch-id and no local-only merge commit, base history excluded, the `localIsPureRebase` test): reset to the remote head. This covers a pure rebase of the remote and a remote that a person rebased onto a newer base.
- **remote holds nothing the local lacks** (the mirror test): keep local. This covers unpushed local work, such as a push that failed earlier.
- **both sides hold something the other lacks**: diverged; see R3.

Reuse `PrepareCatchUp`'s primitives rather than writing a second implementation. Extract shared helpers if needed; `PrepareCatchUp`'s own behaviour must stay exactly as it is.

**R2. Local mutation only when safe.** The fast-forward and the reset run only on a clean tree with no `MERGE_HEAD`, `REBASE_HEAD` or `CHERRY_PICK_HEAD`. That is the same condition `PrepareCatchUp` and `RestoreUnpushedRebase` use. When it does not hold, the sync changes nothing and logs why, and the push-time check (R4) remains the protection. A fetch error is logged and the sync is skipped. It is never treated as "remote absent" for the purposes of R4.

**R3. Defined handling for divergence.** A truly diverged branch is neither rebased nor pushed, and the stage is not invoked on it. The item is paused (`fabrik:paused` + `fabrik:awaiting-input` via `pauseIssue`, one comment per episode with the `hasPauseComment`/`reapplyPauseLabels` dedup). The comment names the branch, the local and remote heads, and says both were left untouched. The pause is applied by the engine-level caller, because `WorktreeManager` has no item or pause capability: `EnsureWorktree` reports the outcome as a typed result or error. No automatic merge or rebase of the two sides is attempted.

**R4. Push with an explicit lease on a verified head.** Every engine push of an issue branch replaces the bare `--force-with-lease` with `--force-with-lease=refs/heads/fabrik/issue-N:<sha>`, where `<sha>` is a remote head the engine has verified against the local branch. Immediately before the push:

- **Remote unchanged.** If the remote head equals the head the engine last reconciled the branch against (recorded at the R1 sync, at adoption, or by the engine's last successful push), the local branch is authoritative. Push with the lease on that SHA.
- **Remote moved, or nothing recorded** (e.g. after a restart):
  - Fetch the branch and apply R1's classification against the fresh head. If the remote holds nothing the local lacks, push with the lease on that fetched SHA.
  - If the local is strictly behind or equal, do not push; there is nothing to publish, and a push would rewind the remote.
  - If the remote holds commits the local lacks, refuse the push and pause as in R3.
- **Remote absent.** Push with a lease that requires the ref not to exist.

A lease rejection means the remote moved after verification. Re-verify once. A second rejection is logged and left for the next poll; never force past it. After a successful push, record the pushed SHA and advance the tracking ref, as `PushCatchUp` does.

Plan decides where the "last reconciled head" lives. If it is in memory, a restart falls to the fetch-and-classify branch. If it should survive restarts, the record must not be a ref that `git fetch origin` overwrites.

**R5. Coverage.** R4 applies to all three push wrappers (`pushBranchUnlessQueued`, `pushBranchForNewPR`, `pushBranchOnCancel`):

- The merge-queue skip and the #2089 zero-ahead guard keep their order and behaviour. The zero-ahead guard is not removed.
- `pushBranchOnCancel` refuses without pausing, as #2089 does on that path.
- `PushCatchUp` already leases explicitly. It records its pushed SHA as the reconciled head so the next stage push takes the "remote unchanged" path.

**R6. #2150 consistency.** The head the comment path uses as `preRebaseSHA` must be the head *after* the R1 sync and before the rebase. `EnsureWorktree` reports it, or the sync and rebase are made separately callable. `RestoreUnpushedRebase` therefore restores to the synced head and never undoes a sync.

**R7. Concurrency.** The sync and the pre-push verification run under `wm.mu` for their git work, and never hold it across a GitHub API call or a pause. They run only from the item's own dispatch path, under its in-flight claim.

When `PrepareCatchUp` calls `EnsureWorktree` with `skipUpdate=true`, the R1 sync must not change `PrepareCatchUp`'s outcomes. Pin each case with a test:
- a busy or dirty tree still defers;
- a remote that moved from the snapshot head still defers;
- a diverged local still returns `ErrCatchUpLocalDiverged`;
- a pure rebase still resets.

Either skip the sync on that path, or show that it leaves those outcomes unchanged. Whether R1 runs when `skipUpdate` is true for other reasons (retry, merge-queue suppression) is decided in Plan and documented: R1 never pushes, so it cannot eject a merge-queue entry.

## Scope

In scope:
- the existing-worktree sync;
- explicit-lease pushes for every engine push of an issue branch;
- the divergence pause;
- the #2150 SHA hand-off;
- tests, the sim twin and as-built docs.

Out of scope:
- The worker skills' own `git push --force-with-lease` instructions (review, validate). Record their interaction in the ADR: a worker push from the worktree leaves local equal to remote, which R4 accepts without pausing.
- Train branches (`PushTrainBranch`).
- Automatic integration of diverged branches.
- Changes to `adoptRemoteIssueBranch`'s #1917 landed-branch rules.

Mention #2058 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2058. This issue's own number gets `Closes`.

## Acceptance

Unit tests, real git, `skipIfNoGit`:
- **Out-of-band push, local behind or at base:** a stale local branch is fast-forwarded before the rebase, and the human's commit survives the next push.
- **Report shape with local work:** local carries its own commit, a human pushed a different commit, and the shared tracking ref was refreshed by `FetchOrigin`. The push is refused, the remote is unchanged, and the item is paused with one comment. This is the case #2089 cannot catch.
- **Pure rebase:** an unpushed pure rebase of the remote is reset to the remote head.
- **Unpushed local work:** local ahead of an unchanged remote is kept and pushes normally.
- **Ordinary stage:** the engine rebase plus a new commit, remote unchanged, pushes with a lease on the recorded head.
- **Lease-race test:** the remote moves between verification and push. The push is rejected and nothing is overwritten.
- **Worker-pushed branch:** local equals remote, and no pause follows.
- **Comment path (#2150):** a pass with no commit after a sync restores to the synced head, not the pre-sync head.
- **`PrepareCatchUp`:** its existing tests pass unchanged, plus the R7 cases.

Sim twin: extend `tests/sim/push_guard_test.go`, or add `tests/sim/worktree_sync_test.go`, with the #2058 shape where the stale branch carries its own commit. Assert the remote ref is byte-identical afterwards and the item is paused. Add a scenario where an out-of-band commit on a behind branch survives a stage run.

Non-vacuity: each core test fails with the change neutralised through a per-WorktreeManager or per-Engine test seam, never a package-level variable. With the sync disabled, the fast-forward test fails. With the bare lease restored, the report-shape test overwrites the remote. Record both in the PR.

Docs:
- `docs/state-machine.md`: the "Zero-ahead push guard" section, §4.4 (comment path, #2150 hand-off) and §6.30 (catch-up interplay) describe the sync, the lease and the divergence pause.
- `docs/stage-lifecycle.md`: worktree setup.
- `docs/USER_GUIDE.md`: the operator-visible divergence pause and how to resume it.
- Regenerate `docs/llms-full.txt` (`bash scripts/generate-llms-full.sh`) in the same commit.
- An ADR numbered after this issue.
- Update the `CLAUDE.md` architecture bullets for `engine/push_guard.go` and the worktree files.
~~~~

## Background

Report #2058: a person pushed a conflict resolution to the issue branch. The engine's existing worktree still held the stale local copy of that branch. The engine rebased the stale branch onto base and pushed it over the remote, and GitHub closed the PR. Out-of-band work was silently destroyed.

Today (v0.0.85) two gaps allow this:

- **The engine never syncs an existing worktree from the remote issue branch.** It only brings the branch up to date with *base*. Looking at the remote issue branch happens only when the local branch is missing entirely.
- **The push's "force-with-lease" protects nothing.** The bare form compares against a remote-tracking ref in a bare clone shared by every worktree of the repository. Many routine operations (the merge train's full fetch, other targeted fetches, any worker running `git fetch`) refresh that ref, after which the lease matches whatever the remote currently holds and the push overwrites it.

#2089 (v0.0.84) added a guard that refuses the push when the local branch is zero commits ahead of base while a PR is open. It catches the case where the stale branch carries nothing of its own, but not a stale branch that carries its own commits. Its spec explicitly deferred the underlying fix: sync from origin before work, and push with an explicit SHA lease. This issue is that follow-up.

The merge-train singleton catch-up already behaves correctly in the same situation: it fetches the issue branch, fast-forwards a branch that is strictly behind, resets a pure rebase of the remote to the remote head, refuses anything else, and pushes with an explicit lease on the head it verified. The ordinary stage and comment paths lack this. Related precedents: #2150 (restoring an unpushed engine rebase on the comment path, which interacts with any sync that moves the branch), #1915/#1917 (adoption of a remote issue branch when the local branch is missing).

Stage workers (review, validate) also push the issue branch themselves with a bare lease from the same worktree; that behaviour is not changed here.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Out-of-band commits on the issue branch survive (Priority: P1)

An operator or contributor pushes a commit (for example a conflict resolution) to the issue branch while the engine's worktree holds an older copy of that branch that has nothing of its own the remote lacks. The next time the engine works on the item, it brings the local branch up to the remote before doing anything else, and the person's commit is still on the remote after the engine's next push.

**Why this priority**: This is the data-loss case from report #2058; losing a human's commit and closing the PR is the most damaging failure.

**Independent Test**: With real git, push a commit to the remote issue branch from outside the engine, leave the local branch behind (at base or at an older head), run the worktree-ensure step and then a stage push; verify the remote still contains the commit and the local branch was fast-forwarded before the rebase onto base.

**Acceptance Scenarios**:

1. **Given** a local issue branch strictly behind the remote issue branch, **When** the engine prepares the worktree for work, **Then** the local branch is fast-forwarded to the remote head before any rebase onto base.
2. **Given** that fast-forward and a subsequent stage that adds a commit, **When** the engine pushes, **Then** the remote retains the person's commit and the push succeeds.
3. **Given** the remote issue branch does not exist (never pushed, or deleted), **When** the engine prepares the worktree, **Then** no sync occurs and behaviour is unchanged.

---

### User Story 2 - A diverged branch is never overwritten; the item is paused for a human (Priority: P1)

The local branch carries its own commit, a person pushed a different commit to the remote, and the repository's shared tracking ref was refreshed by an unrelated fetch (the exact shape of #2058 that #2089 cannot catch). The engine neither rebases nor pushes the branch, does not run the stage on it, leaves both sides untouched, and pauses the item with a single comment that tells the operator what happened and how to resume.

**Why this priority**: Without this, the remaining overwrite path from #2058 stays open; it is the reason the push must verify the actual remote head rather than trust the tracking ref.

**Independent Test**: With real git, create local and remote histories that each hold a commit the other lacks, refresh the shared tracking ref with a full fetch, and attempt to ensure the worktree and push; verify the remote ref is byte-identical afterwards, the local branch is unchanged, the stage was not invoked, and the item carries the pause labels with exactly one comment.

**Acceptance Scenarios**:

1. **Given** a diverged local and remote branch, **When** the engine prepares the worktree, **Then** it reports divergence, performs no rebase, merge, reset or push, and the item is paused with `fabrik:paused` + `fabrik:awaiting-input`.
2. **Given** a divergence pause already commented for this episode, **When** the engine meets the same divergence again, **Then** no second comment is posted and the pause labels are reapplied if missing.
3. **Given** the divergence is only detected at push time (for example the remote moved after the worktree was prepared and the remote now holds commits the local lacks), **When** the engine attempts to push, **Then** the push is refused, the remote is unchanged and the item is paused as in scenario 1.
4. **Given** a push from the cancel path meets the same condition, **When** it is refused, **Then** it refuses without pausing.

---

### User Story 3 - Unpushed local work and engine rebases are handled safely (Priority: P2)

Local work that has not yet been published (for example a push that failed earlier) is kept and published normally; an unpushed pure rebase of the remote, or a remote a person rebased onto a newer base, is reset to the remote head instead of being pushed over it.

**Why this priority**: Without these, the sync would either lose legitimate local work or keep producing spurious divergence pauses.

**Independent Test**: With real git, (a) make local ahead of an unchanged remote and verify it is kept and pushes normally; (b) make local a pure rebase of the remote and verify it is reset to the remote head.

**Acceptance Scenarios**:

1. **Given** local is ahead of an unchanged remote (the remote holds nothing the local lacks), **When** the engine prepares the worktree, **Then** the local branch is kept and a later push succeeds.
2. **Given** local holds nothing the remote lacks by content (a pure rebase of the remote, or a remote rebased by a person onto newer base), **When** the engine prepares the worktree, **Then** the local branch is reset to the remote head.
3. **Given** local equals remote (for example a worker pushed from the worktree), **When** the engine prepares the worktree and later pushes, **Then** nothing is rewritten and no pause follows.

---

### User Story 4 - Every engine push leases against a verified remote head (Priority: P1)

Every engine push of an issue branch (normal stage push, new-PR push, cancel-path push, merge-train catch-up push) replaces the bare lease with a lease on a specific remote head that the engine has verified against the local branch. If the remote moves between verification and push, the push is rejected rather than overwriting.

**Why this priority**: This is the structural fix that makes the lease meaningful regardless of which other path refreshed the shared tracking ref.

**Independent Test**: With real git, verify the remote head, move the remote, then push; the push is rejected and the remote keeps the moved head. Also verify an ordinary stage (engine rebase plus a new commit, remote unchanged) pushes successfully.

**Acceptance Scenarios**:

1. **Given** the remote head equals the head the engine last reconciled against, **When** the engine pushes, **Then** it pushes with a lease on that exact head and the push succeeds.
2. **Given** the remote head differs from the recorded head, or nothing was recorded (for example after a restart), **When** the engine pushes, **Then** it fetches the branch and classifies local against the fresh head: pushes with a lease on the fetched head if the remote holds nothing the local lacks; does not push if the local is behind or equal; refuses and pauses if the remote holds commits the local lacks.
3. **Given** the remote branch does not exist, **When** the engine pushes, **Then** it uses a lease requiring the ref to be absent.
4. **Given** the remote moves between verification and push, **When** the push is rejected, **Then** the engine re-verifies once; a second rejection is logged and left for the next poll, and the engine never forces past it.
5. **Given** a successful push, **When** it completes, **Then** the pushed head is recorded as the reconciled head and the tracking ref is advanced.

---

### User Story 5 - The comment path's no-commit restore never undoes a sync (Priority: P2)

On the comment-review path, a pass that produces no commit still restores the unpushed engine rebase (#2150) — but to the head *after* the sync and before the rebase, so the restore neither fails nor undoes the sync.

**Why this priority**: Correctness of an existing, shipped behaviour that the sync would otherwise break.

**Independent Test**: With real git, run a comment pass in which the sync moves the branch and no commit is produced; verify the branch ends at the synced head, not the pre-sync head.

**Acceptance Scenarios**:

1. **Given** a comment pass whose worktree preparation fast-forwards the branch and whose run produces no commit, **When** the unpushed rebase is restored, **Then** the branch is reset to the synced head.

---

### User Story 6 - The merge-train catch-up behaves exactly as before (Priority: P2)

The sync does not change what the merge-train singleton catch-up decides or does.

**Why this priority**: The catch-up is a shipped, tested path that already implements the correct behaviour for a Queued member; it must not regress.

**Independent Test**: The existing catch-up tests pass unchanged, plus cases for a busy/dirty tree (still defers), a remote moved from the snapshot head (still defers), a diverged local (still returns `ErrCatchUpLocalDiverged`) and a pure rebase (still resets).

**Acceptance Scenarios**:

1. **Given** each of the four catch-up cases above, **When** the catch-up runs, **Then** its outcome is identical to before this change.
2. **Given** a catch-up push, **When** it succeeds, **Then** the pushed head is recorded as the reconciled head so the next stage push takes the remote-unchanged path.

---

### Edge Cases

- Dirty tree or an in-progress merge, rebase or cherry-pick when the sync would mutate: no local mutation; the reason is logged; the push-time check remains the protection.
- Fetch of the issue branch fails (network, auth): logged, sync skipped; a failed fetch is never interpreted as "remote absent".
- Remote branch deleted after the engine previously recorded a head: treated as remote absent, with the absent-ref lease.
- Engine restart: nothing recorded in memory leads to the fetch-and-classify path; no spurious pause for a branch whose local equals the remote.
- A worker (review/validate skill) pushes from the worktree: local equals remote; accepted without pause.
- The shared tracking ref is refreshed by another path between sync and push: the explicit lease on the verified SHA is unaffected.
- Merge-queue suppression or retry paths that skip the base update: whether the sync runs there is a decision recorded in Plan; the sync never pushes, so it cannot eject a queue entry.
- The git work for the sync and verification is serialized with other worktree git operations but never held across a GitHub API call or the pause.
- Zero-ahead guard (#2089) and merge-queue skip retain their order and behaviour.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On the existing-worktree path, before any rebase onto base, the engine MUST fetch the remote issue branch and classify the local branch against the fetched remote head as one of: remote absent, equal, local strictly behind, local holds nothing the remote lacks (no local-only patch by patch identity and no local-only merge commit, base history excluded), remote holds nothing the local lacks, or diverged.
- **FR-002**: When the remote issue branch is absent, the sync MUST do nothing; when local equals remote, the sync MUST do nothing.
- **FR-003**: When the local branch is strictly behind the remote, the engine MUST fast-forward it to the remote head before the rebase onto base.
- **FR-004**: When the local branch holds nothing the remote lacks (a pure rebase of the remote, or a remote a person rebased onto a newer base), the engine MUST reset the local branch to the remote head.
- **FR-005**: When the remote holds nothing the local lacks, the engine MUST keep the local branch unchanged.
- **FR-006**: The fast-forward and reset MUST run only on a clean tree with no merge, rebase or cherry-pick in progress; otherwise the sync MUST change nothing and log the reason.
- **FR-007**: A failed fetch MUST be logged and the sync skipped; it MUST NOT be treated as "remote absent" for push-time verification.
- **FR-008**: The sync classification MUST be implemented by reusing the merge-train catch-up's existing primitives (shared helpers extracted if needed), and the catch-up's own behaviour MUST remain exactly as it is.
- **FR-009**: When the local and remote branches are diverged, the engine MUST NOT rebase, merge, reset or push, MUST NOT invoke the stage on the branch, and MUST NOT attempt any automatic integration of the two sides.
- **FR-010**: On divergence, the engine MUST pause the item with `fabrik:paused` and `fabrik:awaiting-input`, posting one comment per episode (deduplicated like other pauses; labels reapplied if missing) that names the branch, the local and remote heads, states that both were left untouched, and tells the operator how to resume.
- **FR-011**: The worktree layer MUST report the sync outcome (including divergence) to its engine-level caller as a typed result or error; the pause MUST be applied by the engine-level caller, not by the worktree layer.
- **FR-012**: Every engine push of an issue branch MUST use an explicit lease naming the ref and a specific remote head the engine has verified against the local branch, replacing the bare lease. This applies to the normal stage push, the new-PR push and the cancel-path push.
- **FR-013**: Immediately before a push, if the remote head equals the head the engine last reconciled the branch against (recorded at the sync, at adoption, or by the engine's last successful push), the engine MUST push with the lease on that head.
- **FR-014**: If the remote head differs from the recorded head, or none is recorded, the engine MUST fetch the branch and classify against the fresh head: if the remote holds nothing the local lacks, push with the lease on the fetched head; if the local is strictly behind or equal, not push; if the remote holds commits the local lacks, refuse the push and pause as in FR-010.
- **FR-015**: If the remote branch is absent at push time, the engine MUST push with a lease requiring the ref not to exist.
- **FR-016**: On a lease rejection the engine MUST re-verify once; a second rejection MUST be logged and left for the next poll; the engine MUST NOT force past a rejection.
- **FR-017**: After a successful push the engine MUST record the pushed head as the reconciled head and advance the tracking ref.
- **FR-018**: The merge-queue skip and the zero-ahead push guard (#2089) MUST keep their existing order and behaviour; the cancel-path push MUST refuse without pausing.
- **FR-019**: The merge-train catch-up push MUST record its pushed head as the reconciled head.
- **FR-020**: The head the comment-review path uses to restore an unpushed engine rebase MUST be the head after the sync and before the rebase, so the restore never undoes a sync.
- **FR-021**: The sync and the pre-push verification MUST perform their git work under the worktree manager's serialization, MUST NOT hold it across a GitHub API call or a pause, and MUST run only from the item's own dispatch path under its in-flight claim.
- **FR-022**: The sync MUST NOT change the merge-train catch-up's outcomes — a busy or dirty tree still defers, a remote moved from the snapshot head still defers, a diverged local still returns `ErrCatchUpLocalDiverged`, a pure rebase still resets — either by skipping the sync on that path or by demonstrating equivalence; each case is pinned by a test.
- **FR-023**: Whether the sync runs when the base update is skipped for other reasons (retry, merge-queue suppression) MUST be decided in Plan and documented.
- **FR-024**: Each core behaviour MUST be neutralisable through a per-worktree-manager or per-engine test seam (never a package-level variable) so non-vacuity can be shown: with the sync disabled the fast-forward test fails; with the bare lease restored the report-shape test overwrites the remote. Both results are recorded in the PR.
- **FR-025**: The change MUST ship with: unit tests (real git, guarded by the existing git-availability skip) for each Acceptance case in the original request; a sim twin asserting the #2058 shape leaves the remote ref byte-identical and pauses the item, and that an out-of-band commit on a behind branch survives a stage run; as-built updates to the state-machine doc (zero-ahead push guard section, comment path and catch-up sections), the stage-lifecycle doc (worktree setup) and the user guide (the divergence pause and how to resume); a regenerated documentation bundle in the same commit; an ADR numbered after this issue (recording the worker-skill push interaction); and updated architecture notes in the project development guide.
- **FR-026**: The PR body MUST mention #2058 by bare number without any closing keyword adjacent to it, and MUST carry a closing keyword for this issue's own number only.

### Key Entities *(if applicable)*

- **Issue branch**: `fabrik/issue-N`, existing both as a local branch in the per-issue worktree and as a remote branch.
- **Reconciled head**: the remote head the engine last verified or published for an issue branch; the basis for the explicit lease. Where it lives (in memory versus durable, and in that case not in a ref a full fetch overwrites) is a Plan decision.
- **Sync outcome**: the typed result of the existing-worktree sync (absent / equal / fast-forwarded / reset / kept-local / skipped-unsafe / diverged), carrying the post-sync head and, for divergence, both heads.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the #2058 shape (stale local branch carrying its own commit, a person's different commit on the remote, shared tracking ref refreshed), the remote ref is byte-identical after the engine runs, in both the unit tests and the sim twin.
- **SC-002**: A human commit pushed to a branch whose local copy is behind is still present on the remote after the engine's next stage push, in 100% of the covered scenarios.
- **SC-003**: Every divergence produces exactly one pause comment per episode and leaves both the local and remote heads unchanged.
- **SC-004**: When the remote moves between verification and push, the push is rejected and the remote keeps the moved head.
- **SC-005**: A worker-pushed branch (local equals remote) and an unpushed local-ahead branch produce no pause and publish normally.
- **SC-006**: All existing merge-train catch-up tests pass unchanged, and the four new catch-up outcome tests pass.
- **SC-007**: Disabling the sync fails the fast-forward test, and restoring the bare lease makes the report-shape test overwrite the remote; both recorded in the PR.
- **SC-008**: An operator reading the user guide can identify a divergence pause, understand that both sides were left untouched, and resume it.

## Assumptions

- This issue's dependency (#2299) is resolved before work starts; the spec does not depend on its content.
- The merge-train catch-up's fetch, classification and explicit-lease push are the correct behavioural reference, and its existing primitives can be reused or extracted without altering its behaviour.
- Pausing via the engine's standard pause pair, with its existing comment dedup and resume-by-human-comment rule, is the right operator-facing treatment for divergence; no new label is introduced.
- A worker push from the worktree (review/validate skills) leaves local equal to remote, which the push-time check accepts without pause; changing the skills themselves is not needed.
- In-memory storage of the reconciled head is acceptable, since a restart falls back to the fetch-and-classify path; Plan may choose durable storage if it avoids refs a full fetch overwrites.
- The sync runs only from an item's own dispatch path; it never pushes and so cannot eject a merge-queue entry.

## Out of Scope *(optional)*

- Changing the worker skills' own push instructions (review, validate); their interaction is recorded in the ADR only.
- Merge-train branch pushes.
- Automatic merging, rebasing or other integration of diverged branches.
- Changes to the landed-branch rules of remote-branch adoption (#1917).
- Removing or altering the zero-ahead push guard (#2089).

## Source References *(optional)*

- Report #2058 (the originating incident); #2089 (zero-ahead push guard, whose spec deferred this work); #2150 (restoring an unpushed engine rebase on the comment path); #1915/#1917 (adoption of a remote issue branch); #2044/#2065 (merge-train singleton catch-up, the behavioural precedent).
- Code orientation for Research (from the original request): `engine/worktree.go` (`EnsureWorktree`, `updateWorktreeFromMain`, `adoptRemoteIssueBranch`, `PushBranch`, `FetchOrigin`), `engine/worktree_catchup.go` (`PrepareCatchUp`, `PushCatchUp`, `localIsPureRebase`), `engine/worktree_restore.go` (`RestoreUnpushedRebase`), `engine/merge_queue.go` (the three push wrappers), `engine/push_guard.go`, `engine/comments.go`, `engine/item.go`, `engine/merge_train_catchup.go`.
