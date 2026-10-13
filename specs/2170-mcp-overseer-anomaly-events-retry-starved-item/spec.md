# Feature Specification: MCP overseer anomaly events — retry-starved, item-wedged, cache-divergence

**Feature Branch**: `fabrik/issue-2170`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: the original request, verbatim:

> ## Problem
>
> In the 2026-10-11 in-situ MCP session, every engine anomaly that mattered was found only in `.fabrik/fabrik.log`. None was found in a push or in `fabrik_status`. A reacting overseer cannot see the moment the pipeline stops making progress on an item for a reason the engine already knows.
>
> - **A3/A8 (starved retry):** #1805, #2129 and #2135 were killed by `max_wall_time` / `max_turns`. Each logged "will retry after 1m0s", then waited 30+ min for a worker slot.
> - **A11 (wedged gate):** #2125's feedback gate held advance for over an hour ("holding advance — 1 unprocessed review body 5483772616") with no dispatch to address it (engine defect #2160). Nothing was pushed.
> - **A9 (cache divergence):** a light reconcile reverted recent engine label writes. Three phantom label pushes followed, two of them reverted three minutes later.
>
> Poll stalls and merge-train idleness are covered by #2124 (`pipeline-stalled`/`pipeline-resumed`, train liveness). This issue covers the per-item and cache anomalies #2124 does not.
>
> ## Requirements
>
> - **R1 — `retry-starved`.**
>   - **When:** published once per episode when an item has a scheduled retry or re-dispatch (after a kill, a usage-limit lift or a cooldown) that has not been dispatched N minutes after it became due.
>   - **Carries:** the original cause (`wall-time-kill`, `max-turns`, …), the due time, and the reason it is not running (`no worker slot` with in-use/max, or `poll overdue`).
>   - **Recovery:** a matching recovery event is published on dispatch.
> - **R2 — `item-wedged`.**
>   - **When:** published once per episode when a gate the engine applies (feedback, review, CI, merge) has held an item's advance or landing for longer than a threshold, and no worker for that item has been dispatched or queued since the hold began.
>   - **Carries:** the gate, the holding reason as the engine logs it (e.g. the pending feedback IDs), and how long it has held.
>   - **Recovery:** the episode ends on dispatch, on advance, or when the gate clears.
>   - **Detection:** this is a liveness detector; it does not fix #2160. It must fire on #2125's shape.
> - **R3 — `cache-divergence`.** Published when a reconcile or probe changes an item's cached labels or column in a way that **reverts** an engine write made within the last few minutes. It names the write, the reverting path and both values. Label events derived from such a change are marked `unverified=true` (see #2169's provenance).
> - **R4 — Thresholds.** These are configurable with defaults chosen in the ADR, read once at startup, and classified in `internal/workerenv`.
> - **R5 — Observation only.** Detection reads only what the channel layer already may (`Peek`/`Scan`, `Change`, already-guarded engine state); no GitHub calls. `TestChannelEventsSourceDoesNotReachGitHub` and the observation-only sim stay green unchanged.
> - **R6 — Docs.**
>   - The event catalogue in `docs/state-machine.md` §7.14 and in `docs/USER_GUIDE.md` (enforced by the docs gate).
>   - Regenerate `docs/llms-full.txt`.
>   - Add an ADR.
>
> ## Scope
>
> In scope:
> - the three events, with their detection and thresholds.
>
> Out of scope:
> - fixing the underlying engine defects (#2114, #2160, the reconcile race);
> - poll and train liveness (#2124).
>
> ## Acceptance
>
> - **Starved retry:** a sim test kills a stage by wall time while all slots are held. It asserts exactly one `retry-starved` after the threshold, naming `wall-time-kill` and `no worker slot`, and recovery on dispatch.
> - **Wedged gate:** a sim test reproduces a feedback-gate hold with no dispatch. It asserts one `item-wedged` naming the feedback ID, and none when a reinvoke is dispatched within the threshold.
> - **Cache divergence:** an engine test applies an engine label write, then a reconcile snapshot lacking it. It asserts `cache-divergence`, with the derived label event marked `unverified=true`.
> - Each test is shown non-vacuous.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

In the 2026-10-11 in-situ MCP overseer session, every engine anomaly that mattered was discovered only by reading `.fabrik/fabrik.log`; none surfaced through a channel push or `fabrik_status`. A reacting overseer therefore cannot see the moment the pipeline stops making progress on an item for a reason the engine already knows. Three anomaly classes were observed:

- **Starved retry (A3/A8).** #1805, #2129 and #2135 were killed by `max_wall_time` / `max_turns`. Each logged "will retry after 1m0s", then waited 30+ minutes for a worker slot.
- **Wedged gate (A11).** #2125's feedback gate held advance for over an hour ("holding advance — 1 unprocessed review body 5483772616") with no dispatch to address it (engine defect #2160). Nothing was pushed.
- **Cache divergence (A9).** A light reconcile reverted recent engine label writes. Three phantom label pushes followed, two of them reverted three minutes later.

Poll stalls and merge-train idleness are already covered by #2124 (`pipeline-stalled` / `pipeline-resumed`, which have shipped). This issue covers the per-item and cache anomalies #2124 does not. Related: #2169 defines the provenance vocabulary (`source`, `unverified`) these events build on; #1966 is the human feature thread.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Know when a scheduled retry is starved (Priority: P1)

An overseer session subscribed to channel events is told, once, when an item that the engine has scheduled to retry or re-dispatch (after a kill, a usage-limit lift or a cooldown) has not started running well after it became due, and why it is not running. It is told again, with a recovery event, when the item is finally dispatched.

**Why this priority**: This is the most frequent observed anomaly (three items in one session) and silently costs the most wall time.

**Independent Test**: In the sim bed, kill a stage by wall time while every worker slot is held; advance time past the threshold; assert exactly one `retry-starved` event, then release a slot and assert the recovery event on dispatch.

**Acceptance Scenarios**:

1. **Given** an item killed by `max_wall_time` whose retry is due and all worker slots are in use, **When** the due retry has gone undispatched for longer than the threshold, **Then** exactly one `retry-starved` is published naming cause `wall-time-kill`, the due time, and reason `no worker slot` with the slots in use and the maximum.
2. **Given** an open `retry-starved` episode, **When** the item is dispatched, **Then** a matching recovery event is published once and the episode ends.
3. **Given** an item whose retry is due but the poll loop itself is overdue, **When** the threshold passes, **Then** the event names reason `poll overdue` instead of a slot shortage.
4. **Given** an item whose retry becomes due and is dispatched before the threshold, **When** time passes, **Then** no `retry-starved` is published.

---

### User Story 2 - Know when a gate has wedged an item (Priority: P1)

An overseer is told, once per episode, when a gate the engine applies (feedback, review, CI or merge) has held an item's advance or landing longer than a threshold while no worker has been dispatched or queued for that item since the hold began. The event states which gate, the holding reason as the engine logs it (for the feedback gate, the pending feedback IDs) and how long the hold has lasted.

**Why this priority**: A hold with no worker to resolve it never clears by itself (#2125's hour-long hold). It is a liveness detector, independent of fixing the underlying defect #2160.

**Independent Test**: In the sim bed, reproduce a feedback-gate hold with no dispatch; assert one `item-wedged` naming the feedback ID. Repeat with a reinvoke dispatched within the threshold; assert none.

**Acceptance Scenarios**:

1. **Given** a feedback-gate hold on an item with one unprocessed review body and no worker dispatched or queued, **When** the hold exceeds the threshold, **Then** one `item-wedged` is published naming the gate `feedback`, the pending feedback ID and the hold duration.
2. **Given** the same hold, **When** a reinvoke is dispatched within the threshold, **Then** no `item-wedged` is published.
3. **Given** an open `item-wedged` episode, **When** the item is dispatched, advances, or the gate clears, **Then** the episode ends and a recovery event is published once.
4. **Given** a hold that persists after the first event, **When** more time passes with no change, **Then** no further `item-wedged` is published for that episode.

---

### User Story 3 - Know when the cache reverted an engine write (Priority: P2)

An overseer is told when a reconcile or probe changes an item's cached labels or column in a way that reverts an engine write made within the last few minutes. The event names the write, the reverting path and both values, and any label event derived from that change is marked `unverified=true` so the overseer can tell a phantom from a real change.

**Why this priority**: It explains and defuses the phantom label pushes of A9. It matters less than the other two because the downstream label events are already misleading rather than missing.

**Independent Test**: In an engine test, apply an engine label write, then apply a reconcile snapshot lacking that label; assert `cache-divergence` is published and the derived label event carries `unverified=true`.

**Acceptance Scenarios**:

1. **Given** an engine label write applied moments ago, **When** a reconcile snapshot arrives without that label, **Then** `cache-divergence` is published naming the write, the reverting path (`reconcile`) and both values, and the label-removed event derived from the change carries `unverified=true`.
2. **Given** the same sequence via a probe, **When** the probe reverts a cached column or label write, **Then** the event names the reverting path `probe`.
3. **Given** an engine write older than the recency window, **When** a reconcile legitimately differs from it, **Then** no `cache-divergence` is published.
4. **Given** a reconcile that agrees with a recent engine write, **When** it is applied, **Then** no `cache-divergence` is published.

---

### User Story 4 - Operators can tune and trust the detectors (Priority: P3)

An operator can adjust each detection threshold at startup, and can rely on the detectors being observation-only.

**Why this priority**: Defaults are chosen in the ADR, but a fixed number would be wrong for some installations; the observation-only property protects the existing channel guarantees.

**Independent Test**: Start the engine with a changed threshold and confirm detection follows it; run the existing static scan and observation-only sim unchanged.

**Acceptance Scenarios**:

1. **Given** a threshold set at startup, **When** the engine runs, **Then** detection uses that value and does not change it without a restart.
2. **Given** the new detection code, **When** the channel no-GitHub scan and observation-only sim run, **Then** both pass unchanged.

---

### Edge Cases

- A retry held back by an account-wide Claude usage-limit hold is waiting for the hold to lift and is not starved; starvation is measured from the later of the due time and the hold lift.
- A paused or blocked item already produces its own push and must not also be reported as `retry-starved` or `item-wedged`.
- An item whose worker is running, or queued for a slot, is not wedged; `item-wedged` requires that no worker has been dispatched or queued since the hold began.
- A daemon restart loses in-memory episode state; a condition that is still true afterwards raises a fresh episode, consistent with `pipeline-stalled`.
- A flapping hold (clears and re-holds) starts a new episode each time and must not suppress a later genuine one; episode dedup keys must be unique per episode, since the hub persists dedup keys.
- When the poll loop is overdue (#2124), per-item events carry that as the reason or are not raised, so the poll cause is not repeated as a separate anomaly on every item.
- An engine write that is reverted and then re-applied by the engine within the window yields at most one `cache-divergence` per reverting change.
- A reconcile that changes a label the engine never wrote recently is not a divergence.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST publish a `retry-starved` event once per episode when an item has a scheduled retry or re-dispatch (after a kill, a usage-limit lift or a cooldown) that has not been dispatched a configurable number of minutes after it became due.
- **FR-002**: A `retry-starved` event MUST carry the original cause (for example `wall-time-kill`, `max-turns`), the due time, and the reason the item is not running: `no worker slot` together with slots in use and maximum, or `poll overdue`.
- **FR-003**: The system MUST publish a matching recovery event once when a `retry-starved` item is dispatched, ending the episode. The recovery event's name is decided in the ADR.
- **FR-004**: The system MUST publish an `item-wedged` event once per episode when a gate the engine applies (feedback, review, CI or merge) has held an item's advance or landing longer than a configurable threshold and no worker for that item has been dispatched or queued since the hold began.
- **FR-005**: An `item-wedged` event MUST carry the gate, the holding reason as the engine logs it (for the feedback gate, the pending feedback IDs) and how long the hold has lasted.
- **FR-006**: An `item-wedged` episode MUST end, with a recovery event published once, on dispatch, on advance, or when the gate clears. The recovery event's name is decided in the ADR.
- **FR-007**: `item-wedged` detection MUST fire on the shape of #2125's hold (a feedback-gate hold on an unprocessed review body with no dispatch). It is a liveness detector and MUST NOT change gate or dispatch behaviour.
- **FR-008**: The system MUST publish a `cache-divergence` event when a reconcile or probe changes an item's cached labels or column in a way that reverts an engine write made within a configurable recent window. It MUST name the write, the reverting path and both values.
- **FR-009**: Every label event derived from a change that triggered `cache-divergence` MUST carry `unverified=true`, using the provenance vocabulary of #2169.
- **FR-010**: The `retry-starved` and `item-wedged` thresholds and the `cache-divergence` recency window MUST be configurable, with defaults chosen in the ADR, read once at startup, and classified in `internal/workerenv`.
- **FR-011**: Detection MUST read only what the channel layer already may read (`Peek`/`Scan`, `Change`, already-guarded engine state) and MUST make no GitHub calls. `TestChannelEventsSourceDoesNotReachGitHub` and the observation-only sim MUST pass unchanged.
- **FR-012**: Each episode MUST be published at most once per condition: a persisting condition does not repeat, and dedup keys are unique per episode.
- **FR-013**: The new events MUST be documented in the event catalogue in `docs/state-machine.md` §7.14 and in `docs/USER_GUIDE.md` (satisfying the docs gate), `docs/llms-full.txt` MUST be regenerated, and an ADR numbered after this issue MUST be added.
- **FR-014**: Each acceptance test MUST be shown non-vacuous, meaning it fails when the detection is disabled or neutralised.

### Key Entities *(if applicable)*

- **Anomaly episode**: one continuous occurrence of a condition (starvation, wedge, divergence) for an item, with a start, a once-only announcement and, for the first two, a recovery.
- **Scheduled retry**: an item's pending re-dispatch after a kill, a usage-limit lift or a cooldown, with a cause and a due time.
- **Gate hold**: the engine withholding an item's advance or landing (feedback, review, CI, merge), with the engine's own holding reason and a start time.
- **Engine write record**: a recent label or column write the engine made, retained for the recency window so a later reconcile or probe can be recognised as reverting it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a sim run reproducing A3/A8, a subscribed overseer receives exactly one `retry-starved` (cause `wall-time-kill`, reason `no worker slot`) after the threshold, and one recovery on dispatch.
- **SC-002**: In a sim run reproducing A11, a subscribed overseer receives exactly one `item-wedged` naming the pending feedback ID, and receives none when a reinvoke is dispatched within the threshold.
- **SC-003**: In an engine test reproducing A9, a reverting reconcile produces one `cache-divergence` naming both values, and the derived label event is marked `unverified=true`.
- **SC-004**: Each of these tests fails when its detector is disabled.
- **SC-005**: The channel no-GitHub scan and the observation-only sim pass without modification, and `go test -race ./...` is clean.
- **SC-006**: The three events and their thresholds appear in `docs/state-machine.md` §7.14 and `docs/USER_GUIDE.md`, the docs gate passes, and `docs/llms-full.txt` is regenerated.

## Assumptions

- The three events are item-scoped and follow the existing channel subscription model (repo/issue/milestone scoping and the default exclude-labels behaviour). Whether they are immediate or digest-eligible, and which names the recovery events use, are decided in the ADR.
- Defaults for the thresholds and the recency window are chosen in the ADR; the issue's "N minutes" and "last few minutes" are placeholders for them.
- #2124 has shipped: the poll-overdue verdict and the mirrored worker-slot figures (in use / maximum) are available to the channel layer without any new GitHub call.
- `unverified=true` is defined by #2169's provenance work. If #2169 has not landed when this is implemented, the key is introduced here with the same meaning and defined in exactly one place so #2169 reuses it.
- "No worker has been dispatched or queued since the hold began" is judged from engine state the channel layer may already read; if some of it is not yet exposed, the ADR records the minimal in-memory mirror added, under the same no-GitHub rule.
- Episode state is in memory only and is lost on restart, matching the `pipeline-stalled` precedent.
- Item-level events for conditions already explained by an overdue poll are suppressed or reference it, following #2124's attribution rule.

## Out of Scope *(optional)*

- Fixing the underlying engine defects: #2114 (slot starvation), #2160 (the feedback gate not dispatching) and the reconcile race itself.
- Poll and merge-train liveness, covered by #2124.
- Restart/reconnect delivery, downtime deltas and the default event family (#2169).
- Any change to gate or dispatch behaviour.
- Live end-to-end tests; coverage is through the sim bed and engine tests.

## Source References *(optional)*

- #2124 — pipeline liveness and stall events (ADR 2124), `docs/state-machine.md` §7.13/§7.14
- #2169 — provenance (`source`, `unverified`)
- #2125 / ADR 2125 — actor attribution vocabulary
- #2114, #2160 — engine defects behind the observed anomalies
- #1966 — the human feature thread (mention by bare number only; no closing keyword)
