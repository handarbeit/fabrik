# Feature Specification: MCP overseer attention for non-stage workers, slot-queued work, waiting-for-dispatch items, and state-change causes

**Feature Branch**: `fabrik/issue-2121`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `fabrik_status` and `fabrik_board`'s attention verdict ("stuck, or waiting correctly?") is wrong whenever a worker is anything other than a plain stage run. Those are the busy moments when an overseer most needs it.
>
> These were observed in an in-situ test against the live dev daemon on 2026-10-11. Each was confirmed against GitHub and the daemon log.
>
> - **F4: a comment-review worker shown as a stage run, waiting on CI that has already failed (#2096).**
>   - **Status said:** `worker.in_flight: true, stage: Validate`, `pr.ci: red`, and the summary "waiting for CI on the linked PR (stage completion is deferred until it is green)".
>   - **In reality:** the worker was a comment review (`fabrik:editing`, no `stage:Validate:in_progress`) responding to a 12:12 comment, and CI had already failed.
>   - **Also:** the live worker reported `turns_used: "unknown"` eight minutes in.
> - **F15: a running review-reinvoke shown as waiting on a cooldown (#2110).**
>   - **Status said:** `worker.in_flight: true` alongside `attention.state: waiting`, `cooldown:feedback-pending`, "re-dispatched at 13:36:40Z".
>   - **In reality:** the log showed `review-reinvoke re-invoking stage "Review" … (review-body:…)`, with Claude invoked at 13:08:49Z.
>   - **Also:** `fabrik:reworking:<Stage>` has no entry in `reasons`, although every other state-bearing label has one.
> - **F16: a reinvoke waiting for a worker slot shown as running, with `in_use` over the maximum (#2110).**
>   - **Shown:** `fabrik_health` said `workers.in_use: 6, max_concurrent: 5`, and `fabrik_status` showed in flight since 13:09:53Z.
>   - **In reality:** for about 110 s there was no process and no lock. The item was queued.
>   - **Cause:** `dispatchReinvoke` (`engine/reinvoke.go`) applies `itemstate.WorkerEntered` on the poll goroutine *before* its goroutine acquires `e.sem`.
> - **F10: a cruise item waiting for a slot shown as idle (#2113).**
>   - **Shown:** `idle: nothing to do right now`.
>   - **In reality:** it was in Specify with cruise and waiting for dispatch while 5 of 5 slots were busy.
>   - **Effect:** an overseer who has just set cruise reads this as "the label was ignored".
>
> ## Requirements
>
> - **R1 — Recognise every kind of worker.** The worker record and attention distinguish: stage run; comment review; review reinvoke; CI-fix reinvoke; rebase reinvoke; merge-train conflict resolution. The worker's kind and trigger (comment id, review-body id) are reported. Any of these in progress is `working`, with a summary naming what it is addressing.
> - **R2 — Queued is not running.** `WorkerEntered`, or an equivalent record, distinguishes *queued for a slot* from *holding a slot*. Status reports `queued: waiting for a worker slot (since …)` until the slot is acquired, and `started_at` is the slot-acquired time. `fabrik_health.workers` reports `in_use` (held slots, never more than `max_concurrent`) and a separate `queued` count. Fix the ordering in `dispatchReinvoke`, and check the other dispatch paths for the same ordering.
> - **R3 — Waiting for dispatch.** An item that has work to do (its stage is not complete, it has autonomy or is otherwise dispatchable, and nothing gates it) but no worker is reported as `waiting for dispatch`. The reason is one of: all N slots busy; the next poll; a cooldown that will really re-dispatch it. It is never `idle`.
> - **R4 — A cooldown is not the verdict while a worker runs.** When a worker is in flight or queued, attention reports that, not a dispatch cooldown.
> - **R5 — Every state-bearing label has a meaning.** `fabrik:reworking:<Stage>` and any other `fabrik:*` label the engine sets get a `reasons` entry. Add a test that enumerates the label constants so that a new label without a meaning fails.
> - **R6 — Turns.** A live worker reports its turns used so far when the engine knows them, otherwise `unknown`.
> - **R7 — Docs.** `docs/state-machine.md` §7.13. The `docs/USER_GUIDE.md` MCP section. Regenerate `docs/llms-full.txt`.
>
> ## Scope
>
> In scope: the attention classification of worker kinds, queueing and waiting for dispatch; worker-record ordering; health worker counts.
>
> Out of scope: the pending-decision state (separate issue); PR/CI fields (separate issue); the poll loop blocking on the semaphore (#2114).
>
> ## Acceptance
>
> All of the following are shown non-vacuous:
> - **Classifier unit tests** for each worker kind: in flight → `working` with the kind named, and never `cooldown:*`.
> - **Engine test:** a reinvoke dispatched with all slots held is reported as queued, not in flight; `in_use` stays at or below `max_concurrent`; and once the slot is acquired it is in flight, with `started_at` set to that time.
> - **Engine test:** a cruise item with an incomplete stage and all slots busy is reported as waiting for dispatch with the "all slots busy" reason, not `idle`.
> - **Labels:** the label-meaning enumeration test fails if a `fabrik:*` constant lacks a meaning.
> - `go test -race ./...` is clean, and the local-API and channel no-GitHub scans stay green.
>
> Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

`fabrik_status` and `fabrik_board` give an overseer (a person or an agent supervising the daemon over MCP) a verdict on each item: is it stuck, or waiting correctly? That verdict is wrong whenever the worker is anything other than a plain stage run. Those are the busy moments when an overseer most needs it to be right.

An in-situ test against the live dev daemon on 2026-10-11 found four defects, each confirmed against GitHub and the daemon log:

- **F4 — a comment-review worker shown as a stage run (#2096).** Status showed a Validate stage run "waiting for CI" with CI red. The worker was really a comment review responding to a comment, and CI had already failed. The live worker also reported `turns_used: "unknown"` eight minutes in.
- **F15 — a running review-reinvoke shown as waiting on a cooldown (#2110).** Status showed a worker in flight and, at the same time, `waiting` on `cooldown:feedback-pending`. The log showed a review-reinvoke already running. `fabrik:reworking:<Stage>` also had no `reasons` entry, although every other state-bearing label has one.
- **F16 — a reinvoke waiting for a worker slot shown as running (#2110).** `fabrik_health` reported more slots in use than the maximum (6 of 5), and status showed the item in flight since a time for which there was in fact no process and no lock, for about 110 seconds. The worker was recorded as entered on the poll side before it had acquired a slot.
- **F10 — a cruise item waiting for a slot shown as idle (#2113).** An item in Specify with cruise, waiting for dispatch while 5 of 5 slots were busy, was reported as "nothing to do right now". An overseer who has just set cruise reads that as "the label was ignored".

Later evidence from the same session adds two cases where attention gave no reason although the engine knew one:

- **Starved retry after a kill.** Three items were killed by the wall-time limit or hit the turn limit. Each logged a retry shortly afterwards, then waited over 30 minutes for a worker slot. Status reported `stalled`, "no worker, no wait … nothing scheduled", with no reasons. The facts were in the last invocation record (errored, about 3600 s), but attention never named the kill or the pending retry. The in-progress and lock labels stayed on the item and nothing explained them.
- **Feedback-gate hold.** An item was held over 35 minutes by "holding advance — 1 unprocessed review body" and reported as `stalled / stage-complete-not-advanced` with no reasons.

The maintainer-approved overseer proposal adds a general requirement: every state change says why. The cause is stated when the engine knows it, and omitted rather than guessed when it does not.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Every kind of worker is recognised and named (Priority: P1)

An overseer checks an item with a worker running. Whether that worker is a stage run, a comment review, a review reinvoke, a CI-fix reinvoke, a rebase reinvoke or a merge-train conflict resolution, the overseer sees it as working, sees which kind it is, what triggered it (for example the comment or review-body it is addressing), and how many turns it has used so far if the engine knows.

**Why this priority**: This is the core defect (F4, F15). Wrong verdicts during active work send the overseer to intervene on healthy items.

**Independent Test**: For each worker kind, build the classifier input for an item with that worker in flight, plus a competing dispatch cooldown, CI state or label that would previously have produced a different verdict. Assert the result is working, names the kind and trigger, and carries no cooldown reason.

**Acceptance Scenarios**:

1. **Given** an item with a comment-review worker in flight and CI red, **When** the overseer reads its status, **Then** it is reported as working on a comment review naming the comment, not as a stage run waiting for CI.
2. **Given** an item with a review-reinvoke worker in flight and a feedback-pending cooldown recorded, **When** status is read, **Then** it is reported as working on a review reinvoke naming the review body, and the cooldown is not the verdict.
3. **Given** a live worker whose turns used so far are known, **When** status is read, **Then** the turns used are reported; **Given** they are not known, **Then** the value is `unknown`.
4. **Given** an item carrying `fabrik:reworking:<Stage>`, **When** status is read, **Then** the label has a human-readable meaning among the reasons.

---

### User Story 2 - Queued for a slot is not running (Priority: P1)

An overseer looking at a worker that has been dispatched but has not yet got a worker slot sees "queued: waiting for a worker slot (since …)". Once the slot is acquired the item is in flight, and the start time is the time the slot was acquired. Health reports held slots and queued workers separately, and held slots never exceed the maximum.

**Why this priority**: Without this, health over-reports use (6 of 5) and status claims a process that does not exist (F16).

**Independent Test**: Dispatch a reinvoke with every slot held. Verify it is reported as queued and not in flight, held slots stay at or below the maximum and the queued count is 1. Release a slot and verify the item becomes in flight with its start time equal to the acquisition time.

**Acceptance Scenarios**:

1. **Given** all worker slots are held, **When** a reinvoke is dispatched for an item, **Then** status reports it queued with a since-time, no in-flight worker, and health reports held slots at most the maximum and a queued count including it.
2. **Given** that queued reinvoke, **When** a slot is released and the reinvoke acquires it, **Then** the item is reported in flight with start time equal to the acquisition time, and the queued count drops.
3. **Given** any other dispatch path that records a worker before waiting for a slot, **When** all slots are held, **Then** it is reported as queued in the same way.

---

### User Story 3 - Work waiting for dispatch is never idle (Priority: P1)

An overseer sets cruise on an item that has an incomplete stage while every worker slot is busy. Status reports the item as waiting for dispatch, with the reason: all slots busy, the next poll, or a cooldown that will really re-dispatch it. It does not say idle.

**Why this priority**: Idle on dispatchable work reads as "the label was ignored" (F10) and invites needless intervention.

**Independent Test**: Seed a cruise item with an incomplete stage, hold all slots, and read status; then free the slots and let the item sit until the next poll, and read again.

**Acceptance Scenarios**:

1. **Given** a cruise item with an incomplete stage, nothing gating it and no worker, with all slots busy, **When** status is read, **Then** it is reported as waiting for dispatch with the reason "all slots busy" (naming the slot count).
2. **Given** the same item with a free slot and no dispatch yet, **When** status is read, **Then** it is waiting for dispatch with the reason "next poll".
3. **Given** an item whose only pending dispatch is a cooldown that will really re-dispatch it, **When** status is read, **Then** the reason names that cooldown and when it re-dispatches.
4. **Given** an item that is gated (paused, blocked, awaiting a wait label), **When** status is read, **Then** it is not reported as waiting for dispatch.

---

### User Story 4 - Retries after a kill and feedback-gate holds are explained (Priority: P2)

An overseer looking at an item whose last invocation was killed (by the wall-time limit or the turn limit) and which has a retry pending sees a retry waiting for a slot, with a reason naming the kill and the retry, not a stall. For an item that is held from advancing by the feedback gate, the overseer sees that as the reason it has not advanced, naming the kind and ID of the pending feedback, and not a stall.

**Why this priority**: These are real stall false-positives observed in the session; they cost the overseer 30 to 35 minutes of confusion each.

**Independent Test**: Classify an item whose last invocation was killed with a retry pending, and an item with a recorded gate hold on an unprocessed review body. Assert neither is `stalled`, and each reason names the cause.

**Acceptance Scenarios**:

1. **Given** an item whose last invocation was killed by the wall-time limit with a retry scheduled and no free slot, **When** status is read, **Then** it is reported as a retry waiting for a slot, with a reason naming the wall-time kill and the retry.
2. **Given** the same with the turn limit, **Then** the reason names the turn-limit kill and the retry.
3. **Given** a stage-complete item held by the feedback gate on one unprocessed review body, **When** status is read, **Then** the reason names that feedback kind and ID as why it has not advanced, and the item is not `stalled`.
4. **Given** an item killed with no retry remaining or scheduled, **When** status is read, **Then** it is not described as a pending retry.

---

### User Story 5 - Every state change says why (Priority: P2)

An overseer (or a derived push event) sees, with each attention state, a `cause` when the engine knows one, drawn from a fixed, documented vocabulary. When the engine does not know, `cause` is absent.

**Why this priority**: The maintainer has approved it as a requirement; it lets the overseer distinguish expected waits from faults without reading logs.

**Independent Test**: For each cause in the vocabulary, classify inputs that establish it and assert the cause; remove those inputs and assert `cause` is absent.

**Acceptance Scenarios**:

1. **Given** inputs establishing a wall-time kill, **When** classified, **Then** `cause` is `wall-time-kill`; **Given** those inputs missing, **Then** `cause` is absent.
2. **Given** each other vocabulary entry with its establishing inputs, **When** classified, **Then** the matching cause is reported, and absent when the inputs are missing.
3. **Given** a state change published as a derived push, **When** the engine knows the cause, **Then** the push carries the same `cause` that status would.

---

### User Story 6 - Every state-bearing label has a meaning, and the docs say so (Priority: P3)

Every `fabrik:*` label the engine sets has a human-readable meaning in the attention reasons, and adding a new label without one fails the test suite. The as-built state-machine doc and the user guide's MCP section describe the new behavior.

**Why this priority**: Prevents the `fabrik:reworking:<Stage>` gap from recurring and keeps the docs honest.

**Independent Test**: Add a new `fabrik:*` label constant without a meaning and confirm the enumeration test fails; remove it and confirm it passes.

**Acceptance Scenarios**:

1. **Given** the full set of `fabrik:*` label constants, **When** the enumeration test runs, **Then** each has a meaning, or is on an explicit audited exemption list with a reason.
2. **Given** a label constant with no meaning, **When** the test runs, **Then** it fails naming the label.

---

### Edge Cases

- A queued worker's item changes while it waits (paused, closed, moved columns): the queued record must not outlive the work, and the item must not be reported as queued afterwards.
- A worker finishes between the classifier's snapshot of the worker record and of slot counts: held slots must still never be reported above the maximum.
- Several workers queue for slots at once: the queued count is the total, and each item shows its own since-time.
- An item has both an in-flight worker and a dispatch cooldown (F15): the worker wins, and no cooldown reason is shown.
- A cooldown that will not really re-dispatch the item (for example one consumed on admission, or one on an item that is gated) is not offered as the reason for waiting for dispatch.
- A killed invocation whose retries are exhausted or escalated is not shown as a pending retry.
- A feedback-gate hold on several pieces of feedback names at least the first and says how many more.
- A worker kind or trigger the engine cannot determine is reported as unknown for that field, and the item is still reported as working.
- The cause is unknown (inputs missing, or cause not in the vocabulary): `cause` is absent, never guessed.
- Turns used unavailable for a live worker: `unknown`, never zero.
- Stale `stage:*:in_progress` and lock labels left after a kill are explained as such alongside the retry reason, not left unexplained.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The worker record and the attention classification MUST distinguish the worker kinds: stage run, comment review, review reinvoke, CI-fix reinvoke, rebase reinvoke, and merge-train conflict resolution.
- **FR-002**: Each worker MUST report its kind and its trigger when it has one (comment id, review-body id), in `fabrik_status` and `fabrik_board`.
- **FR-003**: An item with any worker kind in progress MUST be classified as working, with a summary naming what the worker is addressing.
- **FR-004**: While a worker is in flight or queued, attention MUST report that condition and MUST NOT report a dispatch cooldown (including `cooldown:*`) as the verdict or the reason.
- **FR-005**: The worker record MUST distinguish "queued for a slot" from "holding a slot". A worker MUST NOT be recorded as holding a slot before the slot is acquired.
- **FR-006**: Status MUST report a queued worker as "queued: waiting for a worker slot (since …)" until the slot is acquired, and MUST report `started_at` as the time the slot was acquired.
- **FR-007**: `fabrik_health.workers` MUST report `in_use` (slots actually held, never more than `max_concurrent`) and a separate `queued` count.
- **FR-008**: The reinvoke dispatch path MUST be fixed so a reinvoke does not appear to be in flight before it holds a slot, and every other dispatch path MUST be checked for the same ordering and fixed where it has it.
- **FR-009**: An item that has work to do (stage not complete, dispatchable by autonomy or otherwise, nothing gating it) but has no worker MUST be reported as "waiting for dispatch" and never as idle.
- **FR-010**: The waiting-for-dispatch reason MUST be exactly one of: all slots busy (stating the slot count), the next poll, or a cooldown that will really re-dispatch the item (stating when).
- **FR-011**: Every `fabrik:*` label the engine sets as state, including `fabrik:reworking:<Stage>`, MUST have a meaning in the attention `reasons`.
- **FR-012**: A test MUST enumerate the `fabrik:*` label constants and fail when one has no meaning, with any exemption explicit and carrying a reason.
- **FR-013**: A live worker MUST report turns used so far when the engine knows them, and `unknown` otherwise.
- **FR-014**: An item whose last invocation was killed by the wall-time limit or reached the turn limit, and which has a retry pending, MUST be reported as a retry waiting for a slot and not as stalled; the reason MUST name the kill and the retry.
- **FR-015**: Labels left on a killed item (in-progress, lock) MUST be explained in the reasons while the retry is pending.
- **FR-016**: An item held from advancing by the feedback gate MUST be reported with that hold as the reason, naming the kind and ID of the pending feedback, and MUST NOT be reported as stalled.
- **FR-017**: Every attention state reported by `fabrik_status`, and every derived push for a state change, MUST carry a `cause` when the engine knows one, and MUST omit `cause` when it does not.
- **FR-018**: The `cause` vocabulary MUST be fixed and documented, and MUST include at least `daemon-stop`, `slot-starved`, `wall-time-kill`, `max-turns`, `gate-hold:<gate>` and `rebase-conflict-after-landing`.
- **FR-019**: `docs/state-machine.md` §7.13 and the MCP section of `docs/USER_GUIDE.md` MUST describe the worker kinds, queued versus in-use, waiting for dispatch, the retry and gate-hold reasons, the `cause` vocabulary and the label-meaning rule; `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-020**: The changes MUST NOT make the local-API and channel read paths reach GitHub; their no-GitHub scans MUST remain green.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For each worker kind in FR-001, a classifier test shows an in-flight worker yields working with the kind named and never a `cooldown:*` reason, and the test fails when the kind handling is removed.
- **SC-002**: An engine test shows that a reinvoke dispatched with all slots held is reported as queued and not in flight, held slots never exceed the maximum, and once the slot is acquired the item is in flight with `started_at` equal to the acquisition time.
- **SC-003**: An engine test shows a cruise item with an incomplete stage and all slots busy is reported as waiting for dispatch with the "all slots busy" reason, never idle.
- **SC-004**: The label-meaning enumeration test fails when a `fabrik:*` constant has no meaning.
- **SC-005**: Classifier tests show a killed item with a pending retry and a feedback-gate-held item are neither reported as stalled, and their reasons name the kill (or feedback kind and ID).
- **SC-006**: For every cause in the vocabulary, a classifier test asserts the cause from the inputs that establish it and asserts it is absent when those inputs are missing.
- **SC-007**: `go test -race ./...` is clean, the docs-drift check on `docs/llms-full.txt` passes, and the local-API and channel no-GitHub scans stay green.

## Assumptions

- "Queued" is reported as its own named condition, neither working, idle nor stalled. Which attention state it maps to, and how "waiting for dispatch" is represented in the existing attention vocabulary, is left to planning, provided the reasons in this spec are met.
- The `cause` vocabulary is the six entries in FR-018, extended only by a cause that a requirement in this issue establishes. Future causes require a documented addition.
- `cause` applies to the current attention state as reported by status, and the derived pushes carry the same value for the transition into that state.
- A "retry pending" means a retry the engine has actually scheduled; an exhausted or escalated retry is not one.
- The slot-queue time is the time the work began waiting for a slot; the in-flight start time is the slot-acquired time.
- Turns used are reported only when the engine already has them for the live worker; this issue does not require new instrumentation beyond that.

## Out of Scope

- The pending-decision attention state (separate issue).
- PR and CI fields in status (separate issue).
- The poll loop blocking on the worker-slot semaphore (#2114).
- Changing retry, cooldown, gate or kill policy themselves; this issue only changes how they are reported.

## Source References

- #1966 (the MCP overseer work this belongs to; mention by bare number only in the PR body, and do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it)
- #2096, #2110, #2113 (the live items on which F4, F15/F16 and F10 were observed)
- #2114 (poll loop blocking on the semaphore, out of scope)
- In-situ MCP session against the live dev daemon, 2026-10-11 (comments on this issue)
