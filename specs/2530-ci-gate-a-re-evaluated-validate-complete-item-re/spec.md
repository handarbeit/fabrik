# Feature Specification: Stop re-adding an already-present completion label on re-evaluation, and stop it showing up as reconcile drift

**Feature Branch**: `fabrik/issue-2530`
**Created**: 2026-10-30
**Status**: Specified
**Input**: User description (verbatim):

> ## Problem
>
> An item at Validate that is already `stage:Validate:complete` (under `fabrik:cruise`, waiting for a human merge decision) is reported as drifted by every 30-minute light reconcile for as long as it waits. On a production daemon (`poll: 180`) one item showed this on every light reconcile for about 26 hours, from 2026-10-29 07:44 until it landed.
>
> Mechanism, confirmed from the log, the item's timeline and a direct test:
>
> 1. Each periodic re-evaluation (10 × PollSeconds) admits the item. Catch-up Phase 1 `handleMergeAndCIGates` (`engine/catch_up_handlers.go`) reaches `checkCIGate` unconditionally. It does not check that the completion label is already present and `fabrik:awaiting-ci` is absent.
> 2. The PR is `clean`, so the gate takes the `PRMergeReady` branch and calls `addCompleteLabelAndRemoveCI` (`engine/ci.go`). That function re-POSTs `stage:Validate:complete`, which is already on the issue, and re-records the validate SHA.
> 3. GitHub records no timeline event for re-adding a present label, but it does bump the issue's `updated_at`. Verified: re-adding an already-present label on an issue in this repo moved `updated_at` by 6 s with no event. On the affected item, the board `updatedAt` moved 1–2 s after each `CI gate clears` line, and the timeline showed nothing at those times.
> 4. The cache does not count this write as an update, so the next `LightReconcile` (`boardcache/boardcache.go`) sees a newer `updatedAt`, flags drift and resyncs. Each re-evaluation also logs `activity detected — idle backoff reset`.
>
> Log shape, every 30 min:
>
> ```
> [#A settle] PR #P mergeable_state="clean"
> [#A ci-gate] CI gate clears (...)
> [#A validate-sha] recorded CI-completion SHA ...
> ... 30 min later ...
> [reconcile] light reconcile: 1 item(s) drifted ([#A])
> ```
>
> Effect: no wrong decision. It costs a label write and a resync per item per re-evaluation, adds a drift line that hides real drift, and resets the idle backoff, so a daemon whose only activity is items waiting at Validate never backs off. Any item parked at Validate-complete with `wait_for_ci` does this.
>
> ## Requirements
>
> 1. When the CI gate clears on an item that already carries `stage:<name>:complete` and has no `fabrik:awaiting-ci`, the engine writes nothing to GitHub. Research picks the narrowest correct place: skip the add in `addCompleteLabelAndRemoveCI` when the label is already present (from a live or Store read that is trustworthy here), or skip the gate's clear branch entirely for that state. The validate-SHA record must keep its current meaning (#2158, #2343).
> 2. Audit the other engine label adds that can run on a re-evaluation of an unchanged item (add of a label already present). Report each one in the ADR; fix those on a re-run path in the same way, or record why not.
> 3. A self-write that does reach GitHub must not appear as light-reconcile drift on the next pass. Research checks whether label writes raise the `SelfWriteObserved` baseline from the `Date` header (#1945), and why the drift fired anyway here (for example, light reconcile compares against a field the baseline does not cover).
> 4. A re-evaluation that writes nothing must not count as activity for the idle backoff.
> 5. No change to gate decisions, stage completion or landing.
>
> Record the decisions in an ADR named after this issue.
>
> ## Acceptance
>
> - Sim twin (`tests/sim`): an item at Validate-complete under cruise, with a clean PR, is re-evaluated several times. It makes zero `AddLabelToIssue` calls and the light reconcile reports no drift. Neutralising the fix (seam) restores the repeated add.
> - A unit test that the CI-gate clear still adds the label when it is absent (the ordinary `fabrik:awaiting-ci` path).
> - If requirement 3 changes the baseline or the drift comparison, a test that a real external edit is still reported as drift.
> - `CLAUDE.md` and `docs/state-machine.md` updated where behaviour changes; `docs/llms-full.txt` regenerated.
>
> ## Notes
>
> - Related: #2528 (same daemon, admission after an advance), ADR 2180 and ADR 1945 (freshness and self-write baseline), ADR 2096 (periodic re-evaluation).

## Background

An item waiting at a stage whose completion label is already set (the observed case is Validate under `fabrik:cruise`, waiting for a human merge decision) is periodically re-evaluated by the engine. On every re-evaluation the CI gate finds the PR clean and "clears" again, which re-applies the completion label that is already on the issue and re-records the validate completion SHA.

GitHub does not record a timeline event when an already-present label is re-added, but it does bump the issue's `updated_at` (verified: a 6 s bump with no event in this repo). The engine's cache does not account for that bump, so the next light reconcile reads a newer `updatedAt`, reports the item as drifted and resyncs it. Each re-evaluation also logs an activity line that resets the idle backoff.

On a production daemon polling every 180 s, one item produced this every 30 minutes for about 26 hours (2026-10-29 07:44 until it landed). No wrong decision results. The costs are:

- a pointless label write and a resync per item per re-evaluation;
- a recurring "drifted" line that masks real drift;
- an idle backoff that never engages on a daemon whose only activity is items waiting at a completed stage, so it polls at the busy rate indefinitely.

Any item parked at a completed stage with `wait_for_ci` behaves this way.

Related work: #2528 (admission after an advance on the same daemon), ADR 2180 and ADR 1945 (cache freshness and the self-write baseline), ADR 2096 (periodic re-evaluation).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A waiting, already-complete item is quiet (Priority: P1)

An operator has items parked at Validate-complete under `fabrik:cruise`, each waiting for a human to merge. While they wait, the engine re-evaluates them periodically, and the PR stays clean throughout. The operator expects the engine to write nothing to GitHub for them and the reconcile log to report no drift.

**Why this priority**: This is the defect itself: a write and a spurious drift report on every re-evaluation for as long as an item waits.

**Independent Test**: Seed an item at Validate with `stage:Validate:complete`, no `fabrik:awaiting-ci`, and a clean PR under `fabrik:cruise`. Re-evaluate it several times and run a light reconcile after each. Count the label-add calls and read the reconcile result.

**Acceptance Scenarios**:

1. **Given** an item that already carries its stage's completion label and no `fabrik:awaiting-ci`, with a clean PR, **When** the periodic re-evaluation runs the CI gate repeatedly, **Then** the engine makes zero label-add calls and zero other writes to GitHub for that item.
2. **Given** the same item, **When** a light reconcile runs after each re-evaluation, **Then** it reports no drift for the item.
3. **Given** the same item, **When** the fix is switched off through its test seam, **Then** the repeated label add (and the drift report) returns, so the test is not vacuous.

---

### User Story 2 - The ordinary completion path is unchanged (Priority: P1)

An item whose CI gate clears for the first time (it carries `fabrik:awaiting-ci` and no completion label) still gets its completion label and still has `fabrik:awaiting-ci` removed.

**Why this priority**: The fix must not suppress the one write that is actually needed.

**Independent Test**: Unit-level: run the gate-clear on an item that lacks the completion label and observe that the label is added, the awaiting-ci marker removed, and the validate SHA recorded.

**Acceptance Scenarios**:

1. **Given** an item with `fabrik:awaiting-ci` and no completion label, **When** CI clears, **Then** the completion label is added, `fabrik:awaiting-ci` is removed and the validate completion SHA is recorded exactly as before.
2. **Given** an item that carries the completion label **and** `fabrik:awaiting-ci`, **When** CI clears, **Then** the awaiting-ci marker is still removed (only the redundant label add is skipped).

---

### User Story 3 - Other redundant label adds on re-runs are accounted for (Priority: P2)

The maintainer wants to know whether any other engine label add can fire on a re-evaluation of an unchanged item, adding a label that is already present, and wants each one either fixed the same way or explicitly justified.

**Why this priority**: The same hidden `updated_at` bump and drift would recur wherever such an add exists; one fix for one site is not a guarantee of a quiet idle engine.

**Independent Test**: The ADR lists every audited site with its disposition. Each fixed site has a test that a re-evaluation of an unchanged item makes no add call for a label already present.

**Acceptance Scenarios**:

1. **Given** the audit, **When** a reader opens the decision record, **Then** every engine label add reachable on a re-evaluation of an unchanged item is listed with "fixed" or a recorded reason for leaving it.
2. **Given** a site judged to be on a re-run path, **When** its item is unchanged and already carries the label, **Then** no add is made.

---

### User Story 4 - A self-write that does reach GitHub is not drift (Priority: P2)

When the engine does write a label (including an add of a label that was already there) the resulting `updated_at` movement must not be reported as drift by the next light reconcile. Genuine external edits must still be reported.

**Why this priority**: Defence in depth: even if some write slips through, the drift log should stay trustworthy. It also explains why drift fired here despite an existing self-write baseline mechanism.

**Independent Test**: Make an engine label write, run a light reconcile, and expect no drift; make an external edit to the same item, run a light reconcile, and expect drift.

**Acceptance Scenarios**:

1. **Given** the engine has just written a label to an item, **When** the next light reconcile runs, **Then** the item is not reported as drifted on account of that write.
2. **Given** a real external edit to an item (by a person or another tool), **When** the next light reconcile runs, **Then** the item is still reported as drifted.

---

### User Story 5 - A silent re-evaluation does not defeat idle backoff (Priority: P2)

A re-evaluation that results in no write and no state change must not be treated as activity, so a daemon whose only work is such re-evaluations backs off as an idle daemon does.

**Why this priority**: The backoff is the engine's way of saving API budget when nothing is happening; it is currently defeated by items that are merely waiting.

**Independent Test**: With only already-complete waiting items on the board, run several polls and observe that the idle backoff grows and is not reset by the re-evaluations.

**Acceptance Scenarios**:

1. **Given** a board whose only items are waiting at an already-complete stage with clean PRs, **When** several polls and periodic re-evaluations pass, **Then** no "activity detected" reset occurs and the poll interval backs off.
2. **Given** a re-evaluation that does change something (a label write, a state change), **When** it happens, **Then** it still counts as activity.

---

### Edge Cases

- The completion label appears present in the engine's view but was removed on GitHub moments earlier (stale view). The skip must rest on a read that is trustworthy for this decision; if presence cannot be established, the engine must behave as it does today (add the label).
- The item carries both the completion label and `fabrik:awaiting-ci` (inconsistent state): the awaiting-ci marker must still be cleared when CI clears.
- The PR head moved since the completion SHA was recorded: the validate-SHA invalidation behaviour (#2158, #2343) must be unchanged, so a moved head still invalidates the completion and a skipped add never leaves a stale or missing SHA record where one was required.
- Multiple stages with `wait_for_ci` (not only Validate) show the same state; the behaviour must be consistent across them.
- A non-Validate stage whose completion label is present but whose item is about to advance: the advance path is not altered.
- A human removes the completion label while the item waits: the next re-evaluation sees it absent and the ordinary path applies.
- The light reconcile runs while an engine write is in flight or right after one, within the same second as GitHub's `updated_at` resolution.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the CI gate clears on an item that already carries its stage's completion label and has no `fabrik:awaiting-ci`, the engine MUST write nothing to GitHub for that item (no label add, no label removal, no comment).
- **FR-002**: The decision that the label is already present MUST be based on a read the engine can trust for this purpose; when presence cannot be established, the engine MUST behave as it does today.
- **FR-003**: When the CI gate clears on an item that lacks the completion label, or that carries `fabrik:awaiting-ci`, the engine MUST add the completion label and remove `fabrik:awaiting-ci` as it does today.
- **FR-004**: The validate completion SHA record MUST keep its current meaning for the SHA-invalidation and post-restart rebuild behaviour (#2158, #2343); skipping the label write MUST NOT change when a head change invalidates a completion.
- **FR-005**: The engine's label adds that can execute on a re-evaluation of an unchanged item MUST be audited. Each site MUST be recorded in the decision record with its disposition: fixed so that an add of an already-present label is skipped, or left as is with the reason.
- **FR-006**: Each site judged to be on a re-run path MUST skip the add when the item already carries the label, subject to FR-002.
- **FR-007**: A label write by the engine that does reach GitHub MUST NOT cause the next light reconcile to report that item as drifted solely because of that write. The decision record MUST state whether label writes already raise the self-write baseline from GitHub's response, and why drift was reported here regardless.
- **FR-008**: A genuine external change to an item (not made by the engine) MUST still be reported as drift by the light reconcile.
- **FR-009**: A re-evaluation that makes no write and changes no state MUST NOT be logged or counted as activity for the idle backoff; a re-evaluation that does write or change state MUST still count.
- **FR-010**: Gate decisions, stage completion and landing behaviour MUST be unchanged.
- **FR-011**: The fix MUST be switchable off through a test-only seam so that the behaviour before the fix can be restored in tests, as with the engine's other neutralisation seams.
- **FR-012**: The decisions MUST be recorded in an ADR numbered after this issue, and the behaviour changes MUST be reflected in `CLAUDE.md` and `docs/state-machine.md`, with `docs/llms-full.txt` regenerated.

### Key Entities

- **Completion label (`stage:<name>:complete`)**: Durable marker that a stage has finished; already present on the waiting items in question.
- **Awaiting-CI marker (`fabrik:awaiting-ci`)**: Marker that a finished stage is waiting on CI; absent on the waiting items in question.
- **Validate completion SHA**: The PR head recorded when CI cleared, used to invalidate a completion if the head later changes.
- **Self-write baseline**: The engine's record of the latest GitHub-side time of its own writes, used so its own writes are not read back as external change.
- **Light reconcile**: The periodic (30 min) cheap listing that compares board data to the cache and reports drift.
- **Idle backoff**: The poll-interval growth applied when no activity is detected.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An item parked at Validate-complete under cruise with a clean PR, re-evaluated at least five times, results in zero label-add calls to GitHub.
- **SC-002**: Over the same run, the light reconcile reports zero drifted items attributable to that item.
- **SC-003**: With the fix switched off through the seam, the same scenario reproduces the repeated label add, demonstrating the test detects the defect.
- **SC-004**: The first-time CI clear on an item with `fabrik:awaiting-ci` still ends with the completion label present and `fabrik:awaiting-ci` absent.
- **SC-005**: A daemon whose only items are waiting at an already-complete stage with clean PRs reaches its maximum idle backoff interval instead of staying at the busy rate.
- **SC-006**: An external edit to an item is still reported as drift by the next light reconcile.
- **SC-007**: The decision record lists every audited label-add site with a disposition.

## Assumptions

- The root cause is as diagnosed in the issue (a redundant label add bumps `updated_at`, which the cache does not absorb); the Research stage confirms or corrects the narrowest place to fix it.
- Skipping a redundant add is preferred over making the cache tolerate the bump alone, because it also saves the API write; the cache-side change (FR-007) is defence in depth and is made only if Research shows a real gap.
- A label whose presence is confirmed from a trustworthy read is sufficient to skip; no new persistent state is needed.
- Bug-class reports like this are fixed in the engine, with a sim twin as the primary regression test; live end-to-end coverage is not required.

## Out of Scope *(optional)*

- Changing which items are admitted to periodic re-evaluation, or its interval (ADR 2096).
- Admission after an advance (#2528).
- Changing gate decisions, stage completion, or landing behaviour.
- Redesigning the light reconcile or the cache freshness rule beyond what FR-007 and FR-008 require.
- Redundant writes that are not label adds (unless Research finds one on the same path and records it as follow-up).

## Source References *(optional)*

- Issue #2530 (this issue), #2528
- ADR 2180 (cache freshness rule), ADR 1945 (self-write baseline anchored to GitHub's clock), ADR 2096 (admission cooldowns and periodic re-evaluation)
- #2158, #2343 (validate completion SHA and its post-restart rebuild)
