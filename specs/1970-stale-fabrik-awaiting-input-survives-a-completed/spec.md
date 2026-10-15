# Feature Specification: Stale `fabrik:awaiting-input` survives a completed comment-review run

**Feature Branch**: `fabrik/issue-1970`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

An operator has reported that `fabrik:awaiting-input` (with `fabrik:paused`) sometimes survives a comment-review run that completed the requested work and did not emit `FABRIK_BLOCKED_ON_INPUT`. The item then stays blocked until someone removes the labels by hand. The report is in #1966's notes, mentioned for context; this issue is independent of that feature.

No minimal reproduction exists yet. The expected behavior is that a comment-review run which does not end blocked on input leaves no `fabrik:awaiting-input` behind. An exception is when a different pause cause is still in force, and then the label must carry that cause's explanatory comment.

## Requirements

- **R1 — Find the path.** Trace every writer and remover of `fabrik:awaiting-input` across `processComments`, the stage-dispatch path and the reinvoke family (ci-fix, review-reinvoke). Identify each path where a run can finish without `FABRIK_BLOCKED_ON_INPUT` and still leave the label applied. Candidates:
  - the label is applied by an earlier pause and never cleared by the resuming run's success path;
  - a race between the resume gate removing the label and a concurrent writer re-adding it;
  - a failure classification (turn limit, tools denied, resume failure) that re-pauses after real work completed.
- **R2 — Reproduce it** with a unit or sim test (`tests/sim`) that fails on current `main`.
- **R3 — Fix it** so that a completed run that is not blocked on input clears `fabrik:awaiting-input`, unless another documented pause cause still applies.
- **R4 — Document it.** Update `docs/state-machine.md` §4.4 if the clearing rule changes, and regenerate `docs/llms-full.txt`.

## Scope

In scope: the stale-label cause and its fix. Out of scope: any MCP/notification work.

**PR linkage:** mention #1966 for context only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near #1966.

## Acceptance

- A test that reproduces the stale label fails on `main` and passes with the fix, and is shown to be non-vacuous by neutralizing the fix.
- A run that genuinely emits `FABRIK_BLOCKED_ON_INPUT` still pauses with the label, as before.
- If R1 finds no reachable path, the PR says so with the trace, and the issue is closed as not reproducible rather than patched on speculation."

## Background

An operator reported that an item carrying the engine's pause pair (`fabrik:paused` + `fabrik:awaiting-input`) sometimes stays blocked after a comment-review run that did the requested work and never asked for input. Nothing in the run signals a block, yet the item sits idle until a person removes the labels by hand. That is a silent stall: the pipeline looks like it is waiting on a human who has nothing left to answer.

No minimal reproduction exists yet, so this issue is a bug hunt as well as a fix. The investigation must be done against current `main`. Two changes merged on 2026-10-14 altered when comment passes run and when items pause: #2220 (a stage runs before pending comments, `stageRanOnEntry`) and #2259 (two-phase SIGHUP drain). A trace made against the code as it stood when this was filed may be stale.

Three candidate causes are suspected, none yet confirmed:

1. The label is applied by an earlier pause and the resuming run's success path never clears it.
2. A race between the resume gate removing the label and a concurrent writer re-adding it.
3. A failure classification (turn limit, tools denied, resume failure) re-pauses the item after real work completed.

This issue is independent of the MCP/notification work in #1966, which is mentioned for context only.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Find and prove the stale-label path (Priority: P1)

An engine maintainer needs to know whether a reachable code path exists on current `main` that leaves `fabrik:awaiting-input` set after a run that finished without `FABRIK_BLOCKED_ON_INPUT`, and if so, to have it captured as a failing test.

**Why this priority**: Nothing else is actionable until the cause is known. The issue explicitly forbids patching on speculation.

**Independent Test**: The trace of every writer and remover of the label can be reviewed on its own, and the reproducing test can be run on its own against unmodified `main`.

**Acceptance Scenarios**:

1. **Given** current `main`, **When** every place that adds or removes `fabrik:awaiting-input` is traced across comment processing, stage dispatch and the reinvoke family (ci-fix, review-reinvoke), **Then** each path that can end a run without `FABRIK_BLOCKED_ON_INPUT` while leaving the label applied is identified, or the trace shows there is none.
2. **Given** a path is found, **When** a sim or unit test drives it, **Then** the test fails on unmodified `main` by observing the stale label.
3. **Given** no reachable path is found, **When** the work concludes, **Then** the PR states this with the trace and the issue is closed as not reproducible, with no speculative code change.

---

### User Story 2 - A completed, unblocked run clears the pause (Priority: P1)

An operator who answers an item's question by comment expects the item to resume, finish the requested work, and not remain flagged as waiting for input.

**Why this priority**: This is the user-visible defect.

**Independent Test**: Resume a paused item with a human comment, let the comment-review run complete without a blocked-on-input marker, and check the item's labels afterwards.

**Acceptance Scenarios**:

1. **Given** an item paused with `fabrik:paused` + `fabrik:awaiting-input`, **When** a human comment resumes it and the comment-review run completes without `FABRIK_BLOCKED_ON_INPUT`, **Then** neither `fabrik:awaiting-input` nor the `fabrik:paused` it accompanies remains.
2. **Given** the same resume, **When** the run also hits a failure classification that is documented as a pause cause, **Then** the pause is kept and the explanatory comment for that cause is posted.
3. **Given** a run that emits `FABRIK_BLOCKED_ON_INPUT`, **When** it ends, **Then** the item is paused with `fabrik:awaiting-input` exactly as before.

---

### User Story 3 - The clearing rule is documented (Priority: P2)

A maintainer reading the as-built docs should find the rule for when `fabrik:awaiting-input` is cleared.

**Why this priority**: The state-machine doc is the as-built specification and must match behavior.

**Independent Test**: Read the comment-processing section of the state-machine doc and the regenerated documentation bundle.

**Acceptance Scenarios**:

1. **Given** the fix changes the clearing rule, **When** the PR is reviewed, **Then** `docs/state-machine.md` §4.4 describes the rule and `docs/llms-full.txt` is regenerated to match.

---

### Edge Cases

- A run that completes real work and then is classified as tools-denied, turn-limit or resume-failure: the label may be kept only where that classification is a documented pause cause, and then with its explanatory comment. A completed run must not be re-paused silently.
- A resume gate removing the pause pair while a concurrent writer (poll goroutine vs. worker goroutine) re-adds it.
- Changes from #2220 (stage runs first when `stageRanOnEntry` is false) and #2259 (SIGHUP drain, shutdown-time pause) altering which pass resumes the item or re-pauses it.
- A shutdown-time engine pause (`daemon-stop`) followed by a resume: the pair must not be treated as a stale label from a completed run.
- Stage dispatch and the reinvoke family (ci-fix-reinvoke, review-reinvoke) share the same label semantics, so any rule applied to comment review must hold for them too.
- A human who removes the labels by hand while a run is in flight.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The investigation MUST enumerate every writer and remover of `fabrik:awaiting-input` on current `main`, across comment processing, stage dispatch and the reinvoke family (ci-fix, review-reinvoke), and record which paths can finish a run without `FABRIK_BLOCKED_ON_INPUT` while leaving the label set. It MUST evaluate the three candidate causes named in the Background and take the post-#2220 and post-#2259 code as its baseline.
- **FR-002**: A reproducing test (unit or `tests/sim`) MUST exist for each reachable path found, and MUST fail on unmodified `main`.
- **FR-003**: A comment-review run that completes without emitting `FABRIK_BLOCKED_ON_INPUT` MUST NOT leave `fabrik:awaiting-input` behind, unless another documented pause cause still applies.
- **FR-004**: Where `fabrik:awaiting-input` is kept for a different documented pause cause, the item MUST carry that cause's explanatory comment.
- **FR-005**: A run that emits `FABRIK_BLOCKED_ON_INPUT` MUST still pause the item with `fabrik:paused` + `fabrik:awaiting-input`, unchanged.
- **FR-006**: The fix MUST NOT weaken the existing pause-resume rule (a paused item is resumed only by a human comment created at or after the pause) or the circuit breakers and retry counters that bound re-pausing.
- **FR-007**: If the investigation finds no reachable path, the PR MUST say so with the trace, and the issue MUST be closed as not reproducible with no code change made on speculation.
- **FR-008**: If the clearing rule changes, `docs/state-machine.md` §4.4 MUST be updated and `docs/llms-full.txt` regenerated in the same change.
- **FR-009**: The PR body MUST mention #1966 by bare number for context only and MUST NOT place a closing keyword (`Closes`/`Fixes`/`Resolves`) near it.

### Key Entities *(if applicable)*

- **Pause pair**: `fabrik:paused` + `fabrik:awaiting-input`, applied together by the engine; `awaiting-input` always co-occurs with `paused`.
- **Comment-review run**: an invocation that processes human comments on an item (also the vehicle for ci-fix and review reinvokes).
- **`FABRIK_BLOCKED_ON_INPUT`**: the marker by which a run states it needs input before it can continue.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every reachable stale-label path identified, a test reproduces the stale label on unmodified `main`, passes with the fix, and fails again when the fix is neutralized (shown non-vacuous).
- **SC-002**: After a human comment resumes a paused item and the run completes without a blocked-on-input marker, the item shows no stale `fabrik:awaiting-input` unless a documented pause cause applies, in which case its explanatory comment is present.
- **SC-003**: A run that emits `FABRIK_BLOCKED_ON_INPUT` pauses with the label in 100% of existing test scenarios covering that case, with no behavior change.
- **SC-004**: The full unit and sim suites pass, with no new regressions in resume, circuit-breaker or retry behavior.
- **SC-005**: Either a documented fix and updated state-machine doc land, or the PR records a trace showing no reachable path and the issue is closed as not reproducible.

## Assumptions

- The defect is real until the trace shows otherwise. This issue takes no side before the investigation and does not presuppose that a fix is needed.
- "Another documented pause cause" means a pause the state-machine doc already describes (for example turn-limit, tools-denied or resume-failure bounds, cycle limits, shutdown pauses). The fix does not invent new causes.
- The trace is made against `main` at or after the merges of #2220 and #2259.
- No change is made to the MCP/notification surface; #1966 is a separate feature.
- The existing rule that only a human comment at or after the pause resumes an item stays as is.

## Out of Scope *(optional)*

- Any MCP or notification work, including #1966.
- Changing the pause-resume rule or the circuit-breaker thresholds.
- Adding new pause causes or new labels.
- Fixing unrelated label-leak classes that the trace may turn up; these are filed as separate issues.

## Source References *(optional)*

- #1966 (context only; no closing keyword).
- #2220 (stage runs before pending comments) and #2259 (two-phase SIGHUP drain), merged 2026-10-14.
- `docs/state-machine.md` §4.4, Comment Processing Entry Points.
