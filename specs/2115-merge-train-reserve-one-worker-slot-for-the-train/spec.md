# Feature Specification: Reserve one worker slot for the merge train on top of `max_concurrent`

**Feature Branch**: `fabrik/issue-2115`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "merge-train: reserve one worker slot for the train on top of max_concurrent, so stage work can never starve it

> **Problem** — The merge train shares the `max_concurrent` worker pool with stage workers. Since #2046 it takes a slot only around its Claude calls (`acquireTrainSlot`, `engine/merge_train.go:2661`): conflict resolution during trial assembly, `landGreenBatch`'s rebase, `landOneAtATime`, bisection and the singleton catch-up. When every slot is held by stage workers, the train waits. While it waits, Queued members aren't landing, so their PRs keep going stale against main, more members queue up behind them, and each later trial has more to reconcile and more CI to run. Starving the train causes CI pile-ups.
>
> ADR-2046 (decision 5) declined a dedicated pool "for no demonstrated need" and said to file it separately if saturation proved a problem. Running 0.0.84 on the dev daemon with five slots routinely saturated by stage workers has made it one, and #2114 (the poll stalling on a full semaphore) shows how often the pool is full. Report #2037 originally proposed a dedicated train slot as its proposal (2).
>
> **R1. A reserved train slot.** When `merge_train` is on, the engine provides one worker slot reserved for the merge train, in addition to `max_concurrent`. With `max_concurrent: 5`, up to five stage workers and the train's Claude call can run at once. Stage dispatch, reinvokes and comment processing can never take the reserved slot. With `merge_train` off, nothing changes.
>
> **R2. Several partitions.** Trains run one per (repo, base) partition (#1648), so two partitions can need a Claude call at the same time. Plan decides whether a train that finds the reserved slot busy waits for it, or may also take a free general slot. Either way, a train never waits on the general pool while the reserved slot is free, and stage work is never blocked by the reserved slot.
>
> **R3. Keep #2046's semantics.** `acquireTrainSlot`'s contract is unchanged: a cancelled wait is "not attempted" (no ejection, no `recordTrial`, no bisect fallback, no `dissolveBatch`, the catch-up defers uncharged). Suspension is re-checked after the wait. `waiting for slot` still feeds the TUI row (#2050) and the status line.
>
> **R4. Validate auto-repair.** #2045's red-singleton auto-repair re-enters Validate as an ordinary stage dispatch, which competes for general slots. Plan decides whether that Validate run should be able to use the reserved slot, recording why in the ADR. The default is no, since it is stage work, unless starving it demonstrably stalls the train.
>
> **R5. Visibility and docs.** The local API's `workers` and the TUI show the reserved slot separately from the general pool (for example `5/5 + train 1/1`). `docs/USER_GUIDE.md` states that the host can run `max_concurrent + 1` Claude processes when the merge train is on. An ADR amends ADR-2046 decision 5.
>
> **Scope** — In scope: the reserved slot, `acquireTrainSlot`, the worker-count display, tests (unit and a `tests/sim` scenario), docs, ADR. Out of scope: a configurable number of train slots (one is enough; it can be made configurable later if measured); #2114's non-blocking poll dispatch (a separate issue, though the two complement each other). Mention #2037 in the PR body for context. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2037. This issue's own number gets `Closes`.
>
> **Acceptance** — A test with all `max_concurrent` general slots held by stage workers shows a merge-train conflict resolution acquiring the reserved slot and running immediately. A test shows a stage dispatch never takes the reserved slot, even when it is idle. A test with `merge_train: off` shows the pool size and behaviour are unchanged. The existing #2046 cancellation and "not attempted" tests pass unchanged."

## Background

The merge train and stage workers draw from one `max_concurrent` pool. Since #2046 the train holds no slot while it waits on CI. It takes one only around each Claude call it makes: conflict resolution while assembling a trial, the rebase in `landGreenBatch`, `landOneAtATime`, bisection, and the singleton catch-up.

On a busy board every slot is routinely held by stage workers (observed on the dev daemon running 0.0.84 with five slots; #2114 shows the pool is often full). A train that needs a Claude call then waits for a stage worker to finish. While it waits:

- Queued members do not land, and their PRs go stale against the base branch.
- More members queue up behind them.
- Each later trial has more to reconcile and more CI to run, so CI piles up.

ADR-2046 decision 5 declined a dedicated train pool "for no demonstrated need" and said to file one separately if saturation delayed conflict resolution in practice. That has now happened. Report #2037 proposed a dedicated train slot as its proposal (2).

This change gives the train one worker slot of its own, on top of `max_concurrent`, so stage work can never starve it. Stage work also must not be blocked by that slot.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The train can always make progress on a saturated pool (Priority: P1)

An operator runs Fabrik with `merge_train` on and `max_concurrent: 5`, and five stage workers are running. A trial needs a conflict resolved by Claude. The train's Claude call starts immediately in the reserved slot instead of waiting for a stage worker to finish.

**Why this priority**: This is the whole point of the change. Without it the train still starves under load.

**Independent Test**: Hold all `max_concurrent` general slots with stage workers, then trigger a train conflict resolution. It acquires the reserved slot and runs without waiting.

**Acceptance Scenarios**:

1. **Given** `merge_train` is on and all general slots are held by stage workers, **When** the train needs a Claude call, **Then** it acquires the reserved slot and runs immediately.
2. **Given** the same state, **When** the train's call finishes, **Then** the reserved slot is released and available for the next train call.

---

### User Story 2 - Stage work is never blocked by, and never uses, the reserved slot (Priority: P1)

An operator wants the existing stage throughput unchanged. Stage dispatch, reinvokes (CI-fix, review, rebase) and comment processing use only the `max_concurrent` general pool, even while the reserved slot is idle. A train never takes a general slot away from stage work while the reserved slot is free.

**Why this priority**: If stage work could take the reserved slot it would no longer be reserved. If the reserved slot reduced the general pool, the change would be a regression.

**Independent Test**: With the reserved slot idle and all general slots held, a further stage dispatch still waits. It does not run in the reserved slot.

**Acceptance Scenarios**:

1. **Given** `merge_train` is on, the reserved slot is idle and all general slots are held, **When** a stage worker, reinvoke or comment-processing run is dispatched, **Then** it waits for a general slot and does not take the reserved slot.
2. **Given** a train Claude call is running in the reserved slot, **When** stage workers are dispatched, **Then** up to `max_concurrent` of them run alongside it.

---

### User Story 3 - Several partitions share one reserved slot predictably (Priority: P2)

Trains run per (repo, base) partition, so two partitions can need a Claude call at the same time. When the reserved slot is busy, a partition's train does not deadlock or starve. It waits for the reserved slot, or the plan may let it use a free general slot. Either way, a train never waits on the general pool while the reserved slot is free.

**Why this priority**: Multi-partition setups are less common than a single train, but the slot behaviour must be defined for them.

**Independent Test**: Two partitions request a Claude call at once. Both calls eventually run, only one holds the reserved slot at a time, and neither waits on the general pool while the reserved slot is free.

**Acceptance Scenarios**:

1. **Given** partition A's train holds the reserved slot, **When** partition B's train needs a Claude call, **Then** it waits (for the reserved slot, or for a free general slot if the plan allows that fallback) and runs once one is available.
2. **Given** the reserved slot is free, **When** any train needs a Claude call, **Then** it does not block on the general pool.

---

### User Story 4 - Existing cancellation and suspension semantics are preserved (Priority: P1)

A train's wait for a slot can be cancelled by shutdown or a worker-context cancel. Behaviour stays exactly as ADR-2046 defines it: a cancelled wait is "not attempted", with no ejection, no `recordTrial`, no bisect fallback and no `dissolveBatch`, and the catch-up defers uncharged. The account-wide Claude suspension is re-checked after the wait. The `waiting for slot` phase still appears in the TUI row and the status line.

**Why this priority**: These behaviours protect members from wrongful ejection. A regression there would be worse than the starvation being fixed.

**Independent Test**: The existing #2046 cancellation and "not attempted" tests pass unchanged.

**Acceptance Scenarios**:

1. **Given** a train is waiting for a slot, **When** its context is cancelled, **Then** the call is "not attempted" and no member is ejected, no trial is recorded, no bisect fallback starts and no batch is dissolved.
2. **Given** a train waited for a slot and the account became suspended meanwhile, **When** the slot is obtained, **Then** the suspension is re-checked and the Claude call is not made.
3. **Given** a train must wait for a slot, **When** the wait begins, **Then** `waiting for slot` is shown in the TUI row and the status line, as today.

---

### User Story 5 - The operator can see and plan for the extra slot (Priority: P2)

An operator looking at the local API's `workers` result or the TUI sees the reserved slot reported separately from the general pool (for example `5/5 + train 1/1`). The user guide states that with the merge train on, the host can run `max_concurrent + 1` Claude processes.

**Why this priority**: The extra process affects host capacity planning. Reporting it separately keeps the general-pool figure meaningful.

**Independent Test**: With a train Claude call running and general slots full, the local API and TUI show the general pool and the reserved slot as separate counts.

**Acceptance Scenarios**:

1. **Given** `merge_train` is on, **When** the operator views worker usage in the TUI or the local API `workers`, **Then** the reserved slot's use is shown separately from the general pool.
2. **Given** the updated user guide, **When** an operator reads the `max_concurrent` documentation, **Then** it says that `max_concurrent + 1` Claude processes can run when the merge train is on.

---

### User Story 6 - `merge_train` off is unchanged (Priority: P1)

An operator with `merge_train` off sees no change: the pool size is `max_concurrent`, no reserved slot exists, and nothing new appears in the worker display.

**Why this priority**: The feature must not affect installations that do not use the train.

**Independent Test**: With `merge_train` off, the semaphore capacity, the worker-count display and the dispatch behaviour match the pre-change values.

**Acceptance Scenarios**:

1. **Given** `merge_train` is off, **When** the engine starts, **Then** the general pool has `max_concurrent` slots and no reserved slot exists.
2. **Given** `merge_train` is off, **When** worker usage is displayed, **Then** it shows only the general pool, as before.

---

### Edge Cases

- Two or more partitions request a train Claude call while the reserved slot is busy. This must neither deadlock nor starve a partition indefinitely. A cancelled wait is "not attempted".
- The reserved slot is held while the account-wide Claude usage-limit suspension begins. Suspension is re-checked after the wait, as in #2046.
- The reserved slot is released exactly once even if the release function is called twice (as today's `sync.Once`).
- `max_concurrent` is 1: the general pool has one slot, the reserved slot is separate, and the train still runs while that one stage worker is busy.
- The engine restarts mid-wait: no slot state is persisted, and the restart-resume behaviour of #2051 is unchanged.
- A Validate run started by #2045's red-singleton auto-repair is an ordinary stage dispatch and takes a general slot (see Assumptions).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When `merge_train` is on, the engine provides one worker slot reserved for the merge train in addition to the `max_concurrent` general pool. The total number of concurrent Claude workers can therefore reach `max_concurrent + 1`.
- **FR-002**: Stage dispatch, reinvokes (CI-fix, review, rebase and similar) and comment processing never acquire the reserved slot, including when it is idle.
- **FR-003**: A merge-train Claude call (via `acquireTrainSlot`) takes the reserved slot when it is free and does not wait on the general pool while the reserved slot is free.
- **FR-004**: When the reserved slot is busy, for example because another (repo, base) partition's train holds it, a train's behaviour is defined by the plan: it either waits for the reserved slot or may also take a free general slot. In either case it does not deadlock, and stage work is never blocked by the reserved slot.
- **FR-005**: With `merge_train` off, no reserved slot exists and the pool size and dispatch behaviour are identical to the behaviour before this change.
- **FR-006**: `acquireTrainSlot`'s contract from #2046 is unchanged. A wait cancelled by the worker context returns the "not attempted" error. That causes no ejection, no `recordTrial`, no bisect fallback and no `dissolveBatch`, and the singleton catch-up defers uncharged.
- **FR-007**: Account-wide Claude suspension is re-checked after a slot wait, as in #2046, before the Claude call is made.
- **FR-008**: The `waiting for slot` phase continues to feed the TUI train row (#2050) and the status line whenever a train must wait for a slot.
- **FR-009**: The local API's `workers` result and the TUI show the reserved train slot's usage separately from the general pool (for example `5/5 + train 1/1`). With `merge_train` off, only the general pool is shown.
- **FR-010**: `docs/USER_GUIDE.md` states that with the merge train on, the host can run `max_concurrent + 1` Claude processes, and it updates the existing statements about how the train counts against `max_concurrent`. `docs/llms-full.txt` is regenerated in the same commit, along with any other canonical doc page that is touched.
- **FR-011**: A new ADR, numbered `adrs/2115-…md`, amends ADR-2046 decision 5 (no dedicated pool). It records the reason, the R2 choice (wait for the reserved slot versus also take a free general slot) and the R4 choice (whether auto-repair Validate may use the reserved slot), each with its rationale.
- **FR-012**: Tests cover: (a) a train Claude call acquiring the reserved slot and running immediately while all general slots are held by stage workers; (b) a stage dispatch never taking the reserved slot while it is idle; (c) `merge_train: off` leaving the pool size and behaviour unchanged; (d) a `tests/sim` scenario for the saturated-pool case. The existing #2046 cancellation and "not attempted" tests pass without modification.
- **FR-013**: The reserved slot is a fixed count of one. There is no new configuration key, flag or environment variable for the number of train slots.

### Key Entities *(if applicable)*

- **General worker pool**: the `max_concurrent` slots shared by stage dispatch, reinvokes and comment processing.
- **Reserved train slot**: one additional slot, present only when `merge_train` is on, usable only by merge-train Claude calls.
- **Train partition**: a (repo, base) pair with its own merge-train run (#1648); several partitions share the one reserved slot.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `merge_train` on and every general slot held by stage workers, a train conflict-resolution call starts without waiting for any stage worker to finish.
- **SC-002**: With `merge_train` on, the number of concurrent stage workers that can run is unchanged (`max_concurrent`), and no stage-dispatch, reinvoke or comment-processing run is ever observed holding the reserved slot.
- **SC-003**: With `merge_train` off, the worker pool capacity and observable dispatch behaviour are identical to before the change.
- **SC-004**: All pre-existing #2046 slot-cancellation and "not attempted" tests pass unmodified.
- **SC-005**: An operator reading the TUI or the local API `workers` result can tell general-pool usage from reserved-slot usage at a glance, and the user guide documents the `max_concurrent + 1` process ceiling.

## Assumptions

- The reserved slot count is exactly one. A configurable count is a later change if measurement justifies it.
- Plan chooses the multi-partition policy (wait for the reserved slot, or also take a free general slot) and records it in the ADR. This spec requires only that a train never waits on the general pool while the reserved slot is free and that stage work is never blocked by the reserved slot.
- #2045's red-singleton auto-repair Validate run is an ordinary stage dispatch and uses a general slot. Plan may reverse this only if the ADR records demonstrated train stalling from starving it. Absent that, the default is no.
- The reserved slot is a worker-concurrency control only. It does not change what the train does, its decisions, or its persisted state.
- Slot occupancy is in-memory only and is not persisted across restarts.
- Hosts running with `merge_train` on can absorb one extra concurrent Claude process.

## Out of Scope *(optional)*

- A configurable number of train slots (`merge_train_slots` or similar).
- #2114's non-blocking poll dispatch when the semaphore is full (a separate issue that complements this one).
- Changing when the train takes a slot, or any train decision logic (ejection, bisection, catch-up).
- Giving any non-train work (stage dispatch, reinvokes, comment processing) access to the reserved slot, except as Plan may justify for auto-repair Validate under R4.
- Mentioning #2037 in the PR body is for context only; the PR must not use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2037. Only this issue's own number gets `Closes`.

## Source References *(optional)*

- ADR-2046 (`adrs/2046-merge-train-slot-only-around-claude-calls.md`), decision 5: no dedicated train pool, to be revisited if saturation proved a problem.
- #2037: original report; its proposal (2) was a dedicated train slot.
- #2046: the train takes a slot only around Claude calls (`acquireTrainSlot`).
- #2045: red-singleton auto-repair, re-enters Validate as an ordinary stage dispatch.
- #1648: merge-train partitions per (repo, base).
- #2050: TUI train row and `waiting for slot` phase.
- #2114: poll stalling on a full semaphore.
