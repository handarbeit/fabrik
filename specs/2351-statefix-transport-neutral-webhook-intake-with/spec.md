# Feature Specification: Transport-neutral webhook intake with delivery dedupe, per-item ordering and fast ack

**Feature Branch**: `fabrik/issue-2351`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

The webhook path is built for exactly two transports and has no shared delivery handling. Part of the state foundation from the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15). It must land before a second webhook source (#2143). See §2.9 and §5.

- No engine-level dedupe: gh webhook forward has none; Hookdeck has a private per-source `events.Dedupe` (source.go:233). Two sources applying one delivery double the REST/GraphQL auto-heals and re-apply partial payloads.
- No ordering: handlers run per request (engine/webhook.go:621), and `deltaFn` makes network calls (`ensureIssueInStore`, Layer 1) before the 200. GitHub times out at 10 s and does not retry.
- Echo registration runs after the write-through (engine/mutate.go:46-57, 286-296); a sub-millisecond echo arrives first, is attributed as external, and leaves a pending echo that can swallow a later identical human event. Under Hookdeck echo matching is a no-op.
- Two-transport assumptions: the single `e.webhookMgr`, `RefuseHookdeckWithWebhooks`, and type switches in `transitionMgrHealthState` and the reconcile coverage checks (engine/reconcile.go:58-62, 107-114); a third type silently gets no health transitions.
- Hookdeck reconnect does not run the catch-up reconcile its source documentation promises.
- The listener caps bodies at 10 MiB; GitHub allows 25 MB.

## Requirements

- **R1:** One `X-GitHub-Delivery` dedupe in front of `deltaFn` for every transport, persisted across restarts with a bounded window.
- **R2:** Deliveries are acknowledged fast and applied through a per-item serial queue; network work happens off the ack path.
- **R3:** Echoes are registered before the write they describe, and matching works for every transport.
- **R4:** A transport interface replaces the concrete-type switches, so health and coverage work for any transport; more than one transport can run at once.
- **R5:** Any transport reconnect triggers a catch-up reconcile.
- **R6:** The body cap matches GitHub's limit.

## Acceptance

- Tests: a duplicate delivery applies once (also across a restart); two deliveries for one item apply in order; an echo arriving before its write is still matched; a fake third transport gets health transitions.
- #2143 (Funnel receiver) is built on this interface."

## Background

The engine currently receives GitHub webhooks through exactly two transports (`gh webhook forward` and Hookdeck), and each handles deliveries in its own way. This was acceptable while the transports were slow and rarely overlapped, but the engine state audit (`docs/design/engine-state-audit-2026-10.md`, §2.9 and §5) identified it as a prerequisite for a faster, direct webhook source (#2143, the Funnel receiver). A millisecond-latency source makes each existing weakness real rather than theoretical:

- **Duplicate application.** Nothing at the engine level recognises that one GitHub delivery was already applied. Only Hookdeck has a private, per-source dedupe. If two sources deliver the same event, the engine's follow-up reads from GitHub (auto-heals) run twice and partial payloads are re-applied.
- **No ordering and a slow acknowledgement.** Each request is handled on its own, so two events about one item can be applied out of order. The handler also makes network calls before it answers; GitHub gives up after 10 seconds and does not retry, so a slow apply loses the event.
- **Echoes can be missed.** When the engine writes to GitHub, it remembers that write so the webhook that echoes it back can be recognised as the engine's own. The memory is recorded after the write, so a very fast echo arrives first, is wrongly treated as an external change, and leaves behind a pending entry that can later swallow a genuine human event that happens to look identical. Under Hookdeck, echo matching does not work at all.
- **Two-transport assumptions.** The engine holds a single webhook manager, refuses to run Hookdeck alongside webhooks, and decides health transitions and reconcile coverage by switching on the two known transport types. A third transport would silently receive no health handling.
- **Reconnect gap.** Hookdeck's documentation promises a catch-up reconcile when it reconnects, but none runs, so events missed during a disconnect are not healed until a periodic reconcile.
- **Body size.** The listener rejects bodies above 10 MiB while GitHub can send up to 25 MB.

This work belongs to the `statefix` state-foundation chain and follows #2348 (the observed-at ordering rule for Store writes), which it is blocked on. #2143 is built on the transport interface this issue delivers.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A delivery is applied exactly once (Priority: P1)

An operator runs more than one webhook source, or a source retries or redelivers. The engine applies each GitHub delivery to its state once, regardless of which transport carried it, how many times it arrived, or whether the engine restarted in between.

**Why this priority**: Double application causes duplicate GitHub reads and writes and re-applies stale partial payloads; it is the first thing a second source would break.

**Independent Test**: Deliver the same delivery ID twice through one transport, and once through each of two transports; confirm one application each. Restart the engine and redeliver it; confirm it is still not applied again.

**Acceptance Scenarios**:

1. **Given** a delivery with ID X was already applied, **When** a delivery with ID X arrives again on the same transport, **Then** it is acknowledged and not applied.
2. **Given** a delivery with ID X was applied from transport A, **When** the same ID arrives from transport B, **Then** it is acknowledged and not applied.
3. **Given** a delivery with ID X was applied before a restart, **When** the engine restarts and X arrives again within the dedupe window, **Then** it is not applied.
4. **Given** a delivery ID older than the dedupe window, **When** it arrives, **Then** it is applied (the window is bounded, not unlimited).

---

### User Story 2 - Deliveries are acknowledged fast and applied in order per item (Priority: P1)

GitHub receives its acknowledgement promptly regardless of how long the engine's follow-up work takes, and events about one item are applied in the order they were received.

**Why this priority**: GitHub does not retry after a timeout, so a slow apply loses the event outright; out-of-order application corrupts item state.

**Independent Test**: Make the follow-up work for a delivery take longer than GitHub's timeout and confirm the acknowledgement is still returned promptly and the delivery is applied afterwards. Send two deliveries for one item back-to-back and confirm they are applied in arrival order, while deliveries for different items are not blocked behind one another.

**Acceptance Scenarios**:

1. **Given** a delivery whose application requires network calls, **When** it arrives, **Then** the transport is acknowledged before any of that network work starts.
2. **Given** two deliveries for the same item arrive in sequence, **When** they are applied, **Then** the first is fully applied before the second begins.
3. **Given** a slow delivery for item A, **When** a delivery for item B arrives, **Then** B is not delayed by A.

---

### User Story 3 - The engine's own writes are always recognised as its own (Priority: P2)

When the engine writes to GitHub and the resulting webhook comes back, the engine recognises it as its own echo no matter how fast it arrives or which transport carries it.

**Why this priority**: Misattributed echoes make the engine treat its own changes as human action, and orphaned pending echoes can later suppress a real human event. Real risk only with a fast source, but the fix must precede it.

**Independent Test**: Deliver the echo of a write before the write call returns; confirm it is matched as the engine's own. Do the same under the Hookdeck transport. Confirm a later identical human event is still treated as human.

**Acceptance Scenarios**:

1. **Given** the engine is about to write, **When** the echo arrives before the write call returns, **Then** it is recognised as the engine's own.
2. **Given** an engine write that fails, **When** no echo ever arrives, **Then** no stale pending echo remains to match a later human event.
3. **Given** the Hookdeck transport, **When** the engine's write is echoed, **Then** it is recognised as the engine's own.

---

### User Story 4 - Any transport gets health, coverage and catch-up (Priority: P2)

An operator can run any number of webhook transports concurrently. Each reports health, contributes to reconcile coverage decisions, and triggers a catch-up reconcile when it reconnects. A new transport (such as #2143's) needs no changes to engine health or coverage logic.

**Why this priority**: This is what lets #2143 be built without touching engine internals, and it closes the silent missing-health failure for any third transport.

**Independent Test**: Register a fake third transport; confirm it receives health transitions and counts toward coverage. Run two transports at once without refusal. Reconnect each transport and confirm a catch-up reconcile runs.

**Acceptance Scenarios**:

1. **Given** a fake third transport, **When** it becomes healthy or unhealthy, **Then** the engine records the transitions the same way as for the existing transports.
2. **Given** two transports configured together, **When** the engine starts, **Then** it starts both rather than refusing the combination.
3. **Given** a transport that disconnects and reconnects, **When** it reconnects, **Then** a catch-up reconcile runs.

---

### User Story 5 - Large deliveries are accepted (Priority: P3)

A delivery as large as GitHub is allowed to send is accepted rather than rejected.

**Why this priority**: A rejected large delivery is a lost event; rare, but the cap is simply wrong.

**Independent Test**: Send a body of the maximum size GitHub documents and confirm it is accepted; send one larger and confirm it is rejected.

**Acceptance Scenarios**:

1. **Given** a body at GitHub's documented maximum, **When** it arrives, **Then** it is accepted.
2. **Given** a body above that maximum, **When** it arrives, **Then** it is rejected without exhausting memory.

---

### Edge Cases

- A delivery carries no `X-GitHub-Delivery` ID: it cannot be deduplicated, so it is applied (never dropped).
- The engine crashes after acknowledging a delivery but before applying it: the delivery is lost to the webhook path and is healed by the next reconcile; the spec does not require durable queueing of unapplied deliveries.
- The per-item queue is unusually deep or an apply fails or panics: other items continue; a failed apply is logged and healed by reconcile, and does not wedge that item's queue.
- A delivery that concerns no single item (for example a project-level or repository-level event) is still deduplicated and applied in a defined order relative to itself.
- The persisted dedupe record is missing or corrupt at startup: the engine starts with an empty window rather than failing.
- Two transports deliver the same event nearly simultaneously: exactly one application occurs.
- A transport fails to start while others succeed: the others keep running and the failed one is logged and left out of the active set (it is not attached, so it contributes no health or coverage; if it was the only transport the engine stays poll-only).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine MUST deduplicate webhook deliveries by `X-GitHub-Delivery` ID at a single point in front of delivery application, shared by every transport.
- **FR-002**: The dedupe record MUST be persisted so that a delivery applied before a restart is not applied again after it, and MUST be bounded in time and size so it does not grow without limit.
- **FR-003**: A delivery with no delivery ID MUST be applied and MUST NOT be dropped by dedupe.
- **FR-004**: The engine MUST acknowledge a delivery to its transport before performing any network call needed to apply it.
- **FR-005**: Deliveries MUST be applied through a per-item serial queue, so deliveries for one item are applied one at a time in arrival order, while deliveries for different items proceed independently.
- **FR-006**: A failed or panicking application MUST NOT block later deliveries for the same or other items.
- **FR-007**: The engine MUST record an echo for each write it makes to GitHub before issuing that write, and MUST remove or expire the record if the write fails, so no orphaned pending echo remains.
- **FR-008**: Echo matching MUST work for every transport, including Hookdeck, without depending on transport-specific behaviour.
- **FR-009**: The engine MUST define a webhook-transport interface covering start/stop, health state, and reconnect notification, and MUST use it in place of every switch on concrete transport types, including health transitions and reconcile coverage decisions.
- **FR-010**: The engine MUST support running more than one webhook transport at the same time; the existing refusal to combine Hookdeck with webhooks MUST be removed.
- **FR-011**: A transport that was not previously known to the engine MUST receive health transitions and count toward reconcile coverage without any change to engine health or coverage code.
- **FR-012**: Any transport reconnect MUST trigger a catch-up reconcile, including Hookdeck.
- **FR-013**: The delivery body size limit MUST be at least GitHub's documented maximum payload size (25 MB), while still bounding memory use for larger bodies.
- **FR-014**: Behaviour for the two existing transports MUST otherwise be unchanged for operators: configuration, secret handling and observable results stay as they are.

### Key Entities *(if applicable)*

- **Delivery**: One GitHub webhook event, identified by its `X-GitHub-Delivery` ID, carried by some transport.
- **Delivery record**: The persisted, bounded memory of recently applied delivery IDs.
- **Transport**: A source of deliveries (existing: `gh webhook forward`, Hookdeck; future: Funnel receiver) exposing health and reconnect events through the common interface.
- **Echo record**: The engine's remembered write, registered before the write, used to recognise the webhook that echoes it.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A given delivery ID is applied exactly once across any number of transports, redeliveries and one or more restarts within the dedupe window.
- **SC-002**: Acknowledgement latency for a delivery does not depend on the duration of the network work its application requires.
- **SC-003**: For any two deliveries about one item, application order equals arrival order in 100% of test runs.
- **SC-004**: An echo arriving before its originating write returns is attributed to the engine in 100% of test runs, under every transport, and no pending echo is left after a failed write.
- **SC-005**: A test transport implementing only the interface receives health transitions, coverage handling and reconnect catch-up with zero edits to engine health or reconcile code.
- **SC-006**: A body of GitHub's maximum documented size is accepted by every transport listener.

## Assumptions

- The dedupe window defaults to a period comfortably longer than GitHub's redelivery horizon and than any restart (on the order of a day) with a size cap; exact values are a planning decision and may be configurable.
- Dedupe records a delivery when it is accepted, not after it is applied; a crash between acceptance and application is healed by reconcile rather than by durable queueing.
- Ordering is by arrival order at the engine, not by GitHub's event timestamp; timestamp-based ordering of Store writes is already covered by #2348.
- Signature verification and secret handling for a direct receiver belong to #2143; this issue neither adds nor changes them.
- The Hookdeck source's private dedupe may be removed or retained as a harmless duplicate once the shared one exists.
- A catch-up reconcile on reconnect uses the existing reconcile mechanism and its existing rate limiting.

## Out of Scope *(optional)*

- The Funnel/Tailscale receiver itself and its signature verification (#2143).
- New webhook subscriptions for facts the engine never hears about (`issue_dependencies`, `sub_issues`, review threads, label and milestone changes): #1995 and #2352.
- Filtering by project and Status field IDs: #2345.
- The observed-at ordering rule for Store writes: #2348.
- Durable queueing of acknowledged-but-unapplied deliveries across a crash.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §2.9 (echo registration) and §5 (what a second, direct webhook source needs); §6 item 12 (this issue in the work order).
