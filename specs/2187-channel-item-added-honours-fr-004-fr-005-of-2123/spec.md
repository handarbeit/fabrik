# Feature Specification: Channel `item-added` honours FR-004/FR-005 of #2123, with tests that can fail

**Feature Branch**: `fabrik/issue-2187`
**Created**: 2026-10-12
**Status**: Specified
**Input**: Post-merge review of PR #2175 (#2123) found two places where `item-added` contradicts #2123's spec, plus two tests that pass whether or not the behaviour they claim to pin exists.

## Background

#2123 (PR #2175, merged) added `item-added`, `column-changed` and `closed` channel events and a silent restart baseline. A review after merge, with the code neutralised in a scratch worktree, found:

1. **Unmanaged arrivals are announced.** #2123 FR-004: "Items present when the daemon started, closed items, unmanaged items and PR cards MUST NOT produce `item-added`." `announceArrival` (`engine/channel_lifecycle.go`) drops PR cards and closed items, then announces once `LabelsKnown` and `Status != ""` — it never checks whether `Status` is an `unmanaged: true` stage. `engine/terminal.go` now seeds Backlog cards through the probe (`Created=true`), so a card added to Backlog after startup produces `item-added` with `column=Backlog`. `tests/sim/channel_arrival_column_test.go` only files its Backlog item before startup.
   - The overseer still learns about that item when it matters: the human move Backlog → Specify produces `column-changed` (#2123 SC-002).
2. **Suspected spurious `item-added` after restart.** `boardPopulated` is set after the first successful poll (`engine/poll.go`). `terminal.go` skips open items in a column with no configured stage that is not `unmanaged`, so such an item first enters the Store at the first reconcile — after the flag is set, with `Created=true` — and would be announced as an arrival. Not reproduced; `TestChannelRestartIsSilent` uses only configured columns, Backlog and Done.
3. **FR-005 has no guard.** Removing the `!st.LabelsKnown` check from `announceArrival` leaves every `TestChannelItemAdded*` test passing, so nothing pins "`item-added` never carries an empty label set".
4. **The catch-up test is vacuous.** `TestCatchUpNeverReplaysLifecycleEvents` (`engine/channel_events_test.go`) passes without any catch-up change, because `catchUpEvents` cannot construct those event types at all. Pruefer flagged this on PR #2175; the thread was resolved without a change.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Backlog stays quiet (Priority: P1)

An overseer subscribed to the channel sees nothing when someone files a card into Backlog; it hears about the item when it is moved into a managed column.

**Why this priority**: This is a direct violation of a stated #2123 requirement that produces user-visible noise on every Backlog filing.

**Independent Test**: Sim: daemon running, a new open issue is added to the board in an `unmanaged` column; assert no `item-added`. Move it to Specify; assert exactly one `column-changed` (Backlog → Specify) and no `item-added`.

**Acceptance Scenarios**:

1. **Given** a running daemon and a subscriber, **When** an open issue is added to the board in Backlog, **Then** no `item-added` is published.
2. **Given** that item, **When** a human moves it to Specify, **Then** one `column-changed` is published and no `item-added`.
3. **Given** a running daemon, **When** an open issue is added directly into Specify, **Then** exactly one `item-added` is published (unchanged behaviour).

---

### User Story 2 - A restart never invents arrivals (Priority: P1)

An overseer that stays attached across a daemon restart receives no `item-added` for items that were already on the board before the restart, whatever column they sit in and however late the Store first learns of them.

**Why this priority**: A false "new item" after every restart is noise that erodes trust in the channel; the review suspected a path that causes it.

**Independent Test**: Sim: restart with an open item sitting in a board column that has no configured stage and is not unmanaged, subscriber attached across the restart; run through the first reconcile; assert zero `item-added`. Must fail against current `main` if the suspected defect is real; if it does not fail, record in the PR that the path was disproved and keep the test as a guard.

**Acceptance Scenarios**:

1. **Given** an open item in a column with no configured stage that existed before the daemon restarted, **When** the first reconcile after restart brings it into the Store, **Then** no `item-added` is published.
2. **Given** the same restart, **When** an open issue is genuinely added to a managed column afterwards, **Then** exactly one `item-added` is published.

---

### User Story 3 - Guards that can fail (Priority: P2)

A maintainer changing arrival or catch-up code gets a failing test if they break #2123's label-completeness rule (FR-005) or make catch-up replay lifecycle transitions.

**Why this priority**: The two existing guards pass vacuously, so regressions in them would go unnoticed.

**Independent Test**: Neutralise the `LabelsKnown` check in arrival announcement and confirm a test fails; make `catchUpEvents` emit `item-added`/`column-changed`/`closed` and confirm a test fails.

**Acceptance Scenarios**:

1. **Given** an arrival whose labels are not yet known, **When** the arrival is evaluated, **Then** nothing is published until labels are known, and a test fails if that rule is removed.
2. **Given** a state with a fresh arrival, a recent column move and a recent close, **When** catch-up events are built, **Then** none of `item-added`, `column-changed`, `closed` appear, and a test fails if one does.

---

### Edge Cases

- An item that arrives in Backlog and is moved to a managed column *before* its labels are known: one `column-changed` once known, never an `item-added`.
- An item moved from a managed column back to Backlog: `column-changed` only (unchanged).
- An item that arrives in a managed column but is moved to Backlog before its labels are known: no `item-added` (its column at announcement time is unmanaged).
- A legacy install with no configured `Backlog` stage, where a `Backlog` column is still treated as a parking column: treated as unmanaged for this rule.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `item-added` MUST NOT be published for an item whose column is an `unmanaged` stage, at arrival or later.
- **FR-002**: An item first entering the Store via a reconcile, a deep fetch or any path other than a real post-startup board addition MUST NOT produce `item-added`, including items in columns with no configured stage.
- **FR-003**: A test MUST fail if `announceArrival` (or its successor) announces an item whose labels are not yet known (#2123 FR-005).
- **FR-004**: `TestCatchUpNeverReplaysLifecycleEvents` MUST be replaced or strengthened so that it fails if catch-up emits `item-added`, `column-changed` or `closed` — e.g. by asserting on `catchUpEvents` output for a state that contains a fresh arrival, a recent column move and a recent close.
- **FR-005**: ADR 2123 and `docs/state-machine.md` §7.14 state the unmanaged-column rule explicitly.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Each new test is shown non-vacuous in the PR: name the line neutralised and the failure it produced.
- **SC-002**: No change to `column-changed` or `closed` behaviour; existing channel tests pass unchanged.
- **SC-003**: The observation-only scans (`TestChannelEventsSourceDoesNotReachGitHub`, `TestChannelEventsAreObservationOnly`) pass unweakened.

## Assumptions

- "Unmanaged" in FR-001 means a parking column as the engine already defines it: a configured stage with `unmanaged: true`, or the legacy `Backlog` column when no `Backlog` stage is configured. A configured `Backlog` stage that is not `unmanaged` keeps its configured meaning.
- FR-002 applies to items that were on the board before the daemon started. Distinguishing them from genuine post-startup additions is a design matter for Plan; the observable requirement is that a restart never produces `item-added` for them.
- If the suspected restart defect (Background item 2) cannot be reproduced, FR-002 is satisfied by the guard test plus a note in the PR that the path was disproved.
- The unmanaged check applies when the arrival is announced (at the item's column at that time), so an arrival that moves into Backlog before announcement is also silent.

## Out of Scope *(optional)*

- Attributing a `closed` event to `fabrik` when the landing is inferred only from the snapshot, and registering an engine echo for `issues/closed` (documented limitation in `internal/itemstate/mutation.go`). Separate follow-up if wanted.
- Reporting label changes that happened while the daemon was down (by design; `catch_up` covers it).
- Any change to `column-changed` or `closed` behaviour.

## Source References *(optional)*

- #2123 / PR #2175 — original feature and its FR-004/FR-005
- `adrs/2123-channel-arrival-column-and-restart-baseline.md`
- `docs/state-machine.md` §7.14
- `docs/llms-full.txt` must be regenerated if `docs/state-machine.md` changes.
