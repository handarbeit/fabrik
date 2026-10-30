# Feature Specification: Promptly start the next stage after an advance that lands while the item's own worker is still running

**Feature Branch**: `fabrik/issue-2528`
**Created**: 2026-10-30
**Status**: Specified
**Input**: User description (verbatim original issue body):

````markdown
## Problem

When a stage completes and the worker moves the item to the next stage, the next stage can fail to start for up to the periodic re-evaluation interval (10 × PollSeconds) plus the idle poll backoff. Nothing on the board signals it: the item sits idle in the new column with no wait label.

Observed on a production daemon (App auth, tailscale transport, `poll: 180`, so the re-evaluation interval is 30 min). Times are UTC:

```
09:38:49 [#A done] stage "Research" complete
09:38:51 [#A advance] moving to stage "Plan"
09:38:52 [poll] ... (wake polls at 09:38:52, :54, :57, 09:39:01, ...; none dispatches #A)
09:49:14 [poll] idle backoff: 2x (6m0s)
09:55:17 [poll] idle backoff: 4x (12m0s)
10:07:19 [poll] idle backoff: max (1h0m0s)
10:09:56 (a human comments on #A)
10:11:23 [reconcile] light reconcile: 1 item(s) drifted ([#A]); the poll is woken
10:11:28 [#A process] ... stage: Plan
```

A sibling item, #B, advanced Research → Plan 40 s later in the same burst and was dispatched within 4 s. So the behaviour depends on timing.

Reading of the mechanism (Research to confirm; nothing in the log records the drop):

1. The engine's Status write raises `StatusChanged`, which puts the item in the next poll's `cycleSet` exactly once. The webhook echo of that move is a no-op, because the write-through has already applied it. So the echo admits nothing further.
2. The poll that drains that entry sees the item's worker still in flight. The worker goroutine is still finishing the advance, and `WorkerExited` is deferred. `itemMayNeedWork` is therefore false, and `selectDeepFetchCandidates` re-arms `periodic-re-eval` (`engine/poll.go`, the `cycleSet[iKey]` re-arm after `itemMayNeedWork`).
3. `WorkerExited` wakes a poll but is deliberately excluded from `cycleSetFlags` (`engine/observers.go`, ADR-039), so the item is not admitted again.
4. The re-evaluation comes due at about +30 min. `PollWithBackoff` caps the idle interval only at the short cooldowns (`shortAdmissionCooldownReasons`: `retry-backoff`, `sentinel-live`, `merge-unsettled`). By then the idle backoff had reached 1 h, so no poll was scheduled until something external woke one.

The same shape applies to any engine-driven progression whose admitting change lands while the item's own worker is still in flight.

## Requirements

1. An item whose own worker made a dispatch-relevant change (a column move, a completion label) is re-admitted promptly once that worker exits. The ordinary next stage must start on the poll after the advance, not at the periodic re-evaluation. Research picks the shape, for example: the worker's exit admits the item when the item changed during its run, in the way `postRunFeedbackAdmission` (`engine/post_run_admission.go`) does for feedback; or an in-flight drop of a `cycleSet` admission records an already-due short cooldown instead of `periodic-re-eval`. Record the choice in an ADR named after this issue.
2. The periodic re-evaluation backstop must not be pushed out by the idle backoff. When an item's `periodic-re-eval` falls due, a poll runs within about one configured poll interval of it, not up to the 1 h backoff maximum. Research decides whether that means capping the idle interval at the earliest unexpired admission cooldown of any reason, or something narrower. A cap must not reintroduce poll-rate loops for items whose cooldown keeps re-arming.
3. Keep ADR-039's intent: an early-return worker exit (dep-blocked and similar) carries no new information and must not bypass the cooldown gate.
4. No change to how stages complete or advance, or to the gates.

## Acceptance

- Sim twin (`tests/sim`): a stage completes and advances while a poll drains the `StatusChanged` entry with the worker still registered. The next stage is dispatched within one poll of the worker exiting. Neutralising the fix (seam) reproduces the stall to the periodic re-evaluation.
- Unit test for requirement 2: with the idle backoff at its maximum and a `periodic-re-eval` due sooner, the next poll is scheduled at the cooldown, not at the backoff.
- A test that the ADR-039 early-return exit still does not admit.
- `CLAUDE.md` (the `engine/poll.go` admission-cooldown bullet) and `docs/state-machine.md` updated; `docs/llms-full.txt` regenerated.

## Notes

- Related: #2232 and #2499 (every engine-scheduled retry owns an admission cooldown); ADR 2096 (expired cooldowns admit once, re-arm at the drop sites); #2114 (deferred dispatch re-queue).
````

## Background

When a stage finishes, the engine moves the item to the next stage's column. The first poll that notices the move can find the item's own worker still registered (it is finishing the advance), so the item is judged "not needing work" and its next look is deferred to the periodic re-evaluation. The worker's later exit deliberately does not re-admit the item, and the idle poll backoff can grow to 1 h, so the periodic re-evaluation itself can fire long after it is due.

In the observed production case (30 min re-evaluation interval, 1 h idle backoff), an item sat in the Plan column for about 32 minutes with no wait label and no log line explaining it, until a human comment woke it. A sibling item advanced seconds later in the same burst and was dispatched within 4 s, so the behaviour is timing-dependent and hard to diagnose from the board. The same shape applies to any engine-driven progression whose admitting change lands while the item's own worker is still in flight.

Related work: #2232 and #2499 (every engine-scheduled retry owns an admission cooldown), ADR 2096 (expired cooldowns admit once and are re-armed at the drop sites), #2114 (deferred dispatch re-queue), ADR-039 (the worker's exit alone does not admit an item).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The next stage starts promptly after an advance (Priority: P1)

An operator running the pipeline expects an item that finishes one stage to begin the next stage on the next poll, regardless of whether a poll happened to observe the move while the item's own worker was still winding down.

**Why this priority**: This is the observed failure. Items stall silently for tens of minutes with no signal on the board.

**Independent Test**: In the sim, complete a stage so that the item advances while a poll drains the item's column-move admission with its worker still registered, then let the worker exit. The next stage must be dispatched within one poll of the worker's exit.

**Acceptance Scenarios**:

1. **Given** an item whose worker has just moved it to the next stage and has not yet exited, **When** a poll runs and sees the worker still in flight and then the worker exits, **Then** the item is re-admitted and the next stage is dispatched by the following poll, not at the periodic re-evaluation.
2. **Given** the same situation with the fix neutralised, **When** the same sequence runs, **Then** the next stage does not start until the periodic re-evaluation (this proves the test exercises the failure).
3. **Given** the worker made another dispatch-relevant change during its run (for example a completion label), **When** the worker exits, **Then** the item is re-admitted promptly.

---

### User Story 2 - The periodic re-evaluation backstop is not stretched by idle backoff (Priority: P1)

An operator expects the periodic re-evaluation, the safety net for missed admissions, to run close to when it is due, even after a long idle period.

**Why this priority**: Even with story 1 fixed, any other missed admission relies on this backstop. A backstop that can be delayed by up to the 1 h backoff maximum defeats its purpose.

**Independent Test**: With the idle backoff at its maximum and a periodic re-evaluation due sooner than the backoff, check when the next poll is scheduled.

**Acceptance Scenarios**:

1. **Given** the idle backoff is at its maximum and an item's periodic re-evaluation falls due sooner, **When** the next poll is scheduled, **Then** it is scheduled at about the cooldown's due time, within about one configured poll interval, not at the backoff.
2. **Given** an item whose cooldown keeps being re-armed, **When** polls run, **Then** the poll rate does not degrade into a tight loop.

---

### User Story 3 - A no-information worker exit still does not admit (Priority: P2)

A worker that exits early because nothing changed (for example it is dependency-blocked) must not bypass the cooldown gate and cause repeated re-admission.

**Why this priority**: This preserves the existing protection against poll and dispatch loops, which the fix must not weaken.

**Independent Test**: Run an early-return worker exit and verify the item is not admitted ahead of its cooldown.

**Acceptance Scenarios**:

1. **Given** a worker that exits early without changing anything dispatch-relevant, **When** it exits, **Then** the item is not admitted before its cooldown expires.

---

### Edge Cases

- The item advances twice during one worker run (for example through a stage that completes immediately): it is admitted once per poll, not once per change, and no stage is skipped.
- The worker exits before any poll has drained the column-move admission: behaviour is unchanged (the ordinary admission applies).
- The worker's changes make the item ineligible (paused, blocked, closed): re-admission still goes through the normal eligibility checks and starts nothing.
- A burst of many items advancing together: each is admitted promptly without the poll waiting on workers.
- Daemon restart between the advance and the exit: the periodic re-evaluation remains the backstop.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: An item whose own worker made a dispatch-relevant change (a column move or a completion label) during its run is re-admitted for evaluation promptly once that worker exits, so the ordinary next stage starts on the poll after the advance rather than at the periodic re-evaluation.
- **FR-002**: The mechanism chosen for FR-001 is selected during Research (for example admission on the worker's exit when the item changed during its run, or recording an already-due short cooldown when an admission is dropped because the worker is in flight) and the choice is recorded in an ADR named after this issue.
- **FR-003**: When an item's periodic re-evaluation falls due, a poll runs within about one configured poll interval of it, even if the idle poll backoff is at its maximum.
- **FR-004**: The approach to FR-003 (for example capping the idle interval at the earliest unexpired admission cooldown of any reason, or something narrower) is selected during Research, and must not cause poll-rate loops for items whose cooldown keeps re-arming.
- **FR-005**: A worker exit that carries no new information (an early return such as dependency-blocked) does not admit the item and does not bypass the cooldown gate (ADR-039's intent is preserved).
- **FR-006**: How stages complete or advance, and the gates, are unchanged.
- **FR-007**: A sim twin test covers the advance-while-worker-in-flight scenario, including a neutralisation seam that reproduces the stall at the periodic re-evaluation.
- **FR-008**: A unit test covers the idle-backoff cap: with the backoff at its maximum and a periodic re-evaluation due sooner, the next poll is scheduled at the cooldown.
- **FR-009**: A test confirms the early-return worker exit still does not admit.
- **FR-010**: `CLAUDE.md` (the poll/admission-cooldown bullet) and `docs/state-machine.md` are updated to describe the new behaviour, and `docs/llms-full.txt` is regenerated.

### Key Entities *(if applicable)*

- **Admission cooldown**: A per-item record that gates when the engine next considers an item for work; includes the periodic re-evaluation and short retry-style cooldowns.
- **Periodic re-evaluation**: The safety-net cooldown that re-examines an idle item at 10 × the poll interval.
- **Idle poll backoff**: The growing poll interval used while nothing is happening, up to a 1 h maximum.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a stage completes and advances while the item's own worker is still registered, the next stage is dispatched within one poll of the worker exiting.
- **SC-002**: With idle backoff at its maximum, a due periodic re-evaluation causes a poll within about one configured poll interval of its due time.
- **SC-003**: With the fix neutralised via its seam, the sim reproduces the stall to the periodic re-evaluation; with it enabled, the same scenario does not stall.
- **SC-004**: An early-return worker exit still produces zero admissions ahead of the cooldown.
- **SC-005**: The poll rate for items whose cooldown keeps re-arming does not increase beyond one poll per configured poll interval attributable to that item.

## Assumptions

- The root-cause reading in the issue is plausible but unconfirmed; Research verifies it before the mechanism is chosen.
- "Promptly" means by the poll after the worker's exit, not instantaneously.
- "About one configured poll interval" is the acceptable lateness for the periodic re-evaluation.
- Both fixes (FR-001 and FR-003) are in scope together, since either alone leaves a way to stall.
- The behaviour applies to any engine-driven progression admitted while the item's own worker is in flight, not only Research to Plan.

## Out of Scope *(optional)*

- Changes to how stages complete or advance, or to any gate.
- Adding a board label or other visible wait signal for the stall.
- Changes to the length of the periodic re-evaluation interval or the idle backoff maximum.
- Re-admission of items on worker exit when nothing changed (ADR-039's early-return case).
