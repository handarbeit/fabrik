# Feature Specification: Never block the poll loop on a worker slot

**Feature Branch**: `fabrik/issue-2114`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "poll: never block the poll loop on a worker slot; defer dispatch when all slots are busy

## Problem

`dispatchCandidates` (`engine/poll.go:1978`, acquire at `:2052-2056`) takes a worker slot with a blocking `select { case e.sem <- struct{}{}: case <-ctx.Done(): }` **inside the poll goroutine**. When all `max_concurrent` slots are busy and the poll has a dispatch candidate, the poll stops there until some worker exits. Everything `poll()` runs after dispatch (`poll.go:1730` onward) waits with it:

- `settleTrainRuns` / `handleMergeTrainBatch` (#2051's per-poll train evaluation);
- the queued-review, member-close, runaway-alert, non-default-base, PR-ready and landing-verification settle scans;
- the next poll altogether, including the CI gate and all other admission.

The stall lasts as long as the shortest-remaining worker. A worker that is alive but quiet extends it indefinitely. Reconcile and channel pushes run on separate loops, so the daemon looks alive throughout.

**Seen live on the dev daemon (`dev(3d0a73f)`, 2026-10-11):** the poll started at 12:38:25Z and logged `[awaiting-ci-settle] examined 1 fabrik:awaiting-ci item(s)` at 12:38:29Z. After that, for more than 10 minutes against `poll: 180`, the only lines were the once-a-minute `[#2096 worker-detector] stale heartbeat … process alive`. The TUI showed \"polling…\". The local API reported `last_poll_attempt_at` 12:38:25Z and `workers.in_use 5 of 5`, with slots held by #2091, #2097, #2110, #2112 and #2096 (a Validate comment-review worker). The candidates were two freshly released Specify items. GitHub was ruled out: REST 5000/5000, GraphQL about 4686, status operational.

The reinvoke dispatchers (`engine/reinvoke.go:160`) already acquire inside their own goroutine, not on the poll goroutine. The merge train's `acquireTrainSlot` (#2046) is also on a worker goroutine. `dispatchCandidates` is the only blocking acquire on the poll path.

## Requirements

**R1. Never block the poll on a worker slot.** `dispatchCandidates` acquires a slot non-blockingly. If none is free, the candidate is not dispatched in this poll and nothing is recorded against it: no `WorkerEntered`, no cooldown, no retry count, no labels. The rest of the candidate loop still runs, so a later candidate that is skipped for another reason costs nothing extra. Once the semaphore is full, the remaining candidates are simply deferred.

**R2. A freed slot re-polls promptly.** A worker exit already applies `WorkerExited`, which carries `WorkerLifecycleChanged`, part of `wakeChFlags` (`engine/observers.go`). That wakes the poll loop, so a deferred candidate is picked up on the poll that follows the exit, not a full poll interval later. Plan confirms this path covers every deferred-candidate case, including a candidate whose `itemMayNeedWork` admission depends on the cycle set (`cycleSetFlags` excludes `WorkerLifecycleChanged`). If it doesn't, Plan adds the minimum needed so a deferred candidate is not starved until an unrelated change.

**R3. Say so once.** When at least one candidate is deferred because all slots are busy, log one poll-level line naming the deferred issue numbers and `in_use/max`, rather than one line per item.

**R4. No regression in ordering or safety.** The `snap.Worker() != nil` guard, the R5 sentinel check (#1779) and `WorkerEntered`-before-goroutine ordering are unchanged. The release/`WorkerExited` defer order stays as documented.

**R5. Docs.** Update `docs/state-machine.md` and/or `docs/stage-lifecycle.md` where they describe dispatch and slot acquisition, and regenerate `docs/llms-full.txt` if a canonical page changes.

## Scope

- In scope: `dispatchCandidates`'s slot acquisition, the deferral log line, tests (unit and a `tests/sim` scenario), docs.
- Out of scope: the local API's `fabrik_health` showing \"poll in progress since X\" (tracked separately by the MCP in-situ test); changing `max_concurrent` defaults; the reinvoke and merge-train slot paths.

## Acceptance

- A unit or sim test with `max_concurrent` slots all held and a dispatchable candidate shows `poll()` returning, and the post-dispatch steps (for example a settle scan or `handleMergeTrainBatch`) running in the same poll, without any worker exiting. The test fails against the blocking acquire.
- A test shows that releasing one slot wakes the poll and dispatches the deferred candidate on the next poll.
- A deferred candidate has no cooldown, retry or label side effects."

## Background

The poll goroutine takes a worker slot with a blocking acquire when it dispatches a candidate. When every `max_concurrent` slot is held, the poll waits there until a worker exits. Everything that runs later in the same poll waits with it:

- the per-poll merge-train evaluation;
- the settle scans for queued review, member close, runaway alert, non-default base, PR ready, and landing verification;
- the next poll, including the CI gate and all other admission.

If a worker is alive but quiet, the stall has no upper bound. The reconcile and channel-push loops run separately, so the daemon still looks alive.

Observed on the dev daemon on 2026-10-11:

- **Poll log:** the poll started at 12:38:25Z and logged one `[awaiting-ci-settle]` line at 12:38:29Z. For the next 10+ minutes, against `poll: 180`, the log showed only periodic worker-detector heartbeats.
- **TUI and local API:** the TUI showed "polling…". The local API reported `last_poll_attempt_at` 12:38:25Z and 5 of 5 slots in use. The slots were held by #2091, #2097, #2110, #2112 and #2096 (a Validate comment-review worker).
- **Waiting candidates:** two freshly released Specify items.
- **GitHub ruled out:** REST 5000/5000, GraphQL about 4686, status operational.

The other slot paths already acquire on a worker goroutine: the reinvoke dispatchers and the merge train's per-invocation slot (#2046). The candidate dispatch loop is the only blocking acquire on the poll path.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Poll keeps running when all worker slots are busy (Priority: P1)

As an operator, I want the poll cycle to complete even when every worker slot is held. Settle scans, merge-train evaluation, the CI gate and admission of other items should keep running, and the daemon should not look alive while it is stuck.

**Why this priority**: This is the defect. A full pool currently freezes all engine-side housekeeping for as long as the shortest-remaining worker runs.

**Independent Test**: Hold all `max_concurrent` slots, make one item dispatchable, and run a poll. The poll returns, and a post-dispatch step (a settle scan or merge-train evaluation) has run in that same poll. No worker exits during the test.

**Acceptance Scenarios**:

1. **Given** all slots are held and a dispatchable candidate exists, **When** a poll runs, **Then** the poll completes and the post-dispatch steps run in that poll.
2. **Given** all slots are held and several candidates exist, **When** a poll runs, **Then** none is dispatched and all are deferred without blocking.
3. **Given** the pool is full and one candidate is skipped for an unrelated reason, **When** a poll runs, **Then** that skip costs nothing extra and the rest of the loop still runs.

---

### User Story 2 - A deferred candidate is picked up promptly when a slot frees (Priority: P1)

As an operator, I want a candidate deferred for lack of a slot to be dispatched on the poll right after a worker exits, not a full poll interval later.

**Why this priority**: Deferring is only safe if the deferred work resumes quickly. Otherwise the fix trades a stall for added latency.

**Independent Test**: With all slots held and a candidate deferred, release one slot. The worker-exit wake triggers a poll, and that poll dispatches the deferred candidate.

**Acceptance Scenarios**:

1. **Given** a candidate was deferred because the pool was full, **When** a worker exits and frees a slot, **Then** the next poll is requested immediately and dispatches the candidate.
2. **Given** a deferred candidate whose admission depends on the cycle set, **When** a worker exits, **Then** the candidate is still dispatched on the following poll rather than waiting for an unrelated change.

---

### User Story 3 - Deferral leaves no trace on the item and is logged once (Priority: P2)

As an operator, I want a deferred candidate to behave as if the poll never considered it. I also want one concise log line telling me work is waiting for a slot.

**Why this priority**: Recording attempts, cooldowns or labels for work that never started would distort retry accounting and could suppress the later dispatch. A single clear log line makes the condition diagnosable.

**Independent Test**: Defer a candidate, then inspect its state. It has no `WorkerEntered`, no cooldown, no retry count and no label changes. The poll log contains exactly one deferral line listing the deferred issue numbers and `in_use/max`.

**Acceptance Scenarios**:

1. **Given** a candidate is deferred, **When** its state is inspected, **Then** no worker-entered record, cooldown, retry increment or label change exists for it.
2. **Given** three candidates are deferred in one poll, **When** the log is read, **Then** one line names all three issue numbers and the slot usage.
3. **Given** no candidate is deferred, **When** a poll runs, **Then** no deferral line is logged.

---

### Edge Cases

- Context cancellation (shutdown) during a poll must still end dispatch cleanly, with no hang and no spurious deferral line.
- A slot that frees mid-loop may be taken by a later candidate in the same loop. Candidates the poll has already deferred are not re-attempted within that poll.
- A candidate rejected by the existing in-flight worker guard or the sentinel check is not "deferred for slots" and must not appear in the deferral log line.
- A pool that stays full for many polls defers the same candidates every time. Each poll logs at most one deferral line, with no per-item spam.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The poll's candidate dispatch acquires a worker slot without blocking. When no slot is free, the candidate is not dispatched in that poll.
- **FR-002**: A candidate deferred for lack of a slot has nothing recorded against it: no worker-entered record, no cooldown, no retry count and no label change.
- **FR-003**: The candidate loop continues after a deferral, so the remaining candidates are each evaluated for dispatch or deferral as usual. Once the pool is full, the remaining admitted candidates are deferred.
- **FR-004**: The poll's post-dispatch steps (merge-train evaluation, all settle scans) run in the same poll even when every slot is held and no worker has exited.
- **FR-005**: A worker exit causes a prompt re-poll, and a candidate deferred for lack of a slot is dispatched by the poll that follows, including candidates whose admission depends on the cycle set. If the existing wake path does not cover this, the minimum additional wake needed is added so a deferred candidate is not starved until an unrelated change.
- **FR-006**: When at least one candidate is deferred for lack of a slot, the poll emits exactly one poll-level log line naming the deferred issue numbers and the slot usage as `in_use/max`. No per-item deferral lines are emitted, and no line is emitted when nothing is deferred.
- **FR-007**: Existing dispatch ordering and safety behavior is unchanged: the in-flight worker guard, the sentinel check (#1779), recording the worker-entered state before the worker goroutine starts, and the documented order of slot release and worker-exit notification.
- **FR-008**: The reinvoke dispatchers and the merge train's per-invocation slot acquisition are not modified.
- **FR-009**: The canonical docs that describe dispatch and slot acquisition are updated to describe non-blocking acquisition and deferral. `docs/llms-full.txt` is regenerated in the same change if a canonical page changes.
- **FR-010**: Automated tests cover the following, and the first two fail against the blocking acquire:
  - the poll completing and running post-dispatch steps with all slots held;
  - a slot release waking the poll and dispatching the deferred candidate on the next poll;
  - a deferred candidate having no cooldown, retry or label side effects.

  The coverage includes at least one unit-level test and one `tests/sim` scenario.

### Key Entities

- **Worker slot**: one unit of the `max_concurrent` pool that a worker holds while it runs.
- **Dispatch candidate**: an item the poll has admitted for possible worker dispatch in this cycle.
- **Deferred candidate**: a candidate admitted but not dispatched because no slot was free. It leaves no recorded state and is retried on a later poll.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With all slots held and a dispatchable candidate present, a poll completes, and its post-dispatch steps run, without any worker exiting. This is verified by an automated test that fails against the blocking behavior.
- **SC-002**: After one slot is released, the deferred candidate is dispatched by the next poll with no intervening full poll interval, verified by an automated test.
- **SC-003**: A deferred candidate has zero cooldown, retry-count, worker-entered and label side effects, verified by an automated test.
- **SC-004**: A poll that defers N candidates emits exactly one deferral log line, and one that defers none emits none.
- **SC-005**: On a daemon with a full worker pool, the local API's `last_poll_attempt_at` advances every poll interval rather than stalling for the length of the longest-running worker.

## Assumptions

- Dispatching a candidate on the poll after a slot frees, rather than within the same poll, is acceptable. The existing worker-exit wake makes this near-immediate.
- The wake from a worker exit is already part of the poll loop's wake conditions. That is an assumption to be confirmed in Plan (see FR-005), not a fixed fact.
- Deferral order is the existing candidate order, so earlier candidates win freed slots first. No new fairness or priority policy is introduced.
- The deferral log line is operator-facing diagnostics only. Its exact wording is left to Plan, provided it carries the deferred issue numbers and `in_use/max`.

## Out of Scope

- The local API's `fabrik_health` reporting "poll in progress since X" (tracked separately by the MCP in-situ test).
- Changing `max_concurrent` defaults.
- Changes to the reinvoke dispatchers' slot acquisition.
- Changes to the merge train's slot acquisition.

## Source References

- Live incident on the dev daemon (`dev(3d0a73f)`), 2026-10-11, described in Background.
- `docs/state-machine.md`: the worker semaphore and dispatch sections.
