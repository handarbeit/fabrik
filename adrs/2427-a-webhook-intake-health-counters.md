# ADR 2427-a: Live webhook tests observe the daemon through intake counters in `fabrik_health`, a channel subscriber and (for one warning) the warnings file

## Status

Accepted (#2427). Builds on ADR 2351 (the transport-neutral intake), ADR 2352 (webhook facts), ADR 1995 (`issue_dependencies`), ADR 1978 (the poll hold/trigger seam), ADR 2426 (the `events` axis) and ADR 2408 (the bed transport). Companion: ADR 2427-b (the test-faults env).

## Context

#2427 adds eleven live tests that must fail when webhook ingestion regresses, each asserting through the daemon's local API rather than GitHub. The behaviours they guard are mostly invisible there: whether a delivery was **applied**, rejected as a **duplicate** or matched as an engine **echo** is a log line or an in-memory decision; the **actor** of a label change exists only on channel events; the `tailscale_webhook_config` warning is a file. Parsing bed logs per test is what the gate's events summary does for a leg-level count, and it is a poor per-test assertion (rotation, ordering, no event identity).

## Decisions

1. **`fabrik_health.webhook.intake` reports the intake's counters since process start**: `accepted`, `duplicates`, `dropped`, `echo_matched` and `applied` (a count per GitHub event type, incremented after the apply function returns). They are mutex-guarded, in memory only and reset by a restart. The block is omitted when the daemon has no intake (poll-only). A test reads the counters before and after the act and compares **deltas**; no test depends on an absolute value. The counters are written only from the intake (`Submit`, `run`) and the echo registry (`MatchEchoVia`), never from a poll, so with polls held an `applied` increment can only come from a delivery — that is what makes "the webhook alone did this" structural. `engine/localapi.go` reads them through `Engine.intakeHealth()`, which touches no GitHub, so the local-API source scan is unchanged.

2. **Attribution is read from a channel subscriber, not added to `fabrik_status`.** The actor of a label change (`human | fabrik | overseer | bot | unknown`) is channel-event metadata by design (ADR 1966-b, ADR 2125). A test subscribes under a unique name to `label-applied`/`label-removed` with an explicit empty `exclude_labels` (the default family and the default exclusions hide both), attaches `localapi.RunStream`, filters out catch-up and downtime-delta origins when it wants the live event, and **unsubscribes in cleanup** because subscriptions persist in the bed's hub. This adds no engine surface.

3. **Documented exception: the `tailscale_webhook_config` warning is read from `<bed>/.fabrik/warnings.json`.** The warnings panel has no local API surface. A health field for one test is not worth the surface; the file is the panel's own store and the entry's fingerprint is stable (`tailscale_webhook_config:url`).

4. **Spec points that cannot hold literally, and the proxy used for each:**
   - *FR-003 "resumes with polls held".* A held bed runs no cycle, so nothing can resume an item while polls are held; the delivery's wake-up is itself dropped. The test proves the comment arrived by delivery with the `issue_comment` counter (nothing but a delivery increments it) and then triggers one whole poll, which must lift the pause.
   - *FR-004 "no extra wake".* Not observable. The test asserts the echo match counter, that the label's event is attributed to the engine's actor (overseer, via the requesting subscriber), and that exactly one event exists for the write.
   - *FR-005 "review comment edit updates the feedback view".* The edited text is on no API surface; the edit is guarded by the `pull_request_review_comment` counter, the thread resolution by the reviewer's `outstanding` in `fabrik_status`.
   - *FR-007 with polls held.* `fabrik:blocked` is removed only by a poll's live read. The delivery is proven by the `issue_dependencies` counter and the unblock by one triggered poll.
   - *FR-009 "before its first reconcile would have".* Unprovable: the startup poll precedes the tailscale replay's 90 s grace. The replay is proven by its own health row (`found_failed`, `requested` at least 1), the change by its presence, and "not twice" by exactly one label event.
   - *FR-011 "applied none twice".* There is no per-label apply counter; exactly one `label-applied` event per label across the restart (any origin) is the proxy.

5. **Tests identify their own delivery by payload, never by position** in the App's delivery log (`findAppDelivery`): a shared App's log carries other beds' and tests' deliveries.

6. **FR-010 edits a shared App's webhook URL** (`PATCH /app/hook/config`, the url only; the secret is never sent or decoded). The original URL is written to `<bed>/.fabrik/state/e2e-hook-url.restore` before the PATCH and restored in `t.Cleanup`; every webhook test restores from a stale marker first, which repairs a run killed between the two. A restore failure is reported loudly (`t.Errorf`). The test is `exclusive`.

## Consequences

- A new read-only block in `fabrik_health` (documented in `docs/state-machine.md` §7.8b) and one more field on `localapi.WebhookHealth`.
- The tests carry no new secrets: App credentials are the bed's existing `E2E_APP_*`.
- The counters are a daemon surface other tools can read, but nothing in the engine reads them back.
