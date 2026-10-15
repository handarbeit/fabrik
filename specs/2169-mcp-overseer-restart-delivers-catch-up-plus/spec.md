# Feature Specification: MCP overseer restart delivers catch-up plus downtime deltas, not replays; semantic events by default

**Feature Branch**: `fabrik/issue-2169`
**Created**: 2026-10-12
**Status**: Specified
**Input**: User description: "MCP overseer: restart delivers catch-up plus downtime deltas, not replays; semantic events by default, raw label events opt-in

> ## Problem
>
> For an overseer session that *reacts* to pushes, today's channel stream has the wrong shape across a daemon restart, and its default is too raw. Observed in the in-situ MCP session on 2026-10-11 and discussed with the maintainer.
>
> - **Restart replays are noise, and dangerous.** Every replayed label looks like a trigger to a reacting session. They cost about 400 messages of context per restart, and they bury the real events. #2123's baseline rule removes the false label deltas. It does not define what a restart *should* deliver.
> - **Changes made while the daemon is down are lost.** During the 18:40–18:41Z downtime, `fabrik:paused` / `fabrik:awaiting-input` were removed from #2120 and #2123. Neither removal was ever pushed: the restart treats the first observation as a baseline, so a real change made during downtime is indistinguishable from a replay.
> - **The catch-up snapshot is the right model, but only fires on first attach.** Resubscribing produced exactly five `catch_up=true` messages: #1342 awaiting-input, plus `validate-settled` for #1805, #2091, #2116 and #2128. That is the whole "what needs you" picture. A reconnect after a restart gets nothing comparable.
> - **Raw label events are the default.**
>   - An overseer acts on semantic facts, not on label churn. The facts it acts on: settled, unsettled, pending decision, paused with a cause, merged, column advanced, retry starved, poll stalled.
>   - Label events are the noisiest category, and the actor is often `unknown` (ADR 2125).
>   - Label pushes are not a readiness signal (see #2120's R-unsettle-2).
> - **A re-settle does not say why it happened.** When an item already at Validate-complete is switched from `fabrik:cruise` to `fabrik:yolo`, a second `validate-settled` arrives for the **same `head_sha`**. It differs from the first only in `autonomy` and `next`. There is no cause, actor or via, so a subscriber that has narrowed its feed to semantic events cannot tell that a human made the merge decision; it reads as a fresh settle. Seen on #2123, #2178 and #2138 (in-situ MCP test, 2026-10-12 00:18Z onward, daemon `dev(9ede632)`).
>
> ## Requirements
>
> - **R1 — Restart sequence.** After a daemon restart, an attached or reattaching subscriber receives exactly the following, in order:
>   1. `daemon-reachable`;
>   2. one `catch_up` snapshot of the current "needs you" picture, on **every reconnect** after a daemon restart, not only the first attach;
>   3. real deltas for changes that happened while the daemon was down.
>
>   It receives no replay of unchanged state.
> - **R2 — Downtime deltas.** The daemon persists the last-published per-item state it needs (labels and column at minimum) beside the hub's state. On start, it diffs the first full observation of each item against that persisted state, rather than treating it as a baseline:
>   - a difference is a real event, with actor `unknown` unless known;
>   - an item with no persisted state is a baseline.
>
>   This is the persisted counterpart of #2123's in-memory `LabelsKnown` rule.
> - **R3 — Semantic events by default.**
>   - A new subscription without an explicit `events` list receives the semantic event family and not raw `label-applied` / `label-removed`. Raw label events stay available by naming them.
>   - The ADR lists the default family and decides how existing persisted subscriptions are treated (grandfathered or migrated).
>   - The `fabrik_subscribe` description says label events are opt-in and not a readiness signal.
> - **R3a — Provenance on every push.**
>   - Every event carries `source=live|catch_up|downtime-delta`. There is no `replay`: R1 forbids replays.
>   - A label or column event derived from a reconcile or probe that the daemon cannot attribute to a known write or webhook carries `unverified=true`, so a phantom such as the reconcile-race pushes (A9) can be told apart. `cache-divergence` is in #2170.
>   - The actor uses the ADR 2125 vocabulary (`fabrik`, `overseer` with `via`, `human`/`bot` with `actor_login`, `unknown`).
>   - **Re-settle cause.** Every `validate-settled` after the first in an episode carries `cause=autonomy-changed | revalidated | new-head`, so a subscriber on a semantic-only feed can tell why the item settled again.
>     - The first settle of an episode carries no cause, or `cause=initial`; the ADR picks which.
>     - `cause=autonomy-changed` also carries the actor of the autonomy-label write (for example the `fabrik:cruise` to `fabrik:yolo` swap), in the ADR 2125 vocabulary: `fabrik`, `overseer` with `via`, `human`/`bot` with `actor_login`, or `unknown`.
> - **R3b — Catch-up is the situation digest.** The catch-up snapshot in R1 is the `situation` digest from #2171 once that lands. Until then, it is today's catch-up builder.
> - **R4 — Docs.** Cover the restart sequence, downtime deltas, the default family and the re-settle cause in `docs/state-machine.md` §7.14 and in `docs/USER_GUIDE.md`, regenerate `docs/llms-full.txt`, and add an ADR.
>
> ## Scope
>
> In scope:
> - restart and reconnect delivery;
> - the persisted last-published state;
> - the default subscription family;
> - provenance on pushes, including the cause and actor of a re-settle.
>
> Out of scope:
> - the in-memory baseline rule itself (#2123);
> - the settled/unsettled events (#2120);
> - shim/daemon skew (separate issue).
>
> ## Acceptance
>
> - **Restart sim:**
>   - Several labelled items, with a subscriber attached across a restart.
>   - While the daemon is down, one item's `fabrik:paused` is removed and another moves column.
>   - After the restart the subscriber receives `daemon-reachable`, one catch-up snapshot, then exactly those two deltas, and nothing else.
>   - It is shown non-vacuous against the pre-change behaviour.
> - **Default family:** a subscription created with no `events` receives `validate-settled` but not `label-applied` for a label change. Naming `label-applied` restores it.
> - **Re-settle cause:**
>   - A sim test settles an item on `fabrik:cruise`, then swaps it to `fabrik:yolo` by a human label write.
>   - It asserts that the second `validate-settled` has the same `head_sha`, `cause=autonomy-changed` and `actor=human`.
>   - It is shown non-vacuous against the current behaviour, where the second settle carries no cause or actor.
> - `go test -race ./...` is clean.
>
> Mentioned for context: #1966 (the human feature thread). Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

An overseer is a long-lived Claude session that subscribes to the Fabrik daemon's channel push and *reacts* to what arrives. The stream has the wrong shape for that job in four ways. They were observed in the in-situ MCP session of 2026-10-11/12 and discussed with the maintainer.

1. **A restart replays noise.** Every replayed label looks like a trigger to a reacting session. A restart cost about 400 messages of context and buried the real events. #2123's in-memory baseline rule removes the *false label deltas*, but nothing defines what a restart *should* deliver.
2. **Changes made while the daemon is down are lost.** During the 18:40–18:41Z downtime, `fabrik:paused` / `fabrik:awaiting-input` were removed from #2120 and #2123. Neither removal was ever pushed. The restart treats the first observation of each item as a baseline, so a real change made during downtime cannot be told from a replay.
3. **The catch-up snapshot is the right model but fires too rarely.** Resubscribing produced exactly five `catch_up=true` messages (#1342 awaiting-input, plus `validate-settled` for #1805, #2091, #2116 and #2128). That is the complete "what needs you" picture. Today only a new subscription or a session's *first* attach gets one, so a session that reconnects after a daemon restart gets nothing comparable.
4. **The default is too raw.** An overseer acts on semantic facts: settled, unsettled, pending decision, paused with a cause, merged, column advanced, retry starved, poll stalled. Raw label events are the noisiest category, their actor is often `unknown` (ADR 2125), and label pushes are not a readiness signal (#2120). Yet they are delivered by default.

A fifth gap concerns provenance. When an item already settled at Validate is switched from `fabrik:cruise` to `fabrik:yolo`, a second `validate-settled` arrives for the **same `head_sha`**, differing from the first only in `autonomy` and `next`. It carries no cause, actor or via. A subscriber narrowed to semantic events therefore cannot tell that a human made the merge decision, and the event reads as a fresh settle (seen on #2123, #2178 and #2138, daemon `dev(9ede632)`).

Existing behaviour this builds on (see `docs/USER_GUIDE.md` and `docs/state-machine.md` §7.14): a restart or self-upgrade is currently *silent* (no label events or re-announced pauses until something changes); the catch-up batch goes only to a new subscription and a session's first attach; `fabrik mcp` reconnects by itself after a daemon restart; `events` empty means "all"; unverified-flagged events and `cache-divergence` already exist from #2170; the actor vocabulary is ADR 2125's.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A restart delivers the current picture, then only what changed while down (Priority: P1)

As an overseer session attached across a daemon restart (including a self-upgrade or SIGHUP re-exec), I want to be told the daemon is back, shown what currently needs me once, and then told only about things that really changed during the downtime. This lets me react correctly without wading through a replay of unchanged state.

**Why this priority**: This is the core defect. Replays cost context and can trigger wrong reactions, and real downtime changes are silently lost.

**Independent Test**: In the sim, attach a subscriber to several labelled items, restart the daemon, change one item's `fabrik:paused` label and move another item's column while it is down, and check what arrives after reconnect.

**Acceptance Scenarios**:

1. **Given** a subscriber attached and several labelled items, **When** the daemon restarts and nothing changes during the downtime, **Then** the subscriber receives `daemon-reachable` and one catch-up snapshot of the "needs you" picture, and nothing else (no label replay).
2. **Given** `fabrik:paused` is removed from one item and another item moves column while the daemon is down, **When** the daemon restarts and the subscriber reconnects, **Then** it receives, in order, `daemon-reachable`, one catch-up snapshot, then exactly those two changes as events marked `source=downtime-delta`, and nothing else.
3. **Given** a subscriber that already attached once (not its first attach), **When** the daemon restarts and it reconnects, **Then** it still receives the catch-up snapshot (today it would receive none).
4. **Given** an item the daemon has no persisted state for (new since the last run, or no state file at all), **When** the daemon first observes it after start, **Then** it is treated as a baseline and produces no downtime-delta events.

---

### User Story 2 - Semantic events by default; label events opt-in (Priority: P2)

As an overseer subscribing without specifying event types, I want the semantic event family and not the raw label churn. I name `label-applied` / `label-removed` explicitly when I really want them.

**Why this priority**: It removes the largest source of noise and false triggers, and is independent of the restart work.

**Independent Test**: Create a subscription with no `events` list, apply and remove a label on an item, and settle another item at Validate; then repeat after naming `label-applied`.

**Acceptance Scenarios**:

1. **Given** a subscription created with no `events` list, **When** a label changes on a matching item and another matching item settles at Validate, **Then** the subscriber receives `validate-settled` but not `label-applied` / `label-removed`.
2. **Given** a subscription that names `label-applied` in `events`, **When** a label is applied, **Then** the subscriber receives it.
3. **Given** a subscription persisted before this change, **When** the new daemon starts, **Then** it is handled as the ADR decides (grandfathered or migrated), and that choice is documented.
4. **Given** an agent reading the `fabrik_subscribe` tool description, **When** it considers label events, **Then** the description tells it they are opt-in and not a readiness signal.

---

### User Story 3 - Every push says where it came from and, for a re-settle, why (Priority: P2)

As an overseer on a semantic-only feed, I want each event to carry its provenance (live, catch-up or downtime delta; verified or not; who acted), and each second or later `validate-settled` to carry the cause. That way I can tell a human merge decision from a fresh settle and a phantom from a real change.

**Why this priority**: Without provenance, the new catch-up and delta deliveries are ambiguous, and the cruise to yolo case shows semantic-only feeds silently lose the "who decided" fact.

**Independent Test**: In the sim, settle an item on `fabrik:cruise`, swap the label to `fabrik:yolo` as a human write, and inspect the second `validate-settled`.

**Acceptance Scenarios**:

1. **Given** an item settled at Validate on `fabrik:cruise`, **When** a human replaces `fabrik:cruise` with `fabrik:yolo` without a new head, **Then** the second `validate-settled` has the same `head_sha`, `cause=autonomy-changed` and `actor=human`.
2. **Given** a settled item whose head SHA changes, **When** it settles again, **Then** the event carries `cause=new-head`.
3. **Given** a settled item that is revalidated (`fabrik:revalidate`), **When** it settles again, **Then** the event carries `cause=revalidated`.
4. **Given** the first settle of an episode, **When** it is announced, **Then** it carries no cause (or `cause=initial`, as the ADR decides), so it is never mistaken for a re-settle.
5. **Given** a label or column change that came from a reconcile or probe and cannot be attributed to a known write or webhook, **When** the event is published, **Then** it carries `unverified=true`.
6. **Given** any event pushed to a subscriber, **When** it arrives, **Then** it carries `source=live`, `source=catch_up` or `source=downtime-delta`, and never a replay source.

---

### Edge Cases

- **No prior persisted state** (first run after the upgrade, deleted or corrupt state file): every item is a baseline and no downtime deltas are produced, which is the same quiet behaviour as today. No error reaches the subscriber.
- **Ungraceful exit** (crash or kill): downtime deltas must still be correct, so persisted state must reflect what was last observed or published as the daemon ran, not only what was written at a clean shutdown.
- **Item closed, removed from the board or moved to Done during downtime**: reported through the existing semantic events (`closed`, `column-changed`) as downtime deltas; an item absent from the first observation produces no event of its own.
- **Item both in the catch-up and changed during downtime** (for example it became settled while the daemon was down): it appears in the catch-up snapshot, and any real label/column delta is still delivered once as a downtime delta. Neither delivery suppresses the other.
- **A downtime change the daemon cannot attribute**: delivered with actor `unknown`, never guessed.
- **Reconnect without a daemon restart** (a connection blip, daemon never down): behaviour is unchanged. There is no catch-up and no downtime delta.
- **Events already held for a detached subscriber** when the daemon restarts: these are real, previously published events, not replays, and are still delivered.
- **Re-settle with several triggers at once** (for example a new head after a revalidate): a single cause is chosen by a documented precedence.
- **Autonomy write of unknown origin** (seen only by a reconcile): the re-settle still carries `cause=autonomy-changed`, with actor `unknown`.
- **Subscription naming only raw label events**: still receives exactly those, including as downtime deltas and catch-up-era changes.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a daemon restart (including a self-upgrade or SIGHUP re-exec), an attached or reattaching subscriber receives, in this order: (1) `daemon-reachable`; (2) one `catch_up` snapshot of the current "needs you" picture; (3) real deltas for changes made while the daemon was down. It receives no replay of unchanged state.
- **FR-002**: The catch-up snapshot of FR-001 is delivered on **every** reconnect after a daemon restart, not only on a new subscription or a session's first attach. A reconnect without a daemon restart receives no catch-up, as today.
- **FR-003**: The daemon persists the last-published per-item state it needs to detect downtime changes, at minimum each item's labels and column. It stores this beside the hub's persisted state and keeps it current while the daemon runs, so the result is correct after an ungraceful exit.
- **FR-004**: On start, the daemon compares the first full observation of each item against the persisted state instead of treating it as a baseline. Any difference is delivered as a real event; its actor is `unknown` unless the daemon knows better. An item with no persisted state is a baseline and produces no event. Missing or unreadable persisted state degrades to baseline for every item.
- **FR-005**: A new subscription with no explicit `events` list receives the *semantic event family* and not the raw `label-applied` / `label-removed` events. Naming those events in `events` delivers them. Subscriptions that name events explicitly are unaffected.
- **FR-006**: The ADR enumerates the default semantic family and states how subscriptions persisted before this change are treated (grandfathered or migrated). Existing subscriptions must not silently start receiving more events than they did.
- **FR-007**: The `fabrik_subscribe` tool description states that label events are opt-in and are not a readiness signal.
- **FR-008**: Every event pushed to a subscriber, including shim-generated ones such as `daemon-reachable`, carries `source` with one of `live`, `catch_up` or `downtime-delta`. No `replay` value exists.
- **FR-009**: A label or column event derived from a reconcile or probe that the daemon cannot attribute to a known write or webhook carries `unverified=true`. This extends to downtime deltas that cannot be attributed.
- **FR-010**: The actor on every event that has one uses the ADR 2125 vocabulary: `fabrik`, `overseer` with `via`, `human` or `bot` with `actor_login`, or `unknown`.
- **FR-011**: Every `validate-settled` after the first in an episode carries `cause` with one of `autonomy-changed`, `revalidated` or `new-head`. The first settle of an episode carries no cause or `cause=initial`; the ADR chooses which and documents it. When several triggers coincide, the ADR defines the precedence.
- **FR-012**: A `validate-settled` with `cause=autonomy-changed` also carries the actor of the autonomy-label write (for example the `fabrik:cruise` to `fabrik:yolo` swap) in the FR-010 vocabulary, including `via` for an overseer action and `actor_login` for a human or bot. An unattributable write is `unknown`.
- **FR-013**: The catch-up snapshot is today's catch-up builder. When the `situation` digest of #2171 lands, the snapshot is that digest.
- **FR-014**: The restart sequence, downtime deltas, the default family, provenance fields and the re-settle cause are documented in `docs/state-machine.md` §7.14 and `docs/USER_GUIDE.md`. `docs/llms-full.txt` is regenerated, and an ADR numbered `2169` records the decisions.

### Key Entities *(if applicable)*

- **Persisted last-published item state**: per item, at least its labels and column as last observed or published by the daemon, kept beside the hub's state. It is the reference a restart's first observation is compared against.
- **Event provenance**: the `source` (live, catch_up, downtime-delta), `unverified` flag and actor (with `via` / `actor_login`) carried by an event.
- **Settle episode and cause**: the span during which an item stays settled at Validate. The first announcement has no cause (or `initial`); each re-settle carries `autonomy-changed`, `revalidated` or `new-head`.
- **Default semantic family**: the set of event types a subscription with no `events` list receives, namely every catalogued type except raw label events.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a restart scenario with several labelled items, a subscriber attached across the restart receives exactly `daemon-reachable`, one catch-up snapshot, then one event per real downtime change (and no other event), for both an unchanged and a changed downtime.
- **SC-002**: A restart with no downtime changes delivers zero label or column events to the subscriber, where the pre-change behaviour dropped real changes or flooded replays.
- **SC-003**: A session that has attached before receives the catch-up snapshot on every daemon restart (100% of reconnects after a restart, 0% after a plain connection blip).
- **SC-004**: A subscription with no `events` list receives `validate-settled` and no raw label events for a label change. Naming `label-applied` restores them.
- **SC-005**: A cruise to yolo swap by a human on a settled item produces a second `validate-settled` with the same `head_sha`, `cause=autonomy-changed` and `actor=human`. The test fails against the current behaviour (no cause or actor).
- **SC-006**: Every event observed in the sim tests carries a `source` value from the allowed set.
- **SC-007**: `go test -race ./...` passes, and the new sim tests are each shown to fail against the pre-change behaviour.

## Assumptions

- Downtime deltas are matched against each subscriber's own subscription like any live event. A subscriber on the default family sees a downtime `fabrik:paused` removal only if it names label events or if a semantic event is derived from it; a column move is visible by default through `column-changed`. The restart acceptance test therefore uses a subscription that also names the label events it asserts on.
- The default semantic family is every catalogued event type except `label-applied` and `label-removed`, including lifecycle, pause-family, settle, train, anomaly and `daemon-*` events. The ADR lists it and may refine it.
- Events already held for a detached subscriber are real, undelivered events and are still delivered after a restart. They are not replays, and they do not count as a violation of the "nothing else" rule.
- A reconnect that is not preceded by a daemon restart (a blip) keeps today's behaviour: no catch-up. "Daemon restart" includes the self-upgrade and SIGHUP re-exec.
- A catch-up event carries `source=catch_up` and no `cause`, since it is a snapshot and not a transition.
- Missing, corrupt or version-mismatched persisted state is quiet: all items become baselines, and the condition is logged but not delivered to subscribers.
- The ADR picks between "no cause" and `cause=initial` for the first settle of an episode, and the precedence among coinciding causes; the observable requirement is only that a first settle is distinguishable from a re-settle.
- `#2120` (settle/unsettle events) has landed and `#2123`/`#2125`/`#2170` are in the code base; `#2171` has not, so FR-013 uses the current catch-up builder.

## Out of Scope *(optional)*

- The in-memory baseline rule itself (#2123).
- The settled/unsettled events (#2120).
- Shim/daemon version skew (a separate issue).
- Building the `situation` digest (#2171); this work only makes it the catch-up snapshot once it exists.
- New attribution sources for the actor beyond ADR 2125; `unknown` remains the answer when attribution is not possible.

## Source References *(optional)*

- `docs/USER_GUIDE.md` channel section ("Catch-up", "Held delivery", "Label events are not a readiness signal") and `docs/state-machine.md` §7.14.
- ADR 1966-b (channel events), ADR 2123 (restart baseline), ADR 2125 (actor attribution), ADR 2120 (settle semantics), ADR 2170 (anomaly events and `unverified`).
- For context: #1966 (the human feature thread), mentioned by bare number only.
