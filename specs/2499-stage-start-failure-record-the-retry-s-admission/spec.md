# Feature Specification: Stage start failure records its retry's admission cooldown, and a persistent one is bounded

**Feature Branch**: `fabrik/issue-2499`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

When a stage's Claude process fails to start (`exec.Error`, `exec.ErrNotFound`, `os.PathError`), `processItem` sets `claudeRan = false` (`engine/item.go`, the start-failure classification ahead of the cancellation early-out) and the retry branch logs `stage %q did not start — will retry on the next poll`. But it records no admission cooldown: `recordPostRunRetryCooldown` is called only `if claudeRan`, and no `StageAttempted` is stamped either.

Since #2232 nothing else admits the item, so the retry waits for the periodic re-evaluation (10 × PollSeconds) — the same gap `recordRetryBackoffCooldown`'s comment in `engine/poll.go` describes for another exit. The log line is therefore false on this path. The report shows it on `totalslacker/chronicle` #491: `exec: \"claude\": executable file not found in $PATH`, retried about 5.5 minutes later.

A start failure that persists (binary really missing) also has no bound and no operator signal: it would retry silently forever.

## Requirements

1. The did-not-start branch records an admission cooldown so the item is re-admitted at the time the log line states: an already-due (or few-second) `retry-backoff`, through the existing `recordStageRetryCooldown`/`recordPostRunRetryCooldown` path. The log line and the recorded instant agree (#2232 FR-004).
2. A persistent start failure is bounded and visible. Research picks the shape (e.g. a backoff growing per consecutive start failure, and/or an operator warnings-panel entry via `warnings.Report` naming the error, resolved on the next successful start) and records it in the ADR. It must not count against `max_retries` or apply `stage:<name>:failed` — the stage never ran.
3. No change to how a start failure is classified, nor to the paths where Claude ran.

## Acceptance

- Unit test: a start failure records an admission cooldown due within the stated interval; neutralising the record leaves the item to the periodic re-evaluation.
- Unit/sim test for the persistent case: repeated start failures do not loop at poll rate and surface the chosen signal; a later successful start clears it.
- `CLAUDE.md` (#2232 admission-cooldown bullet) and `docs/state-machine.md` updated; `docs/llms-full.txt` regenerated.

## Notes

- Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`), follow-up comment https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6428197162.
- Gap in #2232 (every engine-scheduled retry owns an admission cooldown).
- Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only."

## Background

Since #2232, every engine-scheduled retry owns an admission cooldown: when the engine decides a stage will be retried, it records a `retry-backoff` cooldown at the decision point, because nothing else will bring the item back to the front of the queue. The exits that follow a Claude run (incomplete stage, API error, usage limit, PR-creation failure) already do this.

The "Claude process failed to start" exit was missed. When the stage's Claude process cannot be launched (executable not found, path error, other exec-level error), the engine classifies the attempt as "Claude did not run", logs that the stage "did not start — will retry on the next poll", and records neither a cooldown nor a stage-attempt stamp. With no cooldown, nothing re-admits the item until the slow periodic safety-net re-evaluation (10 × the poll interval) fires. The log line is therefore false on this path. A real occurrence was observed on `totalslacker/chronicle` #491 (`exec: "claude": executable file not found in $PATH`), where the retry came about 5.5 minutes later (community report #2453).

Fixing the timing alone creates a second problem: if the start failure is persistent (the binary really is missing or unexecutable), a prompt retry would loop at poll rate, and even today's slow retry has no upper bound and no operator-visible signal. The failure would repeat silently forever. The stage never ran, so it must not consume the stage's `max_retries` budget or be marked `stage:<name>:failed`; it needs its own bound and its own visible signal.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A transient start failure is retried when the log says it will be (Priority: P1)

An operator's daemon hits a one-off failure launching Claude for a stage (for example a brief `PATH` problem). The engine says it will retry on the next poll, and it does.

**Why this priority**: This is the reported defect — the engine's own log statement is false and the item stalls for minutes with no reason given.

**Independent Test**: Force a stage start failure once, then let the next launch succeed; observe that the item is re-admitted at the time the log line states rather than at the periodic re-evaluation.

**Acceptance Scenarios**:

1. **Given** a stage whose Claude process fails to start, **When** the engine handles the failure, **Then** it records an admission cooldown that falls due at the interval stated in the log line (an already-due or few-second `retry-backoff`), and the log line and the recorded instant agree.
2. **Given** that cooldown has expired, **When** the next poll runs, **Then** the item is re-admitted and the stage is started again without waiting for the periodic re-evaluation.
3. **Given** the cooldown recording is neutralised (test seam), **When** a start failure occurs, **Then** the item is left to the periodic re-evaluation — demonstrating the test actually exercises the new behaviour.
4. **Given** a start failure, **When** the retry is scheduled, **Then** `max_retries` is not incremented and `stage:<name>:failed` is not applied.

---

### User Story 2 - A persistent start failure is bounded and visible (Priority: P1)

An operator's Claude binary is genuinely missing or not executable. The daemon must not hammer the launch at poll rate, and the operator must be told what is wrong without reading daemon logs.

**Why this priority**: Without a bound, fixing Story 1 turns a silent slow loop into a fast one; the fix is incomplete and potentially harmful without this.

**Independent Test**: Make every launch fail repeatedly; observe that retries slow down over consecutive failures (not poll-rate) and that an operator-visible signal naming the error appears. Then make a launch succeed and observe the signal clear.

**Acceptance Scenarios**:

1. **Given** repeated consecutive start failures for the same cause, **When** the engine schedules each retry, **Then** the retries do not occur at poll rate; the delay between attempts is bounded in a way that reflects persistence (the exact shape is chosen in Research and recorded in the ADR).
2. **Given** a persistent start failure, **When** it is detected as persistent, **Then** the operator is shown a signal naming the error (for example `exec: "claude": executable file not found in $PATH`) through the operator warnings channel or an equivalent chosen in Research.
3. **Given** a start failure signal is active, **When** a later launch of Claude succeeds, **Then** the signal is cleared and the failure count resets.
4. **Given** any number of consecutive start failures, **When** the item is retried, **Then** the stage's `max_retries` budget is never consumed and `stage:<name>:failed` is never applied, because the stage never ran.

---

### Edge Cases

- A start failure on one item must not suppress or delay unrelated items; a persistent missing binary affects every item, and the signal should not be duplicated per item.
- A cancellation or shutdown that coincides with a start failure keeps its existing handling (the cancellation early-out is unchanged).
- A start failure followed by a successful start on a different item (for example the binary was fixed) clears the signal.
- A daemon restart resets in-memory failure counts; the first post-restart failure starts the backoff again from its initial value.
- Run-time failures after Claude has started (non-zero exit, API error, usage limit, turn cap) are unaffected, even if their error text resembles a start error.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a stage's Claude process fails to start, the engine MUST record an admission `retry-backoff` cooldown for the item at the decision point, through the existing `recordStageRetryCooldown`/`recordPostRunRetryCooldown` path, due at the interval the "will retry" log line states (an already-due or few-second cooldown, subject to FR-004).
- **FR-002**: The "did not start — will retry" log line and the recorded cooldown instant MUST agree (#2232 FR-004).
- **FR-003**: A start failure MUST NOT increment the stage's retry count against `max_retries`, MUST NOT apply `stage:<name>:failed`, and MUST NOT pause the item — the stage never ran.
- **FR-004**: A persistent start failure MUST be bounded: consecutive start failures MUST NOT cause retries at poll rate. Research selects the mechanism (for example a delay growing per consecutive start failure up to a cap) and records it in the ADR.
- **FR-005**: A persistent start failure MUST surface an operator-visible signal naming the error, through the operator warnings channel (`warnings.Report`) or an alternative selected in Research and recorded in the ADR; the signal MUST be resolved on the next successful Claude start.
- **FR-006**: The classification of a start failure (`exec.Error`, `exec.ErrNotFound`, `os.PathError`) and the cancellation early-out ahead of it MUST NOT change.
- **FR-007**: Paths where Claude ran (incomplete stage, API error, usage limit, PR-creation failure, time-slice stops) MUST NOT change behaviour.
- **FR-008**: The change MUST be covered by tests: a unit test that a start failure records a cooldown due within the stated interval (and that neutralising the record leaves the item to the periodic re-evaluation), and a unit or sim test for the persistent case showing no poll-rate looping, the chosen signal surfacing, and a later successful start clearing it.
- **FR-009**: Documentation MUST be updated: the `CLAUDE.md` #2232 admission-cooldown bullet and `docs/state-machine.md`, with `docs/llms-full.txt` regenerated; the chosen shape for FR-004/FR-005 MUST be recorded in an ADR numbered after this issue (`adrs/2499-…`).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a transient start failure, the affected item is re-admitted within the interval stated in the log line (seconds, not the 10 × poll-interval periodic re-evaluation, which was ~5.5 minutes in the reported case).
- **SC-002**: With a Claude binary that never starts, the number of launch attempts per item over a fixed observation window is bounded well below one per poll, and the delay between attempts does not shrink.
- **SC-003**: Within one attempt cycle of a persistent start failure being recognised, an operator can see the failing error text without reading daemon logs; after the next successful start, the signal is gone.
- **SC-004**: Across any number of consecutive start failures, the item's retry count and `stage:<name>:failed` label are unchanged.
- **SC-005**: No existing test for Claude-ran retry paths or start-failure classification changes outcome.

## Assumptions

- "Persistent" is determined by counting consecutive start failures, kept in memory (a restart resets it); persisting this state across restarts is not required.
- Start failures are overwhelmingly environmental and shared across items, so the backoff and signal may be keyed per daemon or per item — Research decides — as long as FR-004/FR-005 hold.
- Re-using the existing operator warnings channel is the preferred signal because it is the established path for operator-level conditions; Research may choose differently with justification in the ADR.
- A few-second (or already-due) first retry is acceptable for the first failure; growth only begins on consecutive failures.

## Out of Scope *(optional)*

- Changing how a start failure is classified, or what counts as one.
- Changes to retry behaviour on any path where Claude ran.
- Preflight validation of the Claude binary at daemon startup.
- Closing community report #2453: it is closed by hand when a release containing the fix ships; the PR for this issue closes this work issue only and mentions #2453 by bare number, with no closing keyword.

## Source References *(optional)*

- Community report #2453 (flow analysis of `totalslacker/chronicle`) and its follow-up comment: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6428197162
- #2232 (every engine-scheduled retry owns an admission cooldown) — the invariant this path missed.
