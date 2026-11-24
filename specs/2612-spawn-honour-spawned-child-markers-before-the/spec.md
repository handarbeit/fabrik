# Feature Specification: Honour spawned-child markers before the spawn servability guard

**Feature Branch**: `fabrik/issue-2612`
**Created**: 2026-11-24
**Status**: Specified
**Input**: User description:

> ## Problem
>
> When a Plan declares a cross-repo spawn block on a `repo:`-scoped instance that does not serve the target repo, `spawnChildren` (`engine/spawn.go`) correctly pauses with "Spawn target … is not served by this Fabrik instance". The operator then creates the child by hand in the other repo and links it as `blockedBy`. Once the child closes and the parent returns to Implement, the engine re-parses the immutable Plan comment, runs the same spawn and pauses again with the same error, for ever.
>
> Cause: the servability guard (`spawnTargetServedByThisInstance`, about line 805) runs **before** the resume-marker lookup (`parseSpawnChildLabels(item.Labels)`, about line 852). A `fabrik:spawned-child:<i>:<n>` marker therefore cannot help, and nothing tells the engine that a block's child already exists in a repo this instance does not serve.
>
> Seen on 0.0.93: totalslacker/WebBrain#548 paused 2026-11-17, child created by hand as totalslacker/SemanticHistory#283, re-paused 2026-11-18 after release.
>
> ## Requirements
>
> - **R1.** Read the `fabrik:spawned-child:<i>:<n>` markers before the servability guard. Require servability only for blocks that still need creating. A block with a marker is resolved in the block's own target repo (`blocks[i-1].Repo`), read-only; this instance never writes to that repo for it.
> - **R2.** A marked cross-repo child that resolves (open or closed) counts as created: no `CreateIssue`, no board add on this instance's board, no assignment. The `blockedBy` edge is ensured as today (live check first), only if this instance can write it; if it cannot, it is reported, not paused, when an edge already exists.
> - **R3.** A marker whose child cannot be resolved keeps today's "recorded child can no longer be resolved" pause.
> - **R4.** The "not served" pause comment names the supported manual path: create the child in the target repo, add `fabrik:spawned-child:<i>:<n>` to the parent, then remove `fabrik:paused`.
> - **R5.** Decide in Plan whether an existing `blockedBy` edge to an issue in the target repo whose title matches the block's `TITLE` is also accepted (same idea as `blockedByContainsChild`). Record the decision in the ADR.
>
> ## Scope
>
> In: `engine/spawn.go`, the pause comment text, `docs/state-machine.md` §6.7 (regenerate `docs/llms-full.txt`), an ADR numbered after this issue.
> Out: making a `repo:`-scoped instance create issues in repos it does not serve.
>
> ## Acceptance
>
> - Sim or engine test: a repo-scoped instance, a parent with a cross-repo spawn block, a `fabrik:spawned-child:1:N` label and a closed child in the target repo. The parent proceeds through Implement with no pause and no create-issue call.
> - Without the marker, today's "not served" pause is unchanged (non-vacuity: neutralising R1 makes the first test fail).
> - The pause comment test pins the R4 wording.
>
> **Report linkage:** this restates community report #2601. Mention #2601 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it, not even inside a negation (safe: "mentioned for context: #2601"). The report is closed by hand once a release containing the fix ships.

## Background

When a Plan declares a spawn block that targets a repository this Fabrik instance does not serve (a `repo:`-scoped instance whose repo differs from the target), the engine refuses to create the child and pauses the parent with a "Spawn target … is not served by this Fabrik instance" comment. This is correct: the instance has no board to register such a child on.

The supported recovery is manual: the operator creates the child in the target repository and links it to the parent as a blocker. That recovery does not work today. After the child closes and the parent returns to Implement, the engine re-reads the immutable Plan comment, runs the same spawn, hits the same servability check and pauses again, indefinitely. The engine already has a restart-surviving record that a block's child exists (the `fabrik:spawned-child:<blockIndex>:<childNumber>` marker on the parent), but the servability check runs before that record is consulted, so the marker cannot help. Nothing otherwise tells the engine that a block's child already exists in an unserved repo.

Observed on 0.0.93: totalslacker/WebBrain#548 paused on 2026-11-17, its child was created by hand as totalslacker/SemanticHistory#283, and it re-paused on 2026-11-18 after the child was released. Mentioned for context: community report #2601.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A hand-created cross-repo child resumes the parent (Priority: P1)

An operator running a `repo:`-scoped instance has a parent whose Plan declares a spawn block targeting another repo. The parent paused as "not served". The operator creates the child in the target repo, adds `fabrik:spawned-child:<i>:<n>` to the parent, links the child as a blocker, and removes `fabrik:paused`. After the child closes, the parent proceeds through Implement without pausing again.

**Why this priority**: This is the reported defect; without it the parent is permanently stuck and the only exit is abandoning the spawn.

**Independent Test**: Repo-scoped instance, a parent with a cross-repo spawn block, a `fabrik:spawned-child:1:N` label, and a closed child in the target repo. Run the spawn path and assert there is no pause and no issue-creation call.

**Acceptance Scenarios**:

1. **Given** a repo-scoped instance, a cross-repo spawn block, a `fabrik:spawned-child:1:N` marker and a resolvable closed child in the target repo, **When** the spawn runs on return to Implement, **Then** no pause is applied and no issue is created.
2. **Given** the same setup but the child is still open, **When** the spawn runs, **Then** the child counts as created, and the parent is held only by the existing blocker handling, not by a spawn pause.
3. **Given** the marker and a resolvable child, **When** the spawn runs, **Then** this instance makes no write to the target repo and does not add the child to its own board or assign it.

---

### User Story 2 - Unmarked unserved targets still pause, with clear guidance (Priority: P1)

An operator whose Plan declares a cross-repo block with no marker still gets today's "not served" pause, and the pause comment tells them exactly how to recover.

**Why this priority**: The guard must keep protecting against mis-registered children, and the comment is the only place the supported manual path is discoverable.

**Independent Test**: Same setup as Story 1 without the marker; assert the pause is unchanged and the comment contains the manual-path wording.

**Acceptance Scenarios**:

1. **Given** a cross-repo block with no marker on a repo-scoped instance, **When** the spawn runs, **Then** the parent pauses with the "not served" comment and no children are created.
2. **Given** that pause comment, **When** an operator reads it, **Then** it states: create the child in the target repo, add `fabrik:spawned-child:<i>:<n>` to the parent, then remove `fabrik:paused`.

---

### User Story 3 - A stale marker still fails loudly (Priority: P2)

If a marker names a child that can no longer be resolved, the operator is told, rather than the engine silently proceeding.

**Why this priority**: Preserves the existing safety behaviour; it is a regression guard rather than new value.

**Independent Test**: Marker pointing at a non-existent or inaccessible issue in the target repo; assert today's "recorded child can no longer be resolved" pause.

**Acceptance Scenarios**:

1. **Given** a marker whose child cannot be resolved in the block's target repo, **When** the spawn runs, **Then** the parent pauses with the existing unresolved-recorded-child message naming the marker to remove.

---

### Edge Cases

- A plan with several blocks, some served and some not: marked blocks are resolved read-only wherever they live; servability is required only for blocks that still need creating, so a block that needs creation in an unserved repo still pauses.
- A marked cross-repo child exists but the `blockedBy` edge is missing and this instance cannot write it: the situation is reported; whether it pauses follows the requirement below (an existing edge is required for the no-pause outcome).
- A marked child that is closed counts as created; it is not recreated.
- A marker whose block index is outside the Plan's current block list is not a valid resume record for any block.
- The marker is present on a served (same-repo) target: behaviour is unchanged from today.
- Behaviour on a multi-repo instance (no `repo:` scope), where every target is served, is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The spawn path MUST read the parent's `fabrik:spawned-child:<i>:<n>` markers before it applies the servability check, and MUST require that a target repo be served only for blocks that still need a child created.
- **FR-002**: A block that has a marker MUST be resolved in that block's own target repository, using reads only; this instance MUST NOT write to that repository on behalf of the block.
- **FR-003**: A marked child that resolves (open or closed) MUST count as already created: no issue creation, no addition to this instance's board, and no assignment for it.
- **FR-004**: For a resolved marked cross-repo child, the `blockedBy` edge MUST still be ensured as today (live check first), but only where this instance is able to write it; when it cannot write it and an edge already exists, the situation MUST be reported and MUST NOT cause a pause.
- **FR-005**: A marker whose child cannot be resolved MUST keep today's "recorded child can no longer be resolved" pause unchanged.
- **FR-006**: The "not served" pause comment MUST name the supported manual path: create the child in the target repo, add `fabrik:spawned-child:<i>:<n>` to the parent, then remove `fabrik:paused`.
- **FR-007**: The Plan stage MUST decide whether an existing `blockedBy` edge to an issue in the target repo whose title matches the block's `TITLE` is also accepted as evidence that the child exists (the same idea as the existing child-in-blockers check), and MUST record that decision in the ADR. This spec does not decide it; until Plan does, only marker-based resume is in scope.
- **FR-008**: A cross-repo block with no marker on an instance that does not serve the target MUST behave exactly as today (pause, no children created).
- **FR-009**: Tests MUST cover: (a) a repo-scoped instance with a cross-repo block, a `fabrik:spawned-child:1:N` marker and a closed child in the target repo proceeds with no pause and no issue-creation call; (b) the same setup without the marker keeps today's pause, and neutralising FR-001 makes test (a) fail; (c) the pause comment test pins the FR-006 wording.
- **FR-010**: The as-built description of spawn behaviour in `docs/state-machine.md` §6.7 MUST be updated, `docs/llms-full.txt` regenerated in the same change, and an ADR numbered after this issue added.

### Key Entities *(if applicable)*

- **Spawned-child marker**: a `fabrik:spawned-child:<blockIndex>:<childNumber>` label on the parent, recording by 1-based block index and issue number that a block's child already exists; the owner/repo is re-derived from the Plan block.
- **Spawn block**: one `FABRIK_SPAWN_CHILD` entry in the immutable Plan comment, carrying a target repo and a title.
- **Servability**: whether this instance's own `repo:`/board configuration covers a block's target repo.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A parent whose cross-repo child was created by hand and marked proceeds past the spawn step with zero pauses and zero issue-creation calls once the child is resolvable.
- **SC-002**: An unmarked cross-repo block on an unserving instance still pauses in 100% of cases, as before.
- **SC-003**: The "not served" pause comment contains the full manual recovery steps, verifiable by a test.
- **SC-004**: With the marker-first ordering neutralised, the resume test fails, demonstrating the test is not vacuous.
- **SC-005**: This instance performs no write against the unserved target repository during a marked-child resume.

## Assumptions

- The marker label scheme and its Plan/Implement-only scope are unchanged; this issue changes only the order in which the marker and the servability check are consulted and what a resolved cross-repo marked child permits.
- The instance's token can read an issue in a repo it does not serve; if it cannot, the child is unresolvable and the existing unresolved-child pause applies (FR-005).
- Blocks that need creation in an unserved repo continue to pause, even if other blocks in the same Plan resume from markers.
- Whether a matching-title `blockedBy` edge also counts as evidence (FR-007) is a design decision for the Plan stage, not a question for the requester.

## Out of Scope *(optional)*

- Making a `repo:`-scoped instance create issues in repositories it does not serve.
- Automatically creating the hand-made child or applying the marker for the operator.
- Marker handling for the Review/Validate mid-flight spawn origin, which deliberately does not use markers.
- Changes to how multi-repo instances serve cross-repo targets.

## Source References *(optional)*

- Community report #2601 (mentioned for context)
- `docs/state-machine.md` §6.7 and ADR 1583 (spawned-child markers), ADR 1419 (cross-repo spawn servability)
