# Feature Specification: Same-repo spawned children inherit the parent's `base:<branch>`, applied before Status placement

**Feature Branch**: `fabrik/issue-2090`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "## Problem

Report #2057: when a parent carrying `base:<branch>` spawns children in the **same repo**, the children don't inherit the label. `spawnChildren` copies only `fabrik:yolo` and `fabrik:cruise` (`engine/spawn.go:908-918`). `baseBranchForItem` (`engine/item.go:2446`) reads only the child's own labels, so the child forks from, rebases onto and opens its PR against the repository default.

Under yolo, a child slice can therefore land on `main` while the parent targets `develop`. This is deterministic for every same-repo spawn from a non-default-base parent, from Plan as well as from mid-flight Review/Validate spawns (ADR-1419, same `spawnChildren`). `docs/state-machine.md` (~:2056) documents the non-inheritance as current behaviour.

There is a related ordering race: the autonomy labels are added *after* the child's Status placement (`spawn.go:896` before `:908`). Any inherited label added at that point can lose to the child's first dispatch, and `base:` must be set before the first `EnsureWorktree`.

## Requirements

**R1.** For a child in the same `owner/repo` as the parent, apply the parent's `base:<branch>` label to the child. If the parent has several, apply the one the parent itself resolves to.

**R2.** Apply the inherited `base:` label, and move the existing `fabrik:yolo`/`fabrik:cruise` inheritance, **before** the child's board Status placement, so no dispatch can observe the child without them.

**R3.** Cross-repo children don't inherit `base:`. The branch may not exist in the other repo, and today's fallback-with-comment behaviour stays.

**R4.** The resume path (`fabrik:spawned-child:<i>:<n>`, ADR-1583) applies the same inheritance idempotently to an already-created child that lacks it.

**R5.** Update `docs/state-machine.md`'s spawn section and regenerate `docs/llms-full.txt`.

## Scope

- In scope: label inheritance and ordering in `spawnChildren`, tests, docs.
- Out of scope: a per-child `BASE:` header in the spawn block grammar.
- Mention #2057 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2057. This issue's own number gets `Closes`.

## Acceptance

- Unit tests: a same-repo spawn from a `base:develop` parent gives every child `base:develop` before its Status is set. A cross-repo child gets none. A resumed child missing the label gets it.
- A sim twin: a `base:` parent spawns, and the child's worktree and PR target the parent's base."

## Background

Report #2057 found that a parent carrying `base:<branch>` (for example `base:develop`) that spawns children in the same repository produces children that ignore that base. Today the spawn step copies only the autonomy labels `fabrik:yolo` and `fabrik:cruise` to a child. A child's base branch is read only from the child's own labels, so with no `base:` label it forks from, rebases onto and opens its PR against the repository default branch.

The consequence is serious under autonomous modes. A child slice of a `develop`-targeted parent can be auto-merged onto `main`. This happens for every same-repo spawn from a non-default-base parent, whichever stage declared the spawn: Plan, or a mid-flight Review/Validate spawn (ADR-1419). Both use the same spawn routine. The state-machine documentation currently records the non-inheritance as intended behaviour, so it must change with the code.

There is also an ordering hazard. The existing autonomy-label inheritance runs after the child has been placed in its board Status column. Once a child is in a dispatchable column, the next poll can start its first stage. A `base:` label added after placement can lose that race, and the base branch has to be known before the child's worktree is first created. Any inheritance must therefore complete before the child becomes dispatchable.

Report #2057 is the community-facing thread. It stays open and is closed by hand after a release ships the fix.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Same-repo children target the parent's base (Priority: P1)

An operator runs a parent issue labelled `base:develop`. Plan (or a later Review/Validate stage) spawns child issues in the same repository. Each child must be created on `develop`, with its PR targeting `develop`, with no manual labelling.

**Why this priority**: This is the defect. Without it, autonomous children can merge onto the wrong branch.

**Independent Test**: Spawn two children in the same repo from a `base:develop` parent. Verify each child carries `base:develop` and that its worktree and PR use `develop`.

**Acceptance Scenarios**:

1. **Given** a parent with `base:develop` and a spawn block naming the same repository, **When** the spawn runs, **Then** every created child has the `base:develop` label.
2. **Given** such a child, **When** it is first dispatched, **Then** its worktree is forked from `develop` and its PR targets `develop`.
3. **Given** a parent with no `base:` label, **When** it spawns same-repo children, **Then** no `base:` label is added to them.
4. **Given** a parent with several `base:` labels, **When** it spawns, **Then** the children receive the one label the parent's own base resolution picks.

---

### User Story 2 - Inherited labels are in place before the child can be dispatched (Priority: P1)

Inherited labels (`base:`, `fabrik:yolo`, `fabrik:cruise`) must be on the child before its board Status is set, so that no poll cycle can see a dispatchable child that lacks them.

**Why this priority**: A label applied after placement can be missed by the first dispatch. For `base:`, that means the worktree is created against the wrong branch.

**Independent Test**: Record the order of label and Status writes during a spawn. Every inherited label write precedes the Status placement write for that child.

**Acceptance Scenarios**:

1. **Given** a parent with `base:develop` and `fabrik:yolo`, **When** a child is spawned, **Then** both labels are applied before the child's Status placement.
2. **Given** a failure to apply an inherited autonomy label, **When** the spawn continues, **Then** the failure is logged and does not abort the spawn (today's non-fatal behaviour for autonomy labels is preserved).

---

### User Story 3 - Cross-repo children are unaffected (Priority: P2)

A child created in a different repository from the parent must not receive the parent's `base:` label, because that branch may not exist there. Today's behaviour for a missing or unspecified base is unchanged.

**Why this priority**: Prevents the fix from introducing a new failure mode, where children in other repos fall back to a non-existent branch.

**Independent Test**: Spawn one same-repo child and one cross-repo child from a `base:develop` parent. Only the same-repo child gets `base:develop`.

**Acceptance Scenarios**:

1. **Given** a parent with `base:develop` and a spawn block naming a different `owner/repo`, **When** the spawn runs, **Then** that child receives no `base:` label.
2. **Given** that cross-repo child, **When** it runs, **Then** it resolves its base as it does today (repository default).

---

### User Story 4 - A resumed spawn repairs missing inheritance (Priority: P2)

If a spawn is interrupted and resumed through the `fabrik:spawned-child:<i>:<n>` marker, a child that already exists but lacks the inherited labels is brought into line, without duplicating labels or children.

**Why this priority**: Spawns can be interrupted. A child created by an earlier run, possibly before this fix, must not keep the wrong base after a retry.

**Independent Test**: Seed a parent with a recorded child that lacks `base:develop`, re-run the spawn, and verify the label is added once. Re-run again and verify no further label write is needed.

**Acceptance Scenarios**:

1. **Given** a recorded, already-created same-repo child without the parent's `base:` label, **When** the spawn resumes, **Then** the label is applied to it.
2. **Given** a recorded child that already has the labels, **When** the spawn resumes, **Then** the result is unchanged and no error occurs.
3. **Given** a resumed child that already has a non-empty board Status, **When** inheritance is applied, **Then** its Status is not altered (existing resume behaviour is preserved).

---

### Edge Cases

- Parent has multiple `base:` labels: the child gets the single label the parent's own base resolution selects, not all of them.
- Parent has no `base:` label: no `base:` label is added, and autonomy inheritance is unchanged.
- The parent's `base:` branch no longer exists on the remote: the child inherits the label as-is, and the existing fallback-to-default-with-comment behaviour applies at the child's worktree creation.
- Same repository identified with different casing or formatting: the same-repo decision must match how the engine already compares repositories elsewhere.
- Failure to apply the `base:` label: it is logged and does not abort the spawn, consistent with the existing handling of inherited autonomy labels.
- Spawns declared from Plan, Review and Validate all get identical inheritance, since they share one spawn routine.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the parent and a spawned child are in the same `owner/repo`, the engine MUST apply the parent's `base:<branch>` label to the child.
- **FR-002**: When the parent carries several `base:` labels, the engine MUST apply only the one the parent itself resolves to for its own base branch.
- **FR-003**: When the parent carries no `base:` label, the engine MUST NOT add any `base:` label to a child.
- **FR-004**: The engine MUST apply inherited `base:`, `fabrik:yolo` and `fabrik:cruise` labels to a child before that child's board Status placement.
- **FR-005**: The engine MUST NOT apply the parent's `base:` label to a child in a different repository, and that child's base-branch behaviour (including the fallback with comment) MUST remain unchanged.
- **FR-006**: On the resume path for an already-created child (`fabrik:spawned-child:<i>:<n>`), the engine MUST apply the same inheritance to the child if it lacks the labels, idempotently (no duplicate labels, no error when already present).
- **FR-007**: Failure to apply an inherited label MUST be non-fatal: logged as a warning, with the spawn continuing, matching the current behaviour for autonomy labels.
- **FR-008**: The inheritance MUST behave identically for spawns declared by Plan, Review and Validate.
- **FR-009**: Inheritance on the resume path MUST NOT change a child's board Status or any progress it has made.
- **FR-010**: `docs/state-machine.md`'s spawn section MUST describe the new inheritance (same-repo `base:`, label-before-placement ordering, resume behaviour, cross-repo exclusion) and no longer state that `base:` labels are not inherited. `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-011**: Tests MUST cover: a same-repo spawn from a `base:develop` parent giving every child `base:develop` before its Status is set; a cross-repo child getting none; a resumed child missing the label receiving it; and a sim-bed scenario in which a `base:` parent spawns and the child's worktree and PR target the parent's base.

### Key Entities

- **Parent issue**: The issue whose stage declares the spawn. Its `base:`, `fabrik:yolo` and `fabrik:cruise` labels are the inheritance source.
- **Spawned child issue**: The issue created (or, on resume, found) for each spawn block. It receives inherited labels before board placement.
- **Spawn block**: The unit in the spawn grammar naming a child's target repository. Its repository determines same-repo versus cross-repo.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In 100% of same-repo spawns from a parent with a `base:<branch>` label, every created child carries that label before its Status is set, and its PR targets that branch.
- **SC-002**: In 100% of cross-repo spawns, no child receives the parent's `base:` label.
- **SC-003**: A spawn retried via the resume path leaves every already-created same-repo child with the correct inherited labels, with no duplicate label writes on a second retry.
- **SC-004**: No Fabrik-managed child of a `develop`-targeted parent lands on the default branch because of missing base inheritance.
- **SC-005**: The published state-machine documentation and the `llms-full.txt` bundle match the shipped behaviour; the docs-drift check passes.

## Assumptions

- "Same repo" is decided with the comparison the engine already uses for spawn targets and repository identity, not a new rule.
- The parent's resolved base is the one its own base resolution picks; it is not recomputed by a separate rule for children.
- Moving the autonomy-label inheritance ahead of Status placement is a pure reordering. Their semantics and non-fatal error handling are unchanged.
- A child may be spawned from a parent whose `base:` branch has since been deleted. The existing missing-branch fallback at worktree creation is sufficient, and no new validation is added at spawn time.
- Children created before this fix are repaired only if their spawn is resumed. No bulk migration of existing children is performed.

## Out of Scope *(optional)*

- A per-child `BASE:` header in the spawn block grammar.
- Inheriting `base:` into cross-repo children.
- Inheriting any other labels beyond `base:`, `fabrik:yolo` and `fabrik:cruise`.
- Retroactively relabelling children of spawns that already completed.
- Closing report #2057. It is closed by hand after a release containing the fix ships. The PR body mentions #2057 by bare number only, with no closing keyword (`Closes`/`Fixes`/`Resolves`) near it. This issue's own number gets `Closes`.

## Source References *(optional)*

- Report #2057 (community report; the canonical thread)
- ADR-1419 (cross-repo spawn servability and mid-flight spawn recognition)
- ADR-1583 (spawn resume marker `fabrik:spawned-child:<i>:<n>`)
- `docs/state-machine.md` spawn section (§6.7)
