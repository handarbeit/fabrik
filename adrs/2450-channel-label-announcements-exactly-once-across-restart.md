# ADR 2450: Channel label announcements are exactly-once across a SIGHUP restart

## Status

Accepted. Amends ADR 2169 (the ledger records at observe time) and ADR 2259 (the restart tail). Builds on ADR 2123, ADR 2125, ADR 2351 and ADR 2144.

## Context

In the 0.0.90 gate `TestWebhookRestartUnderLoad` applied 6 labels around a SIGHUP restart; the daemon held all 6 every time, but a channel subscriber counted them zero, once or twice depending on the run.

Two causes, reproduced by `tests/sim/channel_sighup_restart_test.go`, which drives the real restart tail through `PerformSighupRestartForTest` and rebuilds the engine with `RestartEnv`:

1. **Lost.** `channelEvents.onChange` records a label change in the restart ledger and queues its event; the consumer publishes it on its next drain. `performSighupRestart` closes the intake (which applies the last queued deliveries) and then `closeChannelEvents`, whose consumer returned on `stop` without a final drain. A change queued in that window was already in the ledger, so the next process saw no difference and nothing was ever announced. The self-upgrade exec (`flushChannelEvents`) had the same hole.
2. **Repeated.** After the exec an item's labels are unknown until the first listing. A webhook `IssueLabeled`/`IssueUnlabeled` (a replayed delivery included) still produces a label delta, which is announced live, while the item's prior label diff stays pending (`downtimeCheck` diffs only once `LabelsKnown`). When the labels became known the diff announced the same change again as a downtime delta. A replay of a change the previous process had announced was also announced again.

## Decision

1. **Publish on stop.** `channelEvents.flushQueue` publishes the queued events and nothing else (no readiness pass, evaluation or catch-up: a process that is going away starts no new work). The consumer calls it on `stop`; `flushChannelEvents` calls it before the self-upgrade exec. It is ordered against `drain` by `pubMu` (taken before `mu`), and events for an item with a held downtime delta still go through `holdWarm`, so `spillWarm` keeps the delta-then-live order. The ledger keeps recording at observe time: after a clean restart observed now implies published (or in the hub's persisted queue).
2. **Fold live label deltas into the pending prior.** `noteLiveLabelDeltas` applies each live delta to the item's pending prior (in order, per change - an add, remove and re-add nets out correctly), and the live path skips a delta that the diff built in the same call already covers and one that leaves the prior unchanged (a replay of what the previous process recorded and announced). The later diff then announces only the net difference.

## Alternatives rejected

- Record in the ledger only what has been published (so unpublished changes become deltas after the exec): reopens the crash-while-warming problem the `unpub` hold solves and needs a second bookkeeping layer.
- Flush in `closeChannelEvents` after `wg.Wait()`: publishes from a second goroutine outside `pubMu` ordering.
- A "announced live" set keyed by label name: hides a genuine downtime change on the same label.
- Changing the store's label baseline rule (ADR 2123): a webhook label event on an unknown-label item is a real change and must still be announced.

## Consequences

- Actor attribution is unchanged (`actor=unknown` for a listing that applies a label before its delivery, ADR 2125); the contract is the count.
- Crash and hard-kill behaviour is unchanged.
- Per-Engine test seams: `SetFinalChannelFlushDisabledForTest`, `SetLiveLabelDeltaDedupDisabledForTest`, `SetChannelConsumerParkedForTest`, `SetSighupExecForTest`, `PerformSighupRestartForTest`. With the first two the sim twin shows a label announced 0 times and 2 times; without them every label is announced once.
- The live gate (`TestWebhookRestartUnderLoad` on `app/on/webhook`, three consecutive passes) cannot be observed in this repository's CI.
