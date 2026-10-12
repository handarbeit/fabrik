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

### User Story 1 - Backlog arrivals are announced once (Priority: P1)

An overseer subscribed to the channel hears about work as soon as it is filed onto the board, including into an `unmanaged` column such as Backlog. When the item is later moved into a managed column, the overseer hears about the move, not a second arrival.

**Why this priority**: The overseer wants to know when work is filed, not only when it reaches a managed column. Today's behaviour is the desired one, but it is untested for a post-startup Backlog arrival, and the move-out must not re-announce the item.

**Independent Test**: Sim: daemon running, a new open issue is added to the board in an `unmanaged` column; assert exactly one `item-added` with that column. Move it to Specify; assert exactly one `column-changed` (Backlog → Specify) and no second `item-added`.

**Acceptance Scenarios**:

1. **Given** a running daemon and a subscriber, **When** an open issue is added to the board in Backlog, **Then** exactly one `item-added` is published, with `column=Backlog`.
2. **Given** that item, **When** a human moves it to Specify, **Then** one `column-changed` is published and no second `item-added`.
3. **Given** a running daemon, **When** an open issue is added directly into Specify, **Then** exactly one `item-added` is published (unchanged behaviour).

---

### User Story 2 - A restart never invents arrivals (Priority: P1)

An overseer that stays attached across a daemon restart receives no `item-added` for items that were already on the board before the restart, whatever column they sit in and however late the Store first learns of them. An item in a board column with no configured stage never produces `item-added`.

**Why this priority**: A false "new item" after every restart is noise that erodes trust in the channel; the review suspected a path that causes it.

**Independent Test**: Sim: restart with an open item sitting in a board column that has no configured stage, subscriber attached across the restart; run through the first reconcile; assert zero `item-added`. Must fail against current `main` if the suspected defect is real; if it does not fail, record in the PR that the path was disproved and keep the test as a guard.

**Acceptance Scenarios**:

1. **Given** an open item in a column with no configured stage that existed before the daemon restarted, **When** the first reconcile after restart brings it into the Store, **Then** no `item-added` is published.
2. **Given** an open item in a configured `unmanaged` column (Backlog) that existed before the daemon restarted, **When** the daemon restarts and reconciles, **Then** no `item-added` is published.
3. **Given** the same restart, **When** an open issue is genuinely added to a managed column afterwards, **Then** exactly one `item-added` is published.

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

- An item that arrives in Backlog and has been announced, then moves to a managed column: one `column-changed`, never a second `item-added`.
- An item moved from a managed column back to Backlog: `column-changed` only (unchanged).
- An item the daemon first saw in a column with no configured stage (no `item-added` then) and that is later moved into a configured column: exactly one `column-changed` and no `item-added`.
- An item first seen after startup directly in a configured column is an arrival: exactly one `item-added`.
- An item whose arrival is announced only once its labels become known (a pending arrival) is announced at most once, with the column it has at announcement time (unchanged behaviour).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `item-added` is published for an open, non-PR issue that arrives after startup in any column that has a configured stage — managed or `unmanaged` (Backlog included) — exactly once. Moving an announced item into a managed column MUST produce one `column-changed` and no second `item-added`. This deliberately supersedes #2123 FR-004's clause that unmanaged items MUST NOT produce `item-added`.
- **FR-002**: An item in a board column with no configured stage is outside Fabrik's known column set and MUST NOT produce `item-added`, whether it is first seen by the probe, a deep fetch or a reconcile. Likewise an item first entering the Store via a reconcile, a deep fetch or any path other than a real post-startup board addition MUST NOT produce `item-added`. The restart test is the case of an unconfigured-column item first seen at the first reconcile after startup.
- **FR-003**: A test MUST fail if `announceArrival` (or its successor) announces an item whose labels are not yet known (#2123 FR-005).
- **FR-004**: `TestCatchUpNeverReplaysLifecycleEvents` MUST be replaced or strengthened so that it fails if catch-up emits `item-added`, `column-changed` or `closed` — e.g. by asserting on `catchUpEvents` output for a state that contains a fresh arrival, a recent column move and a recent close.
- **FR-005**: ADR 2123 and `docs/state-machine.md` §7.14 state the arrival rule explicitly, using the wording of FR-001/FR-002: arrivals after startup in any configured-stage column (unmanaged included) are announced, an unconfigured-column item never is, and the decision amends #2123 FR-004's exclusion. They also record what happens when an item first seen in an unconfigured column is later moved into a configured column (see Assumptions). `docs/llms-full.txt` is regenerated in the same change.
- **FR-006**: A sim test MUST cover a post-startup arrival into an `unmanaged` column (today only a pre-startup Backlog card is tested), asserting one `item-added` followed, on a move to a managed column, by one `column-changed` and no second `item-added`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Each new test is shown non-vacuous in the PR: name the line neutralised and the failure it produced.
- **SC-002**: No change to `column-changed` or `closed` behaviour; existing channel tests pass unchanged.
- **SC-003**: The observation-only scans (`TestChannelEventsSourceDoesNotReachGitHub`, `TestChannelEventsAreObservationOnly`) pass unweakened.

## Assumptions

- **Decision (2026-10-12, @verveguy):** unmanaged-column arrivals SHOULD be announced as `item-added`, superseding #2123 FR-004's "unmanaged items MUST NOT produce `item-added`" clause and Background item 1 above, which treated that behaviour as a defect. The current behaviour from PR #2175 is the desired one; this issue adds the missing test and records the decision.
- **Decision (2026-10-12, @verveguy):** the rule is stated once in FR-001/FR-002 — `item-added` for an open, non-PR issue arriving after startup in any configured-stage column; never for an item in a column with no configured stage.
- "Configured stage" means a stage loaded from the stage configuration, plus the legacy parking column: a column literally named `Backlog` with no configured stage (installs that predate the shipped Backlog stage) is treated as a parking column by `stages.IsParkingColumn` and **is** announced, so `item-added` is keyed on the `unmanaged` flag, never on the name, with that one compatibility net. (Supersedes the earlier Specify reading that excluded legacy Backlog, per the 2026-10-12 @arbeithand comment.) Any other column with no stage never produces `item-added`.
- Chosen behaviour for an item first seen in an unconfigured column and later moved into a configured column: one `column-changed` and no `item-added` (the move is a transition of an item the daemon already knows about). To be documented in ADR 2123.
- FR-002 applies to items that were on the board before the daemon started, in any column. Distinguishing them from genuine post-startup additions is a design matter for Plan; the observable requirement is that a restart never produces `item-added` for them.
- If the suspected restart defect (Background item 2) cannot be reproduced, FR-002 is satisfied by the guard test plus a note in the PR that the path was disproved.
- Timing of announcement relative to label knowledge (pending arrivals) is unchanged by this issue.

## Out of Scope *(optional)*

- Attributing a `closed` event to `fabrik` when the landing is inferred only from the snapshot, and registering an engine echo for `issues/closed` (documented limitation in `internal/itemstate/mutation.go`). Separate follow-up if wanted.
- Reporting label changes that happened while the daemon was down (by design; `catch_up` covers it).
- Any change to `column-changed` or `closed` behaviour.

## Source References *(optional)*

- #2123 / PR #2175 — original feature and its FR-004/FR-005
- `adrs/2123-channel-arrival-column-and-restart-baseline.md`
- `docs/state-machine.md` §7.14
- `docs/llms-full.txt` must be regenerated if `docs/state-machine.md` changes.
