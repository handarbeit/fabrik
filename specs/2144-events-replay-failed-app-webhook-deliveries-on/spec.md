# Feature Specification: Replay failed App webhook deliveries on start and reconnect for direct receivers

**Feature Branch**: `fabrik/issue-2144`
**Created**: 2026-10-22
**Status**: Specified
**Input**: User description: "events: replay failed App webhook deliveries on start and reconnect for direct receivers (/app/hook/deliveries)

## Problem

A direct receiver (`event_source: direct`, #2142, and `event_source: tailscale`, #2143, which reuses the same transport) has no broker buffering deliveries. If the daemon is down or unreachable, GitHub's delivery fails and GitHub does not retry it. Reconcile (ADR-043) bounds the cost to one reconcile interval, but the event itself is gone, and after a long outage everything waits on reconcile.

GitHub keeps an App's delivery log and can redeliver on request: `GET /app/hook/deliveries` lists recent deliveries with status, `POST /app/hook/deliveries/{delivery_id}/attempts` redelivers one. Both authenticate with the App JWT, which the daemon already holds.

Mentioned for context: #2081 (the proposal this issue implements in part).

## Requirements

- **R1 — replay on start and on reconnect.** Under a direct event source, on startup and after the transport's reconnect signal (the same `SetReconnectHandler` callback that requests a catch-up reconcile; for `direct`, a listener restart), list the App's deliveries since the newest delivery the engine's persisted delivery ledger (`engine/delivery_ledger.go`, ADR 2351) recorded, bounded by the ledger's 24 h window (or a lookback of 1 h when the ledger is empty), and request redelivery of each failed one (non-2xx or no response) not already received.
- **R2 — bounded.** Page through `/app/hook/deliveries` with a hard cap on pages and on redelivery requests per pass; log what was capped. Never redeliver a delivery whose `guid`/`X-GitHub-Delivery` the delivery ledger already holds. The check is a new read-only ledger method (`Seen`), never `Accept`, which records the ID and would make the redelivery itself be dropped as a duplicate.
- **R3 — idempotent with the receiver.** Redelivered events arrive through the normal #2142 handler and the engine intake, which dedupes them on `X-GitHub-Delivery`; replay must never call the sink or `intake.Submit` itself.
- **R4 — non-fatal.** Any API error (including a 403/404 from an App that cannot read its own deliveries) is logged once per pass and degrades to reconcile-only; it never blocks startup or the poll loop.
- **R5 — opt-out.** flat key `replay_failed_deliveries: true|false` (default true for `direct`/`tailscale`, ignored for others); `--replay-failed-deliveries` / `FABRIK_REPLAY_FAILED_DELIVERIES`, classified in `internal/workerenv`.
- **R6 — observable.** The transport's per-transport `fabrik_health` row (#2142 R4) reports the last replay pass: time, deliveries found failed, redeliveries requested, capped/failed counts.

## Scope

In: the replay pass, its triggers, config, health, tests against an `httptest` GitHub stub (failed, succeeded and already-seen deliveries; pagination cap; 403 degrade), USER_GUIDE paragraph, `docs/llms-full.txt` regen, ADR `adrs/<this issue>-webhook-delivery-replay.md`.

Out: replay for Hookdeck (it buffers already) or PAT-mode `--webhooks`; any change to the delivery ledger beyond the read-only `Seen` method (it is already persisted across restarts, ADR 2351, so a lookback after a restart works).

## Acceptance

- AC1: After a simulated outage with failed deliveries in the stub's log, one replay pass requests redelivery of exactly the failed, unseen ones.
- AC2: Already-seen and successful deliveries are never redelivered.
- AC3: Page and request caps hold and are logged.
- AC4: A 403 from the deliveries endpoint degrades to reconcile-only with one log line; startup completes.
- AC5: `replay_failed_deliveries: false` performs no deliveries API calls.
- AC6: Health reports the last pass.
- AC7: Docs + regen + ADR; `go test -race ./...` green."

## Background

A direct webhook receiver (`event_source: direct` from #2142, and `event_source: tailscale` from #2143, which reuses the same transport) has no broker in front of it to buffer deliveries. When the daemon is down, restarting, or unreachable, GitHub's delivery attempt fails and GitHub does not retry it on its own. Today the only recovery is the periodic reconcile (ADR-043), which bounds the cost to one reconcile interval — but the event itself is lost, and after a long outage the engine waits on reconcile for everything it missed.

GitHub keeps a delivery log for every App webhook and supports redelivery on request: listing recent deliveries (with their status) and asking GitHub to redeliver one by ID. Both calls authenticate with the App JWT, which the daemon already holds under GitHub App auth. Replaying failed deliveries when the receiver comes back closes the gap between "event lost" and "reconcile eventually notices". This implements part of the proposal in #2081.

The engine already has the pieces that make this safe: a persisted, bounded (24 h / 10,000 entries) delivery ledger keyed on `X-GitHub-Delivery` (ADR 2351) that survives restarts and dedupes at the intake, and a transport reconnect callback that already requests a catch-up reconcile. Hookdeck is unaffected because it buffers deliveries itself.

Both #2142 (the direct transport and its per-transport health row) and the ledger/intake from ADR 2351 are prerequisites; this issue is blocked on #2142.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Recover deliveries missed while the daemon was down (Priority: P1)

An operator running a direct receiver restarts the daemon (or it was briefly unreachable). When the receiver is back up, the engine asks GitHub to redeliver every webhook that failed during the outage and that the engine never received, so the missed events are applied promptly instead of waiting for reconcile.

**Why this priority**: This is the whole point of the feature; without it a direct receiver silently loses events during any downtime.

**Independent Test**: Against an `httptest` stub of the deliveries API containing a mix of failed and succeeded deliveries, run one replay pass and verify redelivery is requested for exactly the failed deliveries the ledger does not hold.

**Acceptance Scenarios**:

1. **Given** the stub's delivery log holds failed deliveries (non-2xx or no response) that the ledger has not seen, **When** a replay pass runs at startup, **Then** a redelivery request is sent for each of them and for no others.
2. **Given** a delivery in the log succeeded (2xx), **When** a replay pass runs, **Then** no redelivery is requested for it.
3. **Given** a failed delivery whose `guid` the ledger already holds, **When** a replay pass runs, **Then** no redelivery is requested for it.
4. **Given** the transport's reconnect signal fires (for `direct`, a listener restart), **When** the handler runs, **Then** a replay pass runs in addition to the existing catch-up reconcile.

---

### User Story 2 - Replay never harms the engine (Priority: P1)

Replay is a best-effort optimisation. If the deliveries API cannot be read, or the replay misbehaves, the daemon starts and polls exactly as it would without the feature, and replayed events pass through the same receiver and intake as any other delivery.

**Why this priority**: A recovery aid that can block startup, flood GitHub, or double-apply events would be worse than the status quo.

**Independent Test**: Make the stub return 403 for the listing; verify one log line, startup completing, and zero redelivery requests. Separately, verify the replay code never invokes the sink or the intake directly.

**Acceptance Scenarios**:

1. **Given** the deliveries endpoint returns 403 or 404, **When** a replay pass runs, **Then** exactly one log line is written for that pass, no redelivery is requested, and startup and the poll loop proceed unblocked.
2. **Given** a replay requests redelivery of a delivery, **When** GitHub redelivers it, **Then** it arrives through the normal receiver handler and engine intake and is deduped there on `X-GitHub-Delivery`; the replay itself never submits an event.
3. **Given** the stub's log holds more pages or more failed deliveries than the caps allow, **When** a replay pass runs, **Then** it stops at the page cap and the redelivery cap and logs what was capped.

---

### User Story 3 - Operator control and visibility (Priority: P2)

An operator can turn replay off, and can see from `fabrik_health` what the last replay pass did.

**Why this priority**: Opt-out and observability make the feature safe to roll out and debug, but the feature delivers value without them.

**Independent Test**: With replay disabled, run startup against a stub and verify it receives no deliveries-API calls. With replay enabled and a pass completed, read the transport's health row and verify it reports the pass.

**Acceptance Scenarios**:

1. **Given** `replay_failed_deliveries: false` (or the flag/env equivalent), **When** the daemon starts or the transport reconnects, **Then** no call is made to the deliveries API.
2. **Given** the key is unset and the event source is `direct` or `tailscale`, **When** the daemon starts, **Then** replay is enabled.
3. **Given** the key is set for a non-direct source, **When** the daemon starts, **Then** it is ignored and no replay runs.
4. **Given** a replay pass has completed, **When** `fabrik_health` is read, **Then** the transport's row reports the time of the last pass, the number of deliveries found failed, the number of redeliveries requested, and the capped and failed counts.

---

### Edge Cases

- **Empty ledger** (first run, or a ledger file lost or corrupt): the lookback is 1 hour instead of "since the newest recorded delivery".
- **Ledger older than its window**: the lookback never exceeds the ledger's 24 h window.
- **Original failed, redelivery succeeded**: GitHub's log shows the original and the redelivery as separate entries sharing one `guid`; a `guid` that has a successful attempt or that the ledger holds is not redelivered again.
- **Repeated reconnects in quick succession**: a delivery already requested for redelivery in this process whose redelivered copy has not yet reached the ledger is not requested again within the same ledger window.
- **Redelivery request itself fails** (network error, 4xx/5xx for one delivery): counted as failed in the pass summary; remaining deliveries are still attempted, subject to the caps.
- **Deliveries older than the lookback** that failed are left to reconcile.
- **Token/JWT unavailable** (for example not running under GitHub App auth): treated like any other API error — one log line, degrade to reconcile-only.
- **GitHub rate limiting** on the listing or redelivery calls: treated as an API error for that pass; the pass ends and degrades to reconcile-only.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Under a direct event source (`event_source: direct` or `tailscale`) with replay enabled, the engine runs a replay pass at startup.
- **FR-002**: The engine runs a replay pass after each transport reconnect signal (the same callback that requests a catch-up reconcile; for `direct`, a listener restart).
- **FR-003**: A replay pass lists the App's deliveries since the newest delivery the persisted delivery ledger recorded, bounded by the ledger's 24-hour window, using a 1-hour lookback when the ledger is empty.
- **FR-004**: A replay pass requests redelivery of each listed delivery that failed (non-2xx status or no response) and that the engine has not received.
- **FR-005**: A replay pass never requests redelivery of a delivery whose `guid`/`X-GitHub-Delivery` the delivery ledger already holds, and never requests redelivery of a delivery that succeeded (including a `guid` that has a successful redelivery attempt).
- **FR-006**: The ledger check uses a new read-only ledger method (`Seen`) that does not record the ID; it never uses `Accept`. The ledger gains no other behaviour change beyond read-only accessors needed to determine the newest recorded delivery time.
- **FR-007**: A replay pass pages through the deliveries listing with a hard cap on pages and a hard cap on redelivery requests per pass, and logs what was capped.
- **FR-008**: Replay never calls the transport's sink or the engine intake's submit itself; redelivered events reach the engine only through the normal receiver handler and intake, which dedupes them on `X-GitHub-Delivery`.
- **FR-009**: Any API error during a pass (including 403/404 from an App that cannot read its own deliveries, and rate limits) is logged once for that pass and degrades the engine to reconcile-only; it never blocks startup or the poll loop, and the replay does not run on the startup critical path.
- **FR-010**: A failure of an individual redelivery request is counted and logged in the pass summary and does not abort the remaining requests.
- **FR-011**: A new flat configuration key `replay_failed_deliveries: true|false` controls replay; it defaults to true for `direct` and `tailscale` event sources and is ignored for other sources.
- **FR-012**: The setting is also available as the flag `--replay-failed-deliveries` and the environment variable `FABRIK_REPLAY_FAILED_DELIVERIES`, and the variable is classified in `internal/workerenv`.
- **FR-013**: With replay disabled, no deliveries-API call is made.
- **FR-014**: The transport's per-transport `fabrik_health` row (from #2142) reports the last replay pass: its time, the number of deliveries found failed, the number of redeliveries requested, and the capped and failed counts.
- **FR-015**: Replay does not apply to Hookdeck or to PAT-mode `--webhooks`.
- **FR-016**: Tests run against an `httptest` GitHub stub covering failed, succeeded and already-seen deliveries, the page and request caps, and the 403 degrade; `go test -race ./...` passes.
- **FR-017**: A paragraph in `docs/USER_GUIDE.md` describes the feature and its opt-out, `docs/llms-full.txt` is regenerated, and an ADR `adrs/2144-webhook-delivery-replay.md` records the decision.

### Key Entities *(if applicable)*

- **App webhook delivery**: one entry in GitHub's per-App delivery log, with a delivery ID, a `guid` (the `X-GitHub-Delivery` value), a status/status code, a delivery time, and a flag marking it as a redelivery.
- **Delivery ledger**: the engine's persisted, bounded record of accepted `X-GitHub-Delivery` IDs (ADR 2351), used here read-only to decide what was already received.
- **Replay pass**: one bounded run of list → filter → request-redelivery, with a summary (found failed, requested, capped, failed) reported to health.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a simulated outage, a single replay pass requests redelivery of exactly the set of failed, not-yet-received deliveries in the stub's log — no more and no fewer.
- **SC-002**: Zero redelivery requests are made for succeeded deliveries or for deliveries the ledger already holds.
- **SC-003**: No pass exceeds the page cap or the redelivery-request cap, and each capped pass produces a log line stating what was capped.
- **SC-004**: When the deliveries endpoint returns 403, startup completes, one log line is produced for the pass, and no further deliveries-API calls are made in that pass.
- **SC-005**: With replay disabled, the stub observes zero deliveries-API calls over a full start/reconnect cycle.
- **SC-006**: After a pass, `fabrik_health` for the transport shows that pass's time and counts.
- **SC-007**: A replayed event that the engine had in fact already received is applied exactly once.

## Assumptions

- The default caps are fixed constants chosen at Plan time (on the order of ten listing pages and a few dozen redelivery requests per pass), not user-configurable in this issue; the caps and their values are logged and documented.
- The listing's per-delivery status is sufficient to classify "failed" (non-2xx or no response); no per-delivery detail fetch is needed.
- The delivery ledger records receipt time, so "newest recorded delivery" is the ledger's latest receipt time; reading it needs a small read-only accessor alongside `Seen`, which is consistent with the "no ledger change beyond read-only methods" scope.
- An App that is not authenticating with a GitHub App JWT (no App auth configured) cannot replay; this degrades like any other API error.
- Replay runs in the background after the receiver is listening, so redelivered events can be accepted; it does not delay startup.
- The in-process memory of already-requested `guid`s is not persisted; after a restart, the ledger and the lookback bound are the only memory.
- #2142 supplies the direct transport, its reconnect callback and its health row; #2143 reuses that transport, so no separate handling for `tailscale` is required.

## Out of Scope *(optional)*

- Replay for Hookdeck (it buffers deliveries itself) and for PAT-mode `--webhooks`.
- Any change to the delivery ledger beyond read-only accessors (it is already persisted across restarts per ADR 2351).
- Replay of deliveries older than the lookback window, which remain reconcile's job.
- User-configurable caps or lookback.
- Scheduled/periodic replay independent of start and reconnect.

## Source References *(optional)*

- #2081 — the proposal this issue implements in part
- #2142 — direct transport and per-transport `fabrik_health` row (blocking dependency)
- #2143 — Tailscale transport (reuses the direct transport)
- ADR 2351 — transport-neutral intake and persisted delivery ledger; ADR-043 — reconcile
- GitHub REST API: "List deliveries for an app webhook" (`GET /app/hook/deliveries`) and "Redeliver a delivery for an app webhook" (`POST /app/hook/deliveries/{delivery_id}/attempts`)
