# Feature Specification: Feedback gate — a failed live read records a short admission cooldown

**Feature Branch**: `fabrik/issue-2498`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: the original request, verbatim:

> ## Problem
>
> When the feedback gate (`feedbackGateBlocks`, `engine/feedback_gate.go`) holds an advance only because its live feedback read failed (`pf.empty() && !pf.Live`, logged as "live feedback read failed, feedback state unknown"), it records the same `feedback-pending` admission cooldown as a hold on real pending feedback: `githubRecheckInterval()` = 10 × PollSeconds (5 min at a 30 s poll). `feedback-pending` is also not in `shortAdmissionCooldownReasons`, so the idle poll can sleep past it.
>
> A transient failure (one 502) therefore costs the item a full periodic re-evaluation before the advance is retried. The report shows this on `totalslacker/chronicle` #536: the gate held after a 502, the hold cleared about 4 minutes later, and the operator moved the card by hand.
>
> ## Requirements
>
> 1. A hold caused only by a failed live read records a short admission cooldown, not the 10 × PollSeconds one: the same order as the merge gate's `merge-unsettled` (`mergeGateRecheckInterval()`, PollSeconds/2, floor 1 s). Either reuse an existing short reason or add a new one to `shortAdmissionCooldownReasons`, so `PollWithBackoff` caps the idle interval at it.
> 2. A hold on real pending feedback (non-empty `pf`) keeps today's `feedback-pending` cooldown unchanged.
> 3. Repeated read failures must not spin: while the forge-unreachable hold (#2393) is engaged nothing is dispatched anyway; outside it, the short cooldown bounds the re-read rate to one per short interval. Research confirms whether a cap or a fallback to the long interval after N consecutive failures is needed, and records the decision in the ADR.
> 4. The gate's fail-closed behaviour is unchanged: an unreadable feedback state still holds.
>
> ## Acceptance
>
> - Unit test: a failed live read records the short cooldown; real pending feedback records `feedback-pending` at 10 × PollSeconds.
> - Sim twin: a single transient read failure at the advance is retried and the item advances within about one poll; neutralising the change makes it wait for the periodic re-evaluation.
> - `CLAUDE.md` (`engine/feedback_gate.go` / admission-cooldown text) and `docs/state-machine.md` updated; `docs/llms-full.txt` regenerated.
>
> ## Notes
>
> - Source: community report #2453 (totalslacker's flow analysis of `totalslacker/chronicle`), follow-up comment https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6428197162.
> - Same class as #2232 (every engine-scheduled retry owns an admission cooldown).
> - Mention #2453 in the PR body by bare number only. Do **not** use a closing keyword (`Closes`/`Fixes`/`Resolves`) anywhere near it — the report is closed by hand when a release ships. The PR closes this work issue only.

## Background

The feedback gate holds a stage advance (or landing) while any comment, unresolved review thread or unaddressed review body is unprocessed. It reads that state live, and fails closed: if the live read fails, the feedback state is unknown and the advance is held.

Today, a hold caused only by a failed read is treated exactly like a hold on real pending feedback. It records the same `feedback-pending` admission cooldown, which lasts 10 × the poll interval (5 minutes at a 30 s poll). That reason is also not one of the short admission cooldowns, so the idle poll may sleep past it.

A single transient failure, such as one 502, therefore costs the item a full periodic re-evaluation before the advance is retried, even though the next read would very likely succeed. This was observed on `totalslacker/chronicle` #536 (reported in #2453, a community report). The gate held after a 502 and the hold cleared about 4 minutes later. The operator moved the card by hand.

A real pending-feedback hold is different: the item is waiting for the comment/review pass to handle the feedback, and a long recheck interval is appropriate. Only the "state unknown because the read failed" case warrants a quick retry. This matches the principle from #2232 that every engine-scheduled retry owns an admission cooldown sized to the retry it stands for.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A transient read failure at the advance is retried promptly (Priority: P1)

An operator has an item whose stage has just completed. At the advance, the feedback gate's live read fails once (for example a 502). The next read would succeed and find no pending feedback.

**Why this priority**: This is the reported defect. A single blip currently stalls an item for a full periodic re-evaluation and sometimes forces manual intervention.

**Independent Test**: In the sim, make exactly one live feedback read fail at the advance. The item advances within about one poll. With the change neutralised, the item instead waits for the periodic re-evaluation.

**Acceptance Scenarios**:

1. **Given** a completed stage ready to advance and a live feedback read that fails once, **When** the gate evaluates the advance, **Then** the advance is held, and a short admission cooldown (on the order of the merge gate's recheck interval) is recorded.
2. **Given** that hold, **When** the short cooldown expires and the next read succeeds with no pending feedback, **Then** the item advances without waiting for the periodic re-evaluation.
3. **Given** a hold from a failed read and an idle poll loop, **When** the poll computes its idle sleep, **Then** the interval is capped at the short cooldown, so the poll does not sleep past it.

---

### User Story 2 - Real pending feedback keeps its long recheck (Priority: P1)

An item has real unprocessed feedback (a comment, an unresolved review thread, or an unaddressed review body) at the advance.

**Why this priority**: The fix must not change behaviour for the common, correct hold. Shortening that cooldown would add load and churn for no benefit.

**Independent Test**: Unit test: with a successful read returning non-empty pending feedback, the recorded cooldown is `feedback-pending` at 10 × PollSeconds, as today.

**Acceptance Scenarios**:

1. **Given** a successful live read that returns pending feedback, **When** the gate holds the advance, **Then** it records `feedback-pending` at the unchanged 10 × PollSeconds interval.

---

### User Story 3 - Persistent read failures do not spin (Priority: P2)

The forge is unreachable or flaky, and the live read keeps failing.

**Why this priority**: A short retry interval must not turn a sustained outage into a tight loop of reads.

**Independent Test**: Unit/sim test: repeated read failures outside the forge-unreachable hold produce at most one re-read per short interval. While the forge-unreachable hold is engaged, nothing is dispatched.

**Acceptance Scenarios**:

1. **Given** consecutive failed reads and no forge-unreachable hold, **When** polls continue, **Then** the gate re-reads at most once per short cooldown interval for that item.
2. **Given** the forge-unreachable hold is engaged, **When** polls continue, **Then** no dispatch occurs for the item.

---

### Edge Cases

- The read succeeds but returns an empty result that is live: this is not a failed read and not a hold; behaviour is unchanged.
- A read fails while real pending feedback is already known from another source: the failed-read classification applies only when the gate holds solely because the state is unknown.
- Very small poll intervals: the short cooldown respects the existing 1-second floor used by the merge gate's recheck.
- A failure on the first read after a daemon restart behaves like any other failed read.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the feedback gate holds an advance only because its live feedback read failed (no pending feedback known and the read was not live), the engine MUST record a short admission cooldown of the same order as the merge gate's `merge-unsettled` recheck interval (PollSeconds/2, floor 1 s), instead of the 10 × PollSeconds `feedback-pending` cooldown.
- **FR-002**: The short cooldown reason used for a failed-read hold MUST be one the poll's idle-interval cap recognises as short, so the idle poll does not sleep past it. This may be an existing short reason or a new one.
- **FR-003**: A hold on real pending feedback (non-empty pending set from a live read) MUST keep recording `feedback-pending` at 10 × PollSeconds, unchanged.
- **FR-004**: Repeated read failures MUST NOT cause the engine to re-read faster than once per short cooldown interval per item. Research determines whether a consecutive-failure cap, or a fallback to the long interval after N consecutive failures, is also needed; the decision and its rationale are recorded in the ADR for this issue.
- **FR-005**: The gate MUST remain fail-closed: an unreadable feedback state still holds the advance or landing.
- **FR-006**: The log line for the failed-read hold ("live feedback read failed, feedback state unknown") and its meaning remain; the recorded cooldown reason and duration distinguish the two hold causes.
- **FR-007**: Tests: a unit test shows a failed live read records the short cooldown while real pending feedback records `feedback-pending` at 10 × PollSeconds; a sim twin shows a single transient read failure at the advance is retried and the item advances within about one poll, and that neutralising the change (per-Engine test seam) makes it wait for the periodic re-evaluation.
- **FR-008**: Documentation is updated in the same change: the `CLAUDE.md` text for `engine/feedback_gate.go` and the admission-cooldown description, and `docs/state-machine.md`; `docs/llms-full.txt` is regenerated.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a single transient live-read failure at the advance, the item advances within about one poll interval instead of waiting up to 10 × PollSeconds.
- **SC-002**: A hold on real pending feedback still rechecks at 10 × PollSeconds, with no change in recorded cooldown.
- **SC-003**: Under a sustained read failure outside the forge-unreachable hold, an item is re-read no more than once per short cooldown interval.
- **SC-004**: With the change neutralised, the sim twin fails (it observes the long wait), showing the test is not vacuous.

## Assumptions

- "The same order as `merge-unsettled`" means PollSeconds/2 with a 1 s floor; the exact reuse-vs-new-reason choice is a Plan/Implement decision.
- The forge-unreachable hold (#2393) already prevents dispatch during a sustained outage, so the short cooldown only needs to bound re-read rate outside it.
- Whether to add a consecutive-failure cap is decided in Research and recorded in the ADR; absent evidence of a spin risk, no cap is added.
- The ADR is numbered after this issue (`adrs/2498-...`).

## Out of Scope *(optional)*

- Changing how real pending feedback is detected, dispatched or cooled down.
- Changing the gate's fail-closed policy or the forge-unreachable hold.
- Retry behaviour of other gates (merge, CI, review) beyond reusing their short-interval conventions.
- Closing report #2453; the PR mentions it by bare number only and uses no closing keyword. The report is closed by hand once a release ships.

## Source References *(optional)*

- Community report #2453, follow-up comment: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6428197162
- #2232 — every engine-scheduled retry owns an admission cooldown
- #2393 — forge-unreachable hold
- #1953 — feedback gate
