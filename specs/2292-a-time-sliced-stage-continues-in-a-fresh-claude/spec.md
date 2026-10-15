# Feature Specification: Time-sliced stage resumes its Claude session (fix #2232 regression)

**Feature Branch**: `fabrik/issue-2292`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

Since #2232 (v0.0.85), a stage whose run ended as a time slice (turn cap or `max_wall_time`) is redispatched on the next poll without the retry backoff, as intended. But the same change also stops the continuation from resuming the stage's Claude session, and ADR 2232 does not intend that.

`engine/item.go` (v0.0.85):

- ~696: `if snap.BackoffExempt(stage.Name) { lastAttempt = time.Time{} }` zeroes `lastAttempt` so that the backoff check below is skipped.
- ~889: `resume := !lastAttempt.IsZero()` reuses that same variable to decide whether to resume the session.

So every continuation after a slice starts a fresh session. It keeps the committed and WIP work in the worktree, but loses the conversation: what was tried, what's left, and the plan. Before #2232, a continuation after a turn cap resumed.

## Requirements

1. After a time slice, the next dispatch resumes the stage's session exactly as it did before #2232, while still skipping the retry backoff. Separate \"skip backoff\" from \"has run before\" so neither is derived from the other.
2. No other behaviour of #2232 changes (FR-006 dispatch ordering, the already-due cooldown, the log line).

## Acceptance

- A unit test or sim test where a stage ends on its turn cap: the next dispatch is not delayed by backoff **and** is invoked with resume. Prove the test isn't vacuous by restoring the zeroing and showing it fails.
- An ordinary incomplete run (not a slice) keeps its backoff and its resume, unchanged.

## Context

Found while checking #1909 against v0.0.85. #1909 asks for a *deliberate* fresh-session option for comment review and stage re-entry; this regression is an accidental fresh session on slice continuation. Fix this first; #1909's design can then decide whether slices should ever start fresh."

## Background

Since #2232 (v0.0.85, ADR 2232), a stage that ends as a time slice (a turn cap or `max_wall_time` stop) is redispatched on the next poll without waiting out the retry backoff. That is the intended behaviour. The same change also made the continuation start a fresh Claude session instead of resuming the stage's previous one.

The cause is that one piece of per-stage state, the time of the last attempt, does two jobs. Zeroing it is how a slice skips the backoff. The same value is also read as "this stage has run before, so resume its session". Zeroing it for the first purpose therefore silently turns off the second.

The continuation keeps the committed and WIP work in the worktree. It loses the conversation: what was tried, what remains, and the plan. A continuation after a turn cap resumed before #2232.

Related work: #1909 asks for a *deliberate* fresh-session option for comment review and stage re-entry. This bug is an *accidental* fresh session on slice continuation. It is fixed first, and #1909's design can then decide whether slices should ever start fresh.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A time-sliced stage continues in its own session (Priority: P1)

An operator has a stage that hits its turn cap or `max_wall_time` before finishing. On the next poll the engine redispatches it immediately, with no retry backoff, and the new invocation resumes the same Claude session. The agent keeps its conversation, plan and progress.

**Why this priority**: This is the regression. Without it every slice continuation re-derives context from the worktree alone, wasting turns and risking repeated or contradictory work.

**Independent Test**: Drive a stage to end on its turn cap. Poll again with no backoff wait. Assert that the next dispatch happens and that the Claude invocation is flagged as a resume of the existing session.

**Acceptance Scenarios**:

1. **Given** a stage whose previous run ended on its turn cap, **When** the next poll dispatches the stage, **Then** the dispatch is not delayed by retry backoff **and** the invocation resumes the stage's session.
2. **Given** a stage whose previous run ended on `max_wall_time`, **When** the next poll dispatches the stage, **Then** the dispatch is not delayed by retry backoff **and** the invocation resumes the stage's session.

---

### User Story 2 - An ordinary incomplete run is unchanged (Priority: P1)

A stage ends incomplete for a reason other than a time slice (for example, it ended without a completion marker). Its retry backoff still applies and its retry still resumes the session, exactly as before.

**Why this priority**: The fix must not change behaviour outside the slice path. Backoff protects the pipeline from tight retry loops.

**Independent Test**: End a stage incomplete without a slice signal. Verify that the next dispatch waits for the backoff and, once dispatched, resumes.

**Acceptance Scenarios**:

1. **Given** a stage that ended incomplete and was not a time slice, **When** a poll occurs before the retry backoff has elapsed, **Then** the stage is not dispatched.
2. **Given** the same stage once the backoff has elapsed, **When** the stage is dispatched, **Then** the invocation resumes the stage's session.

---

### Edge Cases

- A stage that has never run before: the first dispatch starts a fresh session, as today.
- A slice continuation that is itself sliced again: each further continuation still skips the backoff and still resumes.
- Dispatch ordering (longest-without-a-worker first), the already-due cooldown record and the `will retry on the next poll (time slice)` log line behave exactly as under #2232.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a stage run that ended as a time slice (turn cap or `max_wall_time`), the next dispatch of that stage resumes the stage's Claude session, as it did before #2232.
- **FR-002**: After a time slice, the next dispatch still skips the retry backoff, as specified by #2232.
- **FR-003**: Whether to skip the backoff and whether to resume the session are determined independently. Neither is derived from a value altered for the other.
- **FR-004**: A stage that ended incomplete without being a time slice keeps its retry backoff and its session resume, unchanged.
- **FR-005**: No other #2232 behaviour changes: the FR-006 candidate dispatch ordering, the already-due cooldown recorded for a slice, and the `will retry on the next poll (time slice)` log line.
- **FR-006**: An automated test covers a stage ending on its turn cap, asserting that the next dispatch is neither delayed by backoff nor started as a fresh session. The test fails when the bug is reintroduced (the backoff skip again clears the "has run before" information), which is demonstrated once as part of the change.
- **FR-007**: An automated test covers an ordinary incomplete run, asserting that backoff and resume are both preserved.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the slice-continuation test, 100% of redispatches after a turn-capped run are invoked as a resume of the existing session and are not delayed by backoff.
- **SC-002**: With the bug reintroduced, the slice-continuation test fails, so it is not vacuous.
- **SC-003**: All existing tests covering #2232 behaviour (dispatch ordering, already-due cooldown, log line) and ordinary-retry backoff pass without modification.

## Assumptions

- "Resumes exactly as it did before #2232" means the same session-resume decision a non-slice retry makes today, with no new resume rules.
- Time-slice detection (the structural turn-cap and `max_wall_time` signals) is correct as is; only its effect on session resume is wrong.
- Either a unit test or a sim test is acceptable, as the issue allows; the choice is left to implementation.

## Out of Scope *(optional)*

- A deliberate fresh-session option for comment review or stage re-entry (#1909).
- Deciding whether a slice should ever start fresh (#1909's design).
- Changes to retry backoff durations, dispatch ordering, the already-due cooldown or log wording.

## Source References *(optional)*

- #2232 / ADR 2232: retry backoff and time-slice handling (v0.0.85)
- #1909: deliberate fresh-session option
- `engine/item.go`: backoff exemption near line 697; session resume decision near line 890
