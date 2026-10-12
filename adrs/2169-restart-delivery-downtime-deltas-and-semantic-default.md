# ADR 2169: Restart delivery, downtime deltas, semantic default and re-settle cause

## Status

Accepted. Supersedes ADR-1966-b's "a reconnect never receives a catch-up" and "a restart is silent", and its rule that an empty `events` list means every type (for subscriptions created from now on). Builds on ADR 2123 (the in-memory `LabelsKnown` baseline), ADR 2125 (actor vocabulary), ADR 2120 (settle episodes) and ADR 2170 (`unverified`).

## Context

An overseer is a long-lived session that reacts to channel pushes. Four things were wrong for it:

1. A restart had no defined delivery. ADR 2123 removed the false label deltas, but a real change made while the daemon was down (`fabrik:paused` removed, a column move) was indistinguishable from a replay, so it was lost.
2. The catch-up snapshot, the right model of "what needs you", was queued only for a new subscription or a session's first attach. A session reconnecting after a restart got nothing comparable.
3. Raw `label-applied` / `label-removed` events were delivered by default, although an overseer acts on semantic facts and a label push is not a readiness signal.
4. A second `validate-settled` for the same head, after `fabrik:cruise` became `fabrik:yolo`, said nothing about why or who.

Reproduction (Task 1): the second settle does not occur today inside an open episode, because `settleCandidate` dedups on the open `vs:` episode. So for the cruise to yolo case the work adds the trigger (an autonomy change ends the open episode) as well as annotating the event. The sim twin shows it non-vacuous: with the cause neutralised the second settle has neither `cause` nor actor, and without the trigger there is no second settle.

## Decision

### 1. Restart sequence

After a daemon start a subscriber receives, in queue order: `daemon-reachable` (written by the shim at the attach ack), events already held for it, one catch-up snapshot, then the downtime deltas. Nothing replays unchanged state. A reconnect not preceded by a daemon start gets no catch-up, as before.

The catch-up is queued to **every subscriber with a subscription at daemon readiness**, not at attach time. This makes the order deterministic, covers detached subscribers, and works for older long-lived shims that send `catch_up=false` on reconnect (no protocol change). Restart detection is therefore daemon-side.

**Readiness gate.** A catch-up taken right after a restart would read a label-less store and miss the settled items it exists to show. The daemon is warming until the board is populated and every open non-PR item in a managed stage has `LabelsKnown`, or until a 3-minute real-time timeout, so an item that is never deep-fetched cannot postpone it for ever. Subscribe-time and attach-time catch-up requests while warming are no-ops. Downtime deltas are buffered until the catch-up is queued, then published. Events other than deltas are not held back; the hub's held-dedup stops a first-sight `decision-pending` from doubling in the catch-up.

### 2. Persist what the store observed, not what each subscriber was sent

R2 says "last-published". A per-item global record of the last observation is independent of any subscription filter and correct for every subscriber, including one that was detached. A per-subscriber record is heavier and still loses changes made while the subscriber was away.

The ledger (`.fabrik/state/channel/items.json`) holds labels, column, closed flag and the last announced `validate-settled`. It drops the `DefaultExcludeLabels` patterns, because startup cleanup removes `fabrik:locked:*` and `stage:*:in_progress` and would otherwise appear as phantom deltas. Writes are coalesced (about 1 s) and flushed on close and before the self-upgrade/SIGHUP exec; the loss window after a crash is documented. A missing, corrupt or version-mismatched ledger makes every item a baseline.

### 3. Downtime deltas carry `unverified=true`

R2 calls a downtime difference a real event; FR-009 requires `unverified=true` for unattributable ones. Both hold: it is delivered as an event with `source=downtime-delta`, `actor=unknown`, and the doubt attached to its attribution. Arrivals during downtime stay out (ADR 2123, no record means baseline).

### 4. Default family and existing subscriptions

A subscription created without `events` receives every subscribable type except `label-applied` and `label-removed`. This includes lifecycle (`item-added`, `column-changed`, `closed`), pause family, settle and decision, train, anomaly, pipeline and `daemon-*` events. `Subscription.EventsDefault` records the choice.

Subscriptions persisted before this change have no marker and are **grandfathered** as "every type". Migrating them would silently take raw label events away from a running overseer, and the marker, which `canonical()` ignores, is the only way to keep re-subscribe idempotent. To move to the new default, unsubscribe and subscribe again. `labels`, `exclude_labels` and `progress_via_column` only shape label events, and the subscribe result carries notes when label events are not in effect.

### 5. Provenance

`source` is `live`, `catch_up` or `downtime-delta`; there is no replay. `finishEvent` stamps `live`, the catch-up builders `catch_up`. Claude Code sets the `<channel>` tag's own `source` attribute from the server name, so the **wire key is `origin`**; the hub and daemon keep `source`, mapped in one place (`wireMetaKey`). The shim defaults `origin=live` for events it generates.

`unverified=true` is set on a label, column or closed event built from a change with a reconcile, deep-fetch or probe origin, with no known write or webhook behind it. #2170's `cache-divergence` stays the narrow detector.

### 6. Re-settle cause

The first settle for an item carries **no `cause`** (not `cause=initial`): after a lost ledger the daemon cannot honestly claim "initial". A later settle carries one cause, by precedence `revalidated` > `new-head` > `autonomy-changed` > `other`. `other` is an extension beyond the three named values so a re-settle is never unlabelled (for example a worker re-run at the same head and autonomy).

An autonomy change while the item stays settled ends the open episode without `settlement-withdrawn`, because the item is still settled and this is a human decision, not an engine withdrawal. `autonomy-changed` carries the autonomy writer's actor (`fabrik`, `overseer` with `via`, `human`/`bot` with `actor_login`, `unknown`), taken from the attribution of the `fabrik:cruise` / `fabrik:yolo` deltas; an added label wins over a removed one, since a swap is two deltas.

## Consequences

- A reacting session sees a deterministic restart sequence and loses no downtime change that the daemon could observe.
- Old shims keep working; they ignore `origin` and `cause`, which are plain meta.
- Existing overseers keep their label events until they resubscribe.
- After a crash the ledger can be one flush window stale, and a change already delivered can be reported once more, marked `unverified`. The reverse loss is closed: an item whose downtime delta is still held in memory while the daemon warms keeps its previous ledger record until the delta is published (`releaseUnpub`), so a kill during warming makes the next start diff it again instead of losing it.
- `engine/channel_*.go` still reads item state only through the store and the ledger, and costs no GitHub call (R9/R10).
