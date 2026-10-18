# Feature Specification: Hold stage dispatch when the lock can't be written or the forge is unreachable

**Feature Branch**: `fabrik/issue-2393`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description:

> ## Problem
>
> When the forge cannot be written, the engine still spawns a stage worker. `acquireLockAndVerify` (`engine/item.go`) logs `could not add lock label` and continues unlocked: no lock, no worker handle in the Store, no `in_progress` label. Claude then runs against the model API, which may still be up, and produces results that cannot be recorded.
>
> Reported in #2388: a GitHub Enterprise Server host was unreachable for about 21 hours while the model API stayed up. One issue's Research stage was dispatched again roughly every 8 minutes for that whole time, about 150 runs on the same session, and each run's lock write failed with `context deadline exceeded`.
>
> The poll kept dispatching from the board cache, and it did so even while every GitHub read and write was failing. `feedback-gate: holding advance — live feedback read failed` shows the engine knew the forge was down.
>
> ## Requirements
>
> - **R1:** If the lock label cannot be written, the stage worker is not spawned. The item gets a short admission cooldown, the in-progress label is not attempted, and nothing counts against `max_retries`. One log line names the cause.
> - **R2:** The same rule applies at every dispatch site that takes the lock or `fabrik:editing` before spawning a worker. That covers stage dispatch, the comment-review family (`dispatchReinvoke`) and merge-train conflict resolution. The audit is part of the work.
> - **R3:** An account-wide **forge-unreachable hold**. When GitHub calls have been failing on transport errors for a sustained window, the engine dispatches no new model work until a GitHub call succeeds again. Transport errors are timeouts, dial errors and 5xx; a 4xx is not a transport error. Running workers are left alone. The window is based on observed failures, never a single error.
> - **R4:** The hold is reported once as an operator warning through `warnings.Report` and resolved when the forge answers. It also appears in `fabrik_health`, and a `pipeline-stalled`-style event goes out to channel subscribers.
> - **R5:** Sim twin: a simgh fault that fails every write with a transport error. Assert that no `ClaudeInvoker` call happens after the hold engages, and that dispatch resumes once the fault clears. Neutralising the change makes the scenario fail.
>
> ## Scope
>
> Engine dispatch admission only. Retrying the recording of a run that already finished is a separate issue, and so is capping re-dispatches.
>
> ## Acceptance
>
> - With every GitHub write failing, a ready item is not handed to Claude more than once (sim).
> - Neutralising R1 and R3 lets the sim scenario re-dispatch.
>
> Mention #2388 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) next to it.

## Background

The lock label is how the engine claims an item before it spends model time on it. Today, when the lock write fails, the engine logs `could not add lock label` and spawns the worker anyway. The worker then runs with no lock, no worker record and no `in_progress` label. Its results cannot be recorded, so the next poll sees an item that looks untouched and dispatches it again.

This was observed in #2388. A GitHub Enterprise Server host was unreachable for about 21 hours while the model API stayed up. One issue's Research stage was dispatched about every 8 minutes, roughly 150 runs on the same session. Every run's lock write failed with `context deadline exceeded`, and every run spent model budget to produce output that was thrown away.

Two gaps combine to cause this:

1. **Per item:** a failed lock write is treated as ignorable and the worker is spawned unlocked.
2. **Account-wide:** the engine already knows the forge is down. Other gates log that live reads are failing, for example `feedback-gate: holding advance — live feedback read failed`. Nothing turns that knowledge into a decision to stop dispatching model work.

The fix applies to dispatch admission only. It covers the lock write, every dispatch site that takes a lock-like label before a worker, and an account-wide hold while the forge is unreachable. Retrying the recording of a run that already finished, and capping how often an item is re-dispatched, are separate issues.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A failed lock write never spawns a worker (Priority: P1)

An operator's forge has a brief or partial outage, or one item's label write fails. The engine must not run a model session it cannot lock or record.

**Why this priority**: This is the direct cause of the 150 wasted runs. It is also the smallest change that stops the loss for a single item.

**Independent Test**: With a fault that fails the lock-label write for one item, run a poll. Assert that no model invocation occurs for that item, that no `in_progress` label write is attempted, and that the item's retry count is unchanged. Clear the fault and assert that the item dispatches normally on a later poll.

**Acceptance Scenarios**:

1. **Given** a ready item and a lock-label write that fails, **When** the engine tries to dispatch the stage, **Then** no worker is spawned, the item receives a short admission cooldown, and one log line names the lock write as the cause.
2. **Given** the same failed lock write, **When** the engine decides what to do next, **Then** it does not attempt the `in_progress` label and does not count the attempt against `max_retries`.
3. **Given** the lock write failed on one poll and succeeds on a later poll after the cooldown, **When** the item is admitted again, **Then** it dispatches normally.

---

### User Story 2 - Every lock-taking dispatch site follows the same rule (Priority: P1)

The comment-review family and merge-train conflict resolution also take the lock or `fabrik:editing` before spawning a worker. They must not start a worker when that write fails.

**Why this priority**: Fixing only stage dispatch would leave the same waste on the other paths. The issue explicitly makes the audit part of the work.

**Independent Test**: For each dispatch site that takes a lock or `fabrik:editing`, inject a failing label write and assert that no model invocation occurs at that site. A site that cannot be made to follow the rule is listed in the audit result with a reason.

**Acceptance Scenarios**:

1. **Given** a pending comment on an item and a failing `fabrik:editing` write, **When** the comment-review path dispatches, **Then** no worker is spawned, the comment stays unprocessed and the item gets a short admission cooldown.
2. **Given** a merge-train conflict resolution whose lock or editing write fails, **When** the train tries to start the resolution, **Then** no model invocation occurs and the failure is not charged to the member (no ejection count, no pause).
3. **Given** a dispatch site found in the audit that takes such a label, **When** its write fails, **Then** it follows the same rule as the stage dispatch path.

---

### User Story 3 - An account-wide hold while the forge is unreachable (Priority: P1)

When GitHub calls have been failing on transport errors for a sustained window, the engine stops starting any new model work until the forge answers again.

**Why this priority**: Even with per-item protection, a long outage would still produce many pointless attempts across all items. The hold is cheap, bounded and stops the loss at its source.

**Independent Test**: Make every GitHub call fail with a transport error. Run polls until the window is exceeded. Assert that no model invocation happens after the hold engages. Clear the fault and assert that dispatch resumes after the next successful GitHub call.

**Acceptance Scenarios**:

1. **Given** GitHub calls failing on timeouts, dial errors or 5xx responses for the full sustained window, **When** the engine considers dispatching any item, **Then** it starts no new model work.
2. **Given** the hold is engaged, **When** a GitHub call succeeds, **Then** the hold lifts and dispatch resumes.
3. **Given** a single transport error, or a burst shorter than the window, **When** the engine considers dispatching, **Then** no hold engages.
4. **Given** GitHub returns 4xx responses (for example 403 or 404), **When** the engine evaluates the hold, **Then** those responses do not count as transport errors and never engage the hold.
5. **Given** workers already running when the hold engages, **When** the hold is active, **Then** those workers are left alone to finish.

---

### User Story 4 - The operator can see the hold (Priority: P2)

An operator needs to learn that the engine has stopped dispatching because the forge is unreachable, without reading logs.

**Why this priority**: A silent hold looks like a hung engine. Visibility is part of making the behavior safe, but it does not change what gets dispatched.

**Independent Test**: Engage the hold. Assert that exactly one operator warning is recorded, that `fabrik_health` reports the hold, and that subscribed channel clients receive a stall event. Lift the hold and assert that the warning is resolved and a matching resume event is sent.

**Acceptance Scenarios**:

1. **Given** the hold engages, **When** it is reported, **Then** a single operator warning is recorded through the shared warnings path, not repeated on every poll.
2. **Given** the hold is active, **When** `fabrik_health` is read, **Then** it shows the hold and since when.
3. **Given** a channel subscriber, **When** the hold engages, **Then** the subscriber receives a stall-style event, and a resume event when the hold lifts.
4. **Given** the forge answers again, **When** the hold lifts, **Then** the warning is resolved.

---

### User Story 5 - Sim twin proves the behavior (Priority: P1)

The deterministic sim bed has a scenario in which every write fails with a transport error.

**Why this priority**: The repository requires a sim twin for engine behavior. This scenario is the acceptance evidence for the whole issue.

**Independent Test**: Run the scenario with the change in place, then with the lock-write rule and the hold each neutralised. The first run passes and the second fails.

**Acceptance Scenarios**:

1. **Given** a sim fault that fails every write with a transport error, **When** polls run past the hold window, **Then** the scripted model invoker receives no call after the hold engages, and a ready item is handed to it at most once.
2. **Given** the fault is cleared, **When** polls continue, **Then** dispatch resumes.
3. **Given** the lock-write rule and the hold are both neutralised, **When** the same scenario runs, **Then** the item is dispatched repeatedly and the scenario fails.

---

### Edge Cases

- A lock write fails because of a 4xx (for example permission denied or label not found). It still prevents spawning a worker (User Story 1), but it is not a transport error and does not count toward the hold.
- The lock write succeeds but a later write fails. This is outside the lock rule and is the separate "recording a finished run" issue.
- The hold engages while a worker is mid-run. The worker finishes. Its recording may fail, which is out of scope here.
- The only successful call after an outage is a read. Any successful GitHub call lifts the hold, per the issue.
- The engine restarts during an outage. The hold state is not persisted. The window starts counting again from observed failures after the restart.
- Admission cooldowns for several items expire at once as the forge comes back. Normal dispatch ordering and slot limits apply.
- The lock write times out but actually reached the forge. The engine must not treat the item as locked, and a later cleanup must still be able to remove a stray lock label.
- A transport error and a success interleave rapidly. The window is based on a sustained run of failures with no success in between.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the lock label cannot be written, the engine MUST NOT spawn a stage worker for that item.
- **FR-002**: When a stage dispatch is refused because of a failed lock write, the engine MUST give the item a short admission cooldown. It MUST NOT attempt the `in_progress` label, and MUST NOT count the attempt against `max_retries` or any other retry or failure counter.
- **FR-003**: A refusal under FR-001 MUST produce one log line that names the failed lock write as the cause.
- **FR-004**: The engine MUST apply the same rule as FR-001 to FR-003 at every dispatch site that takes the lock or `fabrik:editing` before spawning a worker. At minimum this covers stage dispatch, the comment-review family (`dispatchReinvoke`) and merge-train conflict resolution.
- **FR-005**: The work MUST include an audit of all such dispatch sites, and the audit result MUST be recorded in the change so that a reviewer can see which sites were checked.
- **FR-006**: A refused merge-train conflict resolution MUST be uncharged: no ejection count, no pause and no trial charge.
- **FR-007**: The engine MUST maintain an account-wide forge-unreachable hold. It engages when GitHub calls have failed on transport errors for a sustained window, with no successful GitHub call in that window.
- **FR-008**: Transport errors for FR-007 are timeouts, connection/dial errors and 5xx responses. 4xx responses MUST NOT count as transport errors.
- **FR-009**: The window MUST be based on observed failures over time. A single error, or a burst shorter than the window, MUST NOT engage the hold.
- **FR-010**: While the hold is engaged, the engine MUST NOT start any new model work, whether stage dispatch, comment review or merge-train conflict resolution.
- **FR-011**: Workers already running when the hold engages MUST be left alone.
- **FR-012**: The hold MUST lift as soon as any GitHub call succeeds, and dispatch MUST resume under normal admission rules.
- **FR-013**: When the hold engages, the engine MUST report it once as an operator warning through the shared warnings path. When the hold lifts, it MUST resolve that warning.
- **FR-014**: While engaged, the hold MUST be visible in `fabrik_health`.
- **FR-015**: Engaging the hold MUST send a stall-style event to channel subscribers, and lifting it MUST send a matching resume event.
- **FR-016**: A sim scenario MUST exist in which a fault fails every GitHub write with a transport error. It MUST assert that no model invocation occurs after the hold engages, that a ready item is handed to the model at most once while all writes fail, and that dispatch resumes once the fault clears.
- **FR-017**: The sim scenario MUST fail when the lock-write rule (FR-001) and the hold (FR-007) are both neutralised, so that it demonstrates the re-dispatch behavior.

### Key Entities *(if applicable)*

- **Lock write**: the label write that claims an item before a worker starts. Its failure is the per-item trigger.
- **Admission cooldown**: the short per-item delay before the engine considers an item for dispatch again.
- **Forge-unreachable hold**: the account-wide state in which the engine starts no new model work because GitHub calls have been failing on transport errors for a sustained window.
- **Transport error**: a timeout, a connection/dial error or a 5xx response. It is distinct from a 4xx response.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With every GitHub write failing, a ready item is handed to the model at most once, however long the outage lasts (sim).
- **SC-002**: After the hold engages, zero model invocations start until a GitHub call succeeds (sim).
- **SC-003**: After the fault clears, dispatch of a ready item resumes within a bounded number of polls (sim).
- **SC-004**: A refused dispatch increments no `max_retries` or failure counter and leaves no `failed` or `in_progress` label on the item.
- **SC-005**: An outage produces one operator warning and one stall event in total, not one per poll. Both are resolved or followed by a resume event when the forge answers.
- **SC-006**: Neutralising the lock-write rule and the hold turns the sim scenario from passing to failing.
- **SC-007**: The audit lists every dispatch site that takes the lock or `fabrik:editing`, and each one follows the rule or has a recorded reason why not.

## Assumptions

- The sustained window length is an engine default chosen conservatively, long enough that a single slow call or short blip never engages it and short enough to stop a dispatch loop within a few polls. It is not a new user-facing setting unless the Plan stage finds one is needed.
- The hold state is held in memory only and starts fresh after a restart.
- Any successful GitHub call, read or write, counts as the forge answering.
- The per-item admission cooldown after a failed lock write is short and uses the engine's existing admission cooldown mechanism.
- The existing `pipeline-stalled` / `pipeline-resumed` channel event family is the model for the new stall-style event. Whether it reuses those event types or adds new ones is for Plan to decide.
- Because the operator applied `fabrik:cruise`, this stage proceeds without waiting for a human to approve the spec.

## Out of Scope *(optional)*

- Retrying the recording of a run that already finished. This is a separate issue.
- Capping the number of re-dispatches of an item. This is a separate issue.
- Changing how running workers are supervised or killed during an outage.
- Any change to the model-API side, such as detecting model outages or usage limits.
- Persisting hold state across restarts.

## Source References *(optional)*

- #2388: the report of the ~21-hour GitHub Enterprise Server outage with about 150 re-dispatches. Mentioned for context only; this work does not close it.
- `docs/state-machine.md` §7.11 (operator warnings) and §7.13/§7.14 (health and channel events), the existing mechanisms the visibility requirements build on.
