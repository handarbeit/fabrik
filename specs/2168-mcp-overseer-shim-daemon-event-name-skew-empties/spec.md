# Feature Specification: MCP overseer subscription lifecycle: version-skew tolerance and hard-stop unsubscribe

**Feature Branch**: `fabrik/issue-2168`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "## Problem

Two defects in the subscription lifecycle between the `fabrik mcp` shim and the daemon, both observed in the in-situ MCP session on 2026-10-11.

**1. Shim/daemon event-name skew rejects the whole subscription.**
- After #2129 renamed the account-wide usage-limit events, a running shim built before the rename still advertised `claude-limit-suspended` / `claude-limit-lifted` in the `fabrik_subscribe` schema.
- The daemon (`dev(9ede632)`) rejected the subscribe with `bad_request: unknown event type \"claude-limit-suspended\"`. The **whole** subscribe failed, not only that entry, and the session was left with **no** subscription.
- This skew is the normal case, not a corner case. The daemon self-upgrades (`--auto-upgrade`, SIGHUP re-exec), while the shim is a long-lived child of a Claude Code session and stays on its old build until the session restarts.
- `internal/channelevents/legacy.go` deliberately migrates the old names only for persisted state and has `Subscription.Validate` reject them in a request.

**2. Unsubscribe does not stop an in-flight flood.**
- During a restart flood (see #2123), `fabrik_unsubscribe` returned `removed: 1, subscriptions: null`, and about 250 more replayed `label-applied` pushes still arrived.
- `Hub.Unsubscribe` clears the held queue, so the leak is elsewhere. Candidates: the attached session's drain loop working from a batch taken before the clear; publishes racing the unsubscribe; the attachment staying bound; or buffering in the shim. It is not yet established which.
- The observing shim was built from `0843c070`, so step one is to reproduce on current main.

## Requirements

- **R1 — Skew never silently empties a session.** A subscribe that names an event type the daemon does not know must not leave the session without a subscription. The ADR picks one of these:
  - **(a)** The daemon accepts the `legacyEventTypes` aliases in requests and maps them, reporting the mapping in the response.
  - **(b)** `hello` advertises the daemon's live event catalogue, and the shim builds its `fabrik_subscribe` schema and validation from it.
  - **(c)** Unknown names are dropped with a warning in the response while the rest of the subscription is applied.

  Whichever is chosen, the response names any type that was rejected or mapped.
- **R2 — Version skew is visible.** `hello`, or `fabrik_health`, reports both the daemon's and the shim's versions. When they differ, the shim's tool results say so once, suggesting a session restart to pick up the new shim.
- **R3 — Unsubscribe is a hard stop.** Once `fabrik_unsubscribe` returns, no further event is delivered for a removed subscription, including entries already taken by the drain loop but not yet written. A last-subscription removal also stops the attachment's delivery. Reproduce on current main first; if it does not reproduce, record that in the PR and add the regression test anyway.
- **R4 — Docs.** Cover skew handling and unsubscribe semantics in `docs/state-machine.md` §7.14 and in the `docs/USER_GUIDE.md` MCP section, and regenerate `docs/llms-full.txt`.

## Scope

In scope:
- request-side validation and aliasing;
- the `hello` catalogue/version fields;
- the drain loop and unsubscribe ordering.

Out of scope:
- restart replay and the reconnect model (separate issue);
- new event types.

## Acceptance

- **Skew:**
  - A test subscribes with `[\"claude-limit-suspended\", \"validate-settled\"]`.
  - It ends with a working subscription that delivers `validate-settled`, and the response reports the old name as mapped or rejected per the ADR.
  - It is shown non-vacuous against today's `Validate`.
- **Unsubscribe:**
  - A hub test publishes N events to an attached subscriber whose sink blocks, then unsubscribes while the drain loop holds a batch and releases the sink.
  - Zero events are delivered after `Unsubscribe` returns.
  - It is shown non-vacuous by removing the fix.
- `go test -race ./...` is clean.

Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

The `fabrik mcp` shim is a long-lived child of a Claude Code session; the daemon self-upgrades (`--auto-upgrade`, SIGHUP re-exec). The two therefore routinely run different builds, so shim/daemon skew is the normal state of affairs, not an edge case. Two defects in the subscription lifecycle were observed in a live MCP session on 2026-10-11:

1. **Event-name skew empties the subscription.** After #2129 renamed the account-wide usage-limit events (`claude-limit-suspended` / `claude-limit-lifted` became `claude-usage-limited` / `claude-usage-limit-lifted`), a shim built before the rename still advertised the old names in its `fabrik_subscribe` schema. The daemon rejected the request with `bad_request: unknown event type "claude-limit-suspended"`. The *whole* subscribe failed, not just the stale entry, and the session was left with no subscription at all — silently, from the point of view of the person relying on pushes. The old names are migrated today only for state persisted on disk; request validation deliberately rejects them.
2. **Unsubscribe does not stop an in-flight flood.** During a restart flood (#2123), `fabrik_unsubscribe` reported success (`removed: 1`) yet about 250 further replayed `label-applied` pushes were delivered. The held queue is already cleared on unsubscribe, so the leak is elsewhere; candidate causes are a drain loop working from a batch taken before the clear, publishes racing the unsubscribe, an attachment that stays bound, or buffering in the shim. The cause is not yet established. The observing shim was built from `0843c070`, so reproduction on current main is the first step.

An important constraint on the fix to defect 1: a shim already running in the field has a fixed schema and fixed validation baked in at its build. Changes confined to the shim's schema generation cannot help such a shim; only daemon-side tolerance reaches sessions that are already running. This is relevant when the design chooses among the options listed under FR-001.

Related, for context only: #1966 (the human feature thread) and #2123 (the restart flood).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A stale shim still gets a working subscription (Priority: P1)

An operator has a Claude Code session whose `fabrik mcp` shim predates a daemon upgrade. The shim's `fabrik_subscribe` call names an event type the daemon has since renamed alongside other, current types. The operator expects to keep receiving the events they asked for.

**Why this priority**: This is the failure that silently leaves a session with no pushes, and it happens on every rename because skew is the normal case. It is the highest-impact defect.

**Independent Test**: Subscribe with `["claude-limit-suspended", "validate-settled"]` against a current daemon; verify a subscription exists and a `validate-settled` event is delivered, and that the response states what happened to the old name.

**Acceptance Scenarios**:

1. **Given** a daemon that knows `claude-usage-limited` but not `claude-limit-suspended`, **When** a subscribe names `["claude-limit-suspended", "validate-settled"]`, **Then** the session ends up with a working subscription that delivers `validate-settled`, and the response names `claude-limit-suspended` as mapped (and to what) or rejected.
2. **Given** a subscribe that names only a completely unknown type, **When** it is processed, **Then** the session's existing subscription state is not silently emptied and the response names the unknown type.
3. **Given** a subscribe naming only current types, **When** it is processed, **Then** behaviour and response are unchanged except for any new, empty reporting field.

---

### User Story 2 - Unsubscribe actually stops delivery (Priority: P1)

An operator in the middle of a burst of replayed or live events calls `fabrik_unsubscribe`. After it returns, they expect silence for that subscription.

**Why this priority**: Unsubscribe is the operator's only brake during an event flood; if it does not brake, the operator cannot trust the tool.

**Independent Test**: At the hub level, attach a subscriber whose sink blocks, publish N events so the drain loop holds a batch, call `Unsubscribe`, release the sink, and count deliveries after `Unsubscribe` returned.

**Acceptance Scenarios**:

1. **Given** an attached subscriber whose drain loop holds a batch not yet written, **When** `Unsubscribe` returns and the sink is then released, **Then** zero further events are delivered for the removed subscription.
2. **Given** the removed subscription was the subscriber's last one, **When** `Unsubscribe` returns, **Then** the attachment's delivery has stopped as well.
3. **Given** events being published concurrently with the unsubscribe, **When** `Unsubscribe` returns, **Then** none of them is delivered for the removed subscription afterwards.
4. **Given** the subscriber has other remaining subscriptions, **When** one is removed, **Then** events matching only the remaining subscriptions continue to be delivered.

---

### User Story 3 - Skew is visible to the operator (Priority: P2)

An operator whose shim and daemon builds differ wants to know, rather than discover through odd behaviour.

**Why this priority**: Tolerance (Story 1) removes the worst symptom, but a visible, actionable signal prevents other, unforeseen skew effects from being a mystery.

**Independent Test**: Connect a shim reporting a version different from the daemon's; verify `hello`/`fabrik_health` carry both versions and the shim's first tool result mentions the difference once.

**Acceptance Scenarios**:

1. **Given** differing daemon and shim versions, **When** the shim calls a tool, **Then** the tool result says the versions differ and suggests restarting the session to pick up the new shim.
2. **Given** the same condition on a later tool call in the same session, **When** the shim returns the result, **Then** the notice is not repeated.
3. **Given** identical versions, **When** tools are called, **Then** no skew notice appears.
4. **Given** `fabrik_health` (or `hello`) is requested, **Then** both the daemon's and the shim's versions are reported.

---

### Edge Cases

- A subscribe mixing a legacy name and its already-current equivalent (duplicate after mapping) results in one entry, not an error.
- A subscribe whose entire event list is unknown or mapped to nothing must not wipe a subscription the session already holds, and must say so in the response.
- Unsubscribe racing a publish, a held-queue replay on attach, or a drain-loop write in progress.
- Unsubscribe of the last subscription while the session is attached, then a later re-subscribe under the same subscriber name: delivery resumes normally and the earlier removal does not suppress it.
- Unsubscribe of a name that has no subscription (`removed: 0`) remains harmless.
- Daemon and shim versions that are equal but built from different dirty trees, or a shim that cannot report its version (e.g. an old shim): the daemon-side behaviour must not depend on the shim reporting a version.
- Events already written to the shim's output before `Unsubscribe` returned cannot be recalled; the guarantee covers what the daemon delivers after the call returns.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A `subscribe` request naming an event type the daemon does not know must never leave the session with an emptied subscription. The design records which of three approaches is used (the daemon accepts and maps legacy aliases in requests; `hello` advertises the live event catalogue from which the shim builds its schema and validation; unknown names are dropped with a warning while the rest is applied) in an ADR. Whichever is chosen, the outcome must hold for a shim already running with a stale schema, since such a shim cannot be changed in place.
- **FR-002**: The `subscribe` response names every event type that was mapped (including its current name) or rejected/dropped, so the caller can see exactly what was applied.
- **FR-003**: A `subscribe` that names both current and legacy/unknown types results in a working subscription for the valid and mapped ones; an entirely invalid request leaves any pre-existing subscription untouched and reports why.
- **FR-004**: Previously valid requests (only current event names) keep their present semantics, including idempotent re-subscription with identical filters.
- **FR-005**: `hello` or `fabrik_health` reports both the daemon's version and the shim's version.
- **FR-006**: When the daemon and shim versions differ, the shim's tool results state this once per shim session and suggest restarting the session to pick up the new shim; it does not repeat on later calls and does not appear when the versions match.
- **FR-007**: Once `fabrik_unsubscribe` returns, the daemon delivers no further event for a removed subscription — including entries already taken from the held queue by the attachment's drain loop but not yet written, and events published concurrently with the unsubscribe.
- **FR-008**: Removing a subscriber's last subscription also stops delivery on its attachment; remaining subscriptions of the same subscriber continue unaffected when only some are removed.
- **FR-009**: The unsubscribe leak is first reproduced against current main. If it does not reproduce, that fact is recorded in the PR description and the regression test is added regardless.
- **FR-010**: A hub-level regression test publishes N events to an attached subscriber with a blocking sink, unsubscribes while the drain loop holds a batch, releases the sink, and asserts zero deliveries after `Unsubscribe` returns; the test is demonstrated to fail when the fix is removed.
- **FR-011**: A skew regression test subscribes with `["claude-limit-suspended", "validate-settled"]`, asserts a working subscription that delivers `validate-settled` and a response reporting the old name per the ADR; the test is demonstrated to fail against the current request validation.
- **FR-012**: `docs/state-machine.md` §7.14 and the MCP section of `docs/USER_GUIDE.md` describe skew handling (including the response fields that report mapped/rejected types and the version fields) and unsubscribe semantics (hard stop, last-subscription behaviour); `docs/llms-full.txt` is regenerated in the same change.
- **FR-013**: The PR body mentions #1966 by bare number only and uses no closing keyword (`Closes`/`Fixes`/`Resolves`) near it.

### Key Entities

- **Subscription**: A subscriber-name-scoped set of event-type and scope filters, persisted by the daemon; subject to request-side validation.
- **Event type catalogue**: The set of event names the daemon currently recognises, plus the legacy aliases from the #2129 rename.
- **Attachment / drain loop**: The daemon-side delivery of a subscriber's queued events to its attached shim session.
- **Version pair**: The daemon's and the shim's build versions, used to detect skew.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A subscribe naming a pre-rename event name together with a current one yields a subscription that delivers the current event, 100% of the time, with the response naming the old type.
- **SC-002**: After `fabrik_unsubscribe` returns, zero events are delivered for the removed subscription in the hub regression test, including with a drain batch in flight and with concurrent publishes.
- **SC-003**: Both regression tests fail when their respective fix is removed (non-vacuous) and pass with it.
- **SC-004**: With differing daemon and shim versions, an operator sees exactly one skew notice per shim session; with equal versions, none.
- **SC-005**: `go test -race ./...` passes with no data races.
- **SC-006**: The updated docs and a regenerated `docs/llms-full.txt` are in the same change, and the `docs-drift` check passes.

## Assumptions

- The choice among the three skew-handling approaches is an architecture decision for the Plan stage/ADR, not this spec; the requirement is the observable outcome, and the approach must work for already-deployed shims with stale schemas.
- An old shim that does not report a version is treated as "version unknown"; the daemon-side tolerance does not depend on shim-reported data.
- The guarantee in FR-007 covers delivery by the daemon; bytes already written to the shim before the call returns, and anything the shim has buffered, are outside the daemon's control. If reproduction shows shim-side buffering is part of the leak, stopping it there is in scope as part of "the drain loop and unsubscribe ordering" end to end.
- The ADR is numbered after this issue (`adrs/2168-...`).

## Out of Scope

- Restart replay volume and the reconnect model (tracked separately; see #2123).
- New event types.
- Removing or changing the persisted-state migration of legacy names.
- Any change to what events mean or when they fire.

## Source References

- `internal/channelevents/legacy.go` — legacy event-name migration (persisted state only today)
- `docs/state-machine.md` §7.14 — channel events and subscription semantics
- `docs/USER_GUIDE.md` — MCP overseer section (`fabrik_subscribe` / `fabrik_unsubscribe`)
- #2129 (event rename), #2123 (restart flood), #1966 (human feature thread; mention by bare number only)
