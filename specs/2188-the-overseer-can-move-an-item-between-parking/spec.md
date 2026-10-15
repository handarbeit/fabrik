# Feature Specification: The overseer can move an item between parking columns

**Feature Branch**: `fabrik/issue-2188`
**Created**: 2026-10-12
**Status**: Specified
**Input**: User discussion: a board may configure more than one unmanaged column (e.g. `Triage` before `Backlog`). The overseer is told when items arrive in either (#2187) but cannot act on the obvious triage decision, "accepted, not yet": Triage → Backlog.

## Background

`fabrik_promote` (#1969, ADR-1966-c) moves an item **out of** an unmanaged parking column (`stages.IsParkingColumn`) **into** a pipeline stage. Its destination list, `promotableTargets` (`engine/overseer_actions.go`), deliberately excludes unmanaged, holding and cleanup columns. So with two parking columns configured, the overseer can promote Triage → Specify but has no way to file Triage → Backlog, or to send an item back from Backlog to Triage. The only remaining route is a human dragging the card.

Parking-to-parking moves start no work: the engine never dispatches for an unmanaged column. They are the lowest-risk board write the overseer could make.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Triage an arrival into Backlog (Priority: P1)

An overseer receives `item-added` for a card in Triage, reads it with `fabrik_status`, and files it into Backlog.

**Why this priority**: This is the whole feature: the triage decision "accepted, not yet" is the one the overseer currently cannot make, and it is the only move the issue adds.

**Independent Test**: Sim with `Triage` and `Backlog` both `unmanaged: true`; the move succeeds, the board Status is Backlog, one audit comment is posted, one `column-changed` (Triage → Backlog, actor `overseer`, `via` = the requesting subscriber) is published, and no stage is dispatched.

**Acceptance Scenarios**:

1. **Given** an open issue in Triage, **When** the overseer moves it to Backlog, **Then** its Status is Backlog, one `🏭 **Fabrik — overseer action: …**` audit comment names the requester, and nothing is dispatched.
2. **Given** an open issue in Backlog, **When** the overseer moves it to Triage, **Then** the same holds.
3. **Given** an item in a pipeline column (e.g. Research), **When** the overseer asks to park it, **Then** the request is refused with the item's current state and nothing is written; this issue adds no way to pull work out of the pipeline.
4. **Given** an item in Triage carrying `fabrik:cruise` or `fabrik:yolo`, **When** the overseer moves it to Backlog, **Then** the move succeeds like any other and no stage runs (an unmanaged destination is never dispatched).

---

### Edge Cases

- The destination is the item's current column: the request is **refused** with the item's current state and nothing is written (no write, no audit comment). This matches the existing action convention that a refused action writes nothing, and avoids a "success" that did nothing.
- The destination is a holding stage, the cleanup stage (Done), a pipeline stage or a column with no configured stage (other than the legacy `Backlog`): refused, listing the valid parking destinations.
- The destination is a parking column that exists in the stage configuration but has no matching option on the board: refused (the move cannot be written), naming the missing column.
- The legacy literal `Backlog` column with no stage configured counts as a parking column on both sides (`stages.IsParkingColumn`); a configured `Backlog` stage that is not unmanaged is a pipeline column on both sides.
- Only one parking column exists: there is no valid destination; the request is refused and says so.
- PR cards, closed items and items not on the board: refused, as `fabrik_promote` refuses them today.
- The column is confirmed with one live, ID-keyed `FetchProjectItemStatus` before writing, failing closed (same guard as `fabrik_promote`); a stale cache can never move an item that has since entered the pipeline.
- A paused or blocked item in a parking column may still be moved; the move changes no labels and lifts no pause.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The overseer MUST be able to move an open, non-PR, managed-board issue from one parking column to another parking column.
- **FR-002**: Both the source and the destination MUST satisfy `stages.IsParkingColumn`; anything else is refused with `CodeRefused` and the item's current state.
- **FR-003**: The move MUST reuse `moveItemToStatus` (requester carried onto `column-changed`), `addLabelChecked`-style guards and `overseerAuditBody`. It adds no new engine transition and no new label.
- **FR-004**: Choose and record in an ADR numbered after this issue whether this widens `fabrik_promote`'s destination set or adds a separate tool (e.g. `fabrik_park`). The deciding constraint: a tool's name must not suggest it starts work when it does not.
- **FR-005**: `hello`/`tools/list` advertise the capability only when the server has an `Actor`, as for the existing actions.
- **FR-006**: The static scans that keep the read path free of GitHub writes (`TestLocalAPISourceDoesNotReachGitHub`, the channel scan) MUST pass unweakened; the write lives in `engine/overseer_actions.go`.
- **FR-007**: A successful move MUST post exactly one audit comment naming the action and the requesting subscriber, and MUST publish exactly one `column-changed` event attributed to the overseer with the subscriber as `via`.
- **FR-008**: A refused move MUST write nothing (no Status change, no comment, no label) and MUST report the item's current state; a destination-related refusal lists the valid parking destinations.
- **FR-009**: A move to the item's current column MUST be refused (see Edge Cases).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A sim test covers User Story 1 end to end, including the `column-changed` event's actor and requester, and is shown non-vacuous.
- **SC-002**: A test fails if a parking move can target a pipeline, holding or cleanup column, or can start from a pipeline column.
- **SC-003**: `docs/state-machine.md` §7.15 and the USER_GUIDE overseer section document the move; `docs/llms-full.txt` is regenerated.

## Assumptions

- Refusing a same-column move (rather than a silent no-op success) is the chosen behaviour; it is low-impact and reversible.
- The requester name comes from the shim's existing `--subscriber` setting; no per-call override is added.
- Parking moves never change autonomy labels, pause labels or any other label.
- The legacy-`Backlog` rule is exactly `stages.IsParkingColumn`'s existing definition; this issue does not redefine it.

## Out of Scope

- Moving an item from a pipeline column back into a parking column ("un-promote").
- Changes to which columns produce `item-added` (#2187).
- Any tool that lifts a pause or posts a free-form comment (still deliberately absent, ADR-1813).

## Source References

- #1969 (overseer actions), ADR-1966-c
- #2123 / #2187 (arrival and column events), ADR-2125 (actor attribution)
