# Feature Specification: MCP overseer: push events for items added to the board, column changes, closes and restart-silence

**Feature Branch**: `fabrik/issue-2123`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Two of the most basic overseer signals are never pushed: **a new item arrived on the board** and **an item moved to a different column**.
>
> This was observed in an in-situ test against the live dev daemon on 2026-10-11, with 51 minutes of push watching:
>
> - **Arrivals are silent.** No push arrived for new items #2112, #2113, #2114, #2115 or #2116.
>   - Four of them were created with `fabrik:cruise` already applied, within 1–3 s of creation. The cruise decision was never pushed either.
>   - Cause: the first time the store populates an item it treats the item as a baseline and emits no label deltas (`internal/itemstate/store.go`, #1968 R3).
>   - Effect: another operator putting a new issue straight onto cruise is invisible to the overseer.
> - **Column moves are silent.** No push arrived when #2112 was moved by hand from Backlog to Specify, or when #1805 was moved into Specify.
>   - A cruise item advancing through stages produces only `label-applied stage:<X>:complete` pushes: 19 of them in 35 minutes, against one push that needed a decision.
>   - "Item moved to column X" would replace that noise with the signal the overseer actually wants.
>
> ## Requirements
>
> - **R1 — `item-added` event.** Published once when a managed, open item first appears on the board after the daemon has completed its initial board population. Items present at startup are not "added". It carries:
>   - issue, title and column;
>   - the item's labels at arrival, including autonomy;
>   - milestone, author (when known) and URL.
>
>   It is matched by the usual subscription scopes (issues/repos/milestone). Its labels are visible to label-pattern scopes as arrival labels, so a subscriber watching `fabrik:cruise` learns that a new item arrived on cruise.
> - **R2 — `column-changed` event.** Published when an item's board Status changes. It carries:
>   - from and to;
>   - who or what moved it: `fabrik` (the engine's own advance), an overseer action (`overseer:<subscriber>`, see the attribution issue), human, or unknown;
>   - the GitHub time when known.
>
>   It is deduped against the engine's own write and its webhook or poll echo, so one move produces one event.
> - **R3 — Noise control.** Add a documented subscription option that suppresses `label-applied`/`label-removed` for `stage:*:complete` and `stage:*:in_progress` labels when the subscriber also takes `column-changed`. That lets an overseer subscribe to "progress and decisions" without the stage-label stream. The default label exclusions stay as they are unless the ADR argues otherwise.
> - **R4 — Catch-up.** A snapshot (`catch_up=true`) does not replay arrivals. The ADR states this explicitly.
> - **R5 — Observation only.** The channel layer still reads item state only through `Peek`/`Scan`.
>   - `TestChannelEventsSourceDoesNotReachGitHub` stays green and is not weakened.
>   - `TestChannelEventsAreObservationOnly` still shows identical GitHub mutations with and without a hub.
> - **R6 — Docs.**
>   - `docs/state-machine.md` §7.14.
>   - The `docs/USER_GUIDE.md` event table.
>   - Regenerate `docs/llms-full.txt`.
>   - Write the ADR.
>
> ## Scope
>
> In scope: the two new event types, their dedup and attribution plumbing, and the noise option.
>
> Out of scope:
> - actor attribution for label events (separate issue; this one consumes it if it lands first);
> - Hookdeck.
>
> ## Acceptance
>
> All of the following are shown non-vacuous:
> - **Arrival:** a sim test where an item appears on the board with `fabrik:cruise` already applied, after startup, publishes exactly one `item-added` carrying `fabrik:cruise`. Items present at startup produce none.
> - **Column changes:** a sim test where a human moves an item Backlog → Specify publishes one `column-changed` with actor `human` or `unknown`. The engine's own advance Specify → Research publishes one `column-changed` with actor `fabrik`, never two.
> - **Noise option:** the option suppresses stage-label pushes and still delivers `column-changed`.
> - **No-GitHub guarantees:** the channel no-GitHub scan and the observation-only test stay green.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

The MCP overseer channel (#1966) lets another operator or agent watch the board through pushed events. An in-situ session against the live dev daemon on 2026-10-11 (51 minutes of push watching) showed that the most basic signals are missing, and that one existing behavior is actively harmful.

**Arrivals are silent.** No push arrived for new items #2112, #2113, #2114, #2115 or #2116. Four of them were created with `fabrik:cruise` already applied within 1–3 s of creation, and that cruise decision was never pushed either. The cause is that the first time the store populates an item it treats the item as a baseline and emits no label deltas (#1968 R3). An operator putting a new issue straight onto cruise is therefore invisible to the overseer.

**Column moves are silent.** No push arrived when #2112 was moved by hand from Backlog to Specify, or when #1805 was moved into Specify. A cruise item advancing through stages produces only `label-applied stage:<X>:complete` pushes: 19 of them in 35 minutes, against one push that needed a decision. "Item moved to column X" would replace that noise with the signal the overseer actually wants.

**Closes and landings are misleading.** When #1343 was closed by hand, the only push was `fabrik:blocked removed`, which reads as "unblocked". On #2112, `awaiting-close` was pushed before `merged`, because `merged` is derived from the landing-verification label, which was applied one second later.

**Restarts flood subscribers with false label events (the most damaging defect seen so far).** After a SIGHUP restart (`dev(62f3920)`, 16:22:54Z), one subscriber received about 150 `label-applied (by unknown)` pushes within about 30 s, covering every label already present on almost every open item: all six `stage:*:complete`, `fabrik:cruise`/`yolo`, `fabrik:blocked`, `bug`/`enhancement`. `awaiting-input` on #1342 also re-fired with `progress_age_seconds 5`. GitHub shows none of those labels changed. The flood came in two waves, about 400 false pushes per restart in total:
- Wave 1 (16:23Z): about 150 pushes as the open items were deep-fetched.
- Wave 2 (16:26:00Z, the first post-restart reconcile): about 250 more, covering every label on the Done and Backlog items, including long-closed ones, each with about eight `stage:*:complete` plus its autonomy label, all `actor=unknown`.

The likely cause is that, after start, the store is first populated from the shallow board query, which carries no labels, and the first fetch that carries labels then diffs against that label-less record and emits every label as a delta. The baseline rule treats only an item's first creation as a baseline, not "first time labels were known". `stage:*:complete` is not in the default `exclude_labels`, so subscription filtering does not hide this; the fix belongs in the baseline rule. Self-upgrades go through the same exec, so every release would hit it. It is the reverse of the arrival problem, and the line that separates "present at startup" from "added later" has to be drawn once and used for both.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Overseer is told when a new item arrives (Priority: P1)

An overseer subscribed to the channel learns, once, that a new managed open item has appeared on the board, with its title, column, labels at arrival (including autonomy such as cruise), milestone, author and URL.

**Why this priority**: Arrivals are the most basic signal. Without it another operator can put work on cruise and the overseer never learns of it.

**Independent Test**: With the daemon running and initial board population complete, add an item that already carries `fabrik:cruise`; a subscriber watching `fabrik:cruise` receives exactly one `item-added` carrying that label.

**Acceptance Scenarios**:

1. **Given** a running daemon past initial board population, **When** a new item appears on the board with `fabrik:cruise` already applied, **Then** exactly one `item-added` is published and its labels include `fabrik:cruise`.
2. **Given** a subscriber scoped to label pattern `fabrik:cruise`, **When** that item arrives, **Then** the subscriber receives the `item-added`; a subscriber scoped to a different milestone or repo does not.
3. **Given** items already on the board when the daemon started, **When** the daemon finishes initial population, **Then** no `item-added` is published for any of them.

---

### User Story 2 - Overseer is told when an item changes column (Priority: P1)

An overseer learns that an item moved from one board column to another, and who or what moved it, as one event per move.

**Why this priority**: "Item moved to column X" is the progress signal the overseer actually wants, and it replaces the stage-label stream as the way to follow a cruise item.

**Independent Test**: Move an item by hand Backlog → Specify, then let the engine advance an item Specify → Research; observe one `column-changed` for each, with the correct from/to and actor.

**Acceptance Scenarios**:

1. **Given** an item in Backlog, **When** a human moves it to Specify, **Then** exactly one `column-changed` is published with from `Backlog`, to `Specify` and actor `human` or `unknown`.
2. **Given** an item in Specify, **When** the engine advances it to Research, **Then** exactly one `column-changed` is published with actor `fabrik`, never two, even though the engine's own write is later echoed by a webhook or poll.
3. **Given** an overseer action moved the item, **When** the event is published, **Then** its actor is `overseer:<subscriber>` if the attribution capability is available.

---

### User Story 3 - A daemon restart or self-upgrade is silent (Priority: P1)

An overseer attached across a restart receives no label events and no re-published state events until something actually changes on GitHub, including through the first deep fetch and the first reconcile, and including for open, Done, Backlog and closed items.

**Why this priority**: Roughly 400 false pushes per restart bury real signals and make the whole channel untrustworthy; every release would trigger it.

**Independent Test**: In the sim, restart the engine with N labelled open items plus Done, Backlog and closed items and a subscriber attached across the restart; run the first deep fetch and the first reconcile; observe zero label events and zero re-published state events. Then apply one real label change and observe exactly one event.

**Acceptance Scenarios**:

1. **Given** an item first populated by a shallow query without labels, **When** a later fetch carries its labels, **Then** no label event is emitted for any of them.
2. **Given** an item first populated with labels by a reconcile (or any other path), including a Done, Backlog or closed item, **When** that population happens after startup, **Then** no label event is emitted for it.
3. **Given** an item that was already awaiting input, stalled or otherwise in a state-derived condition before the restart, **When** the daemon re-evaluates it at startup, **Then** no new episode is published; a subscriber wanting the current picture uses `catch_up`.
4. **Given** the silence above, **When** a label really changes after baseline, **Then** exactly one event is published for it.

---

### User Story 4 - Closes and landings read correctly (Priority: P2)

An overseer is told when an issue is closed, by whom and from which column, rather than inferring it from label fallout, and for a landing sees `merged` before any downstream close-tracking.

**Why this priority**: A manual close currently reads as "unblocked", and a reversed landing order misleads. Both are lifecycle accuracy defects rather than missing basics.

**Independent Test**: Close an item by hand and observe a `closed` event naming the actor and column; run a landing and observe `merged` first.

**Acceptance Scenarios**:

1. **Given** an open item, **When** it is closed by hand outside a Fabrik landing, **Then** a `closed` event is published naming the actor and the item's board column.
2. **Given** a single landing, **When** its events are published, **Then** `merged` precedes every downstream close-tracking event (such as the awaiting-close label event and any `closed` event) for that landing.
3. **Given** a close that is part of a Fabrik landing, **When** a close-tracking event is published, **Then** it is identified as belonging to that landing.

---

### User Story 5 - Overseer subscribes to progress and decisions without the stage-label stream (Priority: P2)

An overseer opts into a documented subscription option that drops `stage:*:complete` and `stage:*:in_progress` label events when it also takes `column-changed`.

**Why this priority**: Without it `column-changed` adds to the noise instead of replacing it.

**Independent Test**: Subscribe with the option and `column-changed`; advance a cruise item through stages; observe `column-changed` events and no stage-label events, while other label events still arrive.

**Acceptance Scenarios**:

1. **Given** a subscription with the option set and `column-changed` taken, **When** a stage label is applied or removed, **Then** no `label-applied`/`label-removed` is delivered for `stage:*:complete` or `stage:*:in_progress`, and `column-changed` is still delivered.
2. **Given** a subscription without the option, **When** the same labels change, **Then** delivery is exactly as before.

---

### Edge Cases

- An item whose first appearance is a shallow population without labels: its `item-added` carries the item's real labels, never an empty set because labels were not yet loaded.
- A closed item, a PR card, or an unmanaged item appearing on the board: no `item-added`.
- An item that is added and moved within the same poll: arrival and column change are separate events, each published once.
- A move whose from-column is unknown (first observation of the item): no `column-changed` is invented; it is treated as baseline.
- Webhook echo arriving before, after or instead of the engine's own write confirmation: one move still yields one event.
- A move to the same column (no real change) produces no event.
- An item that first appears via the first reconcile after startup but already existed (Done, Backlog, closed) is baseline, not an arrival.
- A restart during a landing: `merged` and the close events are not re-published for a landing that already completed before the restart.
- A subscriber attaching or reconnecting after missing an arrival gets no replay of it (see FR-012).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The system MUST publish an `item-added` event exactly once when a managed, open item first appears on the board after the daemon has completed its initial board population.
- **FR-002**: `item-added` MUST carry the issue, title and column; the item's labels at arrival, including autonomy; and the milestone, author (when known) and URL.
- **FR-003**: `item-added` MUST be matched by the usual subscription scopes (issues, repos, milestone). Its arrival labels MUST be visible to label-pattern scopes, so a subscriber watching `fabrik:cruise` learns that an item arrived on cruise.
- **FR-004**: Items present when the daemon started, closed items, unmanaged items and PR cards MUST NOT produce `item-added`.
- **FR-005**: The labels carried by `item-added` MUST be the item's real labels at arrival, never an empty set caused by labels not yet being loaded.
- **FR-006**: The system MUST publish a `column-changed` event when an item's board Status changes.
- **FR-007**: `column-changed` MUST carry the from and to columns; who or what moved it (`fabrik` for the engine's own advance, `overseer:<subscriber>` for an overseer action when the attribution capability is available, `human`, or `unknown`); and the GitHub time when known.
- **FR-008**: One move MUST produce one `column-changed`, deduplicated against the engine's own write and its webhook or poll echo.
- **FR-009**: The system MUST provide a documented subscription option that suppresses `label-applied`/`label-removed` for `stage:*:complete` and `stage:*:in_progress` labels when the subscriber also takes `column-changed`. The default label exclusions MUST stay unchanged unless the ADR argues otherwise.
- **FR-010**: Subscriptions that do not set the option MUST behave exactly as before for stage labels.
- **FR-011**: The ADR MUST state whether the default label exclusions stay as they are and why.
- **FR-012**: A `catch_up=true` snapshot MUST NOT replay arrivals. The ADR MUST state this explicitly.
- **FR-013**: The channel layer MUST continue to read item state only through `Peek`/`Scan`. `TestChannelEventsSourceDoesNotReachGitHub` MUST stay green and unweakened, and `TestChannelEventsAreObservationOnly` MUST still show identical GitHub mutations with and without a hub.
- **FR-014**: An issue closed outside a Fabrik landing MUST produce a `closed` event naming the actor and the item's board column; the close MUST NOT be left to be inferred from label fallout.
- **FR-015**: For a single landing, `merged` MUST be published before any downstream close-tracking event for that landing; where a close-tracking event is published for a landing, it MUST say that it belongs to that landing.
- **FR-016**: An item's first label-bearing population after daemon start MUST be a baseline for label deltas and channel events, whichever fetch carries it: a shallow population without labels followed by a deep fetch with them, a reconcile, or any other path, and for open, Done, Backlog and closed items alike.
- **FR-017**: State-derived events re-evaluated at startup (`awaiting-input`, `stalled` and similar) MUST NOT be re-published as new episodes when their underlying state predates the start. Subscribers wanting the current picture use `catch_up`.
- **FR-018**: A real change after baseline MUST still publish its event.
- **FR-019**: The change MUST be documented in `docs/state-machine.md` §7.14 and the `docs/USER_GUIDE.md` event table, with `docs/llms-full.txt` regenerated, and an ADR written.

### Key Entities

- **Arrival**: the first time a managed, open item appears on the board after initial board population; the subject of `item-added`.
- **Baseline**: an item's first label-bearing population; it produces no label deltas or channel events, whichever fetch or reconcile path carries it.
- **Column move**: a change of an item's board Status, with a from, a to, an actor and optionally a GitHub time.
- **Landing**: the merge-and-close sequence for an item, whose events must be ordered `merged` first.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A sim test where an item appears after startup with `fabrik:cruise` already applied publishes exactly one `item-added` carrying `fabrik:cruise`; items present at startup produce none. The test is shown non-vacuous.
- **SC-002**: A sim test where a human moves an item Backlog → Specify publishes one `column-changed` with actor `human` or `unknown`; the engine's own advance Specify → Research publishes one `column-changed` with actor `fabrik`, never two. The test is shown non-vacuous.
- **SC-003**: With the noise option set, stage-label pushes are suppressed and `column-changed` is still delivered. The test is shown non-vacuous.
- **SC-004**: A sim test restarts the engine with N labelled open items, plus Done, Backlog and closed items first populated through a reconcile, and a subscriber attached across the restart. It asserts zero label events and zero re-published state events through the first deep fetch and the first reconcile, until something actually changes. It is shown non-vacuous against the current baseline rule.
- **SC-005**: A sim test shows a manual close publishes a `closed` event with actor and column, and that for a single landing `merged` precedes every downstream close-tracking event. The test is shown non-vacuous.
- **SC-006**: The channel no-GitHub scan and the observation-only test stay green, and `go test -race ./...` is clean.
- **SC-007**: A restart produces about 0 false pushes, down from about 400 per restart observed on 2026-10-11.

## Assumptions

- "Initial board population" ends when the daemon has completed its first populate of the board and the baseline for labels is established, including items that only get labels from the first reconcile.
- An item that exists on the board before the daemon starts, or that first appears via the first reconcile after start but was already there on GitHub, is never an arrival.
- The `closed` event is published for every close of a managed item; one that is part of a Fabrik landing is identified as belonging to it (FR-015), and one outside a landing names the human or unknown actor. Both close sources therefore read clearly, which is the low-impact default.
- The `overseer:<subscriber>` actor depends on the separate attribution issue; until it lands, overseer-caused moves are reported as `fabrik` or `unknown`, and this issue consumes the attribution when it exists.
- The default label exclusions stay as they are; the noise option is the opt-in remedy.
- Event payload field names and the exact name of the option are decided in the ADR, not here.

## Out of Scope *(optional)*

- Actor attribution for label events (separate issue; this one consumes it if it lands first).
- Hookdeck.
- Replaying arrivals or moves in `catch_up` snapshots.
- Changing the default `exclude_labels`.

## Source References *(optional)*

- #1966 — the MCP overseer channel this extends. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
- #1968 R3 — the baseline rule (`internal/itemstate/store.go`) that makes arrivals silent and, on restart, causes the false label flood.
- `docs/state-machine.md` §7.14 — channel event behavior.
- `docs/USER_GUIDE.md` — the event table.
- In-situ MCP session of 2026-10-11 (arrivals, column moves, manual close of #1343, `awaiting-close` before `merged` on #2112, restart flood at 16:22:54Z and the 16:26:00Z reconcile wave).
