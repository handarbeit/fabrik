# Feature Specification: Don't adopt an already-landed origin issue branch — fork fresh from base

**Feature Branch**: `fabrik/issue-1917`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "worktree: don't adopt an already-landed origin issue branch (merged tip or merged PR head) — fork fresh from base. `EnsureWorktree` (`engine/worktree.go`) creates a missing local issue branch from `origin/fabrik/issue-N` whenever that branch exists on origin (`adoptRemoteIssueBranch`, #1916). The check is existence only. That is right for the case #1916 fixed: a branch with unmerged work whose local copy was lost. It is wrong when the origin branch is already landed. For example: `.fabrik/repos` was rebuilt, or a clone was freshly made; the issue's PR had already merged, and GitHub's head-branch auto-delete is off, which is the default. `fabrik:revalidate`, a reopen, or a landing-verification failure that sends the item back to Validate then adopts the landed branch instead of forking fresh from base. Every later stage works on merged history, producing phantom diffs or conflicts against content that is already on base. The same can happen when the item's `base:<branch>` label changed between attempts: the adopted branch was forked from the old base. This is narrower than the pre-#1916 failure (a worktree that could never push), but it is silent, and it puts wrong work on a real PR."

## Background

When an issue's local branch is missing, the engine creates it from the same-named branch on origin if one exists (the fix from #1916, which stopped worktrees from being unable to push when a branch with unmerged work already existed remotely). That check only asks whether the origin branch exists, not whether its contents are still live work.

If the origin branch has already landed on its base, the engine still adopts it. This happens, for example, after the managed repository clones are rebuilt or freshly made, when the issue's PR has merged and the repository does not auto-delete merged head branches (the GitHub default). When `fabrik:revalidate`, a reopen, or a landing-verification failure then sends the item back to a stage, the stage starts from merged history. The result is phantom diffs or conflicts against content already on base, and wrong work lands on a real PR without any signal. A changed `base:<branch>` label between attempts has a related effect: the adopted branch was forked from the old base.

This is narrower than the pre-#1916 failure, but it is silent. The goal is to keep #1916's protection for branches with unmerged work while refusing to adopt branches whose work is already on base.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A branch fully merged into base is not adopted (Priority: P1)

An operator sends a finished item back to a stage after its issue branch was merged with a merge commit and left on origin. The local branch is missing. The engine sees that every commit on the origin branch is already on base and starts the new local branch from the current base instead.

**Why this priority**: This is the most common landed-branch case and can be detected from git history alone, with no outside information. It is lossless, because everything on the branch is already on base.

**Independent Test**: In a real-git fixture, create an origin issue branch, merge it into base with a merge commit, remove the local branch, call `EnsureWorktree`, and check that the new local branch starts at `origin/<base>` and that one log line explains why the origin branch was skipped.

**Acceptance Scenarios**:

1. **Given** an origin issue branch whose tip is an ancestor of `origin/<base>` and no local branch, **When** the worktree is ensured, **Then** the local branch is created from `origin/<base>`, not from the origin issue branch.
2. **Given** the same state, **When** the worktree is ensured, **Then** exactly one log line states that the origin branch was not adopted because it is already landed on base.

---

### User Story 2 - The head of a merged PR is not adopted, even after squash or rebase merges (Priority: P1)

An item's PR was squash- or rebase-merged, so the origin branch tip is not an ancestor of base even though its content has landed. The caller already knows the merged PR's head commit. The engine compares that commit to the origin branch tip; when they are equal, it starts fresh from base.

**Why this priority**: Squash and rebase merges are common, and git ancestry alone cannot see them as landed. Without this, the problem remains for those merge strategies.

**Independent Test**: In a real-git fixture, create an origin issue branch with commits, squash it into base so the tip is not an ancestor, remove the local branch, call `EnsureWorktree` supplying a merged-PR head SHA equal to the tip, and check the new local branch starts at `origin/<base>`.

**Acceptance Scenarios**:

1. **Given** an origin branch whose tip is not an ancestor of base and a caller-supplied merged-PR head SHA equal to that tip, **When** the worktree is ensured, **Then** the origin branch is not adopted, the local branch starts at `origin/<base>`, and one log line explains why.
2. **Given** the same branch but a caller-supplied merged-PR head SHA that differs from the tip (work was pushed after the merge), **When** the worktree is ensured, **Then** the branch is adopted as today.
3. **Given** no merged-PR information is available to the caller, **When** the worktree is ensured, **Then** only the ancestry rule applies.
4. **Given** the item's linked PR is not merged but its `fabrik:credited-pr:<N>` PR is merged with a head equal to the tip, **When** the worktree is ensured, **Then** the branch is not adopted.

---

### User Story 3 - Branches with unmerged work are still adopted (Priority: P1)

A branch on origin holds commits that are not on base, and no merged PR has it as head. This is the case #1916 fixed. The engine continues to adopt it so the worktree can push without a stale-lease loop.

**Why this priority**: This is a regression guard. Forking fresh from base on doubt would bring back the stale-lease failure and could lose unmerged work.

**Independent Test**: With an origin branch that has commits not on base and no merged-PR information, ensure the worktree and check that the local branch starts at the origin branch tip. Also inject a failure into the ancestry check and check the branch is still adopted.

**Acceptance Scenarios**:

1. **Given** an origin branch with commits not on base and no merged PR at its tip, **When** the worktree is ensured, **Then** the branch is adopted exactly as it is today.
2. **Given** the ancestry check fails with a git error, **When** the worktree is ensured, **Then** the branch is adopted exactly as it is today.

---

### User Story 4 - A base mismatch on adoption is reported loudly (Priority: P2)

An item's `base:<branch>` label changed between attempts, or the origin branch has an open PR targeting a different base than the one the item now resolves to. The engine still adopts the branch, since refusing could lose unmerged work, but it logs a warning naming both bases so an operator can see why the stage may need to rebase.

**Why this priority**: It turns a silent condition into a visible one, but it changes no behavior, so it ranks below the correctness fixes.

**Independent Test**: Adopt a branch whose open PR targets a base other than the item's resolved base, and check that the warning log names both bases and the branch is still adopted.

**Acceptance Scenarios**:

1. **Given** an adopted branch whose open PR targets base A while the item resolves to base B, **When** the worktree is ensured, **Then** the branch is adopted and a warning is logged naming both A and B.
2. **Given** an adopted branch whose open PR targets the item's resolved base, **When** the worktree is ensured, **Then** no base-mismatch warning is logged.

---

### Edge Cases

- The origin branch tip equals base exactly (zero commits ahead): it is an ancestor, so it is not adopted and the new branch starts at base.
- A merged-PR head SHA is supplied but the origin branch has since moved on: the SHAs differ, so the branch is adopted as unmerged work.
- Merged-PR information is unknown or unavailable to the caller: fall back to the ancestry rule alone, with no error.
- The item has both a linked PR and a `fabrik:credited-pr:<N>` PR (merge-train items): either one being merged with head equal to the tip is enough to skip adoption.
- The ancestry check cannot run (git error, base ref missing, unresolvable ref): adopt as today.
- The `base:<branch>` label changed and the old origin branch is fully landed on the old base but not on the new one: the ancestry rule is judged against the item's currently resolved base, so a branch not contained in it is still adopted (with the base-mismatch warning only when an open PR targets a different base).
- The origin branch does not exist: unchanged, fork from base as before.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When creating a missing local issue branch, after fetching the origin issue branch, the worktree manager MUST NOT adopt it if its tip is an ancestor of `origin/<resolved base>`; it MUST fork the new branch from `origin/<resolved base>` as it did before #1916.
- **FR-002**: When FR-001 skips adoption, the manager MUST log exactly one line stating the reason.
- **FR-003**: The manager MUST NOT adopt the origin issue branch when the caller supplies the head SHA of a merged PR (the item's linked PR, or the PR named by its `fabrik:credited-pr:<N>` label) that equals the origin branch tip; it MUST fork from `origin/<resolved base>` and log one line stating the reason.
- **FR-004**: `EnsureWorktree`'s callers MUST supply any already-known merged-PR head SHA to the worktree manager; the worktree manager MUST NOT make any new GitHub call of its own.
- **FR-005**: When merged-PR information is unknown or not supplied, the manager MUST apply FR-001 only.
- **FR-006**: When adopting an origin branch whose open PR targets a base other than the item's resolved base, the manager MUST log a warning naming both bases, and MUST still adopt the branch.
- **FR-007**: An origin branch carrying commits not on base, with no supplied merged-PR head equal to its tip, MUST still be adopted, preserving #1916's behavior.
- **FR-008**: Any git error during the ancestry check (FR-001) MUST result in adopting the branch as today, never in forking from base.
- **FR-009**: The new rule MUST be documented in the worktree section of `docs/stage-lifecycle.md`, and in `docs/state-machine.md` if that document describes adoption; `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-010**: Each behavior above MUST be covered by real-git tests guarded by `skipIfNoGit`, each shown non-vacuous by neutralising the corresponding new check: a merge-commit-merged branch is not adopted and the new branch starts at `origin/<base>`; a squash-merged branch with a merged PR head equal to the tip is not adopted; a branch with unmerged commits is still adopted; a git error in the ancestry check still adopts; a base-mismatch adoption logs the warning.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After an item whose branch has already landed on base is sent back to a stage with no local branch, the new local branch starts at the current base tip in 100% of the merge-commit and merged-PR-head cases covered by tests.
- **SC-002**: A branch with unmerged commits is adopted in 100% of the tested cases, so #1916's scenario shows no regression.
- **SC-003**: Every injected ancestry-check failure in tests results in adoption, never in a fresh fork.
- **SC-004**: Each skip-adoption decision and each base-mismatch adoption produces exactly one operator-visible log line (warning for the mismatch), so an operator can find why a branch was or was not adopted from the log alone.
- **SC-005**: Each new test fails when its corresponding new check is neutralised, demonstrating the tests are not vacuous.
- **SC-006**: The repository's documentation drift check passes, with `docs/llms-full.txt` matching a fresh regeneration.

## Assumptions

- "Resolved base" means the item's base branch as the engine already computes it (the repository default, or the `base:<branch>` label when present); this issue adds no new base resolution.
- A merged PR's head SHA is already available to callers in the places that know about the item's linked PR and `fabrik:credited-pr:<N>` label; where it is not readily available, callers pass "unknown" and only the ancestry rule applies.
- The ancestry check is judged against the freshly fetched `origin/<resolved base>`, so a stale base ref is not a reason to skip adoption (a failed or inconclusive check adopts).
- The base-mismatch warning needs the open PR's base, which callers supply from information they already hold; when it is unknown, no warning is logged.
- Rebasing an adopted branch onto a changed base remains the stage's job; this issue only makes the condition visible.

## Out of Scope

- Rebasing or otherwise repairing an adopted branch whose base changed.
- Deleting landed origin branches, or enabling GitHub's head-branch auto-delete.
- Making new GitHub API calls from the worktree manager.
- Changing how a local branch that already exists is handled.
- Detecting landed work by content comparison (for example patch-id matching) when no merged-PR head is supplied.

## Source References

- #1916 — the original fix that introduced adopting an existing origin issue branch.
- `docs/stage-lifecycle.md` (worktree section) and `docs/state-machine.md` — the as-built docs to update.
