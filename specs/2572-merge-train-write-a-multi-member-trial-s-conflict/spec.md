# Feature Specification: Merge train: write a multi-member trial's conflict resolution back to the member branch

**Feature Branch**: `fabrik/issue-2572`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description: "## Problem

When a merge-train trial has two or more members and one of them conflicts while the trial branch is assembled, Claude resolves the conflict on the disposable trial branch (`assembleAndValidate`, `engine/merge_train.go`). The resolution is never written to the member's own `fabrik/issue-N` branch. It survives only if that trial lands. If the trial is abandoned, bisected, rebuilt without a saved rerere resolution, or the member is ejected, the resolution is lost and the next trial (or the member's own PR) hits the same conflict again.

Watch item #2111 fired at the v0.0.92 cut, on a production repo with the merge train on. A two-member trial was rebuilt after its base moved. One member then conflicted with the new base. Claude resolved it, and rerere replayed the resolution on the next rebuild. The batch landed. The two members' PRs shared no files, so the #2047 overlap filter worked as designed. The conflict was between one member and the moved base, which the filter cannot prevent. It happened once since v0.0.84.

## Requirements

1. When a trial-assembly conflict for member M is resolved and the trial has (or will have) more than one member, the engine writes the resolution back to M's own issue branch: merge the pinned base into `fabrik/issue-M` with the same resolution, push with a lease on M's verified head (the singleton catch-up's `PushCatchUp` path, #2044, is the model), and continue building the trial from the updated member head.
2. The write-back obeys the same ownership checks as the singleton catch-up (no worker in flight, no `fabrik:editing`, clean idle worktree, remote not moved). If any check fails, the trial proceeds as today and the write-back is skipped, logged and uncharged.
3. A rejected push (lease, policy, transport) is never a trial failure. It is logged and the trial proceeds as today.
4. The written-back merge carries the same review-churn protection as the catch-up (the engine-authored marker comment, so a bot review of the pure merge commit is not treated as new feedback).
5. One log line per write-back names the member, the base SHA and the outcome.

## Scope

- In scope: trial-assembly conflicts in multi-member trials, in both the synchronous and asynchronous train drivers.
- Out of scope: singleton trials (the catch-up covers them), `landOneAtATime`, and bisection-time conflicts beyond reusing whatever the assembly path already does.

## Acceptance

- A sim twin: a two-member trial where one member conflicts with a moved base. After the trial is abandoned, the member's own branch contains the resolution, and a fresh trial assembles with no conflict and no Claude invocation.
- With the write-back neutralised by a seam, the same scenario re-invokes Claude for the conflict (non-vacuity).
- A failed ownership check or rejected push leaves today's behaviour unchanged.
- `docs/state-machine.md` and an ADR record the change.

Mentioned for context: #2111 (watch item), #1826 (the original proposal). In the PR body, mention these by number only. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) next to either number."

## Background

When a merge-train trial has two or more members and one member conflicts while the trial branch is assembled, Claude resolves the conflict on the disposable trial branch. The resolution is never written to the member's own `fabrik/issue-N` branch, so it survives only if that trial lands. If the trial is abandoned, bisected, rebuilt without a saved rerere resolution, or the member is ejected, the resolution is lost. The next trial, or the member's own PR, then hits the same conflict again and pays for another Claude resolution.

Watch item #2111 fired at the v0.0.92 cut on a production repo with the merge train on:

- A two-member trial was rebuilt after its base moved.
- One member then conflicted with the new base, and Claude resolved it.
- rerere replayed that resolution on the next rebuild, and the batch landed.
- The two members' PRs shared no files, so the overlap filter (#2047) worked as designed. The conflict was between one member and the moved base, which that filter cannot prevent.
- It has happened once since v0.0.84.

The singleton catch-up (#2044) already writes a base merge back to a member's own branch for one-member trials. This work extends the same idea to multi-member trials, where today the resolution stays on the throwaway branch. The original proposal is #1826.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A conflict resolution outlives an abandoned trial (Priority: P1)

An operator runs the merge train with several Queued members. During trial assembly one member conflicts with a base that has moved, and Claude resolves it. The trial is then abandoned, bisected or rebuilt. The resolution remains on that member's own branch, so the next trial assembles that member without a conflict and without invoking Claude.

**Why this priority**: This is the whole point of the change: avoiding repeated conflict resolution and the repeated cost and risk of it.

**Independent Test**: In the sim, build a two-member trial where one member conflicts with a moved base. Abandon the trial. Check that the member's branch holds the resolution, and that a fresh trial assembles with no conflict and no Claude invocation.

**Acceptance Scenarios**:

1. **Given** a two-member trial in which member M conflicts with the pinned base and Claude resolves it, **When** the trial is later abandoned, **Then** M's own branch contains the base merged with that resolution.
2. **Given** M's branch already carries the written-back resolution, **When** a fresh trial is assembled, **Then** M merges without a conflict and Claude is not invoked.
3. **Given** the write-back is disabled by its test seam, **When** the same scenario runs, **Then** the second trial hits the conflict again and Claude is invoked (non-vacuity).

---

### User Story 2 - Write-back never makes the trial worse (Priority: P1)

The write-back is an optimisation with safety conditions. When it cannot be done safely, or the push is refused, the trial behaves exactly as it does today.

**Why this priority**: A merge-train change that can fail trials or overwrite work on a member branch would be worse than the problem it solves.

**Independent Test**: Run the scenario of Story 1 with each ownership check failing in turn, and with the push rejected. In each case the trial proceeds as today, nothing is charged to the member, and one log line records the skip.

**Acceptance Scenarios**:

1. **Given** a worker is in flight for M, or M carries `fabrik:editing`, or M's worktree is dirty or not idle, or M's remote head has moved, **When** the conflict is resolved, **Then** no write-back happens, the trial continues from the trial-branch resolution, and the skip is logged and uncharged.
2. **Given** the push is rejected for a lease, policy or transport reason, **When** the write-back is attempted, **Then** the rejection is logged, the trial continues as today, and the rejection is not a trial failure and does not count toward any ejection or retry bound.

---

### User Story 3 - A written-back merge does not look like new review feedback (Priority: P2)

A bot reviewing the merge commit that was pushed to a member's branch must not cause the member to be treated as having fresh review feedback, as with the singleton catch-up.

**Why this priority**: Without this, each write-back could trigger a pointless review round on the member.

**Independent Test**: After a write-back, a bot review anchored on the written-back head is not counted as unprocessed feedback by the same rules that cover the catch-up.

**Acceptance Scenarios**:

1. **Given** a successful write-back, **When** the push completes, **Then** the engine has posted its own marker comment for that head, as for the catch-up.
2. **Given** that marker, **When** a bot's review is anchored on that exact head, **Then** it is not treated as new feedback by the feedback gates.

---

### User Story 4 - Operators can see what happened (Priority: P3)

Each write-back attempt produces one log line naming the member, the base SHA and the outcome (written back, skipped with the reason, or push rejected with the reason).

**Why this priority**: The behavior is otherwise invisible, and operators debugging the train need to see it.

**Independent Test**: Run each outcome and check for exactly one line with those three facts.

**Acceptance Scenarios**:

1. **Given** any write-back attempt, **When** it ends, **Then** exactly one log line names the member, the base SHA and the outcome.

---

### Edge Cases

- The trial shrinks to a single member after the conflict (an ejection or a bisection half): the write-back that was already made for the earlier multi-member assembly stays. A conflict first met in an already-singleton trial is the catch-up's, not this change's.
- The same member conflicts again on a later rebuild against a newer pinned base: the write-back runs again against the new base, subject to the same checks.
- Several members conflict in one assembly: each is written back independently; one member's skipped or rejected write-back does not affect the others.
- A conflict that Claude cannot resolve (the member is ejected today): nothing is written back.
- A restart between the resolution and the push: the existing restart handling of the train run applies; the write-back adds no new durable state that must survive a restart.
- The asynchronous driver evaluates the trial across polls, while the synchronous one blocks. The write-back happens at assembly time in both drivers and behaves the same in each.
- The merge-train overlap filter (#2047) does not prevent these conflicts, since they arise between a member and a moved base rather than between members.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a trial-assembly conflict for member M is resolved and the trial has, or will have, more than one member, the engine MUST write the resolution back to M's own `fabrik/issue-M` branch by merging the pinned base into it with the same resolution and pushing with a lease on M's verified head, following the singleton catch-up's push path (#2044).
- **FR-002**: After a successful write-back, the engine MUST continue building the trial from M's updated head.
- **FR-003**: The write-back MUST apply the same ownership checks as the singleton catch-up: no worker in flight for M, no `fabrik:editing` on M, a clean and idle worktree for M, and M's remote branch not moved since the engine verified it.
- **FR-004**: If any ownership check fails, the engine MUST skip the write-back, let the trial proceed exactly as it does today, log the skip and charge nothing (no ejection count, retry count, trial-rate window entry or pause).
- **FR-005**: A rejected push (lease, policy or transport) MUST NOT fail the trial. It MUST be logged, the trial MUST proceed as it does today, and nothing is charged.
- **FR-006**: A successful write-back MUST carry the same review-churn protection as the singleton catch-up: the engine posts its own marker comment for the pushed head, so a bot review of that merge commit is not treated as new feedback.
- **FR-007**: The engine MUST emit exactly one log line per write-back attempt naming the member, the base SHA and the outcome.
- **FR-008**: The behavior MUST apply to trial-assembly conflicts in both the synchronous and the asynchronous train drivers.
- **FR-009**: A test seam MUST allow the write-back to be disabled so tests can show the same scenario re-invokes Claude (non-vacuity).
- **FR-010**: Singleton trials, `landOneAtATime`, and bisection-time conflicts MUST keep their current behavior, except that bisection reuses whatever the assembly path already does.
- **FR-011**: `docs/state-machine.md` MUST describe the write-back, and an ADR MUST record the decision, in the same change set as the code. `docs/llms-full.txt` MUST be regenerated.

### Key Entities *(if applicable)*

- **Trial**: the disposable branch assembled from a batch of Queued members for validation.
- **Member branch**: a member's own `fabrik/issue-N` branch and PR, which exists independently of any trial.
- **Pinned base**: the base commit a trial or run was assembled against.
- **Write-back**: the merge of the pinned base, with its conflict resolution, into a member's own branch.
- **Catch-up marker comment**: the engine-authored comment that marks a pushed merge head so bot reviews of it are not counted as new feedback.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim scenario (two-member trial, one member conflicting with a moved base, trial then abandoned), the member's own branch contains the resolution, and a fresh trial assembles with zero conflicts and zero Claude invocations.
- **SC-002**: With the write-back disabled by its seam, the same scenario invokes Claude again for that conflict.
- **SC-003**: For each failed ownership check and for a rejected push, the trial outcome and all charged counters are identical to those without the feature, and one log line records the reason.
- **SC-004**: Every write-back attempt produces exactly one log line carrying the member, the base SHA and the outcome.
- **SC-005**: A bot review anchored on a written-back head adds no unprocessed feedback to the member.
- **SC-006**: Both train drivers pass the same scenario.

## Assumptions

- "Has (or will have) more than one member" is judged on the batch at the time the conflict is met, so a later shrink does not undo a write-back already made.
- The ownership checks, lease push and review-churn marker are the singleton catch-up's existing ones and are reused as they are, not redefined here.
- The written-back merge may include Claude's resolution of the conflict. What the marker records about the merge's content is defined by the catch-up's existing marker rules; the planning stage decides how to apply them to a resolved merge.
- The write-back adds no durable state of its own; an interrupted attempt is simply made again on the next assembly if it is still needed.
- Singleton trials keep their own catch-up path, and no change to that path is intended.

## Out of Scope *(optional)*

- Singleton trials (the catch-up covers them).
- `landOneAtATime`.
- Bisection-time conflicts beyond reusing whatever the assembly path already does.
- Preventing member-versus-moved-base conflicts in the first place (the overlap filter cannot, and this change does not try).

## Source References *(optional)*

- #2111 (watch item that fired at the v0.0.92 cut), mentioned for context.
- #1826 (the original proposal), mentioned for context.
- #2044 (singleton catch-up, the model for the push path, ownership checks and marker comment) and #2047 (overlap-aware batch composition).
