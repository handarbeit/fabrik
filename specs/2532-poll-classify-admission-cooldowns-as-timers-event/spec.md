# Feature Specification: Classify admission cooldowns as timers, event-replaceable re-checks or the safety net, and schedule each accordingly

**Feature Branch**: `fabrik/issue-2532`
**Created**: 2026-11-14
**Status**: Specified
**Input**: User description: the original issue body, verbatim:

> ## Problem
>
> Every admission cooldown other than the short ones (`shortAdmissionCooldownReasons`: `retry-backoff`, `sentinel-live`, `merge-unsettled`, `feedback-read-failed`) is treated alike: it is stamped about `githubRecheckInterval()` (10 × `PollSeconds`) ahead, and since #2528 (ADR 2528) the idle interval is capped at the earliest such due + one poll interval. The reasons recorded today include `periodic-re-eval`, `dep-blocked`, `review-blocked`, `feedback-pending`, `sha-blocked`, `decision-pending`, `lock-write-failed` and `spawn-*`.
>
> They are three different things:
>
> 1. **Timers**: a deadline that no GitHub event announces (for example the review-wait timeout and its bot re-prompt ladder, the CI backstop, the unknown-merge-state bound). Something has to wake at the deadline. Today they run only when the item happens to be admitted, through one of the cooldowns, so they fire anywhere between the deadline and the next re-admission.
> 2. **Event-replaceable re-checks**: conditions GitHub already delivers events for (reviews and review threads, comments, `issue_dependencies`, blocker closes through `DependentRefreshObserver`). On a daemon with healthy webhook ingestion these re-checks are redundant; they matter for poll-only daemons and for a degraded transport.
> 3. **The safety net**: `periodic-re-eval`, which exists only to catch an admission the engine missed (the #2528 shape) or a lost delivery. It does not need the poll interval's cadence.
>
> Because all of them are capped the same way, an idle, webhook-healthy daemon with any in-pipeline item polls about every 11 × `PollSeconds` (about 5.5 min at `poll: 30`) mostly to re-check conditions an event would have announced.
>
> ## Requirements
>
> 1. Inventory every admission cooldown reason and every time-based check that runs on admission (deadlines anchored on label applied times, comment times or head arrival). Classify each as timer, event-replaceable or safety net, with the evidence (which event replaces it, or why none does). Record the table in an ADR named after this issue.
> 2. **Timers wake at their deadline.** A time-based check whose deadline is known is scheduled so a poll runs within about one poll interval of it (the short-cooldown cap shape, keyed on the deadline), not at the next generic re-admission.
> 3. **Event-replaceable re-checks follow transport health.** While webhook ingestion is healthy for the item's repo (the engine's existing transport/route health, not a new signal), these re-checks are stretched or dropped; when the transport is unhealthy, degraded, or the daemon is poll-only, today's cadence applies. A missed event must still be caught, by the safety net.
> 4. **The safety net has its own interval**, independent of the poll interval and configurable, with a default chosen in Research from the delivery-loss and missed-admission data we have. The ADR 2528 cap applies to it and to timers, not to stretched event-replaceable re-checks.
> 5. Keep ADR 2096 (expired cooldowns admit once, re-arm at the drop sites), ADR 2232/2499 (every engine-scheduled retry owns a cooldown) and ADR 039 intact.
> 6. No change to what any gate decides; only to when it is re-evaluated.
>
> ## Acceptance
>
> - The ADR's classification table covers every reason found by a scan of `CooldownRecorded`/cooldown call sites, and a test fails when a new reason is recorded without a classification (the `shortAdmissionCooldownReasons` precedent, extended).
> - Sim twins: (a) an idle webhook-healthy daemon with an item held on an event-replaceable cooldown does not poll at the old cadence, and the event admits it promptly; (b) the same with the transport unhealthy keeps today's cadence; (c) a timer deadline is acted on within about one poll interval of the deadline while the daemon is idle; (d) a dropped event is recovered by the safety net within its interval. Each with a neutralisation seam.
> - `CLAUDE.md` (the `engine/poll.go` admission-cooldown bullet) and `docs/state-machine.md` updated; `docs/llms-full.txt` regenerated.
>
> ## Notes
>
> - Follows #2528 (ADR 2528), which made the idle interval respect long cooldowns; this narrows which cooldowns need that.
> - Related: #2530 (ADR 2530: a periodic-only admission is not poll activity); ADR 2096, ADR 2232, ADR 2452 (route liveness, the transport-health input).

## Background

Apart from the short cooldowns (`retry-backoff`, `sentinel-live`, `merge-unsettled`, `feedback-read-failed`), every admission cooldown is currently treated the same way. It is stamped roughly ten poll intervals ahead. Since #2528 (ADR 2528), the idle poll interval is capped at the earliest such deadline plus one poll interval. The reasons recorded today include `periodic-re-eval`, `dep-blocked`, `review-blocked`, `feedback-pending`, `sha-blocked`, `decision-pending`, `lock-write-failed` and `spawn-*`.

These cooldowns serve three different purposes:

1. **Timers.** A deadline that no GitHub event announces, such as the review-wait timeout and its bot re-prompt ladder, the CI backstop, and the unknown-merge-state bound. Something must wake at the deadline. Today these checks run only when the item happens to be re-admitted through a cooldown, so they fire anywhere between the deadline and the next re-admission.
2. **Event-replaceable re-checks.** Conditions GitHub already delivers events for: reviews and review threads, comments, `issue_dependencies`, and blocker closes through `DependentRefreshObserver`. With healthy webhook ingestion these re-checks are redundant. They matter only for poll-only daemons and for a degraded transport.
3. **The safety net.** `periodic-re-eval` exists only to catch an admission the engine missed (the #2528 shape) or a lost delivery. It does not need the poll interval's cadence.

Because all of them are capped identically, an idle, webhook-healthy daemon with any in-pipeline item polls about every 11 × `PollSeconds` (about 5.5 minutes at `poll: 30`), mostly to re-check conditions an event would have announced, while real timers can still fire late.

This issue follows #2528 (ADR 2528) and narrows which cooldowns need its cap. Related work: #2530 (ADR 2530, a periodic-only admission is not poll activity), ADR 2096 (expired cooldowns admit once and re-arm at the drop sites), ADR 2232 and ADR 2499 (every engine-scheduled retry owns a cooldown), ADR 039, and ADR 2452 (route liveness, the transport-health input).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Every admission cooldown and time-based check is classified (Priority: P1)

A maintainer can see, in one place, every admission cooldown reason and every time-based check that runs on admission. Each is classified as timer, event-replaceable re-check or safety net, with the evidence for the classification. If someone adds a new cooldown reason without classifying it, the test suite fails.

**Why this priority**: Every scheduling change depends on the classification, and an unclassified reason would silently get the wrong cadence.

**Independent Test**: Scan the cooldown call sites, compare the reasons found with the classification table, and add an unclassified reason in a test to confirm the guard fails.

**Acceptance Scenarios**:

1. **Given** the set of reasons recorded anywhere in the engine, **When** the classification guard runs, **Then** every reason has exactly one class and, for event-replaceable ones, the event that replaces it.
2. **Given** a new cooldown reason is recorded without a classification, **When** the unit suite runs, **Then** it fails and names the reason.
3. **Given** a time-based check that runs on admission (deadline anchored on a label applied time, comment time or head arrival), **When** the inventory is reviewed, **Then** it appears in the table with its class and the evidence.

---

### User Story 2 - Timer deadlines are acted on promptly while the daemon is idle (Priority: P1)

An operator with an idle daemon and an item waiting on a deadline that no event announces (review wait and its re-prompt ladder, CI backstop, unknown-merge-state bound) sees the deadline acted on within about one poll interval of when it falls due, not at the next generic re-admission.

**Why this priority**: This is a correctness-of-timing gap that exists today and is independent of the cadence savings.

**Independent Test**: In the sim, hold an item on a known deadline with the daemon otherwise idle, advance the clock past the deadline, and measure when the check runs.

**Acceptance Scenarios**:

1. **Given** an idle daemon and an item whose review-wait deadline is known, **When** the deadline passes, **Then** a poll runs and the check is evaluated within about one poll interval of the deadline.
2. **Given** several timer deadlines for different items, **When** the daemon is idle, **Then** the idle interval is capped by the earliest unexpired deadline, not by a generic re-admission time.
3. **Given** timer wake-up scheduling is neutralised, **When** the same scenario runs, **Then** the check fires late, so the test is not vacuous.

---

### User Story 3 - Event-replaceable re-checks follow transport health (Priority: P1)

An operator running with healthy webhook ingestion no longer pays the poll cadence for re-checks that events already announce. When the transport is unhealthy, degraded, or the daemon is poll-only, today's cadence applies unchanged.

**Why this priority**: This is the main saving: an idle, webhook-healthy daemon stops polling at about 11 × `PollSeconds` just to re-check conditions an event would have announced.

**Independent Test**: In the sim, hold an item on an event-replaceable cooldown with the daemon idle, once with healthy ingestion and once with unhealthy ingestion, and compare poll cadence. Deliver the event and confirm prompt admission.

**Acceptance Scenarios**:

1. **Given** an idle daemon with healthy webhook ingestion for the item's repo and an item held on an event-replaceable cooldown, **When** time passes, **Then** the daemon does not poll at the old cadence because of that cooldown.
2. **Given** the same state, **When** the relevant event arrives, **Then** the item is admitted promptly.
3. **Given** the same state but the transport is unhealthy or degraded, or the daemon is poll-only, **When** time passes, **Then** the cadence is the same as today.
4. **Given** healthy ingestion that then becomes unhealthy while a stretched cooldown is outstanding, **When** the health change is observed, **Then** the re-check reverts to today's cadence without waiting out the stretched interval.
5. **Given** the stretching is neutralised, **When** scenario 1 runs, **Then** the old cadence is observed, so the test is not vacuous.

---

### User Story 4 - The safety net recovers a missed admission or lost delivery on its own interval (Priority: P2)

An operator can rely on `periodic-re-eval` to catch an admission the engine missed or a delivery that was lost, within a configurable interval that is independent of the poll interval.

**Why this priority**: Stretching event-replaceable re-checks is only safe if a missed event is still caught.

**Independent Test**: In the sim, drop an event that would have admitted an item while ingestion reports healthy, and confirm the item is admitted within the safety-net interval.

**Acceptance Scenarios**:

1. **Given** healthy ingestion and an event that is dropped, **When** the safety-net interval elapses, **Then** the item is re-evaluated and admitted.
2. **Given** a configured safety-net interval different from the poll interval, **When** the daemon is idle, **Then** the periodic re-evaluation runs on the configured interval, capped per ADR 2528.
3. **Given** the safety net is neutralised, **When** the dropped-event scenario runs, **Then** the item is not recovered, so the test is not vacuous.

---

### Edge Cases

- A cooldown reason that fits more than one class (for example a retry that also waits on an event) is classified conservatively: it is never stretched unless an event is proven to replace the re-check.
- An item holds several cooldowns of different classes at once; each class's scheduling applies independently and the earliest applicable wake time governs.
- Webhook health differs per repo; an item in a repo whose transport is unhealthy keeps today's cadence even if another repo is healthy.
- Transport health is unknown (daemon just started, no route verdict yet); it is treated as not healthy.
- A daemon in poll-only mode never stretches an event-replaceable re-check.
- A restart or SIGHUP resets in-memory cooldowns; re-admission after restart is unchanged.
- A stretched cooldown expiring is consumed exactly once (ADR 2096), not re-admitted repeatedly.
- A short cooldown (`retry-backoff`, `sentinel-live`, `merge-unsettled`, `feedback-read-failed`) is unaffected.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every admission cooldown reason recorded by the engine, and every time-based check that runs on admission (deadlines anchored on label applied times, comment times or head arrival), is inventoried and classified as exactly one of timer, event-replaceable re-check or safety net, with the evidence for the class (which event replaces it, or why none does).
- **FR-002**: The inventory and classification table is recorded in an ADR named after this issue (`adrs/2532-…`).
- **FR-003**: A test fails when a cooldown reason is recorded without a classification, extending the `shortAdmissionCooldownReasons` precedent. The test is driven by a scan of the cooldown call sites, so a new reason is caught without editing the test.
- **FR-004**: A time-based check with a known deadline is scheduled so that a poll runs within about one poll interval of the deadline, using the same cap shape as the short cooldowns keyed on the deadline, instead of waiting for the next generic re-admission.
- **FR-005**: An event-replaceable re-check is stretched or dropped while webhook ingestion is healthy for the item's repo. "Healthy" means the engine's existing transport and route health (including the ADR 2452 route verdict); no new health signal is introduced.
- **FR-006**: When ingestion is unhealthy, degraded, of unknown health or absent (poll-only), event-replaceable re-checks keep today's cadence.
- **FR-007**: A transition from healthy to unhealthy ingestion restores today's cadence for already-stretched re-checks within about one poll interval of the change being observed.
- **FR-008**: The safety net (`periodic-re-eval`) has its own interval, independent of the poll interval, set through the daemon's usual configuration mechanisms. Its default is chosen in Research from the delivery-loss and missed-admission data available.
- **FR-009**: The ADR 2528 cap (idle interval limited to due plus one configured poll interval) applies to timers and to the safety net, and does not apply to stretched event-replaceable re-checks.
- **FR-010**: A dropped or lost event for an item held on a stretched re-check is recovered by the safety net within its interval.
- **FR-011**: ADR 2096 (an expired cooldown admits once, re-armed explicitly at the drop sites), ADR 2232 and ADR 2499 (every engine-scheduled retry owns an admission cooldown), and ADR 039 remain intact.
- **FR-012**: No gate's decision changes. Only the time at which an item is re-evaluated changes.
- **FR-013**: The short cooldowns (`retry-backoff`, `sentinel-live`, `merge-unsettled`, `feedback-read-failed`) keep their current scheduling.
- **FR-014**: Each new scheduling behaviour has a neutralisation seam so its sim twin can show the old behaviour on demand.
- **FR-015**: Sim twins cover four scenarios: (a) an idle, webhook-healthy daemon with an item on an event-replaceable cooldown does not poll at the old cadence and the event admits the item promptly; (b) the same with the transport unhealthy keeps today's cadence; (c) a timer deadline is acted on within about one poll interval of the deadline while the daemon is idle; (d) a dropped event is recovered by the safety net within its interval.
- **FR-016**: `CLAUDE.md` (the `engine/poll.go` admission-cooldown bullet) and `docs/state-machine.md` describe the new behaviour. `docs/llms-full.txt` is regenerated in the same change.

### Key Entities *(if applicable)*

- **Admission cooldown**: A per-item, per-reason record of when an item next becomes eligible for re-admission to a poll cycle.
- **Cooldown class**: The classification of a cooldown reason as timer, event-replaceable re-check or safety net.
- **Transport health**: The engine's existing per-repo verdict on webhook ingestion (transport state and route liveness).
- **Safety-net interval**: The configurable cadence of the periodic re-evaluation, separate from the poll interval.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of cooldown reasons found by scanning the cooldown call sites have a recorded class, and adding an unclassified reason fails the test suite.
- **SC-002**: With healthy webhook ingestion and an idle daemon holding only event-replaceable cooldowns, the number of polls over a fixed idle period is materially lower than today's (about one per 11 × `PollSeconds`). The reduction is shown in the sim twin, with the neutralised run showing the old cadence.
- **SC-003**: With unhealthy ingestion or poll-only operation, the poll cadence over the same idle period is unchanged from today's.
- **SC-004**: A timer deadline on an idle daemon is acted on within about one poll interval of the deadline in the sim.
- **SC-005**: A dropped event for an item on a stretched re-check is recovered within the configured safety-net interval.
- **SC-006**: No existing gate-decision test changes its expected outcome; only re-evaluation timing assertions change.

## Assumptions

- The default for the safety-net interval and the stretch factor for event-replaceable re-checks are chosen in Research from the delivery-loss and missed-admission data available, not here. The requirement here is that the interval is configurable and independent of the poll interval.
- Healthy ingestion is judged per repo from the engine's existing transport and route health. Unknown health counts as unhealthy.
- A reason whose class is ambiguous defaults to the conservative class: it keeps today's cadence unless an event is shown to replace the re-check.
- Webhook-driven admission of the item on the relevant event already works as designed (the intake, `issue_dependencies` handling and `DependentRefreshObserver` are not changed here). This issue only changes the fallback re-check schedule.
- "About one poll interval" tolerates one configured poll interval of lateness after a deadline, matching the ADR 2528 cap shape.

## Out of Scope *(optional)*

- Changing what any gate decides, or any gate's thresholds or timeouts.
- Changing the short cooldowns' scheduling.
- Introducing a new transport-health signal, or changing webhook ingestion, subscriptions or transports.
- Adding new GitHub event subscriptions to make more re-checks event-replaceable.
- Changing ADR 2096, ADR 2232, ADR 2499 or ADR 039 semantics.
- Changing PAT-mode defaults for webhook events.

## Source References *(optional)*

- #2528 / ADR 2528: idle interval respects long cooldowns
- #2530 / ADR 2530: a periodic-only admission is not poll activity
- ADR 2096, ADR 2232, ADR 2499, ADR 039: admission-cooldown and retry-scheduling decisions to preserve
- ADR 2452: webhook route liveness, the transport-health input
