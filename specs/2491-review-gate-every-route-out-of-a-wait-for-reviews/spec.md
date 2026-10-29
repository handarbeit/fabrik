# Feature Specification: Review gate — every route out of a wait_for_reviews stage consults the same verdict

**Feature Branch**: `fabrik/issue-2491`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description: the original issue request, verbatim below.

> ## Problem
>
> With `review_authority: authoritative`, the report saw items leave the Review stage by different routes while the gate was still reporting "requested changes". Of 10 PRs after the reporter's config change: 4 went to Validate via "PR not mergeable (base conflict)" while still blocked, 5 advanced seconds after a fix round while the log still said the gate was blocking, and 1 was moved by hand. An item must not leave a `wait_for_reviews` stage on a path that skips the gate, and the log must not say "blocking" about a gate that is about to let the item through.
>
> ## Requirements
>
> 1. Research traces every route by which an item leaves a `wait_for_reviews` stage (catch-up advance, `handleStageComplete` after a comment or reinvoke pass, the not-mergeable/rebase path, `fabrik_advance`, the landing decision) and lists, per route, whether the review gate verdict is consulted. Use the reporter's examples (chronicle #508, #510, #514, #527, #529, #463) as the shapes to reproduce in the sim.
> 2. Every automatic route consults the same verdict (`reviewGateHold` / the #2465 helper); a route that cannot must hold the item instead.
> 3. A base conflict while the review gate is blocking is handled in the stage (rebase reinvoke) without bypassing the gate.
> 4. The gate's log line reflects the decision actually taken on that poll.
>
> ## Out of scope
>
> The verdict rule itself (#2465), the declined-finding decision (#2466) and the wait clock (#2467).
>
> ## Acceptance
>
> - Sim twins for the not-mergeable route and the post-fix-round route; each fails with the fix neutralised.
> - `docs/state-machine.md` §6.6.6 updated; `docs/llms-full.txt` regenerated.
>
> ## Notes
>
> - Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`, Oct 1–26: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874; data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec).
> - Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only.

## Background

A stage configured with `wait_for_reviews: true` and `review_authority: authoritative` promises that an item will not move on while a reviewer's change request stands. The reporter (community report #2453, a flow analysis of a real project over Oct 1–26) found that promise does not hold in practice. After they switched to `authoritative`, ten PRs were observed leaving the Review stage:

- **4** advanced to Validate through the "PR not mergeable (base conflict)" path while the review gate was still reporting requested changes.
- **5** advanced seconds after a fix round finished, while the log still said the gate was blocking.
- **1** was moved by hand.

So an item can leave a `wait_for_reviews` stage by several routes (catch-up advance, completion after a comment or reinvoke pass, the not-mergeable/rebase path, the overseer's advance action, the landing decision), and at least some of them do not consult the gate verdict. Separately, the gate's log wording ("blocking") can disagree with what the engine then does on the same poll, which makes the behaviour impossible to diagnose from the log.

This issue is about **route coverage and honest logging**. The rule that decides the verdict (#2465, head-aware authoritative verdict), the handling of declined findings (#2466) and the review wait clock (#2467) are separate work and stay as they are. #2466 is a declared dependency of this issue.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A base conflict does not bypass a blocking review gate (Priority: P1)

An operator runs a stage with `wait_for_reviews: true` and `review_authority: authoritative`. A reviewer has requested changes and the gate is blocking. Meanwhile the PR's base moves and the PR stops merging cleanly.

**Why this priority**: This was the most frequent bypass in the report (4 of 10), and it moves an item toward merge with an unresolved change request.

**Independent Test**: In the sim, seed an item at the review stage with a standing change request from an expected reviewer and a PR that is not mergeable because of a base conflict. Run polls. The item must stay in the review stage, a rebase is dispatched in-stage, and the item does not reach Validate while the verdict is blocking.

**Acceptance Scenarios**:

1. **Given** an item at a `wait_for_reviews` stage whose gate verdict is blocking, **When** the PR is found not mergeable because of a base conflict, **Then** the engine handles the conflict inside the stage (rebase reinvoke) and the item does not advance.
2. **Given** the same item after the rebase lands and the gate verdict is still blocking, **When** the next poll runs, **Then** the item still does not advance.
3. **Given** the same item once the gate verdict clears, **When** the next poll runs, **Then** the item advances through the normal path.

---

### User Story 2 - A fix round does not let the item through while the gate still blocks (Priority: P1)

A reviewer requests changes, the engine runs a fix round (comment or reinvoke pass), and the pass completes. The gate verdict on the new state is still blocking (for example the reviewer has not re-reviewed the new head).

**Why this priority**: 5 of 10 observed items advanced seconds after a fix round while the log still said "blocking". This is the second bypass shape.

**Independent Test**: In the sim, seed an item with a standing change request, run a review fix round that completes the stage, and assert the item is not advanced on that poll or the next while the verdict is blocking.

**Acceptance Scenarios**:

1. **Given** a blocking gate verdict, **When** a comment or reinvoke pass finishes and signals stage completion, **Then** the item is held at the stage and not advanced.
2. **Given** the verdict then clears, **When** the next evaluation runs, **Then** the item advances.

---

### User Story 3 - Every other route consults the same verdict or holds (Priority: P2)

The engine and the overseer have more ways to move an item out of a `wait_for_reviews` stage: the catch-up advance, the landing decision, and the overseer's advance action.

**Why this priority**: These routes were not in the reporter's observed shapes, but the same class of bypass applies to any route that skips the gate. Closing only the two observed routes would leave the problem to recur.

**Independent Test**: A route-by-route table produced in Research shows each route consulting the shared verdict, or holding when it cannot. Each automatic route has at least a unit-level test of that behaviour.

**Acceptance Scenarios**:

1. **Given** a blocking verdict, **When** any automatic route attempts to advance or land the item, **Then** the item is held.
2. **Given** a route that cannot compute the verdict (for example, a failed read), **When** it would otherwise advance the item, **Then** it holds the item instead of advancing.

---

### User Story 4 - The gate log matches what the engine did (Priority: P2)

An operator reads the log to understand why an item moved or stayed.

**Why this priority**: The reporter could not tell from the log whether the gate had blocked, because it said "blocking" on polls where the item then advanced.

**Independent Test**: For each poll in the sim scenarios above, the gate's log line states the decision actually taken (held, or allowed) and never says "blocking" on a poll where the item is advanced.

**Acceptance Scenarios**:

1. **Given** a poll where the gate blocks and the item is held, **Then** the log says so.
2. **Given** a poll where the gate allows and the item advances, **Then** the log does not describe the gate as blocking.

---

### Edge Cases

- A base conflict arrives while the verdict is *not* blocking: the existing not-mergeable handling is unchanged.
- The rebase cycle limit is reached while the gate is still blocking: the item is paused for a human as today, never advanced.
- The gate verdict cannot be read (read error): automatic routes hold, consistent with the existing fail-closed behaviour.
- `review_authority: advisory` (or no `wait_for_reviews`): behaviour is unchanged; advisory gates never block on a change request.
- An operator moves an item by hand: that is a human decision and is not constrained.
- `fabrik_advance` (overseer) on an item with a blocking verdict: refused, as it is for other gated holds.
- `fabrik:cruise`/`fabrik:yolo` are present: they control timing only once the gate is satisfied and never bypass a blocking verdict.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Research MUST produce a per-route table of every way an item leaves a `wait_for_reviews` stage (catch-up advance, completion after a comment or reinvoke pass, the not-mergeable/rebase path, the overseer's advance action, the landing decision, and any others found), stating for each whether the review gate verdict is consulted today.
- **FR-002**: Every automatic route out of a `wait_for_reviews` stage MUST decide using the same review gate verdict (the shared hold computation backed by the #2465 head-aware helper), so two routes can never disagree on the same item at the same time.
- **FR-003**: A route that cannot obtain or evaluate the verdict MUST hold the item in its stage rather than advance it.
- **FR-004**: When the PR is not mergeable because of a base conflict while the gate verdict is blocking, the engine MUST resolve it in the stage through the rebase reinvoke path and MUST NOT advance the item to a later stage.
- **FR-005**: When a comment or reinvoke pass completes a `wait_for_reviews` stage while the verdict is blocking, the engine MUST hold the item at the stage.
- **FR-006**: The gate's log output on each poll MUST describe the decision actually taken for that item on that poll, and MUST NOT describe the gate as blocking on a poll where the item is advanced.
- **FR-007**: The overseer's advance action MUST refuse to advance an item out of a `wait_for_reviews` stage while the verdict is blocking.
- **FR-008**: Items with `review_authority: advisory`, and stages without `wait_for_reviews`, MUST behave exactly as today.
- **FR-009**: The not-mergeable (base conflict) and post-fix-round routes MUST each have a sim scenario reproducing the reporter's shapes (chronicle #508, #510, #514, #527, #529, #463), and each scenario MUST fail when the fix is neutralised through a per-engine test seam.
- **FR-010**: `docs/state-machine.md` §6.6.6 MUST be updated to describe the route coverage and logging behaviour, and `docs/llms-full.txt` MUST be regenerated in the same change.

### Key Entities

- **Review gate verdict**: the single decision (hold or allow, with a reason) for an item at a `wait_for_reviews` stage, computed once by the shared hold computation.
- **Exit route**: any code path that moves an item out of such a stage or toward landing.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the reporter's scenario shapes, 0 of the reproduced items leave a `wait_for_reviews` stage while the gate verdict is blocking.
- **SC-002**: 100% of automatic exit routes listed in the Research table consult the shared verdict or hold when they cannot.
- **SC-003**: On every poll in the sim scenarios, the gate log line agrees with the action taken (no "blocking" line on a poll where the item advances).
- **SC-004**: Each of the two required sim twins passes with the fix in place and fails with the fix neutralised.

## Assumptions

- The verdict rule from #2465 is correct and is used as-is; this work only ensures every route asks it.
- #2466 (declined-finding decision) lands first, as declared by the dependency; this spec does not change how declined findings affect the verdict.
- "Automatic route" means anything the engine or the overseer API does without a human moving the item on the board; a human's manual board move is out of the gate's control.
- Holding an item means leaving it in its current stage with existing labels and handling untouched, with no new pause or failure state, unless an existing limit (such as the rebase cycle cap) already applies.
- Where a route has no review-gate concept today and consulting the verdict would change a non-review stage's behaviour, the route is only gated for stages that have `wait_for_reviews: true`.

## Out of Scope *(optional)*

- The verdict rule itself (#2465).
- The declined-finding decision (#2466).
- The review wait clock (#2467).
- Changes to how a human moves items by hand.
- Changing `review_authority` defaults.

## Source References *(optional)*

- Community report #2453 (mentioned for context: the PR must mention it by bare number only and must not use a closing keyword; the report is closed by hand when a release ships).
- Reporter's flow analysis: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874
- Reporter's data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec
- Related, already-shipped or separate work: #2465 (head-aware verdict), #2466, #2467.
