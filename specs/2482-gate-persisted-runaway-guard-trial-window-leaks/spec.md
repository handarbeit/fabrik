# Feature Specification: Gate legs start with empty persisted train caps

**Feature Branch**: `fabrik/issue-2482`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "gate: persisted runaway-guard trial window leaks across legs on the same bed (TestCrossRepoSpawn paused). The merge-train runaway guard's trial window has been persisted since #2349 (ADR 2349, `.fabrik/state/engine-decisions/train-caps.json`). It therefore survives the bed restart that a gate leg switch performs. One leg's deliberate guard trip can then pause an unrelated test's member in the next leg on the same bed. Seen on the 0.0.90 gate (engine `d6866128`, bed A, invocation `20261026T045615Z-e31de8`): (1) In the `pat/on` leg, `TestMergeTrainRunawayGuardPausesBatch` recorded 6 trials on `handarbeit/fabrik-test-beta`, between 10:19 and 10:33Z. That trips the guard, which is what the test is for. (2) The bed then switched to `app/on/webhook`, about 10:38Z. (3) There, `TestCrossRepoSpawn`'s child `fabrik-test-beta#633` reached Queued at 10:56Z. The engine logged `runaway guard already tripped for handarbeit/fabrik-test-beta (6 trial(s)) — pausing 1 Queued member(s) before dispatch`. (4) The child never landed. The test timed out waiting for it to close: `--- FAIL: TestCrossRepoSpawn (3121.06s)`. The leg order decides whether it happens. It needs a guard-tripping leg followed by a train-on leg that queues on the same repo within an hour. Earlier gates simply did not hit it. Requirements: (1) Start each leg with empty train caps. Every gate leg must start with no persisted train-cap state (trial windows, ejection counts, auto-repair attempts) left by an earlier leg on that bed. For example, have the leg restart (`TestSwitchTrainMode`) and `gate reset` remove the bed's `.fabrik/state/engine-decisions/train-caps.json` while the bed is stopped. Do not clear the usage-limit hold file: that is account state, not test state. Choose the place in Research. (2) Clean up after the guard-trip test. `TestMergeTrainRunawayGuardPausesBatch` (and any other test that deliberately trips the guard or spends ejection counts) should also leave no persisted trial record behind for its repo. A later test in the same leg must be no more exposed than before #2349. (3) No engine change. Persistence across a real restart is the intended production behaviour (ADR 2349). Acceptance: A gate unit test (`tests/gate`, fake commander) shows that the leg switch removes the bed's train-caps file and leaves `usage-limit.json` alone. A test with that removal neutralised fails. Mentioned for context: #2349."

## Background

Since #2349 (ADR 2349) the engine persists its merge-train decision state, including the runaway guard's trial window, ejection counts and auto-repair attempts. It writes them to `.fabrik/state/engine-decisions/train-caps.json` so that a real production restart does not reset them. That is intended behaviour and stays.

The live e2e gate runs several legs back to back on one bed. Each leg switch restarts the bed's engine, which now reloads the previous leg's train caps. A test that trips the guard on purpose therefore leaves a tripped guard for the next leg.

This happened on the 0.0.90 gate (engine `d6866128`, bed A, invocation `20261026T045615Z-e31de8`):

1. In the `pat/on` leg, `TestMergeTrainRunawayGuardPausesBatch` recorded 6 trials on `handarbeit/fabrik-test-beta` between 10:19 and 10:33Z. This trips the guard, which is the test's purpose.
2. The bed switched to `app/on/webhook` at about 10:38Z.
3. `TestCrossRepoSpawn`'s child `fabrik-test-beta#633` reached Queued at 10:56Z. The engine logged `runaway guard already tripped for handarbeit/fabrik-test-beta (6 trial(s)) — pausing 1 Queued member(s) before dispatch`.
4. The child never landed and the test timed out: `--- FAIL: TestCrossRepoSpawn (3121.06s)`.

Whether this happens depends on leg order. It needs a guard-tripping leg followed by a train-on leg that queues on the same repo within the trial window (an hour). Earlier gates did not hit it. This is a gate-isolation defect, not an engine defect.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Each gate leg starts with clean train caps (Priority: P1)

A release engineer runs the live gate across several legs on one bed. Whatever an earlier leg did to the guard, the next leg's engine starts with no trial window, ejection counts or auto-repair attempts carried over.

**Why this priority**: This is the observed failure. Without it, a passing release can be blocked by an unrelated test's pause, and a ~10-hour run is wasted.

**Independent Test**: In a `tests/gate` unit test using a fake commander, seed the bed's engine-decisions directory with a train-caps file and a usage-limit file, perform the leg switch, and check that the train-caps file is gone and the usage-limit file is unchanged.

**Acceptance Scenarios**:

1. **Given** a bed whose `train-caps.json` holds a tripped trial window from the previous leg, **When** the gate switches the bed to the next leg, **Then** the next engine starts with no persisted train caps.
2. **Given** a bed whose `usage-limit.json` holds an active hold, **When** the gate switches legs, **Then** `usage-limit.json` is untouched.
3. **Given** a bed with no `train-caps.json` (first leg, or already clean), **When** the leg switch runs, **Then** it succeeds without error.
4. **Given** `gate reset` is run on a bed, **When** it finishes, **Then** the bed holds no `train-caps.json` and its `usage-limit.json` is unchanged.

---

### User Story 2 - The guard-trip test cleans up after itself (Priority: P2)

A test that deliberately trips the runaway guard or spends ejection counts leaves no persisted trial record for its repo. Later tests in the same leg are no more exposed than they were before #2349.

**Why this priority**: The leg-boundary clean (Story 1) does not protect later tests inside the same leg, and #2349 made that exposure worse by persisting the state.

**Independent Test**: Run the guard-trip test and check that, after it ends, the bed holds no persisted trial record for the repo it used.

**Acceptance Scenarios**:

1. **Given** `TestMergeTrainRunawayGuardPausesBatch` has tripped the guard, **When** the test ends (pass or fail), **Then** no persisted trial record remains for its repo.
2. **Given** another test that spends ejection counts on a member, **When** that test ends, **Then** no persisted count for that member remains.

---

### Edge Cases

- The leg switch or `gate reset` finds no train-caps file: nothing to remove, no error.
- The file cannot be removed (permissions, a directory in its place): the leg switch reports this clearly rather than starting the leg with stale caps.
- The bed is running when removal would happen: the file is removed only while the engine is stopped. Removing it under a running engine is useless because the engine rewrites it from memory.
- Multi-bed runs: each bed's file is cleaned at that bed's own leg switch, never another bed's.
- A resumed gate invocation (`--resume`) that skips completed legs still starts each leg it does run with empty caps.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every gate leg starts on a bed with no persisted train-cap state (trial windows, ejection counts, auto-repair attempts) left by an earlier leg on that bed.
- **FR-002**: The gate's leg switch removes the bed's `.fabrik/state/engine-decisions/train-caps.json` while the bed's engine is stopped, before the next engine starts.
- **FR-003**: `gate reset` removes the same file for each bed it resets.
- **FR-004**: Neither the leg switch nor `gate reset` removes or alters `.fabrik/state/engine-decisions/usage-limit.json`.
- **FR-005**: A missing train-caps file is not an error. A file that cannot be removed fails the leg switch loudly instead of continuing silently.
- **FR-006**: `TestMergeTrainRunawayGuardPausesBatch`, and any other live test that deliberately trips the guard or spends ejection counts, leaves no persisted trial or ejection record for its repo or members when it ends.
- **FR-007**: A test in the same leg that runs after a guard-tripping test is no more exposed to the tripped guard than it was before #2349.
- **FR-008**: The engine is not changed. Persistence of decision state across a real restart stays as ADR 2349 defines it.
- **FR-009**: A gate unit test (in `tests/gate`, fake commander, no bed, network or `gh`) shows that the leg switch removes the train-caps file and leaves the usage-limit file alone.
- **FR-010**: A variant of that test with the removal neutralised fails, showing the test is not vacuous.

### Key Entities

- **Train-caps file**: the engine's persisted record of per-member ejection counts, the trial-rate window and auto-repair attempts. It lives in the bed's engine-decisions state directory.
- **Usage-limit file**: the engine's persisted account-wide Claude usage-limit hold. It sits beside the train-caps file and is account state, not test state.
- **Leg switch**: the bed restart the gate performs between legs (`TestSwitchTrainMode`).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a leg that trips the runaway guard, the next leg on the same bed shows no `runaway guard already tripped` pause for a member that did not itself cause a trip.
- **SC-002**: The `tests/gate` unit test for the leg-switch cleanup passes on the fix and fails when the removal is neutralised.
- **SC-003**: The same unit test shows `usage-limit.json` byte-identical before and after the leg switch.
- **SC-004**: After the guard-trip live test ends, the bed holds no persisted trial record for the repo that test used.
- **SC-005**: A gate run whose leg order is `pat/on` then `app/on/webhook` completes `TestCrossRepoSpawn` without a guard-related pause.

## Assumptions

- Removing the file while the bed is stopped is enough: the engine, when it starts with no file, begins with an empty state (ADR 2349 treats a missing file as absent).
- The files that matter are only the train-caps file. Other engine-decisions files are account or other state and are left alone.
- The best place for the cleanup (the leg-switch path, the bed start path, or both, and how the live test cleans up after itself) is a Research decision. The requirement is the outcome, not the call site.
- The guard-trip test's cleanup can be done without an engine change, using only what the test harness can already do to the bed while it runs or at the leg boundary. If it cannot, that is a Research finding to raise, not a reason to change the engine here.
- Legs are not expected to share train-cap state on purpose. No live test relies on caps carried over from an earlier leg.

## Out of Scope

- Any change to the engine, to ADR 2349, or to production persistence behaviour.
- Clearing `usage-limit.json` or any other account-level state.
- Reordering legs or changing the gate's leg plan.
- Reducing the runaway guard's sensitivity or the length of its trial window.
- Isolating other kinds of cross-leg leakage (labels, board items, worktrees) not tied to the train-caps file.

## Source References

- #2349 / ADR 2349 — decision state that survives a restart
- Gate invocation `20261026T045615Z-e31de8`, engine `d6866128` (0.0.90)
