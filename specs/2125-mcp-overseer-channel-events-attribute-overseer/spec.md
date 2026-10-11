# Feature Specification: Correct actor attribution on MCP channel label events

**Feature Branch**: `fabrik/issue-2125`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: the issue body as originally filed, reproduced verbatim below.

> ## Problem
>
> Channel pushes misattribute who changed a label, which makes "who did this?" unanswerable once more than one overseer or operator is involved.
>
> This was observed in an in-situ test against the live dev daemon on 2026-10-11:
>
> - **An overseer action shows as the engine.** `label-applied fabrik:cruise #2112 (by fabrik)` was pushed with `actor=fabrik`. In fact it was a `fabrik_set_autonomy` call from another overseer session (subscriber `overseer-cli`), as its audit comment shows. To an overseer, "by fabrik" reads as the engine deciding something on its own.
> - **A human operator shows as unknown.** `fabrik:cruise` applied by the human operator (`arbeithand`) on #2113 and #1805 was pushed as `actor=unknown`. With webhooks unavailable, the daemon has no `sender`. GitHub's issue event log does name the actor.
>
> ## Requirements
>
> - **R1 — Overseer actions are attributed.** Writes made by an overseer action (`engine/overseer_actions.go`) record the requesting subscriber with the engine's echo, so the resulting label, column and comment events carry `actor=overseer` and `via=<subscriber>`, not `fabrik`. The engine's own autonomous writes keep `actor=fabrik`.
> - **R2 — Resolve unknown actors where it is cheap.**
>   - With no webhook `sender`, the actor may be resolved from GitHub's event data only where the engine already reads it. Examples:
>     - a label-applied time the engine fetches anyway;
>     - the reconcile/deep-fetch path, if it already carries timeline actors.
>   - The local API and channel layer still make no GitHub calls.
>   - Where it cannot be resolved cheaply, `unknown` stays. The ADR records which paths resolve it and the cost.
> - **R3 — Human versus bot.** A resolved actor is classified `human` or `bot`, as today, and the login is carried in the event's attributes.
> - **R4 — Docs.**
>   - `docs/state-machine.md` §7.14 (actor semantics).
>   - The `docs/USER_GUIDE.md` event attributes.
>   - Regenerate `docs/llms-full.txt`.
>
> ## Scope
>
> In scope: actor attribution for channel events.
>
> Out of scope:
> - new event types;
> - authentication of subscribers (the socket stays unauthenticated, and the name stays an advisory audit aid per ADR-1966-c).
>
> ## Acceptance
>
> All of the following are shown non-vacuous:
> - **Overseer action:** an engine/sim test runs `SetAutonomy` from subscriber `x`, and the resulting `label-applied` push has `actor=overseer`, `via=x`.
> - **Autonomous write:** an engine autonomous label write still has `actor=fabrik`.
> - **Unknown actor:** for each path R2 chooses, a test where an unattributed label change is resolved shows the login and the human/bot class.
> - **No-GitHub guarantees:** the channel no-GitHub scan and the observation-only test stay green.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.

## Background

MCP channel events (#1966, ADR-1966-b) push `label-applied` / `label-removed` notifications to overseer sessions. Each carries an `actor` of `fabrik`, `human`, `bot` or `unknown` (`docs/state-machine.md` §7.14, `docs/USER_GUIDE.md` event table). The purpose of the field is to answer "who did this?" for an overseer deciding whether to act.

Two observed cases make the field misleading once several overseers and operators share a board:

1. **Overseer actions look like the engine.** The overseer action set (`fabrik_promote`, `fabrik_set_autonomy`, `fabrik_revalidate`, `fabrik_clear_claude_limit`, ADR-1966-c) writes through the engine's own label idioms. Those writes pass through the same write-through and echo registration as the engine's autonomous writes, so the resulting event reads `actor=fabrik`. A second overseer session therefore sees "by fabrik" for a change a peer overseer requested. The requester's subscriber name is already known to the engine (it is in the audit comment) but is not attached to the write.
2. **Human operators look unknown.** When the daemon has no webhook `sender` (webhooks unavailable), a label change first seen by a poll or reconcile is `actor=unknown`. GitHub's issue event log does name the actor. The engine already reads that log in places (for example the label-applied time it fetches for gates), so some of those reads may already carry the actor at no extra cost.

The channel and local-API layers must remain free of GitHub calls (R9) and observation-only (R10). Any resolution therefore has to happen where the engine already reads event data, not through new reads from the channel layer.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Overseer actions are attributed to the overseer (Priority: P1)

An overseer session subscribed to the channel sees a label change that another overseer session requested through an action tool. The event names an overseer as the actor and names the requesting subscriber. It does not say the engine did it.

**Why this priority**: This is the misattribution that reads as the engine deciding something on its own. The requester name is already known to the engine, so the fix is deterministic and needs no GitHub data.

**Independent Test**: Run `SetAutonomy` from subscriber `x` against a sim item and assert the resulting `label-applied` push has `actor=overseer`, `via=x`. Then perform an engine autonomous label write and assert it still has `actor=fabrik` with no `via`.

**Acceptance Scenarios**:

1. **Given** a subscribed channel session and an item without autonomy labels, **When** subscriber `x` runs `set_autonomy` with mode `cruise`, **Then** the `label-applied fabrik:cruise` event carries `actor=overseer` and `via=x`.
2. **Given** the same setup, **When** the engine itself applies a label in the course of its own work (for example a stage-state label), **Then** the event carries `actor=fabrik` and no `via`.
3. **Given** an overseer action that adds a label and removes another (`set_autonomy` switching yolo to cruise), **When** both deltas are published, **Then** both events carry `actor=overseer` and `via=<subscriber>`.
4. **Given** the webhook echo of an overseer-written label matches the engine's echo registry, **When** the echo is processed, **Then** the attribution is `overseer`/`via`, not `fabrik`, and no duplicate event is produced.

---

### User Story 2 - A human operator's label change shows who did it (Priority: P2)

When the daemon has no webhook sender for a label change, the event names the human or bot that made it wherever the engine already holds that information, instead of `unknown`.

**Why this priority**: It fixes the second observed case, but depends on which engine reads already carry timeline actors, so its reach is narrower and less certain than Story 1.

**Independent Test**: For each path chosen to resolve unknown actors, drive an unattributed label change (no webhook sender) through that path and assert the event carries the actor login and the `human` or `bot` class. For a path that cannot resolve it, assert `unknown` is retained.

**Acceptance Scenarios**:

1. **Given** no webhook sender and an engine read that already returned the actor of a label event, **When** the change is published, **Then** the event carries `actor=human` and the login in its attributes.
2. **Given** the resolved actor login ends in `[bot]` and is not the engine's own identity, **When** the change is published, **Then** the event carries `actor=bot` and the login.
3. **Given** no webhook sender and no already-held actor data on the path, **When** the change is published, **Then** `actor=unknown` is retained and no additional GitHub call is made to find out.

---

### User Story 3 - Operators and integrators can read the new semantics (Priority: P3)

The documentation states what each actor value means, when `via` is present, which unknown-actor paths resolve and which do not.

**Why this priority**: The behaviour is useless to overseers if undocumented, and CI enforces the docs bundle.

**Independent Test**: The documentation drift check passes after regenerating the bundle, and the docs name `overseer`, `via`, and the login attribute.

**Acceptance Scenarios**:

1. **Given** the change is complete, **When** a reader looks up the `label-applied` event attributes in the user guide, **Then** they find `actor` (including `overseer`), `via`, and the actor login.
2. **Given** the change is complete, **When** a reader looks up actor semantics in the state-machine document, **Then** they find which paths resolve an otherwise-unknown actor and that the rest remain `unknown`.

---

### Edge Cases

- **Race with an autonomous write of the same label.** An overseer write and an engine autonomous write of the same label on the same item close together: each delta is attributed according to the write that actually produced it, and a pending overseer attribution is never applied to a later, unrelated engine write.
- **Failed overseer write.** A write that fails leaves no pending attribution that could mislead a later event.
- **Partial `set_autonomy`.** When the first label write lands and the second fails, the event for the landed write is attributed to the overseer.
- **Command labels removed by the engine.** `fabrik:revalidate` and `fabrik:clear-claude-limit` are added by an overseer action but removed by the engine's settle scan. The removal is the engine's own write and stays `actor=fabrik`.
- **App auth.** Under GitHub App auth the overseer action is executed with the App identity, so a webhook sender equal to the App's bot login would read as `fabrik`. The overseer attribution takes precedence over that.
- **PAT auth.** The operator's own login cannot be told apart from Fabrik's. That limit is unchanged; an overseer action is still attributed via the echo, not via the login.
- **Echo missed.** If the echo registration for an overseer write is unavailable, the event may fall back to today's attribution; this must not produce a wrong specific claim such as attributing to a different subscriber.
- **Sanitised subscriber name.** `via` carries the same sanitised subscriber name (reduced character set, at most 64 characters) already used in the audit comment, never the raw input.
- **Already-published events.** An event already pushed as `unknown` is not retracted or re-sent when the actor is learned later.
- **Subscription filters.** Existing subscription filters, deduplication and digest batching continue to work with the new `actor` value and the extra attributes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Label changes produced by an overseer action (`promote`, `set_autonomy`, `revalidate`, `clear_claude_limit`) are published with `actor=overseer` and `via=<requesting subscriber>`, where the subscriber is the sanitised name already used for the action's audit comment.
- **FR-002**: Label changes produced by the engine's own autonomous writes continue to be published with `actor=fabrik` and without `via`.
- **FR-003**: The overseer attribution is carried to the channel through the engine's existing write-through and echo-registration path, so it applies equally when the change is observed first by the write-through and when it is observed first by a webhook that matches the echo registry; either way exactly one event is published per change.
- **FR-004**: An overseer attribution applies only to the write that carried it. A failed write, an unrelated engine write, or a later write of the same label by another actor is never attributed to the overseer.
- **FR-005**: When no webhook sender is available, the actor of a label change may be resolved from GitHub event data, but only where the engine already reads that data for its own purposes; no new GitHub call is added for attribution.
- **FR-006**: The channel layer, the local API and the `internal/channelevents` package continue to make no GitHub calls and read item state only through their existing read paths; any actor resolved under FR-005 reaches them as data already present on the state change.
- **FR-007**: Where an actor cannot be resolved under FR-005, `actor=unknown` is retained.
- **FR-008**: A resolved actor is classified `human` or `bot` using the same classification rules as a webhook sender today, and its login is carried in the event attributes under a documented, stable attribute name.
- **FR-009**: Overseer attribution does not alter any engine decision, GitHub mutation or its ordering; the existing observation-only guarantee holds.
- **FR-010**: The `docs/state-machine.md` §7.14 actor semantics describe the `overseer` value, `via`, the login attribute, which unknown-actor paths resolve and at what cost, and which remain `unknown`; the "known limits" text is updated to match.
- **FR-011**: The `docs/USER_GUIDE.md` event attribute table lists `actor` (with `overseer`), `via` and the login attribute for `label-applied` / `label-removed`.
- **FR-012**: `docs/llms-full.txt` is regenerated in the same change as the documentation edits.
- **FR-013**: A new ADR for this issue records the attribution design, which unknown-actor paths were chosen to resolve, the GitHub cost of each (none beyond reads already made), and the paths left `unknown`.
- **FR-014**: The existing documentation statement in §7.15 that an action's label write surfaces as actor `fabrik` is corrected to reflect FR-001.

### Key Entities *(if applicable)*

- **Event actor**: the classification of who caused a label change: `fabrik`, `overseer` (new), `human`, `bot`, `unknown`.
- **Requester (`via`)**: the sanitised subscriber name of the overseer session that invoked an action; present only on overseer-attributed events.
- **Actor login**: the GitHub login of a resolved human or bot actor, carried in the event attributes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test running `SetAutonomy` from subscriber `x` observes a published `label-applied` event with `actor=overseer` and `via=x`; the test fails against the pre-change behaviour (non-vacuous).
- **SC-002**: A test of an engine autonomous label write observes `actor=fabrik` and no `via`.
- **SC-003**: For each path chosen to resolve unknown actors, a test with an unattributed label change observes the login and the `human`/`bot` class, and fails against the pre-change behaviour; for a path left unresolved, a test observes `actor=unknown`.
- **SC-004**: The channel no-GitHub source scan and the observation-only sim test pass unchanged.
- **SC-005**: Test coverage includes the echo-first webhook case (one event, overseer-attributed) and the failed-write case (no stale attribution).
- **SC-006**: `go test -race ./...` passes and the documentation drift check passes.

## Assumptions

- "Column and comment events" in the issue's R1 refers to the overseer action's other writes; today only label changes produce channel events with an `actor`, and new event types are out of scope. So the observable change is on `label-applied` / `label-removed`, and the column move and audit comment need no event attribution of their own.
- The `overseer` actor value is an addition to the existing actor vocabulary, not a new event type.
- The login attribute is named `actor_login`; the final name is chosen at Plan time and documented. `via` is named exactly as in the issue.
- Resolution under FR-005 happens at the time the engine observes the change. Events already published are not amended later.
- Which engine reads carry timeline actors (the label-applied-time read, the reconcile/deep-fetch path) is a Research question. If a candidate path would need a new or wider GitHub read to carry the actor, it is left unresolved rather than adopted.
- Existing subscription filtering does not gain an actor filter; this change adds none.
- The PR for this issue mentions #1966 by bare number only and uses no closing keyword near it; it closes only this issue.

## Out of Scope *(optional)*

- New channel event types.
- Authentication or authorisation of subscribers; the socket stays unauthenticated and the name stays an advisory audit aid (ADR-1966-c).
- Adding any new GitHub read or call purely to resolve an actor.
- Retroactively amending events already published as `unknown`.
- An actor-based subscription filter.
- Changing the engine's PAT-auth limitation that the operator's own login is indistinguishable from Fabrik's.

## Source References *(optional)*

- `docs/state-machine.md` §7.14 (channel events, actor of a label event) and §7.15 (overseer actions, attribution)
- `docs/USER_GUIDE.md` channel event table (`label-applied`, `label-removed`)
- ADR-1966-b (channel events), ADR-1966-c (overseer actions)
- #1966 (mentioned for context only)
