# Feature Specification: Probe-loop apply honours engine-write protection

**Feature Branch**: `fabrik/issue-2178`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

#2159 (ADR 2159) protects the engine's recent writes against a stale board listing, but only on the light-reconcile path. The probe loop's apply, `applyProbeItem` (`internal/itemstate/project_apply.go`), still overwrites `Status` and `State`/`IsClosed` from a board query that lags writes in the same way. It carries no fetch time and never consults `EngineWriteAt`.

Sequence:
1. The engine moves an item's Status (`UpdateItemStatus` → `LocalStatusUpdated`) or closes it (`LocalIssueClosed`). The cache gets the new value and an `EngineWriteAt` record.
2. A probe listing fetched before GitHub reflects the write arrives as `ProbeBoardItemUpdated`.
3. `applyProbeItem` restores the old Status or open state, sets `StatusChanged`/`StateChanged`, and resets `StatusEnteredAt`.

Gates and dispatch then read the pre-write column or state until a later probe or reconcile corrects it. A reverted Status can re-dispatch a stage that just completed. A reverted close can re-admit a landed item.

ADR 2159's Consequences section records this as an unprotected residual, with \"routing the probe through `protectShallow` is a follow-up\".

## Requirements

- **R1:** The probe apply honours the same engine-write protection as the reconcile apply. Stamp the probe listing with its fetch time (taken before the query is issued) and route its `Status` and closed-state fields through `itemstate.protectShallow`, deciding inside the store under its mutex, exactly as `ShallowBoardItemUpdated` does.
- **R2:** One rule. The probe and reconcile paths share `protectShallow` and `DefaultEngineWriteGrace`. No second copy of the rule.
- **R3:** Real external changes still win. A probe listing fetched after write + grace overwrites as before. A human moving the card or reopening the issue outside the grace window is applied on the next probe.
- **R4:** A kept value produces no `StatusChanged`/`StateChanged` flag and no channel event. `StatusEnteredAt` is not reset.
- **R5:** Docs. Update `docs/state-machine.md` (probe loop §7, D.4/D.5 engine-write protection) and ADR 2159's residuals note.

## Scope

- In scope: `applyProbeItem` and the probe listing's fetch-time plumbing.
- Out of scope: the deep-fetch label write-back (a separate residual in ADR 2159), and the reinvoke lock leak (separate issue).

## Acceptance

- Unit test in `internal/itemstate`: an engine Status write, then a probe listing fetched before write + grace carrying the old Status. The cached Status is kept, with no `StatusChanged` and an unchanged `StatusEnteredAt`. The same holds for an engine close against a stale open probe.
- Unit test: a probe listing fetched after write + grace overwrites.
- Each new test fails with the probe routed around `protectShallow`.
- The full test suite passes."

## Background

#2159 (ADR 2159) made the engine's own recent writes (a Status move, an issue close) survive a stale board listing, but only on the light-reconcile path. The probe loop applies board listings through a separate path that carries no fetch time and never consults the engine's write record. GitHub's board query lags writes, so a probe listing fetched just before a write can arrive just after it and restore the old value.

The sequence is:

1. The engine moves an item's Status or closes the issue. The cache takes the new value and records when the engine wrote it.
2. A probe listing fetched before GitHub reflects that write arrives.
3. The probe apply restores the old Status or open state, flags the change, and resets the time the item entered its column.

Until a later probe or reconcile corrects it, gates and dispatch read the pre-write column or state. A reverted Status can re-dispatch a stage that just completed. A reverted close can re-admit an item that has already landed. ADR 2159 records this as an unprotected residual and names routing the probe through the same protection as the follow-up. This issue is that follow-up.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A stale probe does not revert the engine's Status move (Priority: P1)

After the engine moves an item to a new board column, a probe listing that was fetched before GitHub reflected the move must not put the item back in its old column.

**Why this priority**: A reverted Status is the failure that can re-dispatch a stage that has just completed, which wastes a worker and can corrupt pipeline state.

**Independent Test**: Record an engine Status write on an item, then apply a probe listing that was fetched before the write plus the grace period and that carries the old Status. Assert the cached Status is the engine's value, no Status-change flag is set, and the time the item entered its column is unchanged.

**Acceptance Scenarios**:

1. **Given** the engine has just written a new Status for an item, **When** a probe listing fetched before the write plus the grace period carries the old Status, **Then** the cached Status stays the engine's value, no Status-change flag is raised, and the entered-column time is unchanged.
2. **Given** the same item, **When** the probe listing carries the engine's new Status, **Then** the result is identical to the pre-change behaviour.

---

### User Story 2 - A stale probe does not reopen an issue the engine closed (Priority: P1)

After the engine closes an issue, a probe listing fetched before GitHub reflected the close must not mark it open again.

**Why this priority**: A reverted close can re-admit a landed item to dispatch.

**Independent Test**: Record an engine close on an item, then apply a probe listing fetched before the write plus the grace period that shows the issue open. Assert the cached state stays closed with no state-change flag.

**Acceptance Scenarios**:

1. **Given** the engine has just closed an issue, **When** a stale probe listing shows it open, **Then** the cached state stays closed and no state-change flag is raised.

---

### User Story 3 - Real external changes still win (Priority: P1)

A human moving a card or reopening an issue must still be picked up by the probe loop once the engine's write is no longer recent.

**Why this priority**: Protection that also suppressed genuine external edits would leave the cache wrong indefinitely.

**Independent Test**: Record an engine write, then apply a probe listing fetched after the write plus the grace period carrying a different value. Assert the listing's value is applied with the usual change flags.

**Acceptance Scenarios**:

1. **Given** an engine Status write, **When** a probe listing fetched after the write plus the grace period carries a different Status, **Then** that Status is applied, the Status-change flag is raised, and the entered-column time is reset as before.
2. **Given** an engine close, **When** a probe listing fetched after the write plus the grace period shows the issue open, **Then** the open state is applied as before.
3. **Given** an item with no recent engine write, **When** any probe listing is applied, **Then** behaviour is unchanged.

---

### Edge Cases

- A kept (protected) value must not produce a Status-change or state-change flag, must not reset the time the item entered its column, and must not produce a channel event.
- A probe listing with an unknown or zero fetch time must not cause protection to be skipped silently in a way that differs from how the reconcile path treats an unknown fetch time.
- The Status and the closed-state fields are decided independently: a recent engine Status write must not shield a genuine external close, and a recent engine close must not shield a genuine external Status move.
- The fetch time is taken before the probe query is issued, so a response that was slow to arrive is not mistaken for a fresh one.
- The protection decision is made inside the store under its lock, so a write that lands between the listing's fetch and its application is still honoured.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Each probe board listing carries the time at which it was fetched, taken before the query is issued.
- **FR-002**: When a probe listing is applied, its Status and closed-state values are passed through the same engine-write protection that the light-reconcile apply uses, and the decision is made inside the store while holding its lock.
- **FR-003**: The probe and reconcile paths share one protection rule and one grace-period constant; there is no second copy of either.
- **FR-004**: A probe listing fetched before the engine's write plus the grace period does not overwrite the engine-written Status or closed state.
- **FR-005**: A probe listing fetched after the engine's write plus the grace period overwrites as it does today, so a human moving the card or reopening the issue outside the grace window is applied on the next probe.
- **FR-006**: When protection keeps the cached value, no Status-change or state-change flag is raised, no channel event results, and the time the item entered its column is not reset.
- **FR-007**: Items with no recent engine write are applied exactly as before.
- **FR-008**: `docs/state-machine.md` is updated (probe loop, §7, and the D.4/D.5 engine-write protection sections) and ADR 2159's residuals note is updated to record the probe as protected. `docs/llms-full.txt` is regenerated in the same change.
- **FR-009**: Unit tests in `internal/itemstate` cover: a stale-Status probe being kept, a stale-open probe against an engine close being kept, and a post-grace probe overwriting. Each test fails if the probe apply is routed around the protection.

### Key Entities

- **Probe board listing**: the lightweight per-item board snapshot the probe loop fetches and applies; gains a fetch time.
- **Engine write record**: the per-item record of when the engine last wrote Status or closed state, consulted by the shared protection rule.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After an engine Status move or close, no probe listing fetched before the write plus the grace period changes the cached Status or closed state.
- **SC-002**: A probe listing fetched after the write plus the grace period still changes the cached value, so a genuine external change is reflected by the next probe.
- **SC-003**: A protected (kept) value produces zero Status-change or state-change flags and zero channel events, and leaves the entered-column time unchanged.
- **SC-004**: The new tests fail when the probe path is routed around the protection, and the full test suite passes.
- **SC-005**: Exactly one definition of the protection rule and one grace-period constant exist, used by both the probe and reconcile paths.

## Assumptions

- #2159 lands first. As of this spec the shared protection rule, the grace constant and the engine write record exist only on the `fabrik/issue-2159` branch, not on `main`; this issue builds on them and does not re-implement them.
- The probe loop's Store writes keep using their existing probe-origin marking so they still do not request an early poll; this change does not alter that.
- The grace period is the existing default used by the reconcile path; no new configuration knob is introduced.
- The "fetch time" stamped on a listing is the time just before the query is issued, matching the reconcile path's convention.

## Out of Scope *(optional)*

- The deep-fetch label write-back, a separate residual recorded in ADR 2159.
- The reinvoke lock leak, tracked as a separate issue.
- Any change to the grace period's length or making it configurable.
- Protecting fields other than Status and closed state.

## Source References *(optional)*

- #2159 / ADR 2159: engine-write protection on the light-reconcile path, and its Consequences note naming this follow-up.
- `docs/state-machine.md` §7 (probe loop) and the D.4/D.5 engine-write protection sections.
