# Feature Specification: Merge-train singleton catch-up tolerates an unpushed engine rebase

**Feature Branch**: `fabrik/issue-2150`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

A comment-processing invocation (ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke`) on an item with an open PR rebases the worktree onto `origin/<base>` before Claude runs. When that invocation makes no commit, nothing is pushed. The local issue branch is then a rebased copy of the remote branch, with the same patches on a newer base. The two have **diverged**, but neither carries work the other lacks.

The merge train's singleton catch-up (#2044, `PrepareCatchUp`) treats local-diverged-from-remote as an ownership conflict. It defers, uncharged, and the member takes the full trial path instead of catch-up plus the singleton fast path. That costs a whole trial's CI run (about 8 minutes) and a trial PR, every time.

**Observed twice on 2026-10-11** (daemon at `3d0a73f`):
- **#2097:**
  - At 14:05:28Z, `review-reinvoke` ran Validate via comment processing. The log shows `[#2097 worktree] rebased onto origin/main` at 14:05:30. The reflog shows `rebase (finish): refs/heads/fabrik/issue-2097 onto 4430b24b` → `c550abbe`. Claude resolved one thread in 4 turns with no commit, and there was no push.
  - At 14:50:00Z: `singleton catch-up not done for #2097: local member branch is ahead of or diverged from the remote: local c550abbe…, remote 5d9aaf6b…`, followed by `merged #2097 cleanly into trial branch`.
- **#2096:** the 12:59Z Validate comment review reported \"making no changes\" and left local `5f3cc5aa` against remote `f7ab19a0`, ahead 16 / behind 9. At 14:08:16Z: `singleton catch-up not done for #2096: local member branch is ahead of or diverged from the remote` → trial PR #2134.

A rebase that is never pushed also leaves the worktree describing a head nobody has CI for, which `ci-status.md`'s `HEAD == head_sha` skip rule (#1997) then refuses.

## Requirements

**R1. No unpushed engine rebase.** After a comment-processing invocation that produced no commit, the issue branch is not left diverged from its remote by the engine's own pre-invocation rebase. Plan chooses the mechanism and records why in an ADR. Candidates:
- (a) restore the local branch to the remote tip when the invocation made no commit and the only difference is the engine's rebase;
- (b) skip the pre-invocation rebase for comment processing on an item that already has an open PR whose CI the gate is waiting on;
- (c) push the rebase.

Option (c) re-runs CI and moves the head the CI gate has recorded, so it should be rejected unless Plan shows otherwise. Option (a) must never discard a commit that is not on the remote. Use a patch-id (`git cherry`) or equivalent check, and on any doubt leave the branch alone and log it.

**R2. The catch-up tolerates a pure rebase.** Independently, `PrepareCatchUp` treats \"local differs from remote only by being the same patches rebased onto a newer base\" (no local-only patch-ids) as safe. It catches up from the **remote** head, which is the head CI validated, and never pushes the local rebase. Any local-only patch still defers, as today. This covers worktrees already in this state, including ones from before R1 landed.

**R3. #2089 is untouched.** The zero-ahead push guard and its refusal remain as they are. Nothing here force-pushes a branch that is zero commits ahead.

## Scope

- In scope: `engine/comments.go` (the pre-invocation rebase for comment processing), `engine/worktree.go` / `engine/worktree_catchup.go` (`PrepareCatchUp`), unit tests with real git, a `tests/sim` scenario (review-reinvoke with no commit, then a lone Queued member behind main takes catch-up and the fast path, not a trial), `docs/state-machine.md` §6.30, and an ADR.
- Out of scope: the stage-dispatch rebase path, which pushes after the stage already; the trial path itself.

## Acceptance

1. A comment-processing invocation that makes no commit leaves `fabrik/issue-N` equal to `origin/fabrik/issue-N`, or not diverged from it, under R1's chosen mechanism.
2. If the invocation does make a commit, it is pushed as today.
3. A Queued singleton whose worktree holds a pure local rebase is caught up from the remote head and lands via the fast path. The log never says \"local member branch is ahead of or diverged\".
4. A worktree with a genuine local-only commit still defers the catch-up.
5. The tests are non-vacuous: neutralising R1 and R2 each fails the corresponding test."

## Background

Before a comment-processing Claude invocation (ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke`) on an item that has an open PR, the engine rebases the item's worktree onto the latest `origin/<base>`. When Claude then makes no commit, nothing is pushed, so the local issue branch stays a rebased copy of the remote branch: the same patches on a newer base, with different commit SHAs. Neither side carries work the other lacks, yet git sees them as diverged.

The merge train's singleton catch-up (#2044) treats a local branch that is ahead of or diverged from its remote as carrying unpushed work, and so as owned by someone else. It defers, uncharged. The member then takes the full trial path, costing a trial PR and roughly 8 minutes of CI, instead of the cheap catch-up plus singleton fast path. This was observed twice on 2026-10-11, on #2097 and #2096 (daemon at `3d0a73f`), each time after a Validate comment review that made no changes.

There is a second cost. A rebase that is never pushed leaves the worktree on a head nobody has CI for, so the `HEAD == head_sha` rule that lets test-running skills skip a redundant local run (#1997) refuses.

Two independent defects are therefore addressed: the engine creates a divergence it never resolves (R1), and the catch-up cannot tell a harmless pure rebase from genuine unpushed work (R2). R2 also covers worktrees already in the diverged state, including ones created before R1 ships.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A no-commit comment pass leaves no divergence (Priority: P1)

An operator or reviewer comments on an item with an open PR. The engine rebases onto the base branch and runs a comment-processing invocation, and Claude makes no commit (it resolved a thread, or answered a question). Afterwards the issue branch is not diverged from its remote because of the engine's own rebase.

**Why this priority**: It removes the cause. Every later consumer of the worktree (catch-up, `ci-status.md` skip rule, any further invocation) sees a head that matches what CI validated.

**Independent Test**: With real git, set up a worktree and remote with a rebased-onto-newer-base situation, run the post-invocation handling for a no-commit comment pass, and assert the local branch is equal to, or not diverged from, the remote branch.

**Acceptance Scenarios**:

1. **Given** an item with an open PR and a comment-processing invocation that makes no commit, **When** the invocation ends, **Then** `fabrik/issue-N` is equal to `origin/fabrik/issue-N`, or at least not diverged from it.
2. **Given** the same invocation makes a commit, **When** it ends, **Then** the commit is pushed as it is today.
3. **Given** a local commit that is not on the remote (a patch with no counterpart on the remote), **When** the post-invocation handling runs, **Then** that commit is never discarded.
4. **Given** the engine cannot establish with confidence that the only difference is its own rebase, **When** the post-invocation handling runs, **Then** the branch is left alone and the reason is logged.

---

### User Story 2 - Catch-up accepts a pure local rebase (Priority: P1)

A lone Queued merge-train member that is behind the base branch has a worktree whose local branch is a pure rebase of the remote branch (no local-only patches). Catch-up proceeds from the remote head, the head CI validated, and the member lands via the singleton fast path rather than a trial.

**Why this priority**: It saves a full trial per affected member and also repairs worktrees already in this state, which no change to the comment pass can reach.

**Independent Test**: With real git, build a worktree whose local branch is the remote's patches rebased onto a newer base, run the catch-up preparation, and assert it succeeds from the remote head and the local rebase is never pushed.

**Acceptance Scenarios**:

1. **Given** a local branch that differs from its remote only by being the same patches rebased onto a newer base, **When** the catch-up is prepared, **Then** it proceeds from the remote head and does not log "local member branch is ahead of or diverged".
2. **Given** that situation, **When** the catch-up completes, **Then** the local rebase is never pushed, and the pushed result is based on the remote head.
3. **Given** a local branch with at least one local-only patch, **When** the catch-up is prepared, **Then** it defers uncharged, as today.

---

### User Story 3 - End to end: review pass, then a fast-path landing (Priority: P2)

An item receives a review-reinvoke with no commit, then becomes a lone Queued member behind main. It takes catch-up and the singleton fast path, with no trial PR.

**Why this priority**: It proves the two fixes compose in the real flow and guards against regression of the observed incident.

**Independent Test**: A deterministic `tests/sim` scenario drives the sequence and asserts no trial was assembled.

**Acceptance Scenarios**:

1. **Given** a review-reinvoke with no commit followed by a lone Queued member behind main, **When** the merge train runs, **Then** the member is caught up and lands via the fast path, with no trial branch or trial PR created.

---

### Edge Cases

- The comment pass makes a commit and also the engine rebased: the commit is pushed as today; nothing is restored or discarded.
- The local branch has both a rebase of remote patches and one extra local-only patch: not a pure rebase; catch-up defers; R1 leaves the branch alone.
- The remote branch itself moved since the last fetch: the equality check must use fresh remote state, and on doubt the branch is left alone.
- The remote branch holds patches the local branch lacks (someone pushed to it): not a pure rebase of remote onto newer base in the sense that matters; behaviour for this case stays as today (defer) unless the local side carries no work, and it must never lose remote work.
- The item has no open PR, or the invocation is on the stage-dispatch path: unchanged.
- Empty-patch or merge-commit cases where patch-ids cannot be computed: treated as doubt; branch left alone and logged.
- A branch zero commits ahead of base with an open PR: the #2089 push guard still refuses and pauses the item; nothing here force-pushes such a branch.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a comment-processing invocation (ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke`) on an item with an open PR that made no commit, the issue branch is not left diverged from its remote by the engine's own pre-invocation rebase.
- **FR-002**: Whatever mechanism achieves FR-001 must never discard a commit that is not present on the remote, determined by a patch-id comparison (e.g. `git cherry`) or an equivalent check; on any doubt it leaves the branch untouched and logs why.
- **FR-003**: When a comment-processing invocation makes a commit, that commit is pushed exactly as it is today.
- **FR-004**: The mechanism for FR-001 is chosen during Plan from the candidate set (restore to remote tip; skip the pre-invocation rebase when a PR with a pending CI gate exists; push the rebase) and its rationale is recorded in an ADR. Pushing the rebase is rejected unless Plan shows that re-running CI and moving the recorded head is acceptable.
- **FR-005**: The singleton catch-up preparation treats a local branch that differs from its remote only by being the same patches rebased onto a newer base (no local-only patch-ids) as safe to proceed.
- **FR-006**: In that case the catch-up proceeds from the remote head (the head CI validated) and never pushes the local rebase.
- **FR-007**: A local branch carrying any local-only patch still defers the catch-up, uncharged, as today.
- **FR-008**: The pure-rebase tolerance in FR-005 applies independently of FR-001, so worktrees already in the diverged state are covered.
- **FR-009**: The zero-ahead push guard (#2089) and its refusal behaviour are unchanged; nothing introduced here force-pushes a branch that is zero commits ahead.
- **FR-010**: Real-git unit tests cover FR-001 to FR-003 and FR-005 to FR-007, and a `tests/sim` scenario covers a no-commit review-reinvoke followed by a lone Queued member behind main taking catch-up and the fast path rather than a trial.
- **FR-011**: The tests are non-vacuous: neutralising the FR-001 mechanism fails its test, and neutralising the FR-005 tolerance fails its test, independently of each other.
- **FR-012**: `docs/state-machine.md` §6.30 describes the new catch-up behaviour and the comment-pass post-invocation behaviour, and `docs/llms-full.txt` is regenerated in the same change.

### Key Entities *(if applicable)*

- **Pure rebase**: a local issue branch whose patches, compared by patch-id, are all present on the remote branch (and vice versa for the purposes of safety), differing only in base and commit SHAs.
- **Local-only patch**: a commit on the local branch whose patch-id has no counterpart on the remote branch; evidence of unpushed work.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a no-commit comment-processing invocation, the issue branch is equal to its remote branch or not diverged from it in 100% of tested cases.
- **SC-002**: A lone Queued member whose worktree holds a pure local rebase lands via catch-up and the singleton fast path with zero trial PRs created, and the operator log never contains "local member branch is ahead of or diverged" for it.
- **SC-003**: A worktree with a genuine local-only commit still causes the catch-up to defer in 100% of tested cases, and no test case results in a local-only commit being discarded.
- **SC-004**: Neutralising the comment-pass mechanism fails at least one test, and neutralising the catch-up tolerance fails at least one test.
- **SC-005**: The existing #2089 zero-ahead push-guard tests pass unmodified.

## Assumptions

- Patch-id comparison (`git cherry` or equivalent) is a sufficient test for "same patches" for the purposes of both R1 and R2; a conflict-resolved rebase that changes a patch's content is treated as having a local-only patch, which safely defers.
- The remote head is the head CI validated, so catching up from it is correct even when the local worktree differs.
- Comment processing on an item with an open PR is the only path that rebases without pushing; the stage-dispatch rebase pushes afterwards and is unaffected.
- The ADR is numbered after this issue (`adrs/2150-...`) per repository convention.

## Out of Scope *(optional)*

- The stage-dispatch rebase path, which already pushes after the stage.
- The merge-train trial path itself.
- Changing the #2089 zero-ahead push guard.
- Pushing the engine's rebase as a general policy (rejected unless Plan shows otherwise).

## Source References *(optional)*

- #2044 / ADR-2044: merge-train singleton catch-up; `ErrCatchUpLocalDiverged` in `engine/worktree_catchup.go`
- #2089 / ADR 2089: zero-ahead push guard
- #1997: `ci-status.md` and the `HEAD == head_sha` skip rule
- Incidents: #2097 (catch-up deferred 2026-10-11 14:50Z), #2096 (deferred 14:08Z, trial PR #2134)
