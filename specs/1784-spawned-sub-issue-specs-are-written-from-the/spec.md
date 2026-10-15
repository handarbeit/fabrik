# Feature Specification: Spawned sub-issue specs must be satisfiable from the base branch

**Feature Branch**: `fabrik/issue-1784`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description:

> ## Problem
>
> A spawned sub-issue's description is written from the **spawning branch's** state rather than from
> the base branch, so the child spec asserts that files created by the parent's *still-open PR* already
> exist — and then makes them acceptance-critical. The child cannot satisfy criteria that depend on
> work downstream of itself.
>
> Reported in #1703 (@verveguy), v0.0.82: parent `#421` opened PR #483 at 12:38, its Review stage
> spawned child `#484` at 12:47, and the child's spec was written against the parent's branch.
>
> ## Requirements
>
> **R1 — Children are specified against the base, not the branch.** A child's acceptance criteria must
> be satisfiable from the base branch at spawn time. Where the child genuinely depends on the parent's
> work, that is a `DEPENDS_ON:`/`blockedBy` relationship, not a silent assumption baked into prose.
>
> **R2 — Make the dependency explicit rather than implicit.** If a spawning stage wants to reference
> work still in flight, it must declare the dependency — the engine already creates `blockedBy` edges
> and `DEPENDS_ON:` already exists in the spawn block grammar.
>
> **R3 — Tell the spawning stage.** This is substantially a skill-prompt problem: the stage authoring
> the child block is describing what it can see in its own worktree. The `FABRIK_SPAWN_CHILD_*`
> documentation in the stage skills must state that child specs are read by an agent starting from the
> base branch, and that anything not yet merged must be declared as a dependency.
>
> **R4 — Applies to every spawn origin.** Plan, Review and Validate can all spawn (ADR-1419). Whatever
> is done must cover all three, not just Plan.
>
> ## Scope
>
> **In scope:** the `FABRIK_SPAWN_CHILD_*` guidance in the stage skills, any engine-side child-body
> construction in `engine/spawn.go`, `docs/state-machine.md` §6.7/§6.7.2, tests.
> **Out of scope:** #1659 (the parent not stopping — separate issue, same family); the spawn mechanism;
> sub-issue sizing.
>
> ## Acceptance
>
> 1. A child spawned from a stage with unmerged work in its worktree does not assert that unmerged
>    artifacts exist; where it depends on them, a `DEPENDS_ON:` relationship is declared.
> 2. Reproduced against the reported shape (Review-stage spawn with an open parent PR).
> 3. Covered for all three spawn origins (R4).
> 4. A spawn with no dependency on unmerged work is unchanged.
> 5. Tests demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.
>
> ## Notes
>
> Reported in #1703 — human-owned community thread. Mention by bare number only; no closing keyword
> adjacent to that number in any form, including inside a negated sentence. Closed by hand on release.
> Sibling of #1659. Prior art: ADR-1419, ADR-1583.

## Background

When a stage declares a sub-issue (a `FABRIK_SPAWN_CHILD_*` block), the stage writes the child's spec while looking at its own worktree. That worktree contains the parent's in-progress work, but the child later starts from the **base branch**, which contains none of it. In the reported case (a community report, referenced here as #1703, seen on v0.0.82), a parent opened a PR and its Review stage spawned a child nine minutes later. The child's spec described files that exist only on the parent's open PR as if they were already present. It then made them acceptance-critical, so the child could not be completed.

The problem is mostly in the guidance given to the spawning stage. Plan, Review and Validate may all spawn children (ADR-1419), and nothing currently tells them that a child's reader starts from the base branch. This issue is about the content of what a spawning stage writes. It does not change how children are created, linked or placed.

Two properties of the existing mechanism constrain the fix, and the requirements below are written to respect them:

- **A child always blocks its spawning parent.** The engine links every spawned child as a blocker of the issue that spawned it. A child therefore cannot also wait on the parent's own unmerged work, because that would be a circular wait.
- **The ordering header covers siblings only.** `DEPENDS_ON:` is documented for Plan-declared batches and points forward to an earlier block in the same batch. It does not point at the parent or at arbitrary in-flight work.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A Review-stage child does not rely on the parent's open PR (Priority: P1)

A parent issue has an open PR that adds new files. Its Review stage discovers a blocker and declares a spawned child. The child's spec, read by an agent starting from the base branch, can be satisfied without any of the parent's unmerged work.

**Why this priority**: This is the reported failure. The child is unsatisfiable, and because a child blocks its parent, the parent is stuck as well.

**Independent Test**: Run a Review-stage spawn in a setup where the parent's branch holds artifacts that the base branch lacks. The spawning stage is given the guidance. Check that the resulting child body does not state that those artifacts exist and does not require them in its acceptance criteria.

**Acceptance Scenarios**:

1. **Given** a parent with an open PR adding files not on the base branch, **When** its Review stage spawns a child, **Then** the child's spec does not assert that those files exist and no acceptance criterion needs them.
2. **Given** the same setup, **When** the child genuinely needs something that can only come from the parent's unmerged work, **Then** the spec handles it as described in User Story 3 rather than presenting it as present.

---

### User Story 2 - The guidance covers every spawn origin (Priority: P1)

Whichever stage spawns (Plan, Review or Validate), it is told that child specs are read by an agent starting from the base branch and that unmerged work must not be treated as present.

**Why this priority**: The issue says the fix must not be Plan-only. Review and Validate spawn mid-flight, usually when an open parent PR exists, so they are the most exposed.

**Independent Test**: For each of the three origins, check that the stage guidance carries the base-branch rule and the dependency-declaration rule, and that a spawn from each origin is exercised in tests.

**Acceptance Scenarios**:

1. **Given** the Plan, Review and Validate stage guidance, **When** each is read, **Then** each states that child specs are read from the base branch and that unmerged work must not be asserted as existing.
2. **Given** a spawn declared from each of the three origins, **When** the child is created, **Then** the behaviour required by this issue is the same for all three.

---

### User Story 3 - Genuine dependencies on in-flight work are stated, not assumed (Priority: P2)

When a child really does depend on work that has not been merged, that dependency is stated openly through a mechanism that works, instead of being hidden in prose.

**Why this priority**: This is the root of the reported problem. A silent assumption becomes an unsatisfiable acceptance criterion.

**Independent Test**: Spawn a child whose subject involves in-flight work. Check that the child's spec either declares the dependency through an engine-recognised relationship that does not create a circular wait, or is reframed so it stands on the base branch alone.

**Acceptance Scenarios**:

1. **Given** a Plan batch where one child needs an earlier sibling's output, **When** the batch is declared, **Then** the ordering is expressed with `DEPENDS_ON:` and not only in prose.
2. **Given** a Review or Validate spawn where the child would need the parent's unmerged work, **When** the child is written, **Then** it is reframed to be satisfiable from the base branch (for example, the child produces what it needs itself, or the in-flight work is named as context outside its acceptance criteria), because the parent is blocked on the child.

---

### User Story 4 - Independent spawns are unaffected (Priority: P2)

A spawn whose child has no dependency on unmerged work behaves exactly as it does today.

**Why this priority**: Regression guard. Most spawns are independent and must not be burdened or altered.

**Independent Test**: Run an existing independent spawn (Plan batch with no ordering, and a Review or Validate blocker) and check that the created issues, labels, board placement and `blockedBy` links are unchanged.

**Acceptance Scenarios**:

1. **Given** a spawn with no dependency on unmerged work, **When** it is processed, **Then** the issues, board placement, assignment, labels and `blockedBy` edges are identical to the current behaviour.

---

### Edge Cases

- A child references a file or symbol that exists on the base branch and is merely modified by the parent's PR. This is allowed. Only content that exists solely on the unmerged branch is the concern.
- A Plan batch where several children depend on earlier siblings. Ordering within the batch continues to use `DEPENDS_ON:`.
- A parent whose PR has already merged by the time the child is spawned. The merged work is then on the base branch and may be referenced normally.
- A multi-level case where a child spawns its own children (Plan permits arbitrary depth). The same rule applies to each level, relative to its own base.
- A child body that mentions unmerged work only as background (not as a present fact and not acceptance-critical). This remains allowed, provided it is marked as in-flight and not required.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A spawned child's acceptance criteria MUST be satisfiable from the base branch as it stands when the child is created.
- **FR-002**: A spawned child's spec MUST NOT state or imply that artifacts existing only on the spawning branch (unmerged files, symbols, behaviour) are already present.
- **FR-003**: The `FABRIK_SPAWN_CHILD_*` guidance in the Plan, Review and Validate stage skills MUST state that the child's spec is read by an agent starting from the base branch and that unmerged work must not be treated as present.
- **FR-004**: The same guidance MUST tell the spawning stage that when a child depends on other in-flight work, the dependency must be stated explicitly through a mechanism the engine recognises, not left implicit in prose.
- **FR-005**: The guidance MUST account for the fact that a spawned child blocks its parent: a child must not be specified to depend on its own parent's unmerged work, since that would never complete. For that case it MUST direct the stage to reframe the child so it stands on the base branch.
- **FR-006**: Ordering between siblings in a Plan-declared batch MUST continue to be declared with `DEPENDS_ON:` and not in prose.
- **FR-007**: FR-001 through FR-005 MUST hold for all three spawn origins (Plan, Review, Validate) with no origin treated as secondary.
- **FR-008**: A spawn whose child has no dependency on unmerged work MUST behave unchanged: same issue creation, board placement, assignment, labels and `blockedBy` links.
- **FR-009**: The as-built documentation of spawning (the spawn sections of the state-machine specification) MUST be updated to describe the base-branch rule and the dependency rule.
- **FR-010**: Tests MUST cover the reported shape (a Review-stage spawn while the parent has an open PR with branch-only artifacts) and each of the three origins, and MUST be shown to fail without the change (non-vacuous).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reproduced reported shape, the resulting child spec contains no statement that branch-only artifacts exist and no acceptance criterion that requires them.
- **SC-002**: Each of the three spawn origins has at least one test exercising the new behaviour, and each such test fails when the change is removed.
- **SC-003**: The existing independent-spawn tests pass unmodified, showing that behaviour without a dependency is unchanged.
- **SC-004**: The full test suite (`go test -race ./...`) and `go vet ./...` pass.
- **SC-005**: The Plan, Review and Validate guidance each contain the base-branch rule, verifiable by reading them.

## Assumptions

- Prior art is ADR-1419 (spawn from Plan, Review and Validate) and ADR-1583 (spawn resumption). This issue changes what spawning stages write, not those mechanisms.
- "Base branch" means the branch the child's worktree is created from at the time it starts, normally the repository default or the issue's `base:` label.
- Because a child blocks its parent, a child cannot wait on its parent's unmerged work. Where the original request says such a dependency should be declared as `DEPENDS_ON:`/`blockedBy`, this is read as applying to dependencies that can actually be expressed (siblings in a Plan batch, or work already tracked by another issue). For dependence on the parent's own unmerged work, the spec requires reframing the child. Research and Plan decide the exact wording and whether any engine-side safeguard is warranted beyond the guidance.
- The primary fix is the guidance to spawning stages, as the original request describes. Any engine-side change to child-body construction is permitted but not required, and is chosen during Research and Plan.
- A child body that mentions in-flight work as clearly marked background, not required by any acceptance criterion, is acceptable.

## Out of Scope

- The parent not stopping after it spawns (a separate issue in the same family, #1659).
- The spawn mechanism itself: block grammar, issue creation, board placement, assignment, `blockedBy` linking.
- Sub-issue sizing or how to decompose work.
- Closing or updating the community report. That is done by hand after a release.

## Source References

- ADR-1419 (cross-repo spawn servability and mid-flight recognition) and ADR-1583 (spawned-child resumption)
- State-machine specification, sections 6.7 and 6.7.2
- Stage guidance for Plan, Review and Validate (`FABRIK_SPAWN_CHILD_*` sections)
