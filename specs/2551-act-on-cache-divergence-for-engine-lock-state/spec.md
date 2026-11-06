# Feature Specification: Act on cache-divergence for engine lock/state labels

**Feature Branch**: `fabrik/issue-2551`
**Created**: 2026-11-04
**Status**: Specified
**Input**: User description (verbatim, as filed):

> ## Problem
>
> The channel's `cache-divergence` anomaly detects when a later listing or probe contradicts an engine label write, but nothing acts on the divergence. On 0.0.90 (2026-10-31, no GitHub incident), four such events occurred on one repo within two hours:
>
> - The engine added `fabrik:locked:<id>`, and a probe minutes later showed it absent while the worker ran. The item had no lock while a worker was in flight.
> - The engine removed `stage:Review:complete` when a Review rework started, and a probe 233 s later still showed it present while the rework ran. If that stale label is trusted when the worker exits, the item can advance without its rework being checked.
>
> The observed-at rule (ADR 2348) keeps the engine's own value for the grace window and then accepts the listing, so a write whose reported success did not stick is silently lost.
>
> Reported in a follow-up on community report #2533 (mention by number only).
>
> ## Requirements
>
> - R1: When a divergence is detected on an engine-written lock or state label (`fabrik:locked:<id>`, `fabrik:editing`, `stage:<name>:complete`, `stage:<name>:in_progress`, the pause pair), confirm with one live label read. If GitHub disagrees with the engine's intended state while that intent is still current (worker in flight, rework open), re-apply the intended state once and log it.
> - R2: While the intended state and GitHub disagree, no advance or landing decision trusts the diverged label. In particular, a `stage:<X>:complete` the engine removed for a rework that is still present does not complete or advance the stage when the worker exits; the gate re-reads instead.
> - R3: A re-apply that fails, or a divergence that recurs on the same item and label within the window, pauses nothing but is reported (warning or `item-wedged`-class event) so a person can see it.
> - R4: First establish, with a reproduction or captured evidence, whether these divergences are lost writes (GitHub never applied them) or stale reads (the listing lagged past the grace window). If they are stale reads, R1's re-apply must not fight a lagging listing. Record the finding in the ADR.
>
> ## Acceptance
>
> - Sim twins for both shapes (lock add not held; complete-label removal not held during a rework). They show re-apply in the first case and no advance on the stale label in the second, and fail with the fix neutralised.
> - The ADR records the R4 finding.
>
> ## Linkage
>
> Mention #2533 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) with it. Related: ADR 2170, ADR 2348, ADR 2159.

## Background

Fabrik keeps its operational state in GitHub labels. Some of them are engine-written locks and state markers: `fabrik:locked:<id>`, `fabrik:editing`, `stage:<name>:in_progress`, `stage:<name>:complete`, and the pause pair (`fabrik:paused` + `fabrik:awaiting-input`). Decisions such as "this item is locked by a running worker" and "this stage is complete, advance" rest on those labels.

The engine already detects when a later listing or probe contradicts one of its own recent label writes. It reports this as the `cache-divergence` channel event (ADR 2170). Detection is observation only: nothing confirms the divergence against GitHub and nothing acts on it. The observed-at ordering rule (ADR 2348, building on ADR 2159) protects an engine write for a grace window and then accepts the listing's value. A write that GitHub reported as successful but that did not stick is therefore silently lost once the window passes.

On 0.0.90 (2026-10-31, no GitHub incident) four such divergences occurred on one repo within two hours. The two shapes with consequences:

1. **Lock add not held.** The engine added `fabrik:locked:<id>` and a probe minutes later showed it absent while the worker was still running, so the item had no lock while a worker was in flight.
2. **Complete-label removal not held.** The engine removed `stage:Review:complete` when a Review rework started. A probe 233 s later still showed the label present while the rework ran. If that stale label is trusted when the worker exits, the item can advance without its rework having been checked.

It is not yet known whether these are lost writes (GitHub never applied them) or stale reads (the listing lagged past the grace window). The right remedy depends on which, so the work starts by establishing that. The issue was raised in a follow-up on community report #2533.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An engine-written lock or state label that did not stick is restored (Priority: P1)

An operator running Fabrik wants an item with a worker in flight to keep its lock and in-progress markers, even if one of the engine's label writes was silently lost.

**Why this priority**: A running worker with no lock label lets a second engine instance or a later poll treat the item as free. That risks duplicate work and conflicting writes.

**Independent Test**: In the sim, make the engine's `fabrik:locked:<id>` add appear not to hold (the next listing or probe shows it absent) while the worker is in flight. Check that the engine confirms with one live label read, re-applies the lock once, and logs it. Then run with the fix neutralised and check that the lock stays absent.

**Acceptance Scenarios**:

1. **Given** a worker in flight whose lock label the engine added, **When** a later listing or probe contradicts that write and a live read confirms the label is absent, **Then** the engine re-applies the label once and logs the re-apply.
2. **Given** a divergence detected on an engine-written label, **When** the live read shows GitHub agrees with the engine's intended state, **Then** nothing is re-applied (a lagging listing is not fought).
3. **Given** the engine's intent is no longer current (the worker has exited or the rework has closed), **When** a divergence is detected, **Then** nothing is re-applied.

---

### User Story 2 - A diverged completion label never advances a stage (Priority: P1)

An operator wants a stage that is being reworked to be checked before it advances, even when a stale `stage:<X>:complete` label is still visible.

**Why this priority**: This is the failure that costs correctness. An item can move forward with its rework unchecked.

**Independent Test**: In the sim, remove `stage:Review:complete` for a rework and make the label remain visible on GitHub while the worker runs. When the worker exits, check that the stage is not completed and the item does not advance on the stale label. Check that the gate re-reads instead. Then run with the fix neutralised and check that the item advances.

**Acceptance Scenarios**:

1. **Given** the engine removed `stage:<X>:complete` for a rework and the label is still present on GitHub, **When** the worker exits, **Then** the stage is not treated as complete and no advance or landing decision trusts the label.
2. **Given** the intended state and GitHub disagree on a label, **When** any advance or landing decision runs, **Then** it re-reads the label's state rather than using the diverged copy.
3. **Given** the disagreement has been resolved (GitHub now matches the intent), **When** the next decision runs, **Then** it proceeds normally.

---

### User Story 3 - Unresolved or recurring divergence is visible to a person (Priority: P2)

An operator wants to know when the engine could not restore a label, or when the same label keeps diverging, without the item being paused.

**Why this priority**: Auto-repair can fail or loop. Without a visible signal, a person sees nothing until work goes wrong.

**Independent Test**: In the sim, make the re-apply fail, and separately make the same item and label diverge repeatedly within the window. Check that a warning or `item-wedged`-class event is reported in each case and that the item is not paused.

**Acceptance Scenarios**:

1. **Given** a re-apply that fails, **When** the failure is recorded, **Then** it is reported to the operator and the item is not paused.
2. **Given** a divergence that recurs on the same item and label within the window, **When** the recurrence is detected, **Then** it is reported to the operator and the item is not paused.

---

### User Story 4 - The cause of the divergences is established before the remedy is fixed (Priority: P1)

A maintainer wants to know whether the 0.0.90 divergences were lost writes or stale reads before the engine starts re-applying labels.

**Why this priority**: If the listing merely lagged, an unconditional re-apply would fight the listing and churn labels. The finding decides how careful the re-apply must be.

**Independent Test**: Read the ADR for this issue. It contains a finding on lost write versus stale read, backed by a reproduction or captured evidence.

**Acceptance Scenarios**:

1. **Given** the investigation is complete, **When** the ADR is read, **Then** it states whether the observed divergences were lost writes or stale reads and cites the evidence.
2. **Given** the finding is "stale reads", **When** the re-apply behaviour is reviewed, **Then** it is shown not to fight a lagging listing.

---

### Edge Cases

- The listing is merely lagging: the live read shows GitHub already matches the intent, so there is no re-apply.
- The worker exits between the divergence being detected and the live read: the intent is no longer current, so nothing is re-applied.
- The live label read itself fails: the engine does not assume GitHub agrees, and it does not trust the diverged label for any advance or landing decision. The failure is reportable.
- A human removed or added the label deliberately while the engine's intent was current: the live read cannot tell this apart from a lost write. The re-apply is bounded to one attempt and the recurrence is reported.
- The same item and label diverge again immediately after a re-apply: this counts as a recurrence and is reported, not re-applied indefinitely.
- A divergence on a label the engine did not write, or on an engine label outside the listed set: out of the behaviour's scope.
- A webhook-less (poll-only) daemon and a webhook-ingesting daemon both see divergences; the behaviour is the same for both.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a divergence is detected on an engine-written lock or state label (`fabrik:locked:<id>`, `fabrik:editing`, `stage:<name>:complete`, `stage:<name>:in_progress`, or the pause pair `fabrik:paused` + `fabrik:awaiting-input`), the engine confirms it with exactly one live label read for that item.
- **FR-002**: If the live read shows GitHub disagrees with the engine's intended state and that intent is still current (a worker is in flight, or a rework is open), the engine re-applies the intended state once and logs the re-apply.
- **FR-003**: If the live read shows GitHub already matches the intended state, or the intent is no longer current, the engine writes nothing.
- **FR-004**: While the engine's intended state and GitHub disagree on a label, no advance or landing decision trusts that label.
- **FR-005**: A `stage:<X>:complete` label that the engine removed for a rework and that is still present does not complete or advance the stage when the worker exits. The gate re-reads the label's state instead.
- **FR-006**: A failed re-apply is reported to the operator through an existing warning or `item-wedged`-class event, and the item is not paused.
- **FR-007**: A divergence recurring on the same item and label within the divergence window is reported to the operator through an existing warning or `item-wedged`-class event, and the item is not paused. The engine does not re-apply more than once per episode.
- **FR-008**: Before the remedy is finalised, the work establishes, by reproduction or captured evidence, whether the observed divergences are lost writes (GitHub never applied them) or stale reads (the listing lagged past the grace window). The finding is recorded in the ADR for this issue.
- **FR-009**: If the finding is that the divergences are stale reads, the re-apply of FR-002 is shown not to fight a lagging listing.
- **FR-010**: The sim suite includes a twin for each shape: lock add not held, and complete-label removal not held during a rework. Each twin fails when the behaviour is neutralised and passes when it is active.

### Key Entities *(if applicable)*

- **Engine-written lock/state label**: a label the engine adds or removes to record that an item is locked, in progress, complete, editing or paused.
- **Intended state**: what the engine last wrote for such a label, together with whether that write is still current (worker in flight, rework open).
- **Divergence**: a later listing or probe contradicting an engine label write, as already reported by the `cache-divergence` anomaly.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the sim, a lock-label add that did not stick during a running worker is restored with exactly one live read and one write, and logged.
- **SC-002**: In the sim, an item whose `stage:<X>:complete` removal did not hold during a rework never advances on that label when the worker exits.
- **SC-003**: In the sim, when the live read shows no disagreement, no label write occurs.
- **SC-004**: In the sim, a failed re-apply and a recurring divergence each produce one operator-visible report and leave the item unpaused.
- **SC-005**: Both sim twins fail with the behaviour neutralised and pass with it active.
- **SC-006**: The ADR states the lost-write versus stale-read finding and the evidence behind it.

## Assumptions

- "Intent still current" means the engine holds a worker for the item or an open rework bracket, as the item's in-memory state already records.
- The divergence window and the detection trigger are the existing `cache-divergence` ones (default 5 minutes). This issue adds no new detector.
- "Reported" reuses an existing channel: the operator warnings panel or an `item-wedged`-class channel event. No new event type is required.
- One re-apply per item and label per divergence episode is enough. Repeating it risks fighting a lagging listing or a deliberate human edit.
- The 0.0.90 evidence is limited to what the logs and the report capture. If a faithful reproduction is not possible, the ADR records what was captured and the confidence level.
- Existing behaviour is unchanged for labels outside the listed set, and for divergences with no current intent.

## Out of Scope *(optional)*

- Changing the observed-at ordering rule (ADR 2348) or the engine-write grace window (ADR 2159).
- Changing the `cache-divergence` detector itself, its window, or its event shape.
- Divergences on labels the engine does not write, on board Status (column) moves, or on human-applied labels.
- Pausing items on divergence.
- Closing community report #2533. It is closed by hand after a release, never by the PR.

## Source References *(optional)*

- Community report #2533 (mention by bare number only; no closing keyword).
- ADR 2170 (channel anomaly events), ADR 2348 (observed-at ordering rule), ADR 2159 (engine-write protection against stale listings).
