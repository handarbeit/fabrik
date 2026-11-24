# Feature Specification: Count live worker turns as progress so long healthy runs are not reported stalled

**Feature Branch**: `fabrik/issue-2604`
**Created**: 2026-11-21
**Status**: Specified
**Input**: User description: the original issue body, verbatim:

```text
## Problem

The `stalled` / `worker-no-progress` attention state fires on healthy workers. A typical push:

> [stalled] liminisapp/liminis-context-graph#850: … has stalled: a worker is in flight but nothing observable changed for 30m20s

At that moment the worker had pushed commits and was at turn 86 of 100. Receiving sessions routinely judge these alerts false alarms, so the signal is noise and a real stall would be ignored.

### Cause

- `progressAnchor` (`internal/attention/attention.go`) ignores live turns. The engine records every turn (`liveTurnRecorder` → `itemstate.WorkerTurnsRecorded`, `engine/attention_workers.go`), but the classifier uses the count only for display ("turn 86 of 100"). `WorkerTurnsRecorded` stores no timestamp.
- With no label, Status or attempt change during a run, the anchor stays at worker start. `workerOverdue` then flags the item once the run passes the stall threshold (30m) and `WorkerBudget + WorkerStallMargin`. When the stage has no `max_wall_time`, the budget is the 15m inactivity timeout, so **every run longer than 30m is flagged**.
- The claim is self-contradictory: a worker past the 15m inactivity timeout that has not been killed is, by construction, still producing output.

## Requirements

- **R1.** Record the time of the latest turn on the live worker (`WorkerHandle.LastTurnAt`, stamped from the store clock when `WorkerTurnsRecorded` raises the turn count; the zero-turn reset at invocation start clears it). Memory only.
- **R2.** Pass it into `attention.Input` (e.g. `WorkerLastTurnAt`) and include it in `progressAnchor`. A turn is real model output, unlike the excluded `Worker.LastSignAt` ticker heartbeat, so it is a valid progress signal. A zero/unknown value contributes nothing.
- **R3.** Optionally count a change of the linked PR head SHA observed during the run as progress too (covers a worker whose turns were not recorded). Decide in Plan; record the decision in the ADR.
- **R4.** With R2, `worker-no-progress` fires only when no turn has arrived for longer than the stall threshold, i.e. the inactivity kill should have ended the worker and did not. The summary must say that, naming the last-turn age and the inactivity timeout, e.g. "no turn for 31m (inactivity timeout 15m did not end it)". Where turns are unknown, keep today's wording.
- **R5.** A long healthy run stays `working` with the existing "(long-running: …)" note; no `stalled` push is sent.
- **R6.** No change to retry accounting, kill behaviour or any engine decision: this is classification and wording only.

## Scope

In: `internal/itemstate` (worker handle, `WorkerTurnsRecorded` apply), `engine/attention_workers.go` (input fill), `internal/attention` (anchor, overdue, summary), `docs/state-machine.md` §7.13 (regenerate `docs/llms-full.txt`), ADR numbered after this issue.

Out: changing the stall threshold, the inactivity timeout or `max_wall_time` defaults.

## Acceptance

- Unit tests in `internal/attention`: a worker past 30m with a recent turn is `working`; with the last turn older than the threshold it is `stalled` with the R4 wording; unknown turns keep today's behaviour.
- Store test: `WorkerTurnsRecorded` stamps `LastTurnAt` only when the count rises; the zero-turn call clears it.
- **Non-vacuity:** a neutralisation seam (e.g. `SetTurnProgressDisabledForTest`) that drops `LastTurnAt` from the input makes the healthy-long-run test report `stalled` again.
- Sim twin in `tests/sim`: a worker taking turns past 30m produces no `stalled` channel event; one whose turns stop does.
- `go test -race ./...` and `go vet ./...` pass.
```

## Background

Operators and overseer sessions receive `stalled` pushes ("a worker is in flight but nothing observable changed for 30m20s") for workers that are healthy: in the reported case the worker had pushed commits and was at turn 86 of 100. Receiving sessions routinely judge these alerts false alarms, so the signal has become noise and a genuine stall would likely be ignored too.

The cause is that the attention classifier's notion of "progress" never sees live worker activity. The engine already records every turn a worker takes, but the classifier uses that count only for display ("turn 86 of 100") and the recording carries no timestamp. During a run no label, board Status or attempt record changes, so the progress anchor stays at worker start. Once the run passes the stall threshold (30 min) and the worker's time budget plus margin, the item is flagged. Where a stage sets no wall-time cap, the budget is the 15-minute inactivity timeout, so every run longer than 30 minutes is flagged regardless of how healthy it is.

The claim is also self-contradictory: a worker that has run past the 15-minute inactivity timeout and has not been killed is, by construction, still producing output. A genuine "stalled" verdict should mean the inactivity kill should have ended the worker and did not.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A long healthy run is not reported stalled (Priority: P1)

An operator or overseer session watching the pipeline sees an item whose worker has been running for over 30 minutes and is still taking turns. The item stays classified as working with the existing long-running note, and no `stalled` push is sent.

**Why this priority**: This is the reported defect. Every long run currently produces a false alarm, which is what makes the signal untrustworthy.

**Independent Test**: Put an item with a live worker past 30 minutes, whose most recent turn is recent, through the classifier and through a sim run; it is `working` and no `stalled` event is emitted.

**Acceptance Scenarios**:

1. **Given** a worker in flight for more than the stall threshold with a turn recorded within the threshold, **When** the item is classified, **Then** it is `working` with the existing "(long-running: …)" note.
2. **Given** the same worker, **When** the channel deriver evaluates it over time, **Then** no `stalled` push is sent while turns keep arriving.

---

### User Story 2 - A worker whose turns stop is still reported stalled, with an honest reason (Priority: P1)

When a worker has produced no turn for longer than the stall threshold (so the inactivity timeout should have killed it but did not), the item is reported `stalled` and the summary says exactly that.

**Why this priority**: The fix must not silence real stalls; it must make the remaining verdict meaningful.

**Independent Test**: Classify an item whose last recorded turn is older than the stall threshold; it is `stalled` and the summary names the last-turn age and the inactivity timeout.

**Acceptance Scenarios**:

1. **Given** a worker in flight whose latest turn is older than the stall threshold, **When** the item is classified, **Then** it is `stalled` with a summary such as "no turn for 31m (inactivity timeout 15m did not end it)".
2. **Given** a worker in flight for which no turn has ever been recorded (turns unknown), **When** the item is classified, **Then** behaviour and wording are exactly as today.

---

### User Story 3 - Classification only; no engine behaviour changes (Priority: P2)

A maintainer can rely on this change affecting only what the attention view and channel report, not what the engine does with a worker.

**Why this priority**: It bounds the risk of a change in a state machine that governs retries and kills.

**Independent Test**: Existing retry, kill and dispatch tests pass unchanged; the new field is read only by the attention input fill.

**Acceptance Scenarios**:

1. **Given** any worker state, **When** turn timestamps are recorded, **Then** retry counters, kill timing and dispatch decisions are identical to before.

---

### Edge Cases

- A worker that has taken no turn yet (just started, or the per-invocation turn count was reset to zero) contributes no progress signal beyond today's; its turn time is unknown, not "now".
- A turn count that is re-reported at the same value (no rise) does not refresh the last-turn time, so a stuck worker cannot appear alive through repeated no-op recordings.
- The reset at the start of a new invocation clears the previous invocation's last-turn time.
- A worker with a recent turn but no other change must not be reported stalled, even past the stall threshold and past its time budget.
- A worker whose turn recording is unavailable (turns unknown) keeps today's verdict and wording; it is neither newly silenced nor newly flagged.
- Restart: last-turn time is memory only; after a restart there is no turn time until the next turn, which behaves as "unknown".

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The live worker record MUST carry the time of its latest turn (`WorkerHandle.LastTurnAt`), stamped from the store clock only when a turn-count update raises the count; a count that does not rise leaves it unchanged, and the zero-turn reset at invocation start clears it. It is held in memory only.
- **FR-002**: The attention classifier input MUST carry that time (e.g. `attention.Input.WorkerLastTurnAt`), filled by the engine's attention input builder, and the progress anchor MUST include it. A zero/unknown value contributes nothing.
- **FR-003**: The latest-turn time MUST be treated as a valid progress signal, distinct from and not subject to the exclusion of the worker's ticker heartbeat (`Worker.LastSignAt`), which remains excluded.
- **FR-004**: The `worker-no-progress` stalled verdict MUST fire only when no turn has arrived for longer than the stall threshold while turns are known.
- **FR-005**: The stalled summary, where turns are known, MUST name the age of the last turn and the inactivity timeout and state that the timeout did not end the worker (e.g. "no turn for 31m (inactivity timeout 15m did not end it)"). Where turns are unknown, the existing wording MUST be kept.
- **FR-006**: An item with a live worker whose latest turn is within the stall threshold MUST stay `working` with the existing "(long-running: …)" note, and MUST NOT produce a `stalled` channel push.
- **FR-007**: The change MUST NOT alter retry accounting, kill behaviour, dispatch, or any other engine decision; it affects classification and wording only.
- **FR-008**: A test-only neutralisation seam (e.g. `SetTurnProgressDisabledForest` naming per the existing `Set…DisabledForTest` convention, `SetTurnProgressDisabledForTest`) MUST drop the last-turn time from the classifier input so a test can show that, without it, the healthy long run reads `stalled` again.
- **FR-009**: Whether a change of the linked PR head SHA observed during a run also counts as progress (covering a worker whose turns are not recorded) is decided in the Plan stage and recorded in the ADR; this spec does not require it.
- **FR-010**: The behaviour MUST be documented in `docs/state-machine.md` §7.13 with `docs/llms-full.txt` regenerated in the same commit, and an ADR numbered after this issue (`adrs/2604-…`) MUST record the decisions, including the FR-009 outcome.

### Key Entities *(if applicable)*

- **Live worker record**: the in-memory description of a running worker for an item (start time, turn count, kind). Gains the time of its latest turn.
- **Attention input / progress anchor**: the plain-value input to the classifier and the latest-of-several-times used to decide how long an item has made no observable progress.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A worker running longer than the stall threshold that has taken a turn within the threshold is reported `working` and generates zero `stalled` pushes over its whole run.
- **SC-002**: A worker whose most recent turn is older than the stall threshold is reported `stalled`, with a summary that states the last-turn age and the inactivity timeout.
- **SC-003**: For a worker with unknown turns, the classification and wording are byte-identical to before the change.
- **SC-004**: With the neutralisation seam active, the healthy-long-run test reports `stalled` again (non-vacuity), and with it inactive it reports `working`.
- **SC-005**: The full existing test suite (`go test -race ./...`) and `go vet ./...` pass with no change to any retry, kill or dispatch test.

## Assumptions

- The stall threshold (30 minutes), the inactivity timeout (15 minutes) and `max_wall_time` defaults stay as they are; only what counts as progress changes.
- The turn count is already recorded reliably for the workers this applies to; a worker without recorded turns is the "unknown" case and keeps today's behaviour.
- The store clock (not the host clock) is the time source for the stamp, so sim tests that drive the clock behave deterministically.
- The last-turn time needs no persistence: after a restart it is unknown until the next turn.
- The linked-PR-head-change signal (R3 in the original request) is optional and is a Plan-stage decision, so it is recorded as an assumption rather than an open question.

## Out of Scope *(optional)*

- Changing the stall threshold, the inactivity timeout, or `max_wall_time` defaults.
- Any change to retry accounting, the inactivity kill, or any other engine decision.
- Persisting the last-turn time across restarts.
- Reclassifying anything other than the `worker-no-progress` stalled verdict.

## Source References *(optional)*

- `docs/state-machine.md` §7.13 (attention classifier) and §7.14 (channel events); `internal/attention` progress anchor and overdue logic; `internal/itemstate` worker handle and turn-count apply; `engine/attention_workers.go` (turn recorder and input fill); ADR 2121 (worker rows).
