# Feature Specification: MCP overseer sees a stalled poll loop (poll liveness, gauge ages, stall events, attribution, train liveness)

**Feature Branch**: `fabrik/issue-2124`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: the original issue body, verbatim below. Two follow-up requests from the same in-situ test (the poll-stall attribution request of 2026-10-11 13:42, and the merge-train liveness request of 2026-10-11 16:17) are folded into FR-014 to FR-021.

> ## Problem
>
> When the daemon's poll loop stops making progress, neither `fabrik_health` nor any push tells the overseer.
>
> This was observed in an in-situ test against the live dev daemon on 2026-10-11:
>
> - **The poll blocked twice:** from 12:38:25Z to 12:54:30Z (about 16 min), and again from 13:09:47Z for 11+ min.
>   - The cause is the engine bug in #2114: `dispatchCandidates` blocks on `e.sem` while every slot is busy.
>   - Every per-poll step stalled behind it: settle scans, the merge train, landing verification and dispatch.
>   - #2113, which had just been put on cruise, was dispatched about 17 min late.
> - **What `fabrik_health` showed:** `since_last_poll_success_seconds: 475`, then 743, with `last_poll_attempt_at` frozen at the start of the blocked poll. It gives:
>   - no configured poll interval;
>   - no "overdue" flag;
>   - no way to tell "a poll has been **in progress** since X" from "**no poll** since X".
> - **Stale gauge:** `graphql_backoff.remaining` stayed at 4673 from 12:29Z onward, because only the poll updates it, but it was presented as current with no age.
> - **No push:** none fired. `daemon-unreachable` covers only the socket.
>
> #2114 fixes the cause. This issue is about the instrument: **an overseer must be able to see that the pipeline has stopped, whatever the cause.** A worker that goes quiet but stays alive, a hung GitHub call or a future regression must all show up.
>
> ## Requirements
>
> - **R1 — Poll liveness in health.** `fabrik_health.poll` reports:
>   - `interval_seconds` (configured);
>   - `in_progress` (bool) and `current_started_at`;
>   - `last_completed_at` and `last_success_at`;
>   - an `overdue` verdict: true when a poll has been in progress longer than K × interval, or no poll has started within K × interval. Document K, e.g. 2.
>
>   When the poll is in progress and the engine knows the step, also report the step it is in, e.g. `dispatch: waiting for a worker slot`. These are published through `daemonHealth`, so local-API goroutines still read only the mirror.
> - **R2 — Gauges carry their age.** Every health value that only the poll refreshes, such as `graphql_backoff`, carries its as-of time.
> - **R3 — `pipeline-stalled` / `pipeline-resumed` events.** Account-wide, immediate, published on the overdue edge and its recovery. They carry:
>   - the interval;
>   - how long the poll has been in progress or absent;
>   - the current step when known.
>
>   They are deduped per episode.
> - **R4 — Docs.**
>   - `docs/state-machine.md` §7.13/§7.14 as applicable.
>   - The `docs/USER_GUIDE.md` MCP section.
>   - Regenerate `docs/llms-full.txt`.
>
> ## Scope
>
> In scope: health fields, gauge ages, and the stall event pair.
>
> Out of scope: fixing the poll's blocking dispatch, which is #2114.
>
> ## Acceptance
>
> All of the following are shown non-vacuous:
> - **Overdue while in progress:** an engine test holds a poll in progress past K × interval, using the clock seam. `fabrik_health` reports `in_progress: true`, `overdue: true`, and the start time. Exactly one `pipeline-stalled` is published, and one `pipeline-resumed` when the poll completes.
> - **Overdue with no poll:** a test where no poll starts within K × interval reports `overdue`.
> - **Gauge ages:** the `graphql_backoff` as-of time is present and moves only when the poll refreshes it.
> - **No-GitHub guarantees:** the local-API and channel no-GitHub scans stay green.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

An MCP overseer (a Claude session watching the daemon through `fabrik_health`, `fabrik_status` and channel pushes) can currently tell that the daemon's socket is up, but not that the pipeline has stopped moving.

In the 2026-10-11 in-situ test against the live dev daemon, the poll blocked twice, for about 16 and 11+ minutes. The cause was the engine bug in #2114: dispatch waited on a worker slot while all slots were busy. Every per-poll step stalled behind it (settle scans, the merge train, landing verification, dispatch). The overseer saw:

- **Poll liveness:** only `since_last_poll_success_seconds` and a frozen `last_poll_attempt_at`. There was no configured interval, no overdue verdict, and no way to distinguish "a poll has been in progress since X" from "no poll since X".
- **Gauges:** `graphql_backoff.remaining` stayed at 4673 for the whole stall, because only the poll refreshes it, yet it looked current.
- **Pushes:** none. `daemon-unreachable` covers only the socket.
- **Blame:** an item switched to yolo mid-stall (#2096) stayed unlanded because no poll ran. Its `stalled` push blamed the item, not the engine.
- **Merge train:** `fabrik_health.merge_train` showed only `members` and `worker_in_flight`. #2096 sat in Queued for 16+ minutes with no statement of how long, what phase the train was in, or why no train had started.

#2114 fixes the cause. This issue builds the instrument: an overseer must be able to see that the pipeline has stopped, whatever the cause. That includes a worker that goes quiet but stays alive, a hung GitHub call, and future regressions.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See that the poll is stuck or overdue in health (Priority: P1)

An overseer calls `fabrik_health` and can tell, from one response, whether the poll loop is healthy, running long, or not starting, and what it is doing if it is running.

**Why this priority**: this is the core blind spot. Everything else (pushes, attribution, train liveness) builds on the same overdue verdict.

**Independent Test**: with the clock seam, hold a poll in progress past K × interval and read health. Separately, run an engine where no poll starts within K × interval and read health.

**Acceptance Scenarios**:

1. **Given** a poll has been in progress for longer than K × the configured interval, **When** the overseer reads health, **Then** `poll.in_progress` is true, `poll.overdue` is true, `poll.current_started_at` is the poll's start, and the interval is reported.
2. **Given** no poll has started within K × interval, **When** health is read, **Then** `poll.in_progress` is false and `poll.overdue` is true.
3. **Given** a healthy engine whose poll is in progress for less than K × interval, **When** health is read, **Then** `overdue` is false.
4. **Given** the poll is in progress and the engine knows its step (for example waiting for a worker slot in dispatch), **When** health is read, **Then** the step is reported, for example `dispatch: waiting for a worker slot`.
5. **Given** the poll has completed, **When** health is read, **Then** `last_completed_at` and `last_success_at` are present and `in_progress` is false.

---

### User Story 2 - Be pushed when the pipeline stalls and when it resumes (Priority: P1)

An overseer that is not polling health is told, once per episode, that the pipeline has stalled and when it recovers.

**Why this priority**: an overseer that must remember to poll health will miss the stall, as happened in the in-situ test.

**Independent Test**: with the clock seam, hold a poll in progress past K × interval and capture published channel events. Release the poll and capture again.

**Acceptance Scenarios**:

1. **Given** the poll becomes overdue, **When** the overdue edge is crossed, **Then** exactly one account-wide immediate `pipeline-stalled` event is published, carrying the interval, how long the poll has been in progress or absent, and the current step when known.
2. **Given** the poll stays overdue across further evaluations, **When** time passes, **Then** no further `pipeline-stalled` is published for the same episode.
3. **Given** an overdue episode, **When** the poll completes, **Then** exactly one `pipeline-resumed` event is published.
4. **Given** a later, separate overdue episode, **When** it begins, **Then** it produces its own `pipeline-stalled`.
5. **Given** an overseer subscribed with narrow repo, issue or milestone scopes, **When** the event fires, **Then** it is still delivered, because it is account-wide.

---

### User Story 3 - Know how old each polled gauge is (Priority: P2)

An overseer reading health can tell whether a value that only the poll refreshes, such as `graphql_backoff`, is current or hours old.

**Why this priority**: a stale number that looks current was a direct source of misreading in the in-situ test. The fix is small and is needed whatever else is built.

**Independent Test**: refresh the poll-owned gauges, advance the clock, and read health. The as-of time must not change. Run another poll that refreshes them, and the as-of time must advance.

**Acceptance Scenarios**:

1. **Given** the poll has refreshed `graphql_backoff`, **When** health is read, **Then** the gauge carries an as-of time equal to that refresh.
2. **Given** time passes with no poll refreshing the gauge, **When** health is read again, **Then** the value and its as-of time are unchanged, so the age grows.
3. **Given** the gauge has never been refreshed since daemon start, **When** health is read, **Then** the as-of time is reported as unknown, never as "now".

---

### User Story 4 - Item verdicts blame the stalled poll, not the item (Priority: P2)

When the poll is overdue, an item whose only obstacle is that no poll is running is described as waiting for the poll. It is not reported as stalled itself.

**Why this priority**: it stops the overseer wasting effort on, or wrongly escalating, healthy items. It depends on the overdue verdict from User Story 1.

**Independent Test**: hold the poll overdue with the clock seam and use an item that is otherwise ready to advance. Check its attention verdict, its `next_action` and that no item-level `stalled` push is published. Then repeat with the poll healthy and an item that genuinely makes no progress, which must still classify as `stalled`.

**Acceptance Scenarios**:

1. **Given** the poll is overdue and an item's next step needs a poll, **When** its status is read, **Then** the summary and `next_action` say it is waiting for the poll, with how long it has been blocked and the step, for example "waiting for the poll: blocked for 923 s, in step dispatch: waiting for a worker slot".
2. **Given** the same item, **When** the stall threshold for item stalls elapses, **Then** no item-level `stalled` push is published for it.
3. **Given** the poll is healthy and an item makes no progress, **When** the stall threshold elapses, **Then** it is classified `stalled` and pushed as before.
4. **Given** the poll recovers, **When** the item is read again, **Then** its verdict reverts to the normal classification.

---

### User Story 5 - See merge-train liveness per partition (Priority: P2)

For each (repo, base) merge-train partition, an overseer can see how long its oldest member has been queued, what phase any run is in, and why nothing is running when members are queued. A partition that sits idle too long raises the stall event.

**Why this priority**: the train has the same blind spot as the poll, and it was what actually left #2096 waiting. It builds on the stall events from User Story 2.

**Independent Test**: queue members in a partition, keep no run active, hold the poll overdue, and read health. Then advance past the train threshold and capture events. Then let a run start and capture again.

**Acceptance Scenarios**:

1. **Given** members are queued in a partition, **When** health is read, **Then** the partition reports the oldest member's queued-since time.
2. **Given** a run is active in the partition, **When** health is read, **Then** the partition reports the run's current phase from the durable train run phases.
3. **Given** members are queued, no run is active and the poll is overdue, **When** health is read, **Then** the idle reason names the blocked or overdue poll.
4. **Given** members are queued, no run is active and the infra-abandon cooldown is holding the partition off, **When** health is read, **Then** the idle reason names the cooldown.
5. **Given** a partition has members and no active run past the train threshold, **When** it is evaluated, **Then** exactly one `pipeline-stalled` is published for that partition, with its own cause and partition identity.
6. **Given** a partition stall episode, **When** a run starts or the members leave Queued, **Then** exactly one `pipeline-resumed` is published for that partition.

---

### Edge Cases

- **Fresh start:** the daemon has just started and the first poll has not run. "No poll started within K × interval" is measured from the daemon's start, so a freshly started daemon is not overdue until K × interval has elapsed.
- **Deliberate back-off:** when the engine itself defers polls (GitHub rate-limit back-off), the overdue deadline follows the engine's own next-poll-due time rather than the configured interval, and health says the engine is backing off. A deliberate pause is not reported as a stall.
- **Long but legitimate poll:** a poll in progress longer than K × interval is reported overdue even if it is merely slow. The step field lets the overseer tell the difference.
- **Step unknown:** if the engine does not know the step, it reports none. It never invents one.
- **Rapid flapping:** a poll that repeatedly crosses the threshold and recovers produces one stall/resume pair per episode and no duplicates within an episode.
- **Daemon restart mid-episode:** in-memory episode state is lost. A still-stalled daemon raises a fresh `pipeline-stalled` after restart, which is acceptable.
- **Queued members whose partition has no resolvable base or whose run phase is unreadable:** report what is known and leave the rest unknown. Never report them as healthy.
- **Held-poll test seam:** polls deliberately dropped by the test-only poll hold are not production behavior and need no special handling beyond not breaking.
- **Attribution scope:** an item whose obstacle is something other than a missing poll (for example paused awaiting human input) keeps its normal verdict even while the poll is overdue.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `fabrik_health.poll` MUST report the configured poll `interval_seconds`.
- **FR-002**: `fabrik_health.poll` MUST report `in_progress` (bool) and, while a poll is in progress, `current_started_at`.
- **FR-003**: `fabrik_health.poll` MUST report `last_completed_at` and `last_success_at`, each unknown until the first such event.
- **FR-004**: `fabrik_health.poll` MUST report an `overdue` verdict that is true when a poll has been in progress for longer than K × interval, or when no poll has started within K × interval. K MUST be a single documented constant, defaulting to 2.
- **FR-005**: While a poll is in progress, health MUST also report the step it is in when the engine knows it, in a human-readable form such as `dispatch: waiting for a worker slot`. The step MUST be updated at the points where a poll can wait on something outside its own control, including worker-slot waits.
- **FR-006**: All new poll fields MUST be published through the `daemonHealth` mirror. API goroutines MUST read only the mirror and MUST NOT reach GitHub or the live poll loop state.
- **FR-007**: Every health value that only the poll refreshes (`graphql_backoff` and any other such gauge) MUST carry an as-of time. The as-of time MUST change only when the poll refreshes the value and MUST be unknown, not "now", if it was never refreshed.
- **FR-008**: The engine MUST publish an account-wide, immediate `pipeline-stalled` event when the poll becomes overdue. It MUST carry the interval, how long the poll has been in progress or absent, the cause, and the current step when known.
- **FR-009**: The engine MUST publish an account-wide, immediate `pipeline-resumed` event when an overdue episode ends, that is, when the poll completes.
- **FR-010**: `pipeline-stalled` and `pipeline-resumed` MUST be deduplicated per episode: one stalled per episode, one resumed per episode, including across held-queue replays.
- **FR-011**: `pipeline-stalled` and `pipeline-resumed` MUST be delivered irrespective of a subscription's repo, issue or milestone scoping, like other account-wide events.
- **FR-012**: Overdue detection MUST be evaluated on the engine's own clock seam, and MUST NOT depend on the poll loop making progress. A blocked poll must still be detected as overdue.
- **FR-013**: When the engine is deliberately deferring polls (rate-limit back-off), the overdue verdict MUST account for the engine's own next-poll-due time, and health MUST indicate that back-off is the reason, so a deliberate pause is not reported as a stall.
- **FR-014**: When the poll is overdue, every per-item verdict whose next step needs a poll MUST say so instead of blaming the item. This covers the attention `summary` and `next_action`.
- **FR-015**: The waiting-for-poll wording MUST state how long the poll has been blocked and, when known, the step. Example: "waiting for the poll: blocked for N s, in step <step>".
- **FR-016**: An item whose only obstacle is the stalled poll MUST NOT be classified `stalled` and MUST NOT receive an item-level `stalled` push. An item with any other obstacle, or any item while the poll is healthy, MUST be classified exactly as before.
- **FR-017**: The cause of a poll stall MUST be carried once, by the account-wide `pipeline-stalled` event, and not repeated on every affected item's push.
- **FR-018**: `fabrik_health.merge_train` MUST report, for each (repo, base) partition: the oldest queued member's queued-since time, and the current train phase read from the durable train run phases when a run is active.
- **FR-019**: When a partition has queued members and no active run, health MUST report an idle reason. The reason MUST name a blocked or overdue poll when the poll is overdue, and the infra-abandon cooldown when that is what holds the partition off. When the reason is not known it MUST say so.
- **FR-020**: A partition with queued members and no active run for longer than the train stall threshold MUST publish one `pipeline-stalled` event per episode, carrying its own cause and the partition identity. It MUST publish one `pipeline-resumed` when a run starts or the members leave Queued. The train stall threshold is the same K × interval as FR-004.
- **FR-021**: The train stall trigger MUST NOT fire while a partition is inside the infra-abandon cooldown, which is an intentional hold. The idle reason is still shown in health during it.
- **FR-022**: All new health, status and event code MUST remain strictly Store/mirror-based. The local-API no-GitHub scan and the channel no-GitHub scan MUST remain green, and the channel events MUST remain observation-only.
- **FR-023**: Documentation MUST be updated: `docs/state-machine.md` (§7.13 and §7.14 as applicable), the `docs/USER_GUIDE.md` MCP section (including K and the new health fields and events), and `docs/llms-full.txt` regenerated in the same change.

### Key Entities *(if applicable)*

- **Poll liveness**: interval, in-progress flag and start time, last completed/success times, current step, overdue verdict.
- **Gauge as-of time**: the time at which the poll last refreshed a poll-owned health value.
- **Stall episode**: the period from an overdue edge to its recovery, for the poll or for one merge-train partition. It is the unit of event deduplication.
- **Train partition liveness**: per (repo, base) oldest queued-since time, run phase, and idle reason.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a poll held in progress past K × interval, health reports `in_progress: true`, `overdue: true` and the start time, exactly one `pipeline-stalled` is published, and exactly one `pipeline-resumed` is published when the poll completes. Each assertion fails if the corresponding behavior is removed.
- **SC-002**: With no poll started within K × interval, health reports `overdue: true` and `in_progress: false`.
- **SC-003**: The `graphql_backoff` as-of time is present after a poll and is unchanged by the passage of time alone. It advances only when the poll refreshes it.
- **SC-004**: With the poll held overdue, an otherwise-ready item reports "waiting for the poll" and receives no item-level `stalled` push. The same item with a healthy poll and no progress still classifies and pushes as `stalled`.
- **SC-005**: With members queued, no run active and the poll held overdue, the partition reports the oldest queued-since time, the phase, and an idle reason naming the blocked poll. Past the threshold, exactly one partition `pipeline-stalled` is published, and one `pipeline-resumed` follows when a run starts or the members leave Queued.
- **SC-006**: The local-API and channel no-GitHub scans stay green, and `go test -race ./...` is clean.
- **SC-007**: In a replay of the 2026-10-11 scenario (poll blocked behind a full worker semaphore), an overseer that reads only health and received pushes can identify within K × interval that the engine, not an item, is stalled, and the step it is stuck in.

## Assumptions

- K defaults to 2, a single documented constant. It is not an operator setting in this issue.
- "Interval" is the configured poll interval. Where the engine is deliberately backing off, its own next-poll-due time takes precedence (FR-013).
- The train stall threshold reuses K × interval, because a healthy poll should start a run for queued members within about one poll. A separate setting would add configuration with no evidence yet that it is needed.
- Known benign idle reasons (the infra-abandon cooldown) do not raise a train stall while they hold (FR-021). They are shown in health, and once the hold expires with no run, the partition counts as stalled.
- A partition stall during an overdue poll still raises its own partition event. The poll-level event carries the poll cause once (FR-017), and the partition event names its own cause and partition, so each has one cause.
- Stall episode state is in memory and resets on restart, like the existing channel stall and settle episode memory. A still-stalled daemon re-raises after restart.
- Ages in health are derived from as-of timestamps on the engine's clock, so a reader computes age as now minus as-of.
- The existing fields (`last_poll_attempt_at`, `since_last_poll_success_seconds`, `graphql_backoff`) stay, with unchanged meaning, so current consumers keep working. Additions are additive.

## Out of Scope *(optional)*

- Fixing the poll's blocking dispatch (#2114).
- A dedicated event for an item joining the Queued column. It was noted as a gap in the in-situ test, but only the train that does not start was requested.
- Any new tool that lifts a pause, restarts the poll, or otherwise acts on a stalled engine. This is observation only.
- Making the K multiplier or the train threshold operator-configurable.
- Detecting stalls in other per-poll subsystems beyond the poll itself and merge-train partitions.

## Source References *(optional)*

- #2114: the engine bug that blocked dispatch on a full worker semaphore; the cause of the observed stall.
- #1966: the MCP overseer work this builds on (mentioned for context only).
- `docs/state-machine.md` §7.13 (attention classifier), §7.14 (channel events), and the merge-train run phase source (`TrainRunPhases()`).
