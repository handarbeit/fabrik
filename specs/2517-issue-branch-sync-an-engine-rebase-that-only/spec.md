# Feature Specification: Issue-branch sync tolerates an engine rebase that only shifts diff context

**Feature Branch**: `fabrik/issue-2517`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description: "An engine rebase that changes only a commit's diff context strands the issue branch: the engine keeps its own unpushed rebase, and the next stage pauses the item as \"issue branch diverged from origin\"."

## Background

Seen on #2498 on 2026-10-29. Timeline from the dev daemon log, in UTC:

1. **00:08:44**: Review completed and pushed `7cab71da` to `origin/fabrik/issue-2498`.
2. **00:08:57**: A Pruefer review body started a review pass. Before Claude ran, the engine rebased the branch onto `origin/main`, which had just moved (#2480 merged). The rebase was clean.
3. **00:09:16**: Claude made no commit. The restore step for an unpushed engine rebase (#2150, ADR 2150) declined to undo the rebase and logged *"the branch carries a commit that was not on the pre-rebase head"*. Nothing pushed the rebased branch.
4. **00:09:32**: Validate dispatched. The issue-branch sync (#2295, ADR 2295) classified local `e6866441` against remote `7cab71da` as diverged and paused the item as "issue branch diverged from origin".

The local branch was the remote's five commits rebased onto a newer main, with no other change. One of them, the ADR docs commit, edits `CLAUDE.md` next to lines #2480 had just changed. The rebase changed that commit's diff context, so its patch-id changed (feat commit: same on both sides; docs/ADR commit: `b19526b6…` on the remote, `0a3e49ed…` locally).

Both safeguards decide "does local hold anything the remote lacks?" by comparing commits by patch-id: the restore step for an unpushed engine rebase, and the "holds nothing beyond the remote" check shared by the issue-branch sync and the merge-train singleton catch-up preparation. A patch-id hashes the context lines, so a commit whose surrounding text moved on the base reads as a new patch. Both checks then fail closed. That is safe, but it pauses an item that holds nothing new.

This is more likely now than before: `CLAUDE.md`, `docs/state-machine.md` and `docs/llms-full.txt` are edited by nearly every PR, and several PRs regularly sit in Review at once, so the base moves under them.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A clean engine rebase with no new commit is undone (Priority: P1)

An operator has an item whose branch was pushed. A later comment-review or reinvoke pass makes the engine rebase the branch onto a newer base, and Claude makes no commit. The engine puts the branch back to the pushed head instead of keeping a local-only rebased copy, even when the rebase changed some commits' diff context.

**Why this priority**: This is the first point where the stranding happens; fixing it prevents the divergence from arising in the common case.

**Independent Test**: Build a branch whose commit edits lines adjacent to a later base change, push it, let the engine rebase it cleanly onto the moved base, run a pass that makes no commit, and check that the branch is back at the pre-rebase head.

**Acceptance Scenarios**:

1. **Given** a pushed branch with a commit whose diff context changes when rebased onto a moved base and a clean engine rebase, **When** a pass completes without any commit, **Then** the branch is restored to the pre-rebase head and one log line names the rule that matched.
2. **Given** the same setup but the rebase required conflict resolution, **When** the pass completes without a commit, **Then** the branch is left alone, as today.
3. **Given** the same setup but the pass made a commit, **When** the pass completes, **Then** no restore happens, as today.

---

### User Story 2 - A content-equivalent local branch is not divergence (Priority: P1)

An item's local branch is the remote's commits replayed onto a newer base, with no local-only content, but it is still left in that state (for example after a restart, or by a path the restore step does not cover). At the next stage dispatch, and when a merge-train singleton catch-up is prepared, the engine treats the branch as holding nothing the remote lacks, resets it to the remote and lets the normal rebase run again, instead of pausing the item.

**Why this priority**: This is the safety net behind Story 1 and the point where the item is actually paused; it must not depend on Story 1 having run.

**Independent Test**: On a separate setup, create a local branch that is the remote's commits replayed onto a newer base with a changed diff context, then dispatch the next stage and check that the item is not paused and the branch is reset to the remote.

**Acceptance Scenarios**:

1. **Given** local is the remote's commits replayed onto a newer base (diff context changed, no other change), **When** the issue-branch sync runs, **Then** it resets to the remote, logs one line naming the matched rule, and the item is not paused.
2. **Given** the same state during merge-train singleton catch-up preparation, **When** preparation runs, **Then** it accepts the branch and resets it to the remote head instead of deferring as diverged.
3. **Given** local holds a real local-only commit, a local-only merge commit, or content that differs from the replayed remote, **When** the sync runs, **Then** it pauses as diverged, with today's comment and log lines.

---

### User Story 3 - The #2498 sequence ends with no pause (Priority: P2)

The full sequence (stage push, base moves, comment pass with no commit, next stage dispatch) completes without a pause and without changing the remote head.

**Why this priority**: It proves the two fixes work together in the engine flow, and protects against regressions.

**Independent Test**: A sim scenario that plays the sequence and asserts no pause label, no divergence comment and an unchanged remote head.

**Acceptance Scenarios**:

1. **Given** the #2498 sequence in the sim, **When** the next stage dispatches, **Then** the item is not paused and `origin/fabrik/issue-N` is at the same SHA it had after the stage push.

---

### Edge Cases

- A rebase that needed conflict resolution is never treated as an unconditional replay; it is left alone (restore) or paused (sync).
- Local contains a local-only merge commit: still diverged.
- Local contains a real local-only commit on top of the replayed commits: still diverged.
- Local's tree differs from the tree obtained by replaying the remote's commits onto local's base: still diverged.
- The branch was rebased more than once onto successively newer bases: still equivalent if the rule holds.
- Any git error while deciding (missing object, failed fetch, failed replay): fail closed, as today.
- HEAD is no longer the post-rebase SHA the engine recorded, or the tree is dirty, or the pre-rebase head is not on the remote: no restore, as today.
- The remote moved after the rebase: handled by today's rules; no new behaviour.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a pass made no commit and the engine's own rebase completed without conflicts, the restore step resets the branch to the pre-rebase head in every case that is exactly "the same commits replayed onto a newer base", including commits whose diff context changed. It may rely on: HEAD is still the post-rebase SHA the engine recorded, the tree is clean, the pre-rebase head is on the remote, and the rebase needed no conflict resolution. It must not depend on patch-ids matching.
- **FR-002**: The issue-branch sync and the merge-train singleton catch-up preparation accept "local is the remote's commits replayed onto a newer base, with no local-only content". They resolve it as they resolve "local holds nothing the remote lacks" today: reset to the remote and let the normal rebase run again.
- **FR-003**: Equivalence in FR-002 is decided from content, not patch-ids. The accepted rule is: replaying the remote's commits onto local's base gives local's tree, and local has no local-only merge commit. Doubt, a git error, or any local-only content keeps today's diverged pause.
- **FR-004**: Neither change adds a push. Neither discards a commit, a conflict resolution, or any content the remote does not hold. The fail-closed behaviour of #2089, #2150 and #2295 on real divergence is unchanged.
- **FR-005**: Each new reset logs one line saying which rule matched. The existing pause comment and log lines for real divergence are unchanged.
- **FR-006**: Each fix has its own neutralisation seam, so a test can disable it and show the test fails without it.

### Key Entities *(if applicable)*

- **Engine rebase**: The rebase of the issue branch onto the latest base that the engine performs before a pass, which may leave a local branch ahead of or different from the remote when no commit follows.
- **Replay equivalence**: The property that local equals the remote's commits replayed onto local's base, with no local-only content.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the #2498 shape (clean engine rebase, changed diff context, no commit in the pass), the branch is restored and the next stage dispatches with no divergence pause.
- **SC-002**: In the same shape with the restore step bypassed, the sync resets to the remote and the item is not paused.
- **SC-003**: Every negative case (local-only commit, local-only merge, rebase with conflict resolution, git error) still pauses or declines exactly as before, with unchanged comment and log text.
- **SC-004**: No scenario in the unit or sim tests produces a push, a discarded local-only commit, or a lost conflict resolution as a result of the new rules.
- **SC-005**: Each new test fails when its fix is neutralised through its seam.
- **SC-006**: The sim twin of the #2498 sequence ends with no pause and the remote head unchanged.

## Assumptions

- "The engine's own rebase completed without conflicts" is a fact the engine already has or can record at the time of the rebase.
- Deciding replay equivalence may use extra git operations in the bare clone or worktree; the cost is acceptable because it only runs when today's patch-id check would already have concluded divergence.
- Where the merge-train singleton catch-up shares the "holds nothing beyond" check, it gets the new rule automatically; no change to the catch-up merge path itself is intended.
- The documentation (state-machine §4.4 and "Issue-branch sync and verified-lease push", `CLAUDE.md`) and the bundle `docs/llms-full.txt` are updated in the same change, with an ADR named after this issue number that amends ADRs 2150 and 2295.

## Out of Scope *(optional)*

- Changing when the engine rebases.
- Pushing engine rebases.
- The merge-train catch-up merge path, except that its preparation step shares the "holds nothing beyond" check.

## Source References *(optional)*

- #2498 (the observed incident), #2480 (the base change that moved the context)
- ADR 2150 (unpushed engine rebase restore), ADR 2295 (issue-branch sync and verified-lease push), ADR 2089 (zero-ahead push guard)
- `docs/state-machine.md` §4.4 and "Issue-branch sync and verified-lease push"
