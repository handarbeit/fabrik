# Feature Specification: Direct App webhook transport (`event_source: direct`)

**Feature Branch**: `fabrik/issue-2142`
**Created**: 2026-10-21
**Status**: Specified
**Input**: User description: "## Problem

Under GitHub App auth the only real-time event path is Hookdeck (`event_source: hookdeck`, ADR-1142/ADR-1254); `--webhooks` is refused (ADR-1752). Every App-mode daemon therefore depends on a third-party SaaS account that every payload passes through, plus one more API key and secret per daemon. With one daemon per App, that cost multiplies.

An operator who can already expose a local port publicly (Tailscale Funnel via the system `tailscaled`, Cloudflare Tunnel, a reverse proxy) has no way to point the App's webhook URL straight at the daemon.

Mentioned for context: #2081 (the proposal this chain implements). This issue is the first of a chain: #2143 (embedded `tsnet`) reuses this transport with a different listener, and #2144 adds delivery replay."

## Background

Under GitHub App authentication, the only real-time event path today is Hookdeck. That means every App-mode daemon depends on a third-party SaaS account that sees every payload, and needs one more API key and secret per daemon. Plain `--webhooks` (gh-forward) is refused under App auth (ADR-1752). With one daemon per App the cost multiplies.

Operators who can already expose a local port publicly (Tailscale Funnel through the system `tailscaled`, Cloudflare Tunnel, a reverse proxy) have no way to point the App's webhook URL straight at the daemon.

This issue is the first of a chain implementing proposal #2081. #2143 (embedded `tsnet`) will reuse this transport with a different listener. #2144 adds delivery replay.

Webhook intake is already transport-neutral (ADR 2351). A transport verifies its own signature and hands a delivery (ID, event type, payload) to the engine's intake. The engine already provides:

- the persisted `X-GitHub-Delivery` dedupe ledger;
- the fast ack (the intake only queues, so a handler acks before any network work);
- echo attribution through the engine-level echo registry;
- reconnect catch-up through the reconcile request;
- the 25 MiB body cap, answered with 413 before signature verification.

This feature must reuse all of these and add none of its own. Hookdeck's consecutive-signature-failure drift handling (ADR-1563) is the model for how this transport reports a persistent misconfiguration.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Receive App webhooks directly (Priority: P1)

An operator running Fabrik under GitHub App auth sets `event_source: direct` with a listen address, exposes that address through their own tunnel or proxy, and points the App's webhook URL at it. Signed deliveries reach the daemon and are applied to the board cache without any third-party relay.

**Why this priority**: This is the whole feature. It removes the Hookdeck dependency for operators who already have a public route to the daemon, and it is the transport #2143 builds on.

**Independent Test**: Start an App-auth engine with `event_source: direct` on a loopback address, POST a correctly signed `issues` payload to the configured path, and observe a 2xx response and the change applied to the board cache.

**Acceptance Scenarios**:

1. **Given** an App-auth engine with `event_source: direct` listening, **When** a correctly signed `issues` delivery is POSTed to the configured path, **Then** it is submitted to the intake, applied to the board cache, and the response is 2xx.
2. **Given** the same engine, **When** the same delivery ID is POSTed twice, **Then** the engine's existing ledger drops the duplicate (the transport adds no dedupe of its own).
3. **Given** a delivery of event type `ping`, **When** it is POSTed with a valid signature, **Then** it is acknowledged and logged and is not submitted to the intake.

---

### User Story 2 - Reject bad requests safely and visibly (Priority: P1)

The handler answers each class of bad request with a distinct status, so a misconfigured App or a probing client is told what is wrong without anything reaching the intake.

**Why this priority**: The listener is exposed through a public route. Verification before submission is what makes direct receipt safe.

**Independent Test**: Send requests with a wrong signature, an oversized body, a non-JSON body, a wrong path and a wrong method, and check each status and that nothing was submitted.

**Acceptance Scenarios**:

1. **Given** a payload with a wrong `X-Hub-Signature-256`, **When** it is POSTed, **Then** the response is 401 and nothing is submitted.
2. **Given** a body larger than the cap, **When** it is POSTed, **Then** the response is 413, before verification, and it is not counted as a signature failure.
3. **Given** a correctly signed body that is not JSON (for example the App's content type is `application/x-www-form-urlencoded`), **When** it is POSTed, **Then** the response is 400 and one log line names the fix (set the App's webhook content type to `application/json`).
4. **Given** a request to a path other than the configured one, **Then** the response is 404; **given** a method other than POST, **Then** the response is 405.

---

### User Story 3 - Surface a sustained signature failure (Priority: P2)

If deliveries keep failing signature verification (typically a wrong or rotated secret), the operator is told through the same channels Hookdeck uses, and the condition clears itself when deliveries succeed again.

**Why this priority**: Without it, a mistyped secret looks like a quiet daemon that simply receives no events.

**Independent Test**: Deliver a streak of wrongly signed payloads up to the drift threshold, check the transport is unhealthy and the warnings-panel entry exists, then deliver one good payload and check both clear.

**Acceptance Scenarios**:

1. **Given** a streak of consecutive signature failures reaching the drift threshold, **When** the threshold is crossed, **Then** the transport is marked unhealthy, one log line is written, and a warnings-panel entry is recorded.
2. **Given** the transport is in signature drift, **When** a correctly signed delivery arrives, **Then** the transport returns to healthy and the panel entry is resolved.
3. **Given** the transport is in signature drift, **When** the reconcile loop suggests the transport is healthy, **Then** that hint does not mask the drift state.

---

### User Story 4 - Know what each transport is doing (Priority: P2)

An operator or overseer reading `fabrik_health` sees one row per event transport, with its state, last delivery time and rejection counts, instead of only an aggregate mode and last-event time.

**Why this priority**: With several possible transports, the aggregate is not enough to tell which one is failing or silent.

**Independent Test**: Run with the direct transport, deliver some good and bad requests, and read the health output.

**Acceptance Scenarios**:

1. **Given** the direct transport is listening, **When** health is read, **Then** its row shows state `listening` and the time of its last delivery.
2. **Given** the listener failed to start, **When** health is read, **Then** its row shows `not listening`.
3. **Given** the transport is in signature drift, **When** health is read, **Then** its row shows `signature drift`.
4. **Given** gh-forward or Hookdeck is the active transport, **When** health is read, **Then** it appears in the same list with what it can report, and anything it cannot report is `"unknown"`. The existing aggregate fields are unchanged.

---

### User Story 5 - Fail loudly at startup on bad configuration (Priority: P2)

Misconfiguration is refused at startup with a message that names the fix, and a listen failure at runtime degrades to poll-only with a visible warning rather than silence.

**Why this priority**: A direct receiver has no relay to hide mistakes behind. Each wrong setup should be caught before the daemon runs as if it were receiving events.

**Independent Test**: Start the engine with each bad configuration and check the refusal; start with a port already in use and check the panel entry and that polling continues.

**Acceptance Scenarios**:

1. **Given** `event_source: direct` with PAT auth, **When** the engine starts, **Then** startup fails with a message saying direct requires GitHub App auth.
2. **Given** `event_source: direct` with no `direct_listen`, **When** the engine starts, **Then** startup fails naming `direct_listen`.
3. **Given** `event_source: direct` with a missing or empty webhook secret, **When** the engine starts, **Then** startup fails naming the secret env var.
4. **Given** the listen address is already in use or malformed, **When** the transport starts, **Then** the daemon continues poll-only, and a warnings-panel entry names the address and clears on a later successful start.
5. **Given** the installation does not grant a managed repo, **When** coverage is checked, **Then** the operator gets the same warning Hookdeck gives for that case.

---

### Edge Cases

- A delivery made while the daemon is down is lost. There is no relay buffer; the reconcile ticker (ADR-043) is the safety net, and replay is #2144.
- The body is exactly at the cap versus one byte over: the handler reads at most the cap plus one byte and answers 413 only above the cap.
- A missing `X-Hub-Signature-256` header is a signature failure (401), not a malformed request.
- A `ping` with a bad signature is rejected like any other request; it is not acknowledged.
- A good delivery between failures resets the consecutive-failure streak.
- Oversized and malformed requests do not advance the signature-drift streak.
- The Hookdeck drift behaviour and threshold stay exactly as they are after the shared tracker is extracted.
- The daemon speaks plain HTTP; a client that attempts TLS to the listener simply fails, since TLS is the fronting proxy's job.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A new App webhook transport implements the engine's event-transport interface and is built around an injected network listener, so another listener source (the embedded `tsnet` of #2143) can reuse the whole transport unchanged.
- **FR-002**: Under `event_source: direct` the listener is a plain TCP listener on `direct_listen`. Ingestion setup gains exactly one `event_source: direct` branch that constructs the transport and attaches it; no other engine wiring changes.
- **FR-003**: The handler accepts `POST` on the configured path only. Any other path gets 404 and any other method gets 405.
- **FR-004**: The handler reads at most the webhook body cap plus one byte and answers 413 for anything above the cap, before signature verification. A 413 is not counted as a signature failure.
- **FR-005**: The handler verifies `X-Hub-Signature-256` against this App's webhook secret using the shared signature verifier and answers 401 on failure without submitting.
- **FR-006**: A verified body that is not JSON gets 400 and one log line naming the fix (set the App's webhook content type to `application/json`).
- **FR-007**: Otherwise the handler hands a delivery (ID from `X-GitHub-Delivery`, event type from `X-GitHub-Event`, the payload) to the engine's intake sink and returns 2xx. `ping` events are acknowledged and logged, not submitted.
- **FR-008**: The handler is independent of the gh-forward webhook handler, whose secret rotation does not apply to a static App secret.
- **FR-009**: The consecutive-signature-failure drift tracker is extracted from the Hookdeck source into the shared events package. Hookdeck keeps its behaviour and threshold, and Pruefer is unaffected.
- **FR-010**: The direct transport uses the shared drift tracker. A sustained streak makes the transport unhealthy, logs once, and reports a warnings-panel entry that is resolved on recovery. The reconcile loop's healthy hint must not mask drift, following the Hookdeck rule.
- **FR-011**: `fabrik_health.webhook` gains a per-transport list. Each entry has a name, state, last delivery time, and counts of rejected signatures, malformed requests and oversized requests. Every transport fills it, and unknown values are `"unknown"`. For the direct transport the state is `listening`, `not listening` or `signature drift`. The existing aggregate fields (`mode`, `last_event_at`) stay as they are.
- **FR-012**: New flat config keys follow the existing `hookdeck_*` precedent, with flag > env > YAML precedence. `event_source: direct` is accepted. `direct_listen` is required (`--direct-listen` / `FABRIK_DIRECT_LISTEN`). `app_webhook_path` defaults to `/webhook` (`--app-webhook-path` / `FABRIK_APP_WEBHOOK_PATH`). `app_webhook_secret_env` names the env var that holds the secret and defaults to `FABRIK_GITHUB_WEBHOOK_SECRET`. `app_webhook_path` and `app_webhook_secret_env` are shared with the later `event_source: tailscale` (#2143).
- **FR-013**: Every new `FABRIK_*` name is classified in the worker-env classification as scrubbed, and `FABRIK_GITHUB_WEBHOOK_SECRET` stays scrubbed, so the secret never reaches a worker environment.
- **FR-014**: Startup refusals sit beside the existing App-auth refusals and each names the fix. `direct` without App auth is refused. A missing `direct_listen` is refused. A missing or empty secret is refused. The unknown-event-source check accepts `direct`. (`--webhooks` under App auth is already refused by ADR-1752.)
- **FR-015**: If the listener fails to start (port in use, bad address), the daemon carries on poll-only, as it does for any transport start failure, and a warnings-panel entry names the address. The entry is resolved by a later successful start.
- **FR-016**: The installation-coverage check Hookdeck uses is generalised to any App transport and applied to the direct transport, so a managed repo the installation does not grant is warned about the same way.
- **FR-017**: The reconcile ticker (ADR-043) is unchanged under `direct`. No buffering or replay is added.
- **FR-018**: The daemon does not terminate TLS. The transport speaks plain HTTP, and the docs and examples bind loopback and say TLS is the fronting proxy's job.
- **FR-019**: The transport adds no dedupe, ack, echo, reconnect or body-cap logic of its own beyond what FR-004 requires; it uses the intake's.
- **FR-020**: Documentation covers the feature. `docs/USER_GUIDE.md` gets a section with an operator recipe: the system `tailscale funnel --bg --set-path /fabrik-<name> http://127.0.0.1:<port>`, a generic reverse-proxy note, and the App's webhook settings (URL, secret, content type `application/json`). `docs/llms-full.txt` is regenerated. ADR `adrs/2142-direct-app-webhook-transport.md` records the decision.

### Key Entities *(if applicable)*

- **Direct transport**: the event transport that owns the listener, the handler and its health and drift state.
- **Delivery**: the (ID, event type, payload) unit handed to the intake after signature verification.
- **Signature-drift tracker**: the shared consecutive-failure counter used by Hookdeck and the direct transport.
- **Per-transport health row**: name, state, last delivery time and rejected/malformed/oversized counts, reported by every transport.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `event_source: direct` and App auth, a correctly signed `issues` payload POSTed to the listen address is applied to the board cache, and the response is 2xx.
- **SC-002**: A wrongly signed payload gets 401 and is never submitted. A streak reaching the drift threshold marks the transport unhealthy and adds the warnings-panel entry, and a later good delivery clears both.
- **SC-003**: Oversize bodies get 413 before verification and are not counted as signature failures. Non-JSON gets 400. A wrong path gets 404, a wrong method 405, and `ping` is acknowledged and not submitted.
- **SC-004**: Each startup refusal (PAT auth, missing listen address, missing secret) fails startup with a message naming the fix, and each is covered by a test.
- **SC-005**: A listen failure produces the warnings-panel entry and the daemon runs poll-only.
- **SC-006**: `fabrik_health` shows the per-transport row with state and last delivery time, and the aggregate fields are unchanged.
- **SC-007**: Hookdeck's drift behaviour is unchanged: its existing tests pass unmodified.
- **SC-008**: Every new `FABRIK_*` literal is classified, and the webhook secret never reaches a worker environment.
- **SC-009**: The USER_GUIDE section, regenerated `docs/llms-full.txt` and the ADR exist, and `go test -race ./...` is green.
- **SC-010**: The feature is verified once live on a test bed behind a system `tailscale funnel`, and the result is recorded in the PR.

## Assumptions

- The App's webhook secret is a single static value held in an environment variable (`FABRIK_GITHUB_WEBHOOK_SECRET` by default). It is not rotated by the engine.
- The drift threshold is the same one Hookdeck uses today (ADR-1563), so the extraction does not change any number.
- `app_webhook_secret_env` holds a variable name, not the secret itself, matching how the Hookdeck keys name their variables.
- A sim-bed twin is added only if the sim's intake seam can host the transport. Otherwise `httptest`-based unit tests delivering signed payloads end to end into the intake are the coverage, and the reason is recorded.
- Operators are responsible for the public route and its TLS. Binding a non-loopback address is allowed but documented as the operator's choice.

## Out of Scope *(optional)*

- Embedding `tsnet` (#2143).
- Replaying failed or missed deliveries (#2144).
- PAT mode.
- Any change to Hookdeck or `--webhooks` beyond the drift-tracker extraction and their per-transport health rows.
- Any change to the intake, the delivery ledger or the echo registry.
- TLS termination inside the daemon.

## Source References *(optional)*

- #2081 (originating proposal), #2143, #2144 (follow-ups in the chain)
- ADR 2351 (transport-neutral intake), ADR-1142/ADR-1254 (Hookdeck), ADR-1563 (signature drift), ADR-1752 (`--webhooks` refused under App auth), ADR-043 (reconcile ticker)
