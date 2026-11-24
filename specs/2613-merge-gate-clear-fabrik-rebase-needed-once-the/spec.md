# Feature Specification: Clear `fabrik:rebase-needed` once the rebased head is mergeable, while CI is still pending

**Feature Branch**: `fabrik/issue-2613`
**Created**: 2026-11-24
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `fabrik:rebase-needed` stays on an item after its rebase has been pushed and the PR merges cleanly again, for as long as CI on the new head runs. On a repo with ~40-minute CI the item reads as "needs a rebase" for most of an hour, which looks stuck to anyone (or any supervising session) reading labels or `fabrik_status.attention.reasons`.
>
> Cause: in `checkMergeabilityGate` (`engine/merge_gate.go`) the label is removed only on `PRMergeReady` or `PRMergeBlocked`. On `PRMergeUnsettled` (CI pending) the gate claims the item and returns without touching it, although GitHub already reports `mergeable: true` (`mergeable_state: blocked`, from pending checks only) for the rebased head.
>
> Seen on 0.0.93 (liminisapp/liminis-context-graph): after a merge-train landing, two items rebased and pushed at ~17:23–17:27 UTC still showed `fabrik:rebase-needed` and the reason "the linked PR no longer merges cleanly; the engine is dispatching a rebase" at 17:38, with `pr.mergeable: "mergeable"`.
>
> ## Requirements
>
> - **R1.** Clear `fabrik:rebase-needed` as soon as the PR is known not to conflict on its current head: `mergeable == true`, or a computed `mergeable_state` other than `dirty`, even while the settle result is `PRMergeUnsettled` because checks are pending.
> - **R2.** An unknown or uncomputed mergeability (`mergeable == nil`, `mergeable_state == "unknown"`) does not clear it.
> - **R3.** Clearing the label changes nothing else: the gate still claims and holds the item until CI settles, rebase cycle accounting is unchanged, and no rebase is dispatched or cancelled by this change.
> - **R4.** While a rebase has been pushed and CI is pending, the attention reason says so (e.g. "rebase pushed; waiting for CI on the new head"), not that a rebase is being dispatched.
>
> ## Scope
>
> In: `engine/merge_gate.go`, the attention reason for this state (`internal/attention` / `engine/attention_input.go`), `docs/state-machine.md` (regenerate `docs/llms-full.txt`), an ADR numbered after this issue if the decision warrants one.
> Out: CI classification and merge-train behaviour.
>
> ## Acceptance
>
> - Test: an item with `fabrik:rebase-needed`, a PR reporting `mergeable: true` / `mergeable_state: blocked` and pending checks loses the label while still being held; with `mergeable: nil` it keeps the label.
> - Test: the attention reason for the pending-CI case matches R4.
> - Sim twin; the label test fails with R1 neutralised.
>
> **Report linkage:** this restates community report #2608. Mention #2608 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, not even inside a negation (safe: "mentioned for context: #2608"). The report is closed by hand once a release containing the fix ships.

## Background

`fabrik:rebase-needed` is applied when a PR with a CI-gated stage no longer merges cleanly onto its base. The engine then rebases and pushes. The label is supposed to disappear once the conflict is gone.

Today the merge gate removes it only when the PR's settle result is "ready" (everything green) or "blocked" (CI failed, no conflict). While checks are pending on the freshly pushed head, the settle result is "unsettled". The gate holds the item and leaves the label alone, even though GitHub already reports the PR as mergeable. GitHub's merge state is `blocked` only because required checks are still running.

On a repository with roughly 40-minute CI, an item that has already been rebased therefore reads as "needs a rebase" for most of an hour. The label and the attention reason ("the linked PR no longer merges cleanly; the engine is dispatching a rebase") are wrong during that window. Anyone reading labels or `fabrik_status`, human or supervising session, concludes the item is stuck or that a rebase is about to start.

This was observed on 0.0.93 in liminisapp/liminis-context-graph. After a merge-train landing, two items rebased and pushed around 17:23–17:27 UTC still showed the label and that reason at 17:38, while the PR reported `mergeable`.

This restates community report #2608 (mentioned for context: #2608). The report stays open until a release containing the fix ships and is closed by hand.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Label clears once the rebased head no longer conflicts (Priority: P1)

An operator or supervising session watches an item whose rebase has just been pushed. As soon as GitHub reports the PR as free of conflicts on the new head, `fabrik:rebase-needed` disappears, even though CI is still running.

**Why this priority**: This is the defect. The label is state, and it is wrong for most of the CI duration.

**Independent Test**: Seed an item carrying `fabrik:rebase-needed` at a CI-gated stage, with a PR reporting `mergeable: true` / `mergeable_state: blocked` and pending checks. After one poll the label is gone and the item is still held by the merge gate.

**Acceptance Scenarios**:

1. **Given** an item with `fabrik:rebase-needed` and a PR reporting `mergeable: true`, `mergeable_state: blocked`, with checks pending, **When** the merge gate evaluates it, **Then** the label is removed and the item is still claimed and held until CI settles.
2. **Given** the same item with a computed `mergeable_state` of `unstable` or `behind` (any computed state other than `dirty`) and checks pending, **When** the gate evaluates it, **Then** the label is removed.
3. **Given** the same item but `mergeable` is unknown (null) or `mergeable_state` is `unknown`, **When** the gate evaluates it, **Then** the label is kept.
4. **Given** a PR whose computed state is `dirty`, **When** the gate evaluates it, **Then** the label stays (or is applied) and a rebase is dispatched as today.

---

### User Story 2 - Attention reason describes the real state (Priority: P2)

A reader of `fabrik_status.attention.reasons` sees an accurate explanation for an item that has been rebased and is waiting for CI, rather than a claim that a rebase is being dispatched.

**Why this priority**: Reading the reason is how supervising sessions decide whether an item is stuck. Story 1 removes the main source of the wrong reason. The reason must also be right in the window before the label is cleared, and on any path where the label persists while a rebase has already been pushed.

**Independent Test**: Classify an item that has a pushed rebase and pending CI on the new head. The reason names "rebase pushed; waiting for CI on the new head" and not "dispatching a rebase".

**Acceptance Scenarios**:

1. **Given** an item that was rebased and pushed, whose PR is not conflicting and whose CI on the new head is pending, **When** its attention is classified, **Then** the reason says the rebase was pushed and the item is waiting for CI on the new head.
2. **Given** an item with a genuinely conflicting PR and no pushed rebase yet, **When** its attention is classified, **Then** the existing "engine is dispatching a rebase" reason is unchanged.

---

### Edge Cases

- Mergeability not yet computed after a push (`mergeable` null, `mergeable_state` `unknown`): the label stays until GitHub computes it; it is never cleared on absence of information.
- A PR that is conflicting again after the label was cleared: the label is re-applied and a rebase dispatched as today.
- The label is already absent: no write, no log churn, no error.
- A failed label removal is handled like the existing removal paths (logged, retried next poll) and never changes the gate's claim or any counter.
- Rebase cycle accounting (`fabrik:rebase-needed` cycle counts, `MaxRebaseCycles`) is identical before and after this change. A cycle already counted is not refunded and one not counted is not added.
- Items not at a CI-gated stage (`wait_for_ci` off) are unaffected.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: While the merge gate holds an item because CI is pending, it MUST remove `fabrik:rebase-needed` when the PR is known not to conflict on its current head, meaning `mergeable` is true or a computed `mergeable_state` is anything other than `dirty`.
- **FR-002**: The gate MUST NOT remove `fabrik:rebase-needed` when mergeability is unknown or not yet computed (`mergeable` null, or `mergeable_state` `unknown`).
- **FR-003**: Clearing the label under FR-001 MUST NOT change the gate's outcome. The item is still claimed and held until CI settles, no rebase is dispatched or cancelled, and no rebase cycle counter changes.
- **FR-004**: A confirmed conflict (`dirty`) MUST continue to apply the label and dispatch a rebase exactly as today. The existing clearing on "ready" and on "CI failed, no conflict" is unchanged.
- **FR-005**: For an item that has been rebased and pushed and is waiting for CI on the new head without a conflict, the attention reason MUST say that the rebase was pushed and the item is waiting for CI on the new head (for example "rebase pushed; waiting for CI on the new head"). It MUST NOT say a rebase is being dispatched.
- **FR-006**: The attention reason for a genuinely conflicting item awaiting a rebase, and for the rebase-after-landing case, MUST remain as it is today.
- **FR-007**: A sim twin scenario MUST reproduce the defect: a rebased item with pending CI and a mergeable PR keeps the label when the new clearing is neutralised, and loses it when it is active.
- **FR-008**: `docs/state-machine.md` MUST describe the new clearing rule and the new attention reason, and `docs/llms-full.txt` MUST be regenerated in the same change.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item with `fabrik:rebase-needed` whose PR reports `mergeable: true` with checks pending has the label removed within one poll, and stays held by the merge gate until CI settles.
- **SC-002**: Across the CI run of a rebased head, neither the label set nor `fabrik_status.attention.reasons` claims that a rebase is needed or being dispatched while GitHub reports the PR as conflict-free.
- **SC-003**: With mergeability unknown, the label is retained in 100% of tested cases (no clearing on missing information).
- **SC-004**: With the clearing neutralised, the new unit and sim tests fail. With it active they pass.
- **SC-005**: No change in rebase dispatch, rebase cycle counts, CI classification or merge-train behaviour in the existing test suites.

## Assumptions

- "Known not to conflict" is judged from the mergeability data the gate already receives for the PR's current head (no extra GitHub call is introduced by this issue).
- A computed `mergeable_state` other than `dirty` (for example `blocked`, `unstable`, `behind`, `clean`, `has_hooks`) means no conflict. `behind` is not a conflict, and the rebase-on-behind policy is not changed here.
- The attention reason in FR-005 can be derived from state the engine already records about a pushed rebase and pending CI. If no such state exists, introducing it is a planning decision and does not change the observable requirement.
- An ADR numbered after this issue is optional and decided at Plan; the behaviour change is small and amends the existing merge-gate description.

## Out of Scope

- CI classification (pending, failed, non-passing) and the CI gate.
- Merge-train behaviour, including train landing and the trial or catch-up rebases.
- Changing when a rebase is dispatched or how rebase cycles are counted or limited.
- Changing how `behind` PRs are handled.
- Closing the community report; it is closed by hand after a release ships.

## Source References

- Community report #2608 (mentioned for context: #2608)
- `docs/state-machine.md` merge gate and attention sections
