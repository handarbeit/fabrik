# Feature Specification: Layer 1 status refresh must apply a GitHub read, not an engine write

**Feature Branch**: `fabrik/issue-2347`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "## Problem

The Layer 1 status refresh records a GitHub read as an engine write. Found by the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15), §1.8.

On every `issues` and `issue_comment` webhook, `applyLayer1StatusRefresh` (engine/reconcile.go:131) reads the item's Status live and writes it through `cache.UpdateItemStatus` → `LocalStatusUpdated` (reconcile.go:198,207; boardcache.go:1277). That:

- stamps `EngineWriteAt[\"status\"]` even when nothing changed (internal/itemstate/store.go:503),
- clears `StatusAtGitHub` and marks the change `EngineStatusWrite`, so channel events attribute it to `fabrik`,
- runs concurrently with, and unordered against, `projects_v2_item.edited` deliveries and the engine's own moves.

A stale read overwrites a newer move and is then shielded from the correcting listing for 30 s by the engine-write protection: the bypass #2178 closed, reintroduced through a different door.

## Requirements

- **R1:** The Layer 1 refresh applies its result through a read-sourced mutation that carries the read's fetch time and is subject to the same protection as a listing (an engine write within the grace wins; a read older than the field's last write is dropped).
- **R2:** It never stamps `EngineWriteAt`, never clears `StatusAtGitHub` with a non-GitHub value, and its changes are not attributed to `fabrik`.
- **R3:** A refresh whose read equals the current value applies nothing.

## Acceptance

- Tests: a refresh read taken before an engine move does not revert the move; a refresh never creates an engine-write record; attribution of a refresh-driven change is not `fabrik`.
- The #2178 regression tests still pass."

## Background

The engine keeps a cached copy of each item's board Status. Two kinds of input update it, and they must be told apart:

- **Engine writes** — the engine itself moved the item. The engine records the time of such a write and, for a 30-second grace window, protects that value from being overwritten by listings or reads that were taken before the write (the protection added by #2178 / ADR 2178).
- **GitHub reads** — values observed from GitHub (board listings, probes, webhook payloads). These carry the time they were fetched and lose to a newer engine write.

The Layer 1 status refresh runs on every `issues` and `issue_comment` webhook. It reads the item's Status live from GitHub, but then applies the result as if the engine had performed a move. Consequences found by the engine state audit (§1.8, rated medium–high severity, "worse" trend):

- The item is recorded as freshly engine-written even when the Status did not change, extending the protection window for no reason.
- The GitHub-provided move time for the Status is discarded, and downstream channel events attribute the change to `fabrik` rather than to whoever actually moved the item.
- The refresh runs concurrently with, and unordered against, Status webhooks and the engine's own moves. A read that was taken before a newer move can overwrite that move, and the 30-second protection then shields the stale value from the next correcting board listing. This reintroduces, through a different path, the bypass that #2178 closed.

The audit's remedy list (item 7) is to apply the refresh as a read-sourced, time-stamped mutation rather than an engine write.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A stale refresh read never undoes a newer move (Priority: P1)

An operator or the engine moves an item to a new column while a webhook-triggered Status refresh is in flight. The refresh read was taken before the move.

**Why this priority**: This is the data-correctness defect: a wrong column that the engine then protects from correction.

**Independent Test**: Start a refresh read, perform an engine move of the same item's Status before the read is applied, then apply the read result. The item's Status must remain the moved-to value.

**Acceptance Scenarios**:

1. **Given** an item whose Status was moved by the engine after a refresh read was fetched, **When** that older read is applied, **Then** the cached Status stays at the engine-moved value.
2. **Given** an item whose Status last changed (by any source) later than a refresh read's fetch time, **When** the read is applied, **Then** the read is dropped and nothing changes.
3. **Given** a refresh read that is newer than every prior write and differs from the cached Status, **When** it is applied, **Then** the cached Status takes the read value.

---

### User Story 2 - A refresh is never recorded as an engine write (Priority: P1)

A webhook triggers the Layer 1 refresh and the read result differs from, or equals, the cached Status.

**Why this priority**: A false engine-write record is what shields stale values from correction for 30 seconds.

**Independent Test**: Apply refreshes (changed and unchanged) and inspect the item's engine-write bookkeeping: no Status engine-write record may exist that did not come from a real engine move.

**Acceptance Scenarios**:

1. **Given** an item with no engine Status write on record, **When** a refresh applies a changed Status, **Then** no engine-write record for Status is created.
2. **Given** a refresh whose read equals the cached Status, **When** it is applied, **Then** no state, record, timestamp or event changes.
3. **Given** a refresh applies a changed Status, **When** the resulting change is observed (for example by channel events), **Then** it is not attributed to `fabrik`.

---

### User Story 3 - The refresh respects the same protection as a listing (Priority: P2)

A real engine move happened recently (within the grace window) and a refresh read from before or around that time arrives.

**Why this priority**: The refresh should be no more able to bypass the #2178 protection than a board listing is.

**Independent Test**: Run the #2178 protection scenarios with the refresh as the input source instead of a listing; outcomes must match.

**Acceptance Scenarios**:

1. **Given** an engine Status write within the grace window and a refresh read fetched before that write plus grace, **When** the read is applied, **Then** the engine value is kept.
2. **Given** the grace window has passed since the engine write and the read was fetched after it, **When** the read is applied, **Then** the read value is applied.

---

### Edge Cases

- The refresh read fails or returns no Status: nothing is applied (unchanged behavior).
- The cached item has no Status yet: a successful read populates it, without an engine-write record.
- The read's Status equals the cached Status but the cached move time is unknown: nothing is applied (R3 wins; no timestamp churn).
- A read has no usable fetch time: treated as unprotected only to the extent a listing with no fetch time is today; it must still never create an engine-write record.
- Concurrent delivery of a Status webhook for the same item: ordering between it and the refresh is decided by times and the existing protection, not by arrival order.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The Layer 1 status refresh MUST apply its live-read Status to the cached item through a read-sourced mutation that carries the time the read was taken, not through the engine-write path.
- **FR-002**: That mutation MUST be subject to the same engine-write protection as a board listing: an engine Status write within the grace window wins over the read.
- **FR-003**: A refresh read older than the Status field's most recent write MUST be dropped without changing any state.
- **FR-004**: Applying a refresh MUST NOT create or update the item's engine-write record for Status (or any other field).
- **FR-005**: Applying a refresh MUST NOT clear the item's GitHub-sourced Status move time with a non-GitHub value; where GitHub's move time is unknown for the read, it stays unknown rather than being replaced by an observation or engine time.
- **FR-006**: A change produced by a refresh MUST NOT be marked as an engine Status write and MUST NOT be attributed to `fabrik` in channel events or any other attribution.
- **FR-007**: A refresh whose read Status equals the current cached Status MUST apply nothing (no state change, no timestamps, no events).
- **FR-008**: Behavior of real engine moves, board listings, probes and webhook-sourced Status updates MUST remain unchanged, including all #2178 / ADR 2178 engine-write protection behavior.

### Key Entities *(if applicable)*

- **Item Status cache entry**: the cached board column for an item, with its engine-write record, GitHub move time and change attribution.
- **Layer 1 status refresh**: the webhook-triggered live re-read of an item's Status.
- **Engine-write protection**: the grace window during which a recent engine write outranks older reads and listings.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test in which a refresh read is taken before an engine move and applied after it leaves the item in the moved-to column, 100% of runs.
- **SC-002**: After any number of refreshes (changed or unchanged), an item that was never moved by the engine has no engine-write record for Status.
- **SC-003**: Every change produced by a refresh is attributed to a non-`fabrik` actor (or left unattributed), never `fabrik`.
- **SC-004**: A refresh whose read equals the cached Status produces zero state changes and zero change events.
- **SC-005**: All existing #2178 engine-write-protection regression tests pass unmodified.

## Assumptions

- "Subject to the same protection as a listing" means reusing the existing engine-write protection rule and grace window (30 s), not introducing a second rule.
- When a refresh read supplies no GitHub-side move time, the stored move time is left as it was (or unknown), rather than being set to the observation time.
- The attribution of a refresh-driven change should follow the same rules as any other GitHub-read-driven change; no new actor category is introduced.
- Other input paths (webhook Status deliveries, listings, probes, real engine moves) are already correct and are not changed.

## Out of Scope *(optional)*

- Changing the engine-write protection rule, its grace window, or #2178's behavior for listings and probes.
- Other findings of the 2026-10 engine state audit.
- Making the dispatch handlers operate on fresher item copies.
- Reworking webhook ordering or concurrency in general.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §1.8 and remediation item 7
- #2178 / ADR 2178 (engine-write protection for probe and listing applies), #2159 / ADR 2159
