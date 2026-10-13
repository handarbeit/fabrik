# Feature Specification: Refuse to force-push a zero-ahead issue branch while its PR is open

**Feature Branch**: `fabrik/issue-2089`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "worktree: refuse to force-push an issue branch that is zero commits ahead of base while its PR is open

## Problem

Report #2058: an existing worktree's local `fabrik/issue-N` branch is never synced from `origin/fabrik/issue-N`. `EnsureWorktree` (`engine/worktree.go:379-389`) calls `updateWorktreeFromMain`, which fetches only `origin <base>` (:851) and rebases onto it (:878). `adoptRemoteIssueBranch` runs only when the local branch is missing.

If someone pushes to the issue branch out-of-band (e.g. a human resolving a conflict), the stale local branch is rebased onto base and pushed by `PushBranch` (`worktree.go:473-478`) with a bare `--force-with-lease`.

That lease checks against the shared bare clone's `refs/remotes/origin/fabrik/issue-N`. Many paths refresh that ref for all branches: `ensureBareCloneAs` at start, `FetchOrigin` on every merge-train run, invalidation and auto-repair, and any full `git fetch` by a worker. After any of those, the lease protects nothing, and the stale branch overwrites the human's commits.

If the stale branch has no commits of its own, its head becomes the base tip, and GitHub closes the PR as having no diff. That is the reporter's incident.

The complete fix is to sync the issue branch from origin before work and use an explicit lease on the synced SHA (the logic `PrepareCatchUp`, `engine/worktree_catchup.go:35-95`, already has for the train path). That is a separate, larger change. **This issue is the cheap guard that stops the destructive outcome now.**

## Requirements

**R1. Zero-ahead push guard.** Before `PushBranch` force-pushes an issue branch with an open linked PR, if the branch has **zero commits ahead of its base** (`origin/<base>`, resolved as everywhere else, including the `base:` label), do not push. Log loudly, and pause the item (`fabrik:paused` + `fabrik:awaiting-input`) with a comment. The comment must say that the local branch carries no commits, that the remote branch was left untouched, and name the PR.

**R2.** The guard applies to every caller of `PushBranch` that can force-push an issue branch with an open PR: the post-stage push (`engine/item.go:~1617`, via `pushBranchUnlessQueued`) and the comment-path spec push from #2034 (`engine/comments.go:~893`). Plan enumerates every caller. A path that legitimately pushes a zero-ahead branch (if one exists) must be justified explicitly and exempted.

**R3.** The guard is a safety net, not a resync. It doesn't fetch the issue branch or fast-forward it; that's the follow-up.

## Scope

- In scope: the zero-ahead guard, its pause comment, tests, and the as-built docs (`docs/state-machine.md`, plus `docs/llms-full.txt` regeneration if a canonical page changes).
- Out of scope: syncing the issue branch from origin before work, an explicit SHA lease in `PushBranch`, and the worker skills' own push instructions (fabrik-review/fabrik-validate). All of that belongs to the follow-up.
- Mention #2058 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2058. This issue's own number gets `Closes`.

## Acceptance

- A unit test with real git: a worktree whose local branch equals base, an open PR, and the push is refused. The remote ref is unchanged and the item is paused with the comment.
- A unit test showing a normal ahead-of-base branch still pushes.
- The test is shown to be non-vacuous: it fails with the guard removed.
- A sim scenario (if the sim models the push) reproducing the reporter's stale-worktree shape."

## Background

Report #2058 describes a data-loss incident. A person pushed to an issue branch outside Fabrik, for example to resolve a merge conflict. Fabrik's local copy of that branch was never refreshed from the remote, because the existing-worktree path only updates from the base branch. The next time Fabrik pushed, it force-pushed its stale local branch. The force-push carries a lease, but the lease compares against a remote-tracking ref that many other engine paths refresh for all branches. After any of them runs, the lease no longer detects the out-of-band push, and the stale branch overwrites the person's commits.

The worst case is a stale local branch with no commits of its own. After the engine rebases it onto the base branch, its head is the base tip. Pushing it makes the remote issue branch identical to base. GitHub then sees no diff and closes the linked PR. That is what the reporter hit.

The complete fix has two parts. First, sync the issue branch from the remote before work begins. Second, push with an explicit lease on the synced commit. The merge-train catch-up path already does both. That fix is larger and is a separate follow-up. This issue is the narrow guard that blocks the destructive outcome in the meantime. It does not remove the underlying staleness: a stale branch that does carry its own commits can still overwrite out-of-band work until the follow-up lands.

Codebase check while specifying: the engine's push helper that skips pushes for queued items has more callers than the two named in the original request. The post-stage push and the comment-path spec push are named. There are further callers in item processing, PR creation and PR output posting. R2 (FR-002) already requires the Plan stage to enumerate them all.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A zero-ahead branch is never pushed over an open PR (Priority: P1)

An operator has an item whose PR is open. Someone pushed to the issue branch out-of-band. The engine's local copy of the branch is stale and carries no commits beyond base. When the engine is about to push, it refuses, leaves the remote branch exactly as it was, and pauses the item with a comment explaining what happened and naming the PR.

**Why this priority**: This is the destructive outcome from #2058: lost human commits and a PR closed for having no diff. Preventing it is the whole purpose of this issue.

**Independent Test**: Using real git, set up a worktree whose local issue branch equals the base tip, with a remote issue branch holding extra commits and an open linked PR. Trigger the push path. Verify that the remote ref is unchanged, the item carries `fabrik:paused` and `fabrik:awaiting-input`, and the comment says the local branch carries no commits, says the remote was left untouched, and names the PR.

**Acceptance Scenarios**:

1. **Given** an item with an open linked PR and a local issue branch with zero commits ahead of `origin/<base>`, **When** the engine attempts to push the branch, **Then** no push occurs, the remote issue branch is unchanged, and the item is paused with `fabrik:paused` and `fabrik:awaiting-input`.
2. **Given** the same state, **When** the guard fires, **Then** a log line is emitted at a visible level and one comment is posted that states the local branch carries no commits, states the remote branch was left untouched, and names the PR.
3. **Given** an item with a `base:<branch>` label and a local branch equal to `origin/<branch>` but ahead of the repository default branch, **When** the engine attempts to push, **Then** the guard measures against `origin/<branch>`, finds zero ahead, and refuses.

---

### User Story 2 - Normal pushes are unaffected (Priority: P1)

An operator's item has a local issue branch with real commits ahead of its base. Pushes continue exactly as before.

**Why this priority**: A guard that blocks ordinary work would break the pipeline. Regression-free behavior is a hard requirement.

**Independent Test**: Using real git, set up a worktree whose local issue branch has at least one commit ahead of base and an open PR. Trigger the push path and verify the push succeeds.

**Acceptance Scenarios**:

1. **Given** an open linked PR and a local issue branch with one or more commits ahead of `origin/<base>`, **When** the engine pushes, **Then** the push proceeds as it does today.
2. **Given** a base branch that has moved ahead of an otherwise unchanged issue branch (the branch is behind base but still carries its own commits), **When** the engine pushes, **Then** the guard does not fire.

---

### User Story 3 - Every force-push path is covered, or exempted with a reason (Priority: P2)

A maintainer reading the as-built docs can see which push paths the guard covers. Any path that deliberately pushes a zero-ahead branch is exempted explicitly, with the reason recorded.

**Why this priority**: A guard on only one call site leaves the same data-loss window open elsewhere. It matters, but it builds on the core guard.

**Independent Test**: Enumerate every non-test caller of the push helper. Each is either covered by the guard, with a test or a documented reason it reaches the guarded code, or listed as exempt with a justification.

**Acceptance Scenarios**:

1. **Given** the post-stage push and the comment-path spec push, **When** each is reached with a zero-ahead branch and an open PR, **Then** each refuses and pauses.
2. **Given** a caller that cannot meet the guard's conditions (for example, a push that happens before any PR exists), **When** the Plan stage reviews it, **Then** it is documented as out of the guard's reach or exempted, with the reason.

---

### Edge Cases

- **No open linked PR.** The guard does not apply, since it is conditioned on an open linked PR. A push before the first PR exists is not blocked.
- **Closed or merged PR.** Not an open PR, so the guard does not apply.
- **Base ref cannot be resolved, or the ahead-count cannot be computed.** The failure mode needs a stated choice. The default is not to push on an unreadable result and to surface it, rather than silently pushing. The Plan stage confirms the choice against how comparable guards in the engine behave.
- **Item already paused.** The guard must not post a duplicate comment or stack a second pause on every poll. One comment per episode.
- **Queued (merge-train) items.** The existing helper already skips pushes for queued items. The guard must not change that behavior.
- **Branch has commits that are all empty or spec-only.** Counted as commits ahead, same as any other, unless existing "empty coordinator" logic says otherwise. The Plan stage checks this against `commitsAheadOfBase`.
- **Resuming after the pause.** A human comment resumes the item through the normal resume path. The guard may fire again if the state is unchanged. That is acceptable, because the human is expected to fix the branch first.
- **Out-of-band push to a branch that is not zero-ahead.** Still unprotected. This is the follow-up's scope (R3).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Before force-pushing an issue branch that has an open linked PR, the engine MUST determine how many commits the local issue branch is ahead of its base. The base is `origin/<base>`, resolved the same way as everywhere else in the engine, including the `base:<branch>` label. If the count is zero, the engine MUST NOT push.
- **FR-002**: The guard MUST apply to every caller of the push helper that can force-push an issue branch with an open linked PR. At minimum this includes the post-stage push and the comment-path spec push. The Plan stage MUST enumerate every non-test caller. Each is either covered by the guard or explicitly exempted with a documented justification.
- **FR-003**: When the guard refuses a push, the engine MUST write a log line that makes the refusal visible, and MUST pause the item by applying `fabrik:paused` and `fabrik:awaiting-input`.
- **FR-004**: When the guard refuses a push, the engine MUST post a comment that (a) states the local branch carries no commits, (b) states the remote branch was left untouched, and (c) names the open PR.
- **FR-005**: When the guard refuses a push, the remote issue branch ref MUST be left unchanged.
- **FR-006**: The guard MUST NOT fetch the issue branch from the remote, fast-forward or reset the local branch, or otherwise attempt to repair the staleness. It is a refusal, not a resync.
- **FR-007**: A local issue branch with one or more commits ahead of its base MUST push exactly as it does today.
- **FR-008**: The guard MUST NOT apply when the item has no open linked PR.
- **FR-009**: The pause MUST follow the engine's existing pause conventions, so that a later human comment resumes the item. The guard's comment MUST be posted once per pause episode, not on every poll.
- **FR-010**: The as-built documentation (`docs/state-machine.md`) MUST describe the guard, its trigger conditions, the covered and exempt callers, and its relationship to the follow-up resync. If a canonical doc page changes, `docs/llms-full.txt` MUST be regenerated in the same change.

### Key Entities *(if applicable)*

- **Issue branch**: `fabrik/issue-N`, the per-issue branch carrying the work and backing the PR.
- **Base branch**: the branch the issue branch is measured against, normally the repository default, or the `base:<branch>` label's value.
- **Zero-ahead branch**: an issue branch whose head has no commits that are not already in `origin/<base>`.
- **Linked open PR**: the open pull request whose head is the issue branch.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reproduction of the #2058 shape (stale zero-ahead local branch, remote branch holding extra commits, open PR), the remote branch ref is byte-for-byte identical before and after the engine's push attempt, and the PR stays open.
- **SC-002**: In that reproduction, the item is paused and exactly one comment is posted. It states that the local branch carries no commits, that the remote was left untouched, and names the PR.
- **SC-003**: A branch with at least one commit ahead of base pushes successfully in the same test harness, which shows the guard does not block normal work.
- **SC-004**: The refusal test fails when the guard is removed (it is shown non-vacuous).
- **SC-005**: Every non-test caller of the push helper is accounted for as covered or exempt in the as-built docs or the plan.
- **SC-006**: The existing unit, sim and wire-contract suites pass unchanged, and a sim scenario reproducing the stale-worktree shape exists if the sim models the push.

## Assumptions

- "Open linked PR" means the PR the engine already discovers for the item through its usual linkage. This issue does not add a new discovery mechanism.
- A push with no open PR (for example, the very first push before PR creation) is not blocked. There is no PR to be closed by the overwrite, and the branch is expected to have commits by then.
- Unreadable state (base ref missing, ahead-count error) leans toward refusing and surfacing rather than pushing silently. The Plan stage confirms this against comparable guards.
- The pause uses the same pause/awaiting-input pair and resume mechanics as other engine pauses. No new label is introduced.
- The follow-up (sync from origin plus explicit SHA lease) is tracked separately. This guard is expected to become redundant, or to be subsumed, once that lands.
- Fabrik itself does not close the PR. The overwrite makes the branch identical to base, and GitHub's no-diff behavior closes it. The guard prevents the push that triggers that.

## Out of Scope *(optional)*

- Syncing the local issue branch from `origin/fabrik/issue-N` before work begins.
- An explicit SHA lease (`--force-with-lease=<ref>:<sha>`) in the push helper.
- Changes to the worker skills' own push instructions (fabrik-review, fabrik-validate).
- Protecting stale branches that do carry their own commits from overwriting out-of-band work. That requires the follow-up.
- Any automatic repair of the local branch after the guard fires.
- A new label for this pause.

## Source References *(optional)*

- Report #2058 (mention by bare number only; do not use a closing keyword near it).
- Issue #2034, the comment-path spec push (`persistSpec`), named in R2.
- Merge-train singleton catch-up (`PrepareCatchUp`, #2044), the existing fetch-and-explicit-lease logic that the follow-up will generalize.
- `docs/state-machine.md`, the as-built spec to update.
