# Feature Specification: Label changes around a SIGHUP restart reach the channel subscriber exactly once

**Feature Branch**: `fabrik/issue-2484`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description (the original issue body, verbatim):

````
## Problem

`TestWebhookRestartUnderLoad` (`tests/e2e/webhook_recovery_test.go`) has failed on every `app/on/webhook` run of the 0.0.90 gate: five runs, at `ad9b801b`, `2e370066`, `624e0e56` (twice) and `d6866128`. #2450 (ADR 2450) removed two of the failure shapes:
- a label announced twice;
- a pre-SIGHUP label attributed `actor=unknown`.

A third shape remains, and it appeared before #2450 as well. The test's channel subscriber receives **no event at all**, of any `origin`, for some of the labels. Label 1 is announced once, as expected.

Latest run, engine `d6866128`, bed A, invocation `20261026T045615Z-e31de8`:

```
label e2e-webhook-load-2 was announced 0 times across the restart, want exactly 1
label e2e-webhook-load-3 … -6: announced 0 times
```

### Timeline from the bed's `.fabrik/fabrik.log{,.1}`

- **Up to 15:44:37Z:** labels 1 and 2 are added with polls held. The intake accepted both before the signal (the test waits for that).
- **15:44:37Z:** `[signal] received SIGHUP — draining …`, then `no workers in flight`. The process restarts about 1 s later.
- **Around the restart:** labels 3 to 6 are added, 2 s apart, after the signal. They reach the daemon by replay or reconcile. The test's `awaitDaemonLabel` passes for all six.
- **15:44:39Z:** new process (`reason=SIGHUP restart`).
- **15:45:49Z:** `[channel] restart: 6 change(s) made while the daemon was down delivered`.

The subscriber (`startLabelCollector`, `RunStream`, `events: [label-applied, label-removed]`, `exclude_labels: []`) had about 10 more seconds, and saw nothing for labels 2 to 6. `allLabelEvents` counts every origin.

**Label 2 is lost in every 0-times run.** It was accepted and applied before the SIGHUP, so its announcement depends on the #2450 final flush (`channelEvents.flushQueue` on stop) and on the hub's held queue surviving the exec. Labels 3 to 6 depend on the downtime-delta path reaching a subscriber whose stream reconnects after the restart.

## Requirements

1. **Find where the events are lost.** Determine where label 2's live event and labels 3 to 6's downtime-delta events are lost: not published, published to a held queue that is not persisted before the exec, or not delivered to the re-attached session. Start with instrumentation on a bed, or a sim reproduction that drives a real SIGHUP exec (`PerformSighupRestartForTest`), with the subscriber attached through the real socket.
2. **Deliver exactly one event per label.** Fix it so that each label change made around a SIGHUP restart reaches an attached subscriber exactly once, whatever the origin.
3. **Make the test's waits match the mechanism.** If the deltas are delivered correctly but after the test stops looking, the test's waits must follow the mechanism: wait on the daemon's channel readiness rather than a fixed 20 s. Never relax the exactly-once assertion.

## Acceptance

- `TestWebhookRestartUnderLoad` passes on `app/on/webhook`.
- A sim or engine test reproduces the lost event, and fails with the fix neutralised.

Mentioned for context: #2450, #2427.
````

## Background

An operator or overseer session that subscribes to the daemon's channel push relies on it to learn about label changes, including ones made while the daemon restarts (a SIGHUP restart, which drains and re-execs the process). The channel's design promise is that a label change is announced once, whether the daemon saw it live or reconstructs it afterwards as a "downtime delta" from its persisted ledger.

The live e2e test `TestWebhookRestartUnderLoad` checks that promise on the `app/on/webhook` leg: it adds six labels around a SIGHUP restart, with a channel subscriber attached, and requires each label to be announced exactly once, with the correct attribution. It has failed on every `app/on/webhook` run of the 0.0.90 release gate (five runs, engine SHAs `ad9b801b`, `2e370066`, `624e0e56` twice, `d6866128`), so the release gate cannot be accepted.

Earlier work (#2450, ADR 2450) removed two failure shapes: a label announced twice, and a pre-SIGHUP label attributed `actor=unknown`. Two shapes remain, and both come down to one fact. The change the intake accepted before the SIGHUP (label 2) is not applied and announced by the old process before the exec. Sometimes it then arrives as a downtime delta (one announcement, `actor=unknown`; seen on bed B, engine `d6866128`). Sometimes it never reaches the subscriber at all (zero announcements; seen on bed A, engine `d6866128`, invocation `20261026T045615Z-e31de8`).

Observed on bed A:

- Labels 1 and 2 were added with polls held, and the intake accepted both before the signal. Label 1 was announced once, as expected.
- At 15:44:37Z the daemon logged `received SIGHUP — draining`, then `no workers in flight`. The process restarted about 1 s later.
- Labels 3 to 6 were added, 2 s apart, after the signal. The daemon learned of them by replay or reconcile.
- The new process started at 15:44:39Z, and at 15:45:49Z logged `[channel] restart: 6 change(s) made while the daemon was down delivered`.
- The subscriber had about 10 more seconds and saw nothing for labels 2 to 6. Its filter is label events only, with the default label excludes turned off, and the test counts events of every origin.

Label 2 is lost in every zero-announcement run. Its announcement depends on the final flush of queued events when the old process stops and on the held queue surviving the exec. Labels 3 to 6 depend on the downtime-delta path reaching a subscriber whose stream reconnects after the restart. Where exactly these events are lost (never published, held but not persisted before the exec, or not delivered to the re-attached session) is not yet known and is the first thing to establish.

Related, for context: #2450, #2427.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A subscriber sees every label change made around a restart exactly once (Priority: P1)

An operator, or an overseer session, has a channel subscription for label events and the daemon is restarted with SIGHUP while labels are being added. Every label change reaches the subscriber exactly once, with the origin and attribution it would have had without the restart.

**Why this priority**: This is the defect. Silent loss of an announcement means the subscriber's picture of the board is wrong and nothing tells them.

**Independent Test**: Reproduce the 0-announcement outcome deterministically without a live bed, with a daemon exec driven through the SIGHUP restart path and the subscriber attached over the real local-API socket. Add labels before, during and after the restart, then check the subscriber's received events label by label.

**Acceptance Scenarios**:

1. **Given** an attached subscriber and a label change the intake accepted before the SIGHUP signal, **When** the daemon restarts, **Then** the subscriber receives exactly one event for that label, whether the old process announces it live or the new process announces it as a downtime delta.
2. **Given** an attached subscriber and label changes made while the daemon is draining or down, **When** the new process becomes ready and the subscriber's stream has re-attached, **Then** the subscriber receives exactly one event for each such label, whatever its origin.
3. **Given** a label change the intake accepted before the SIGHUP, **When** the restart completes, **Then** the event is attributed to the actor that made the change and not `unknown` (the pre-SIGHUP drain applied it).
4. **Given** the fix is neutralised (the behaviour that was lost is put back), **When** the new sim or engine test runs, **Then** it fails.

---

### User Story 2 - The live gate test waits for the mechanism, not a fixed time (Priority: P2)

`TestWebhookRestartUnderLoad` passes on `app/on/webhook` and its waiting logic follows the daemon's own readiness signal, so a correct but slow delivery is not a false failure, and a wrong one is still a failure.

**Why this priority**: Without a passing live test the 0.0.90 release gate cannot be accepted. It depends on Story 1, and only applies if the investigation finds delivery is correct but late.

**Independent Test**: Run the live test on an `app/on/webhook` bed.

**Acceptance Scenarios**:

1. **Given** deltas are delivered correctly but only after the daemon's channel becomes ready, **When** the test waits, **Then** it waits on that readiness and not on a fixed 20 s, and its exactly-once assertion is unchanged.
2. **Given** a label is announced zero times or twice, **When** the test finishes, **Then** it still fails.

---

### Edge Cases

- A label added after the SIGHUP signal but before the exec (during the drain).
- A label added after the new process starts but before the subscriber's stream re-attaches.
- A subscriber that re-attaches later than the daemon's readiness signal.
- Labels added about 2 s apart across the restart boundary, as in the live test, so several arrive by different paths (live apply, replay, reconcile).
- A change both announced live by the old process and also visible to the new process's downtime diff (must not be announced twice; this is #2450's guarantee and must not regress).
- A label removed rather than added around the restart.
- A restart with no workers in flight (as on bed A), where the exec follows the signal by about 1 s: the pre-exec drain must still complete before the exec.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The loss of label 2's live event (a change accepted by the intake before the SIGHUP) and of labels 3 to 6's downtime-delta events must be located and the finding recorded (in the PR and the relevant ADR/as-built doc): never published, published to a held queue not persisted before the exec, not applied by the pre-exec drain, or not delivered to the re-attached session.
- **FR-002**: Every label change the intake accepted before the SIGHUP signal must be applied and announced by the old process before the exec, or else carried so that the new process announces it exactly once (as a downtime delta) with the correct attribution.
- **FR-003**: Every label change made while the daemon is draining or down must reach an attached subscriber exactly once after the restart, whatever its origin (`live`, `catch_up`, `downtime-delta`).
- **FR-004**: A subscriber whose stream re-attaches after the restart, including after the daemon has already reported ready, must receive any held events it has not yet been sent, once.
- **FR-005**: The fix must preserve the existing guarantees: no label announced twice (#2450), and a pre-SIGHUP change not attributed `actor=unknown` when the old process had applied it.
- **FR-006**: A new sim or engine test must reproduce the lost event by driving a real SIGHUP restart (the restart path used by `PerformSighupRestartForTest`) with a subscriber attached through the real socket, and must fail when the fix is neutralised (via a per-Engine neutralisation seam, per the repository convention).
- **FR-007**: If the investigation finds delivery is correct but later than the live test looks, `TestWebhookRestartUnderLoad` must wait on the daemon's channel readiness instead of a fixed 20 s; its exactly-once assertion must not be relaxed in any case.
- **FR-008**: The as-built documentation for restart delivery (`docs/state-machine.md` §7.14 "Restart delivery") must be updated for the fixed behaviour, and `docs/llms-full.txt` regenerated in the same change.

### Key Entities *(if applicable)*

- **Channel subscriber**: A session attached to the daemon's channel push over the local-API socket, with a subscription (events, label filters) persisted across restarts.
- **Held queue**: The per-subscriber queue of events published but not yet written to an attached session; persisted so it can survive a restart.
- **Downtime delta**: An event the new process reconstructs by diffing the first observation of each item after a start against the persisted ledger.
- **Pre-exec drain**: The step on the SIGHUP path in which deliveries the intake had already accepted are applied and queued channel events are published before the process re-execs.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `TestWebhookRestartUnderLoad` passes on the `app/on/webhook` leg, with each of the six labels announced exactly once.
- **SC-002**: A deterministic sim or engine test reproduces the loss of a pre-SIGHUP label's announcement and passes with the fix; with the fix neutralised the same test fails.
- **SC-003**: Over repeated runs of the new sim or engine test with labels added before, during and after the restart, every label produces exactly one event at an attached subscriber: no zeros and no duplicates.
- **SC-004**: No regression in the existing restart-delivery tests for #2450 (announced-once, correct attribution).

## Assumptions

- The defect is in the engine's restart or channel path (final flush, held-queue persistence, pre-exec intake drain, downtime-delta delivery or stream re-attach), not only in the test; the investigation (FR-001) confirms or refutes this, and FR-007 covers the test-only outcome.
- Both remaining failure shapes (zero announcements, and one announcement attributed `actor=unknown`) share one cause: the pre-SIGHUP delivery for label 2 is not applied or not announced by the old process before the exec. The investigation therefore starts with the pre-exec intake drain on the SIGHUP path, then checks the remaining candidates in FR-001.
- A sim test can reproduce the loss by driving a real restart with a real-socket subscriber; if it cannot reproduce a facet, an engine-package test or bed instrumentation is acceptable provided the acceptance test still fails with the fix neutralised.
- "Exactly once" is measured at an attached subscriber across the restart, counting events of every origin.
- A subscriber that is not attached receives its held events on the next attach, as today.

## Out of Scope *(optional)*

- Changes to event types, subscription semantics or attribution rules beyond what is needed for exactly-once delivery across a restart.
- Restarts other than SIGHUP (self-upgrade exec, crash recovery) unless the same root cause is found there; if it is, note it as a follow-up.
- Relaxing the exactly-once assertion of the live test.
- The webhook transport and delivery-replay behaviour in general (#2142, #2144, #2351), except where it determines whether the pre-SIGHUP change is applied before the exec.

## Source References *(optional)*

- #2450 / ADR 2450: announce-once across a SIGHUP restart (final flush, live-delta folding).
- #2427: live webhook coverage tests.
- `docs/state-machine.md` §7.14 "Restart delivery"; `tests/e2e/webhook_recovery_test.go` (`TestWebhookRestartUnderLoad`).
- Prior discussion on this issue (2026-10-26): a run on bed B (engine `d6866128`) announced each label once but attributed label 2 as `actor="unknown"` ("the pre-exec drain did not apply it"), the same root cause as the zero-announcement shape on bed A; the suggested starting point is the pre-exec intake drain on the SIGHUP path.
