# Feature Specification: A reinvoke releases the store lock it acquires

**Feature Branch**: `fabrik/issue-2179`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

Every review/CI/rebase reinvoke leaves a lock marked held in the item store, with no lock label on GitHub and nothing that releases it.

`dispatchReinvoke` (`engine/reinvoke.go`, around line 189) applies `itemstate.LocalLockAcquired{..., Worker: ...}` to register the worker for heartbeat/liveness. That sets `Lock.HeldByThis = true` (`internal/itemstate/store.go`, `LocalLockAcquired`). The goroutine's defer applies only `WorkerExited`. `LocalLockReleased` is never applied, and the reinvoke never adds a `fabrik:locked:<id>` label.

Consequences:
- On shutdown, `cleanupLockedIssues` (`engine/poll.go`, around line 867) visits every item with `Lock.HeldByThis` and tries to remove a lock label that was never applied. That produced the burst of 404s observed at the 2026-10-11 shutdowns. #2159 suppresses the symptom (it skips absent labels and treats a 404 as success), but the store still claims locks it does not hold.
- Any reader of `snap.Lock()` (the attention classifier, the local API, the channel events) sees a stale \"held by this instance\" lock on every item that has ever been reinvoked.

Found during #2159 (ADR 2159, \"Follow-up (not fixed here)\").

## Requirements

- **R1:** A reinvoke releases what it acquires. When the reinvoke goroutine ends, by any path (success, error, did-not-run, ctx cancel), it releases the store lock it set.
- **R2:** The store lock matches GitHub. Either the reinvoke records a worker without claiming a lock (preferred if the liveness/heartbeat machinery needs only the `WorkerHandle`), or it acquires and releases the real lock label like a stage dispatch does. Research decides which, against what reinvoke concurrency actually relies on (`inFlight`, `processedSet`, the `fabrik:locked` label's cross-instance role).
- **R3:** `cleanupLockedIssues` at shutdown attempts no lock-label removal for an item whose only \"lock\" came from a reinvoke.
- **R4:** Docs. Update `docs/state-machine.md` (lock/worker lifecycle) and ADR 2159's follow-up note.

## Scope

- In scope: the reinvoke lock/worker lifecycle and its shutdown cleanup.
- Out of scope: the #2159 shutdown 404 handling, which stays as defence in depth.

## Acceptance

- Unit test: after a reinvoke completes (success, error and did-not-run cases), the item's `snap.Lock()` is nil, or reflects a real lock that was also released.
- Unit test: a shutdown after a completed reinvoke makes no `RemoveLabel` call for a lock label.
- Each test fails today.
- The full test suite passes."

## Background

Every review, CI-fix and rebase reinvoke registers its worker in the in-memory item store so that heartbeat and liveness tracking can see it. The way it does so also records the item as locked by this instance. No `fabrik:locked:<id>` label is ever added on GitHub for a reinvoke, and nothing ever clears the store's lock. The store therefore claims a lock the engine does not hold, and that claim persists for every item that has ever been reinvoked.

Two visible effects follow:

- **Shutdown noise.** Shutdown cleanup walks every item the store says is locked and tries to remove its lock label. For reinvoked items the label never existed, so GitHub answered 404. This produced a burst of 404s at the 2026-10-11 shutdowns. #2159 suppresses the symptom by skipping absent labels and treating a 404 as success, but the underlying false state remains.
- **Misleading readers.** Anything that reads an item's lock from the store (the attention classifier, the local API, the channel events) sees a stale "held by this instance" lock on every item that has been reinvoked.

This was found while fixing #2159 and recorded there as a follow-up that was deliberately not fixed. This issue is that follow-up. It is the root-cause fix, so the #2159 handling stays as defence in depth.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The store never shows a lock a reinvoke has finished with (Priority: P1)

An operator, or a component reading item state, looks at an item after a review, CI-fix or rebase reinvoke has finished. The item shows no lock held by this instance, unless a real lock was acquired and is still legitimately held.

**Why this priority**: This is the defect itself. Everything else (shutdown noise, misleading readers) is a consequence of the store lock outliving the reinvoke.

**Independent Test**: Run a reinvoke to completion against a test engine and a fake GitHub client, then inspect the item's lock in the store.

**Acceptance Scenarios**:

1. **Given** an item with no lock, **When** a reinvoke runs and completes successfully, **Then** the item's lock is nil afterwards.
2. **Given** an item with no lock, **When** a reinvoke runs and ends in an error, **Then** the item's lock is nil afterwards.
3. **Given** an item with no lock, **When** a reinvoke is dispatched but does not run (it bails out before invoking Claude), **Then** the item's lock is nil afterwards.
4. **Given** an item with no lock, **When** the engine's context is cancelled while a reinvoke is running, **Then** the item's lock is nil once the reinvoke goroutine has exited.

---

### User Story 2 - Shutdown does not try to remove labels that were never applied (Priority: P1)

An operator stops the engine after one or more reinvokes have completed. Shutdown cleanup makes no attempt to remove a lock label for items whose only "lock" came from a reinvoke, so the log and the GitHub audit trail show no 404s from this path.

**Why this priority**: This is the observed operational symptom, and it is the user-visible proof that the store matches GitHub.

**Independent Test**: Complete a reinvoke, run shutdown cleanup against a recording fake client, and assert on the recorded label removals.

**Acceptance Scenarios**:

1. **Given** an item whose reinvoke has completed, **When** shutdown cleanup runs, **Then** no `RemoveLabel` call for a lock label is made for that item.
2. **Given** an item that holds a real stage-dispatch lock and also had an earlier reinvoke, **When** shutdown cleanup runs, **Then** the real lock label is still removed.

---

### User Story 3 - A reinvoke running during a stage lock does not corrupt it (Priority: P2)

If a real lock exists on an item, a reinvoke's lifecycle neither overwrites that lock's store record nor clears it on exit.

**Why this priority**: The release fix must not trade a stale lock for a prematurely cleared real one. Whether this case can occur depends on how reinvokes and stage dispatches exclude each other, which Research must establish.

**Independent Test**: Hold a real lock on an item, run and finish a reinvoke on the same item, and check the lock is unchanged.

**Acceptance Scenarios**:

1. **Given** an item with a real lock held by this instance, **When** a reinvoke on that item starts and finishes, **Then** the real lock is still recorded and its label is unaffected.

---

### Edge Cases

- A reinvoke ends by every path: normal completion, error, a run that never invoked Claude, panic recovery if the goroutine has any, and context cancellation or shutdown.
- A reinvoke and a stage dispatch on the same item at the same time (if the engine can ever allow it).
- Several reinvokes on one item in sequence; none leaves residue that accumulates.
- Shutdown arriving while a reinvoke is still running: cleanup must still not attempt a label removal that was never applied.
- Items that were reinvoked before this fix: the store is in-memory only, so a restart clears any stale value and no migration is needed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a reinvoke ends by any path (success, error, did-not-run, context cancellation), any store lock that the reinvoke set is released.
- **FR-002**: After a reinvoke ends, the item's store lock is nil, or reflects a real lock that exists on GitHub and is still held.
- **FR-003**: At any moment, the store's lock for an item corresponds to a lock label actually applied on GitHub. Either the reinvoke registers its worker without claiming a lock, or it acquires and releases the real lock label as a stage dispatch does. Research selects the approach based on what reinvoke concurrency control (the in-flight tracking, the processed set and the cross-instance role of the lock label) actually depends on. If the worker can be registered without a lock, that is preferred.
- **FR-004**: Worker registration, heartbeat and liveness for reinvokes keep working: a running reinvoke is still visible as a worker, and the worker is still marked exited when the goroutine ends.
- **FR-005**: Shutdown cleanup attempts no lock-label removal for an item whose only recorded lock came from a reinvoke.
- **FR-006**: A real lock held on an item by a stage dispatch is never cleared or overwritten by a reinvoke's start or end.
- **FR-007**: The #2159 shutdown handling (skip absent labels, treat 404 as success) is left unchanged.
- **FR-008**: `docs/state-machine.md` describes the reinvoke lock/worker lifecycle as built. `docs/llms-full.txt` is regenerated in the same change, as the repo requires for canonical doc edits. The "Follow-up (not fixed here)" note in ADR 2159 is updated to say it is now resolved, naming this issue.
- **FR-009**: Regression tests cover the item lock after a reinvoke completes (success, error and did-not-run) and the absence of a lock-label `RemoveLabel` call at shutdown after a completed reinvoke. Each test fails on the current code and passes with the fix.

### Key Entities

- **Item lock (store)**: the in-memory record that this instance holds the lock on an item. It should mirror the `fabrik:locked:<id>` label on GitHub.
- **Worker handle**: the in-memory record that a worker is running on an item, used for heartbeat and liveness. It is separate from the lock record.
- **Reinvoke**: a review, CI-fix or rebase dispatch on an item parked at a completed stage.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After any reinvoke ends, by any path, the item's store lock is nil or reflects a lock that is genuinely held (verified by unit tests for the success, error and did-not-run cases).
- **SC-002**: A shutdown following a completed reinvoke produces zero lock-label removal attempts and zero 404s from that path.
- **SC-003**: The new regression tests fail on the code before the fix and pass after it.
- **SC-004**: The full test suite passes, including the race-detector run, with no change to existing reinvoke behaviour (dispatch, cooldowns, cycle limits).

## Assumptions

- The reinvoke needs a worker registration for heartbeat and liveness. It does not necessarily need a lock claim; Research confirms this.
- The store is in memory only, so stale locks from earlier runs vanish on restart and need no migration.
- ADR 2159 and the #2159 fix are not on the current base branch. If #2159 has landed by Implement time, the ADR 2159 note is updated in place. If not, the resolution is recorded in a new ADR 2179 that references #2159, and the follow-up note is updated when #2159 lands.
- The three reinvoke kinds (review, CI-fix, rebase) share one dispatch path, so a single fix covers all three.

## Out of Scope *(optional)*

- Changes to the #2159 shutdown 404 handling, which stays as defence in depth.
- Changes to how stage dispatch acquires and releases its real lock.
- Any change to reinvoke triggering, cooldowns or cycle limits.

## Source References *(optional)*

- #2159 and ADR 2159 ("Follow-up (not fixed here)")
- The reinvoke dispatch path in `engine/reinvoke.go`, the shutdown cleanup in `engine/poll.go`, and the item-store lock mutation in `internal/itemstate`
