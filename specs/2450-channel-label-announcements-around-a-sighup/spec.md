# Feature Specification: Exactly-once channel label announcements across a SIGHUP restart

**Feature Branch**: `fabrik/issue-2450`
**Created**: 2026-10-25
**Status**: Specified
**Input**: User description: "## Problem

Channel label announcements around a SIGHUP restart (ADR 2259) are unreliable on a webhook bed, although the Store ends up correct. `TestWebhookRestartUnderLoad` (#2427 FR-011) adds 6 labels to one item — 2 before the SIGHUP, 4 during and after — and a channel subscriber counts the `label-applied` events. In the 0.0.90 gate (bed B, `app/on/webhook`, engine `ad9b801`) three runs gave three different results; the daemon held all 6 labels every time:

| run | test version | label 2 (queued before SIGHUP) | labels 3–6 |
|---|---|---|---|
| 1 | as merged in #2437 | announced, `actor=unknown` | 3 and 5 announced twice |
| 2 | same | announced, `actor=unknown` | fine |
| 3 | polls held across the pre-SIGHUP labels (#2449) | not announced | 3 and 4 not announced |
| 4 | same as run 3 (full matrix, engine `ad9b801`) | not announced | 3 and 4 not announced |

Logs: `~/dev/fabrik-e2e-logs/gate-0090-sparse-20261025-015051.log`, `…-sparse-resume-20261025-062618.log`, `…-sparse-resume2-20261025-070943.log`, `…-full2-20261025-070943.log` (run 4, lines ~4090–4102); bed log `~/dev/fabrik-test-2/bed-run.log`.

Leads, none confirmed:
- `channelEvents.run` (`engine/channel_events.go`) returns on `stop` without a final `drain`, so changes queued between the last drain and `closeChannelEvents` are never published, while the ledger (written from the observer) already records them, so no downtime delta replaces them after the restart.
- `performSighupRestart` closes the intake (applying queued deliveries) before `closeChannelEvents`; whether every change applied in that window reaches the hub is not established.
- The duplicates in run 1 coincided with a post-restart replay of 5 failed deliveries.
- `actor=unknown` for label 2 fits a wake-poll listing applying the label before its own delivery (ADR 2125's documented limit) — that part may be expected.

## Requirements

- Research: establish, with a sim twin or an engine test that drives a real exec seam, why label events applied around a SIGHUP restart are lost or repeated.
- Every label change applied by the daemon is announced exactly once across a SIGHUP restart — live before the exec, or as a downtime delta after it — never zero times and never twice.

## Acceptance

- The sim/engine twin reproduces a lost and a repeated announcement before the fix and passes after it.
- `TestWebhookRestartUnderLoad` passes on `app/on/webhook`."

## Background

A channel subscriber (an overseer session attached through the MCP shim) relies on `label-applied` / `label-removed` events to follow what happens to board items. Around a two-phase SIGHUP restart (ADR 2259) the daemon's item Store ends up correct, but the announcements to the subscriber are not: in the 0.0.90 release gate the same live test (`TestWebhookRestartUnderLoad`, which applies 6 labels to one item, 2 before the SIGHUP and 4 during and after, on a webhook-fed bed) produced four runs with four different outcomes. Labels were sometimes announced zero times, sometimes twice, and sometimes with an unknown actor, while the daemon always held all 6 labels.

An announcement should come either from the live stream before the restart, or from the restart-delivery mechanism that reports changes made while the daemon was down (downtime deltas, ADR 2169). The failures observed point to a gap between those two: a change that the daemon has already recorded as "seen" but has not yet delivered is dropped at shutdown, and a change that is announced live can be announced again after the restart (runs coincided with a post-restart replay of failed deliveries). The issue lists candidate causes (shutdown without a final drain, ordering of intake close versus channel close, redelivered webhook deliveries) but none is confirmed. Establishing the actual cause is part of this work.

This blocks a green release gate for the webhook-bed live suite, and more broadly erodes the trust an overseer can place in the channel across a routine restart.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A label applied just before a restart is announced exactly once (Priority: P1)

An overseer subscribed to the channel sees a label change that the daemon applied shortly before a SIGHUP restart, including one applied in the final moments before the daemon closes its event pipeline. It receives that change as one announcement, either from the old process before the exec or as a downtime delta from the new process after it.

**Why this priority**: This is the "lost" half of the defect (runs 3 and 4: labels never announced) and the one that makes the channel silently incomplete.

**Independent Test**: In a sim or engine test that drives a real exec seam, apply a label so that it is queued but not yet published when the shutdown begins, perform the SIGHUP restart, and count `label-applied` events for that label across the whole restart.

**Acceptance Scenarios**:

1. **Given** a subscriber is attached and a label change has been applied to the Store but not yet published, **When** the daemon performs a SIGHUP restart, **Then** the subscriber receives exactly one announcement of that change (live before the exec or as a downtime delta after it).
2. **Given** the same state, **When** the restart completes and the subscriber re-attaches, **Then** no second announcement of that change appears from the downtime-delta comparison.

---

### User Story 2 - Labels applied during and after a restart are announced exactly once (Priority: P1)

An overseer follows an item that receives several label changes while the daemon is draining, restarting and catching up on webhook deliveries (including redelivery of deliveries that failed while the daemon was unavailable). Each change is announced once, and a redelivered or replayed delivery of an already-announced change does not produce a second announcement.

**Why this priority**: This is the "repeated" half of the defect (run 1: labels announced twice) and the "lost during the restart window" half (runs 3 and 4: labels 3 and 4 never announced).

**Independent Test**: In the same test, apply further labels during the drain and after the exec, including a replayed delivery of a change that was already applied, and count announcements per label across the restart.

**Acceptance Scenarios**:

1. **Given** labels are applied while the daemon is draining and after the exec, **When** the subscriber counts `label-applied` events per label, **Then** every label is announced exactly once.
2. **Given** a webhook delivery for an already-applied label is redelivered after the restart, **When** it is applied, **Then** it produces no additional announcement.

---

### User Story 3 - The defect is reproduced by an automated twin before the fix (Priority: P2)

A maintainer can run an automated test that exercises the real restart path without a live GitHub or a live bed, and that fails before the fix by showing both a lost and a repeated announcement, and passes after it.

**Why this priority**: The cause is unconfirmed; a deterministic reproduction is how the cause is established and how the guarantee is kept.

**Independent Test**: Run the twin against the unfixed engine (expect failures showing zero announcements for one label and two for another), then against the fixed engine (expect exactly one per label).

**Acceptance Scenarios**:

1. **Given** the unfixed engine, **When** the twin runs, **Then** it demonstrates at least one label announced zero times and at least one announced twice, attributed to a named cause.
2. **Given** the fixed engine, **When** the twin runs, **Then** every label is announced exactly once.

---

### User Story 4 - The live webhook restart test passes (Priority: P2)

The release gate's live test `TestWebhookRestartUnderLoad` passes on the webhook bed (`app/on/webhook`), consistently across repeated runs.

**Why this priority**: This is the end-to-end acceptance and unblocks the gate for that cell.

**Independent Test**: Run `TestWebhookRestartUnderLoad` on `app/on/webhook` and confirm all 6 labels are announced exactly once.

**Acceptance Scenarios**:

1. **Given** a webhook bed, **When** the test applies 2 labels before the SIGHUP and 4 during and after, **Then** the subscriber counts exactly 6 `label-applied` events, one per label.

---

### Edge Cases

- A change applied in the interval between the channel pipeline's last drain and its close (queued, recorded in the restart ledger, never published).
- A change applied while the intake is draining queued webhook deliveries before shutdown, versus one applied after the channel pipeline has closed.
- A label applied by a poll/listing before its own webhook delivery arrives (the delivery must not announce it a second time).
- A webhook delivery that failed during the restart and is replayed afterwards for a change already announced live.
- A label added and then removed (or removed and re-added) across the restart: each distinct change is announced once; the guarantee is per change, not per label name.
- A subscriber that is detached at the moment of the restart: held announcements are delivered once when it re-attaches (existing held-queue behaviour must not double-deliver).
- An unclean exit with no shutdown flush at all (crash): out of the exactly-once guarantee, but must not make duplicates worse than today.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every label add or remove that the daemon applies to its item Store is announced to attached or later-attaching channel subscribers exactly once across a SIGHUP restart: live before the exec, or as a downtime delta after it; never zero times and never twice.
- **FR-002**: A change that has been applied to the Store but not yet published when the daemon begins shutting down is not lost: it is either published before the exec or remains detectable as a change after the exec so that it is announced once.
- **FR-003**: The restart-delivery record of what has been announced is consistent with what was actually published: a change is never recorded as announced unless it was published (or is guaranteed to be announced after the restart).
- **FR-004**: A change announced live before the restart is not announced again after the restart, including when webhook deliveries are replayed or redelivered after the exec.
- **FR-005**: The root cause(s) of the lost and repeated announcements are identified and documented, backed by an automated twin (sim scenario or engine test) that drives the real exec/restart path rather than mocking it away.
- **FR-006**: The twin fails before the fix by demonstrating both a lost announcement and a repeated announcement, and passes after the fix.
- **FR-007**: The guarantee holds for the webhook-fed path (labels arriving by delivery, by listing/poll, and by the engine's own writes) and is not weakened for a poll-only bed.
- **FR-008**: `TestWebhookRestartUnderLoad` passes on `app/on/webhook`, with exactly one announcement per label for all 6 labels.
- **FR-009**: The as-built documentation for restart delivery and the SIGHUP restart describes the corrected behaviour, and the live-test registry entry and any related notes stay accurate.

### Key Entities

- **Label change**: One add or remove of a label on an item, as applied to the Store (by engine write, webhook delivery or listing), the unit that must be announced exactly once.
- **Announcement**: The `label-applied` / `label-removed` event delivered to a channel subscriber for a label change.
- **Restart ledger**: The persisted record of per-item labels/column/closed state used after a restart to detect changes made while the daemon was down (downtime deltas).
- **Held queue**: The per-subscriber queue of undelivered announcements that survives reattachment.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the automated twin, across a SIGHUP restart with labels applied before, during and after it, the count of announcements per label change equals 1 for 100% of changes, over repeated runs.
- **SC-002**: Before the fix the twin shows at least one change with count 0 and at least one with count 2; after the fix neither occurs.
- **SC-003**: `TestWebhookRestartUnderLoad` passes on `app/on/webhook` in consecutive gate runs (at least three) with no variation in the per-label announcement counts.
- **SC-004**: No existing restart-delivery or channel sim/engine tests regress.

## Assumptions

- The contract is the **count** of announcements per label change. Actor attribution (`actor=unknown` for a label applied by a listing before its delivery, ADR 2125's documented limit) is not required to change here; an announcement with an unknown actor still counts as announced.
- A downtime delta (`source=downtime-delta`, `unverified=true`) counts as a valid announcement for a change that could not be published live.
- The guarantee covers a clean SIGHUP restart (ADR 2259). The self-upgrade exec flushes the same hub; if it shares the corrected mechanism it benefits automatically, but it is not an acceptance target here.
- Crash/hard-kill behaviour is unchanged by this work beyond not making duplicates or losses worse.
- The right fix is whichever the research finds; the issue's four leads are hypotheses, not decisions.
- Run-to-run variation in the gate (four different outcomes) is explained by timing of queued-but-unpublished changes and of delivery replay relative to the shutdown, so a deterministic twin is feasible.

## Out of Scope *(optional)*

- Changing actor attribution rules (ADR 2125 limits).
- Crash/hard-kill durability of the channel pipeline.
- Redesigning the webhook intake dedupe, delivery replay, or the downtime-delta mechanism beyond what is needed for exactly-once.
- Non-label channel events (column changes, closes, settle events) except where they share the same defect and the fix naturally covers them.
- The self-upgrade exec path as an acceptance target.

## Source References *(optional)*

- Issue #2450 (this issue), `TestWebhookRestartUnderLoad` (#2427 FR-011), live-test fix #2449
- ADR 2259 (two-phase SIGHUP restart), ADR 2169 (restart delivery and downtime deltas), ADR 2351 (transport-neutral webhook intake), ADR 2144 (delivery replay), ADR 2125 (actor attribution), ADR 1966-b (channel push)
- `docs/state-machine.md` §7.14 "Restart delivery", §7.8b, §7.20
- 0.0.90 gate logs named in the Input
