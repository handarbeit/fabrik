# ADR 2144: Webhook delivery replay for direct receivers

**Date**: 2026-10-22
**Status**: Accepted
**Issue**: #2144 — events: replay failed App webhook deliveries on start and reconnect for direct receivers
**Builds on**: ADR 2142 (direct App webhook transport), ADR 2351 (transport-neutral intake and delivery ledger), ADR 043 (reconcile ticker), ADR 2348 (observed-at ordering)
**Amends**: ADR 2142 decision 2/consequences (deliveries sent while the daemon is down are no longer simply lost)
**Chain**: #2081 → #2142 → #2143 (reuses the transport) → this.

## Context

A direct receiver has no broker buffering deliveries. If the daemon is down or unreachable, GitHub's delivery fails and GitHub does not retry it; the reconcile ticker bounds the cost to one interval, but the event itself is gone. GitHub keeps the App's delivery log and redelivers on request (`GET /app/hook/deliveries`, `POST /app/hook/deliveries/{id}/attempts`, both App-JWT authenticated, which the daemon already holds).

## Decision

1. **A replay pass** (`engine/delivery_replay.go`) lists the App's deliveries, picks the failed ones the engine never received, and asks GitHub to redeliver them. It runs once at startup, in a background goroutine started after the listener is up (it never delays startup, and redeliveries need a listening receiver), and again when the transport's reconnect callback fires. The direct transport never calls that callback (ADR 2142: nothing to reconnect, a failed bind is retried only by a restart), so for `direct` startup is the real trigger; `attachTransportErr` installs a composite handler (reconcile request + replay trigger) so a transport that does signal a reconnect (#2143's `tsnet`) inherits the trigger with no engine change. A new periodic listener re-check was rejected as scope creep that contradicts ADR 2142's "no retry loop".
2. **Replay never applies anything.** It calls the two API endpoints and the ledger's read-only `Seen`; it never touches the transport sink or `intake.Submit` (`TestDeliveryReplayNeverSubmits`). A redelivered copy carries the original `X-GitHub-Delivery`, re-enters through the normal handler and intake and is deduplicated there, so an event that had in fact arrived is applied once.
3. **`Seen` and `Newest` are the only ledger additions, and both are read-only.** `Accept` records the ID, so using it for the check would make the redelivery itself look like a duplicate. `Seen` does not even prune. `Newest` (the latest receipt time) gives the lower bound.
4. **Lower bound** = `max(newest ledger receipt − 5 min, now − 24 h)`, or `now − 1 h` when the ledger is empty. The 5-minute margin covers skew between the ledger's local clock and GitHub's `delivered_at`. Known limitation: a failed delivery older than the newest ledger entry (a failure followed by later successes, then a pass) is left to reconcile; widening to the full 24 h would cost pages for little gain, and the persisted ledger is what makes the bound useful across a restart.
5. **Classification is client-side.** Failed = `status_code` outside 200–299, including 0 (no response) and 3xx; the API's `status=failure` filter covers only 400–599. Entries are grouped by `guid`: any 2xx attempt (a successful redelivery included) means delivered. `ping` is never redelivered (acknowledged, never applied). The newest failed attempt's delivery id is the one redelivered.
6. **Bounded.** Fixed constants, not configuration (variables only so tests can shrink them): 10 pages × 100 listed, 50 redeliveries per pass, ~250 ms between requests. A 422 (GitHub's spam guard), 401/403/404 or a rate limit ends the pass at once; three consecutive other failures also end it. Capped passes log what was left and report outcome `capped`.
7. **Non-fatal.** Any API error is logged once for the pass and degrades to reconcile-only, with outcome `degraded: …` on the health row. No `warnings` panel entry: the spec asks for a log line, the row is visible in `fabrik_health`, and the endpoints need no extra App permission, so this is rare. A Reconciler without credentials (`NewWithDeps` engines) returns an error before any network call.
8. **In-memory request memory.** A `guid` requested in this process is not requested again within the ledger window (covers quick repeated triggers). A request that itself failed is forgotten so a later pass can retry. A redelivery that is requested and then fails again at the daemon stays unseen but is not re-requested until a restart; a short TTL could hammer a daemon that is rejecting deliveries (for example on signature drift). Not persisted: after a restart the ledger and the lookback are the only memory.
9. **Config**: flat `replay_failed_deliveries` (`*bool` in YAML, default true), `--replay-failed-deliveries`, `FABRIK_REPLAY_FAILED_DELIVERIES` (flag > env > YAML), classified `tune` in `internal/workerenv`. It is ignored for sources other than `direct`/`tailscale` (the replayer is only built by the direct start path), silently. `engine.Config.ReplayFailedDeliveries` defaults to off so `NewWithDeps` engines never replay.
10. **Health**: `localapi.TransportHealth.Replay` (`last_pass_at`, `found_failed`, `requested`, `capped`, `failed`, `outcome`) is additive, present with unknown values before the first pass and omitted for transports without replay. The replayer reports through an optional `replayStatusSink` implemented by `appWebhookTransport`, so `HealthRow()` stays the single source of the row.
11. **JWT access** is two narrow methods on `githubauth.Reconciler` (`ListAppHookDeliveries`, `RedeliverAppHookDelivery`) in the `VerifyGrants` style; a fresh JWT is minted per call. `github/app.go` gains a headers-returning `appDo` (cursor pagination reads the `Link` header; a missing or malformed header ends paging).

## Consequences

- A restart or outage of a direct receiver no longer loses events until the next reconcile: they are redelivered within one pass.
- Redelivery of a stale event is ordered by the observed-at rule (ADR 2348), whose receipt-time fallback is the redelivery's arrival.
- Ledger eviction at 10,000 entries can make an applied delivery look unseen, costing one redundant redelivery that the (idempotent) delta handlers absorb.
- GitHub's retention of the delivery log is not documented; anything beyond it is reconcile's job.
- Only `direct` is wired and tested here; `tailscale` (#2143) must create its replayer through `newDeliveryReplayer` the same way.

## Verification

`engine/delivery_replay_test.go` (pass logic against a fake delivery-log API: failed/succeeded/seen/ping/redelivered mix, lookback, page and request caps, 403/404/rate-limit/422 degrade, trigger coalescing), `engine/delivery_replay_wire_test.go` (real listener, intake and ledger: the redelivery path, SC-007, the health row, replay-off makes no API calls, non-direct sources ignore the setting), `github/app_test.go`, `internal/githubauth/hookdeliveries_test.go`, `engine/delivery_ledger_test.go` and `cmd/cmd_envvar_test.go`. There is no sim twin: the sim bed has no transport layer.
