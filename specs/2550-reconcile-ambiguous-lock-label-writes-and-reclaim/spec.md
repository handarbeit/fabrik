# Feature Specification: Reconcile ambiguous lock-label writes and reclaim orphaned engine locks at runtime

**Feature Branch**: `fabrik/issue-2550`
**Created**: 2026-11-04
**Status**: Specified
**Input**: User description: "Reconcile ambiguous lock-label writes and reclaim orphaned engine locks at runtime. Problem: A label write that GitHub answers with a 5xx can still have been applied. In `processCommentsClassified` (`engine/comments.go`, Step 1), a failed `AddLabelToIssue(…, "fabrik:editing")` returns `didNotRunEditingWrite` and records the `lock-write-failed` cooldown on the assumption that the label is absent. If GitHub applied it anyway, the label is on the issue, `itemNeedsWorkRetryAt` (`engine/item.go`, the `fabrik:editing` check) refuses every later dispatch, and the comment pass is never retried. Startup cleanup (`cleanupLockedIssues`) is the only thing that removes an orphaned `fabrik:editing`, so a running daemon holds the item until a person removes the label. Meanwhile `fabrik_status` reports `waiting` / `gate-hold` and nothing reports the item as stuck. This was observed on 0.0.90 during GitHub's 2026-10-30 incident: two user comments went unprocessed for more than 2 hours. Reported in community report #2533 (mention by number only). Requirements: R1 (reconcile an ambiguous write): after a transport-class failure (`gh.IsTransportError`: 5xx or timeout) adding or removing an engine lock/state label (`fabrik:editing`, `fabrik:locked:<id>`, `stage:<name>:in_progress`), re-read the item's labels live before deciding the outcome. If the add actually landed and no worker holds the item, remove it (or proceed as holder). Never back off with the label left in place. A failed re-read keeps today's behaviour plus R2's backstop. R2 (runtime orphan reclaim): a running daemon reclaims an engine lock label (`fabrik:editing`, its own `fabrik:locked:<id>`) that has been present with no in-flight worker for the item for longer than a grace period. This reuses `cleanupLockedIssues`' rules (only Fabrik's own locks, `ErrNotFound` counts as success) and logs one audit line per reclaim. R3 (the retry is real): after an R1 or R2 resolution, the item is re-admitted, so the pending comment pass runs on a later poll without a human. R4 (surface it): `internal/attention` classifies 'engine lock label present, no worker in flight, past grace' as `stalled`, not `waiting`/`gate-hold`. This makes it reach `fabrik_status`, `fabrik_situation` and the stall push. Acceptance: A sim twin: the `fabrik:editing` add returns a 500 but is applied (simgh fault). The pending comments are processed within a bounded number of polls, with no restart and no human. It fails with R1 and R2 neutralised. Unit tests cover R1's three outcomes (landed, not landed, re-read failed) and R2's grace boundary. An attention test covers R4. Linkage: Mention #2533 in the PR body; do NOT use a closing keyword (Closes/Fixes/Resolves) with it. Related for context: #552, #1135, ADR 2393."

## Background

Fabrik uses labels as durable state. Three of them act as engine locks or in-progress markers: `fabrik:editing` (taken by the comment-review path), `fabrik:locked:<id>` (taken by a stage run) and `stage:<name>:in_progress`. While one is on an item, dispatch refuses to touch it, so a label that is present with no worker behind it blocks the item completely.

GitHub can answer a label write with a 5xx or a timeout and still have applied it. When that happens on the `fabrik:editing` add, the engine treats the label as absent. It records a short `lock-write-failed` back-off and gives up on the comment pass. The label is in fact on the issue, so every later poll sees an editing lock held by nobody and skips the item. The only thing that removes such a label is the startup cleanup, so a running daemon holds the item until a person removes the label by hand. The status surfaces meanwhile describe the item as an ordinary wait or gate hold, so nothing tells an operator it is stuck.

This was seen on 0.0.90 during GitHub's 2026-10-30 incident: two user comments sat unprocessed for over two hours. The community report is #2533, mentioned here by number only. Related context: #552, #1135 and ADR 2393, which introduced the lock-write refusal and the back-off that assumes a failed write left no label.

The change closes the gap in two layers. Where the write happens, an ambiguous failure is reconciled against GitHub's actual state. As a backstop, a running daemon reclaims any engine lock that has been orphaned past a grace period and says plainly that the item is stuck while that is the case.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A comment pass survives an ambiguous lock write (Priority: P1)

A user comments on an item while GitHub is degraded. The engine's attempt to add `fabrik:editing` gets a 5xx, but GitHub applied the label. The comment is processed on a later poll without anyone restarting the daemon or editing labels.

**Why this priority**: This is the observed failure. Comments went unprocessed for hours and a human had to intervene.

**Independent Test**: With a simulated GitHub whose `fabrik:editing` add returns a 500 but is applied, and an item with a pending comment, advance polls and check that the comment is processed and the lock is gone within a bounded number of polls.

**Acceptance Scenarios**:

1. **Given** an item with an unprocessed comment and no worker, **When** the `fabrik:editing` add fails with a transport-class error but the label was applied, **Then** the engine does not leave the item locked with no holder, and the comment pass runs on a later poll.
2. **Given** the same failure, **When** the live label re-read shows the label is absent, **Then** behaviour is as today: the `lock-write-failed` back-off, and the comment pass is retried after it.
3. **Given** the same failure, **When** the live label re-read itself fails, **Then** the engine falls back to today's behaviour and the runtime reclaim in User Story 2 is the backstop.

---

### User Story 2 - A running daemon reclaims an orphaned engine lock (Priority: P1)

An engine lock label is on an item, no worker is in flight for it and it has been that way for longer than a grace period. This can come from an ambiguous write whose re-read also failed, or from any other path that leaves a lock behind. The daemon removes the label itself, logs the reclaim and the item is picked up again.

**Why this priority**: It is the backstop for every cause of an orphaned lock, including ones not foreseen. Without it, only a restart recovers.

**Independent Test**: Place `fabrik:editing` on an item with no worker. Advance the clock to just before and just after the grace period. Check that the label is untouched before it and removed after it, with one audit line.

**Acceptance Scenarios**:

1. **Given** an item carrying `fabrik:editing` with no in-flight worker, **When** the label has been present longer than the grace period, **Then** the engine removes it and logs one line for the reclaim.
2. **Given** the same item, **When** a worker is in flight for it, **Then** the label is never reclaimed.
3. **Given** the same item, **When** the label has been present for less than the grace period, **Then** the label is left alone.
4. **Given** an item carrying a `fabrik:locked:<id>` label that belongs to another operator or another Fabrik instance, **When** the grace period passes, **Then** the label is not touched.
5. **Given** the label disappeared between the engine's decision and its removal call, **When** the removal returns not-found, **Then** it counts as success and nothing is logged as a failure.

---

### User Story 3 - A stuck lock is visible as stalled (Priority: P2)

An operator or overseer asking for status sees an item with an engine lock label, no worker and an expired grace period as stalled, not as an ordinary wait or gate hold. The stall reaches the status tool, the situation view and the stall push.

**Why this priority**: The incident went unnoticed for two hours because every surface said the item was waiting. It matters even when reclaim fixes things within a grace period.

**Independent Test**: Classify an item with an engine lock label, no worker and the lock past grace, and check it reports `stalled`. Check that the same item inside grace, or with a worker, does not.

**Acceptance Scenarios**:

1. **Given** an item with an engine lock label, no worker and the label past grace, **When** its attention state is computed, **Then** it is `stalled`, and it appears in the status tool, the situation view and the stall push.
2. **Given** the same item inside the grace period, or with a worker in flight, **When** its attention state is computed, **Then** it is not `stalled` for this reason.

---

### User Story 4 - The recovered item is retried, not just unlocked (Priority: P1)

After the engine resolves an ambiguous write or reclaims an orphan, the item is admitted for work again. The pending comment pass runs on a later poll with no human action.

**Why this priority**: Removing the label without re-admission would leave the item idle until some unrelated event woke it, and the user would still see their comment ignored.

**Independent Test**: After a reconcile or a reclaim removes the lock, check that the item is admitted by the next poll or so and that the comment pass runs.

**Acceptance Scenarios**:

1. **Given** a lock removed by reconcile or reclaim and a pending comment, **When** later polls run, **Then** the comment pass runs and the comment is marked processed.
2. **Given** the removal wrote a `lock-write-failed` back-off earlier, **When** the lock is removed, **Then** the back-off does not delay the retry beyond its normal short duration.

---

### Edge Cases

- The transport failure is on a label **removal** rather than an add (R1 covers both): the re-read shows whether the label is still present, and the engine reconciles rather than assuming.
- GitHub answers the re-read from a stale replica and shows the label absent although the add landed. The runtime reclaim (User Story 2) is the backstop that catches it a little later.
- The re-read shows the label present while a worker for the item is in flight in this daemon: the label is the worker's own and is left alone.
- The label is present, no worker exists, but the item is legitimately held by another running Fabrik instance on the same board. Only this daemon's own locks are reclaimed (its own `fabrik:locked:<id>`); `fabrik:editing` carries no owner, so its reclaim relies on the grace period and the absence of a local worker. The assumptions record the accepted risk.
- A stage run's `stage:<name>:in_progress` marker is orphaned: R1 reconciles an ambiguous write of it, but R2 reclaim is limited to `fabrik:editing` and the daemon's own `fabrik:locked:<id>`.
- The daemon restarts during the grace period: the existing startup cleanup still applies and the in-memory grace clock restarts.
- Repeated ambiguous failures on the same item during a long outage: each is reconciled independently, with no unbounded retry loop and no charge against `max_retries` or any breaker.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a transport-class failure (5xx or timeout, as classified by `gh.IsTransportError`) while adding or removing an engine lock or state label (`fabrik:editing`, `fabrik:locked:<id>`, `stage:<name>:in_progress`), the engine re-reads the item's labels live before deciding the outcome of the write.
- **FR-002**: If the re-read shows an add actually landed and no worker holds the item, the engine either proceeds as the holder of the lock or removes the label. It never backs off while leaving the label in place with no holder.
- **FR-003**: If the re-read shows the label is absent, the engine keeps today's behaviour for a failed add, including the `lock-write-failed` back-off and no charge against retry counters.
- **FR-004**: If the re-read itself fails, the engine keeps today's behaviour and relies on FR-005 as the backstop.
- **FR-005**: A running daemon reclaims an engine lock label (`fabrik:editing`, or its own `fabrik:locked:<id>`) that has been present with no in-flight worker for the item for longer than a grace period.
- **FR-006**: Reclaim follows the rules of the existing startup cleanup: only Fabrik's own locks are removed, a not-found response on removal counts as success, and a reclaim never touches an item that has a worker in flight.
- **FR-007**: Each reclaim logs exactly one audit line naming the item and the label.
- **FR-008**: After a reconcile or reclaim removes a lock, the item is re-admitted so that its pending comment pass runs on a later poll without human action.
- **FR-009**: Neither the reconcile nor the reclaim counts as a failure of the stage or the comment pass: no retry counter, circuit breaker, `stage:<name>:failed` label or pause is charged or applied because of it.
- **FR-010**: The attention classifier reports an item with an engine lock label present, no worker in flight and the label past the grace period as `stalled`, not `waiting` or `gate-hold`. The state reaches the status tool, the situation view and the stall push.
- **FR-011**: An item inside the grace period, or with a worker in flight, is not classified as `stalled` for this reason.

### Key Entities *(if applicable)*

- **Engine lock label**: `fabrik:editing`, `fabrik:locked:<id>` and `stage:<name>:in_progress`. State the engine writes to say it is working on an item.
- **Orphaned lock**: an engine lock label with no in-flight worker for the item in this daemon.
- **Grace period**: how long an engine lock label may be present with no worker before it is reclaimed and reported as stalled.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a simulated run where the `fabrik:editing` add returns a 500 but is applied, the pending comments are processed within a bounded number of polls (a number stated in the test) with no restart and no human action.
- **SC-002**: The same simulated scenario fails when the reconcile and the runtime reclaim are both disabled, so the test proves the fix.
- **SC-003**: Unit tests cover the three reconcile outcomes (label landed, label not landed, re-read failed) and the grace boundary of the reclaim (just before, at and just after).
- **SC-004**: An attention test shows an orphaned lock past grace is `stalled` and the same item inside grace, or with a worker, is not.
- **SC-005**: No orphaned engine lock label survives longer than the grace period plus a bounded number of polls on a running daemon, in the simulated scenarios.

## Assumptions

- The grace period is long enough that normal short lock holds, label propagation delays and a worker that has just exited are never mistaken for an orphan. The exact value is chosen in the Plan stage and is not an operator setting unless the Plan stage finds one is needed.
- The attention classifier uses the same grace period as the reclaim, so an item is reported `stalled` at about the time the reclaim becomes due; a reclaim that succeeds then clears the stall.
- The reclaim covers `fabrik:editing` and the daemon's own `fabrik:locked:<id>`, as the issue states. The reconcile in FR-001 covers the third label (`stage:<name>:in_progress`) as well. Runtime reclaim of orphaned `in_progress` markers is not added here.
- "Worker in flight" means a worker tracked by this daemon. A label held by a different Fabrik instance is not distinguishable for `fabrik:editing`, which carries no owner, and the grace period bounds that risk.
- The reconcile and reclaim need no new operator-facing configuration and no new label.
- Behaviour on a healthy GitHub is unchanged: the re-read happens only after a transport-class failure.

## Out of Scope *(optional)*

- Changing how GitHub outages are detected or the forge-unreachable hold (ADR 2393).
- Reclaiming orphaned markers other than `fabrik:editing` and the daemon's own `fabrik:locked:<id>`, such as `stage:<name>:in_progress`.
- Reconciling ambiguous writes for non-lock labels or for comments, reactions and board moves.
- Any change to how a paused item is resumed.
- Closing or editing community report #2533. It is mentioned in the PR body by number only, with no closing keyword, and is closed by hand after a release containing the fix.

## Source References *(optional)*

- Community report #2533 (observed on 0.0.90 during GitHub's 2026-10-30 incident)
- Related for context: #552, #1135, ADR 2393
