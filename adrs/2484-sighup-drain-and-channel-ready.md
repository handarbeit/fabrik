# ADR 2484: The SIGHUP restart applies every accepted delivery, and the channel reports when it is ready

## Status

Accepted. Amends ADR 2450 (the final flush gains a stop gate), ADR 2259 (the restart tail's intake grace and apply), ADR 2351 (what a closing intake applies) and ADR 2169 (a readiness observable). Builds on ADR 2427-a (the health counters the live test reads).

## Context

`TestWebhookRestartUnderLoad` failed on every `app/on/webhook` run of the 0.0.90 gate after ADR 2450 removed the repeated and mis-attributed announcements. Two shapes remained: a label announced 0 times (bed A, `d6866128`) and the label queued at the intake before the SIGHUP announced once but as `actor=unknown` (bed B). Both mean the delivery the intake accepted before the signal was not applied and announced by the old process before the exec.

No bed run was available while this was written, so the cause on the bed is not confirmed. Three defects that fit the logs were found by reading the code; each has a sim twin that fails when its fix is neutralised. Whether each one is what happened on the beds is for the first bed run after this change to confirm, from the log lines it adds.

1. **The drain loses to its grace (`actor=unknown`, or a loss).** `performSighupRestart` closes the intake with `intakeCloseGrace` (5 s). The production apply is the board cache's delta followed by the Layer 1 status refresh (`applyLayer1StatusRefresh`), a live GitHub call per `issues` / `issue_comment` delivery. Deliveries of one item are applied serially, so N accepted deliveries cost N round trips. A delivery still queued when the grace expires is dropped unapplied and its ID is forgotten. The next process learns of the change from its first listing, as an `actor=unknown` downtime delta. Reproduced by `TestChannelSighupRestartSlowIntakeDrainAppliesEveryAcceptedDelivery` (the sim intake gains the same apply method and a stand-in delay for the round trip).
2. **A change in flight at stop is recorded but never published (zero announcements).** `onChange` records a change in the restart ledger (`observeLedger`) and only later appends its events to the queue. `closeChannelEvents` unsubscribes, but `Store.notify` iterates a captured observer list, so a notification already under way still calls `onChange` afterwards. If it falls between the consumer's final flush and `ledger.Close`, the ledger says the label is known and the event is never published, so the next process finds nothing to announce. Reproduced by `TestChannelSighupRestartChangeInFlightAtStopIsAnnouncedOnce`, which holds one notification between the ledger record and the queue append.
3. **A subscriber on the real socket.** The in-process sink of the ADR 2450 twin never exercises the connection being severed, `RunStream` re-attaching and the hub serving the new session. `TestChannelSighupRestartRealSocketSubscriberGetsEveryLabelOnce` does, with labels changed before the signal, while the intake drains and after the restart, and with the subscriber re-attaching before and after the daemon reports ready. It passes: no loss was found after `Publish`. It stays as a regression guard.

The held deltas are published only once the daemon is ready (ADR 2169), which on a bed was about 70 s after the new process started. The live test slept a fixed 20 s, and nothing external reported readiness.

## Decision

1. **Skip the Layer 1 refresh while the intake is closing.** `intake.Closing()` is true from `Close` only, not from the reversible self-upgrade `Quiesce` (its `Resume` keeps the process running, so deliveries applied meanwhile must keep their Layer 1 refresh). The apply (`Engine.applyDelivery`, shared by `setUpIngestion` and the sim seam) runs the cache delta always and the Layer 1 refresh only when the intake is not closing. The delta is already in the Store and the channel's final flush announces it; the refresh is a freshness check the next process repeats at its first listing.
2. **A SIGHUP grace of its own.** `intakeSighupGrace` (15 s) replaces `intakeCloseGrace` in `performSighupRestart`. The restart is operator-initiated and polling has stopped, so waiting costs nothing the daemon would otherwise do; `dropQueued` remains the safety net for a wedged apply. The tail logs the intake's accepted / applied / dropped counts, and the channel's final flush logs how many events it published.
3. **A stop gate on the channel observer.** `closeChannelEvents` sets `stopped` under `stopMu.Lock()` before closing `stop`; `onChange` holds `stopMu.RLock()` for its whole run and returns without recording if `stopped`. So a ledger record implies a queued event before the final flush, and a refused change is not in the ledger either, so the next process announces it as a downtime delta. It also covers the SIGINT and self-upgrade paths.
4. **`fabrik_health.channel.ready`.** `HealthResult.Channel` (`{"ready": bool}`, omitted with no hub) reports the consumer's readiness flag. The live test waits on it, then for every label to be announced at least once, then settles briefly and asserts exactly once. The exactly-once and `actor=human` assertions are unchanged.

## Alternatives rejected

- Reuse the self-upgrade sequence (`Quiesce(10s)` then `flushBeforeExec`): closes the same gap but leaves the slow apply, and `Close`'s drop-and-forget handling of a wedged apply is the better safety net.
- Reorder the tail so the intake stops before `closeChannelEvents`: does not cover an apply the intake deliberately leaves running past its grace.
- Wait for readiness by parsing the `[channel] restart:` log line: printed only when deltas exist, and fragile.
- Wait on `Applied` before signalling in the live test: defeats the check the test exists for.
- Change ADR 2169's "hold deltas until ready": its ordering guarantee (catch-up, then deltas) is intentional.

## Consequences

- A SIGHUP restart may take up to 15 s longer than before when deliveries are queued behind a slow apply; it takes no longer when none are.
- `fabrik_health` gains an additive `channel` block; clients that ignore unknown keys are unaffected.
- Per-Engine test seams: `SetIntakeLayer1DelayForTest`, `SetIntakeCloseGraceForTest`, `SetSighupDrainSlackDisabledForTest`, `SetChannelStopGateDisabledForTest`, `SetChannelAfterLedgerHookForTest`, `StartLocalAPIForTest`. The real-socket sim test is serial (`parallelExempt`): `localapi.Server.Start` sets a process-wide umask while it binds.
- Not observable here: the live gate (`TestWebhookRestartUnderLoad` on `app/on/webhook`). If it still fails, the bed log shows which defect is live: `intake close: gave up waiting` (1), `final flush: published N` and the ledger's contents (2), `fabrik_health.channel.ready` and the held queue under `.fabrik/state/channel/` (3).
