# Feature Specification: Real webhook delivery for gate beds (`event_source: tailscale` per bed) with a delivery preflight

**Feature Branch**: `fabrik/issue-2408`
**Created**: 2026-10-23
**Status**: Specified
**Input**: User description: the original issue body, verbatim:

> ## Problem
>
> No gate bed receives webhooks. Both beds run poll-only (`webhooks: false`, no `event_source`), so the live suite never exercises event-driven ingestion — the mode production daemons run in (the dev daemon runs App auth + `event_source: hookdeck`; Tailscale, #2142–#2144 and #2143, is the new path). Every webhook-dependent live assertion self-skips, `TestWebhookChangeSurvivesReconcile` (#2348) has never run in a gate, and a regression in intake, dedupe, echo matching, replay or transport health can ship with a green gate.
>
> This issue's original premise — give a **pat** leg gh-forward delivery, because App auth refused webhooks (ADR 1752) — is obsolete: `event_source: direct` / `tailscale` are App-auth transports, and gh-forward is not the direction (ADR 2351). This is now the first of a chain: bed delivery here, the gate's events axis next, webhook coverage tests last.
>
> ## Requirements
>
> - **FR-001**: Each gate bed can run its daemon with `event_source: tailscale` (the embedded `tsnet` Funnel of #2143), one tailnet node per bed, its hostname and auth key supplied through the bed's own `.env` (`FABRIK_TAILSCALE_HOSTNAME`, the auth-key variable `tailscale_auth_key_env` names), never shared between beds. Each bed's GitHub App webhook URL points at that bed's Funnel name (the bed-topology preflight already refuses two beds sharing an App).
> - **FR-002**: The transport is selectable per leg, not baked into the bed: the gate's bed start/restart injects the event source for the leg (poll-only or webhook) the way it injects train mode and the #2417 bed timeouts, so the same bed serves both kinds of leg and a poll leg stays genuinely poll-only.
> - **FR-003**: Before a webhook leg spends live budget, a **webhook delivery preflight** proves delivery end to end: a harness write that GitHub turns into an App delivery (e.g. a label on a scratch issue), observed arriving at the bed daemon (the intake's last-delivery time / delivery ledger via the local API), within a bounded wait. Failure is a preflight failure (exit 4) naming the bed and the step that failed (Funnel not up, App hook URL mismatch, no delivery observed). It is a live probe, so it runs after the pre-gate (ADR 1454), beside the board-lag probe (ADR 1974), and its result is archived in `probes.json`.
> - **FR-004**: `bedWebhooksEnabled` reads the leg's effective event source (not only `FABRIK_WEBHOOKS`/`webhooks:`), so webhook-dependent tests run on a webhook leg and skip on a poll leg.
> - **FR-005**: The poll interval on a webhook leg is the production-shaped one the transport is meant to support, documented in `tests/e2e/README.md`, so that an engine effect arriving quickly is evidence of the webhook path and not of a short poll.
> - **FR-006**: `tests/e2e/README.md` documents the bed setup: tailnet, auth key provisioning and rotation, App webhook URL, secret, and how to check a bed's delivery by hand.
>
> ## Acceptance
>
> - A webhook leg on one bed passes the delivery preflight and runs `TestWebhookChangeSurvivesReconcile` to a PASS (not a skip).
> - The same bed, on a poll leg, runs with no transport attached (the daemon's startup banner/health shows poll-only).
> - A bed with its Funnel down, or its App webhook URL pointing elsewhere, fails preflight with exit 4 naming the cause, before any test runs.
> - The skip in `TestWebhookChangeSurvivesReconcile` no longer cites #2408; its registry `skip_ok_legs` is updated to "poll legs" rather than `app/*`.
>
> ## Out of scope
>
> - The gate matrix axis and ledger keys (#2427).
> - New webhook coverage tests (#2427).
> - Hookdeck or `direct` behind a system proxy on the beds; Tailscale is the bed transport. Mentioned for context: #2081.

## Background

The live e2e gate is the release gate, yet neither gate bed receives webhooks: both run poll-only. Production daemons run event-driven (App auth with a webhook transport), so the live suite never exercises intake, dedupe, echo matching, replay or transport health. Every webhook-dependent live assertion self-skips, so `TestWebhookChangeSurvivesReconcile` (#2348) has never run in a gate and a regression in that machinery can ship under a green gate.

The issue was first filed assuming a PAT leg would get gh-forward delivery, because App auth refused webhooks (ADR 1752). That premise is obsolete: the direct and Tailscale transports (#2142–#2144, #2143) are App-auth transports, gh-forward is no longer the direction (ADR 2351), and Tailscale (an embedded `tsnet` node exposed through Funnel) is the transport chosen for the beds. This issue is the first of a three-step chain: real delivery to the beds (here), then the gate's event-source matrix axis and ledger keys (#2427), then new webhook coverage tests (#2427).

Two existing facts shape the work. The beds already receive per-leg launch injection (train mode, the #2417 review/CI wait timeouts, the poll cadence), so event source is one more launch-time input. And the live preflights that need GitHub run after the free pre-gate and before live budget is spent (ADR 1454, ADR 1974), with results archived in `probes.json`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A bed receives real webhooks on a webhook leg (Priority: P1)

A release operator runs the gate with a webhook leg on a bed. The bed's daemon starts with the Tailscale transport, its own tailnet node and Funnel name, and GitHub's App deliveries for the bed's repositories reach it. `TestWebhookChangeSurvivesReconcile` runs and passes instead of self-skipping.

**Why this priority**: This is the whole point of the issue; without it the gate still cannot exercise event-driven ingestion.

**Independent Test**: Provision one bed (tailnet node, auth key, App webhook URL, secret), start a webhook leg on it, and run `TestWebhookChangeSurvivesReconcile`; it reports PASS, not SKIP.

**Acceptance Scenarios**:

1. **Given** a bed provisioned for Tailscale delivery, **When** a webhook leg starts the bed, **Then** the daemon's startup output and health show the Tailscale transport listening.
2. **Given** that running webhook leg, **When** `TestWebhookChangeSurvivesReconcile` runs, **Then** it runs its assertions and passes.
3. **Given** two beds, **When** both are provisioned, **Then** each has its own tailnet hostname, auth key and App webhook URL, and no value is shared.

---

### User Story 2 - The same bed runs genuinely poll-only on a poll leg (Priority: P1)

The same bed, started for a poll leg, has no transport attached. Webhook-dependent tests skip because there is no webhook path, and a poll leg's fast effects can only come from polling.

**Why this priority**: The gate must keep testing the poll-only mode, and a poll leg that quietly had a transport would invalidate both kinds of leg.

**Independent Test**: Start the bed for a poll leg and read the daemon's startup banner and health: no transport is attached. Run `TestWebhookChangeSurvivesReconcile`; it skips.

**Acceptance Scenarios**:

1. **Given** a bed whose `.env` holds Tailscale credentials, **When** a poll leg starts it, **Then** the daemon runs with no transport and health shows poll-only.
2. **Given** a bed that served a webhook leg, **When** the next leg is a poll leg and the bed restarts, **Then** nothing from the earlier leg's event source persists.

---

### User Story 3 - Broken delivery fails fast, before any test (Priority: P1)

Before a webhook leg spends live budget, a delivery preflight proves a GitHub-originated App delivery reaches the bed daemon. If it cannot, the gate stops with exit 4, names the bed and names the step that failed.

**Why this priority**: A webhook leg with broken delivery would not fail visibly; its webhook tests would time out as inconclusive or skip, burning hours and producing a gate that looks covered but is not.

**Independent Test**: Take a provisioned bed and (a) stop its Funnel, (b) point its App webhook URL elsewhere; each run exits 4 before any test with a distinct, named cause. With delivery healthy, the preflight passes and its result appears in `probes.json`.

**Acceptance Scenarios**:

1. **Given** a healthy webhook leg, **When** the preflight makes a harness write that GitHub turns into an App delivery, **Then** the daemon's last-delivery time advances within the bounded wait and the preflight passes.
2. **Given** the bed's Funnel is not up, **When** the preflight runs, **Then** it fails with exit 4 naming the bed and "Funnel/transport not up".
3. **Given** the App hook URL points at another host, **When** the preflight runs, **Then** it fails with exit 4 naming the bed and "App hook URL mismatch".
4. **Given** the transport is up and the hook URL is correct but no delivery arrives in the bound, **When** the preflight runs, **Then** it fails with exit 4 naming the bed and "no delivery observed".
5. **Given** any preflight outcome, **Then** the result (pass or the failed step) is archived in `probes.json`.

---

### User Story 4 - An operator can provision and check a bed by hand (Priority: P2)

An operator setting up or repairing a bed follows `tests/e2e/README.md` to provision the tailnet node, auth key (including rotation), App webhook URL and secret, and to check a bed's delivery without running the gate.

**Why this priority**: The mechanism is only usable if bed setup is reproducible, but it does not change gate behavior.

**Independent Test**: A second operator follows the README alone to bring up a bed and confirm delivery by hand.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator follows the setup section, **Then** they can provision a bed's tailnet node, auth key, App webhook URL and secret.
2. **Given** a provisioned bed, **When** the operator follows the manual check, **Then** they can observe a delivery arriving without running the gate.

---

### Edge Cases

- A bed's Tailscale auth key is expired, revoked or missing while the node has no stored login: the transport does not come up, and the preflight reports "Funnel/transport not up" rather than letting tests run.
- A node that is already logged in from stored state starts without a usable auth key: the transport still comes up (an auth key is only required while no logged-in node state exists).
- Funnel ingress takes time to route to a freshly started node (the engine itself waits before replaying): the preflight's bounded wait must tolerate that settling time without making a genuinely broken bed look healthy.
- The daemon already shows a last-delivery time from earlier traffic: the preflight must prove a new delivery caused by its own write, not accept a stale value.
- The preflight's scratch artifact must not leak onto the board or into later tests, and must be cleaned up on both pass and fail.
- A PAT-auth leg cannot run a webhook transport (the Tailscale transport is App-auth only): such a leg is always a poll leg.
- A webhook leg restarted mid-run (restart-safety tests, train-mode switches) must keep its event source across the restart.
- The harness's own restart path and the gate runner's launch path must agree on the injected event source, as they already must for the other injected values.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Each gate bed can run its daemon with `event_source: tailscale` (the embedded `tsnet` Funnel of #2143), with one tailnet node per bed. Its hostname and auth key are supplied through that bed's own `.env` (`FABRIK_TAILSCALE_HOSTNAME`, and the auth-key variable `tailscale_auth_key_env` names) and are never shared between beds. Each bed's GitHub App webhook URL points at that bed's own Funnel name; the existing bed-topology preflight already refuses two beds sharing an App.
- **FR-002**: The event source is selected per leg, not baked into the bed. The gate's bed start and restart inject it for the leg (poll-only or webhook), in the same manner as train mode and the #2417 bed timeouts, so one bed serves both kinds of leg, a poll leg is genuinely poll-only (no transport attached), and the harness's restart path and the gate runner's launch path inject it identically.
- **FR-003**: Before a webhook leg spends live budget, a webhook delivery preflight proves delivery end to end: a harness write that GitHub turns into an App delivery (for example a label on a scratch issue) is observed arriving at the bed daemon, via its last-delivery time or delivery ledger read through the local API, within a bounded wait. The observation must be a delivery newer than a baseline taken before the write. Failure is a preflight failure with exit 4 naming the bed and the failed step (Funnel not up, App hook URL mismatch, or no delivery observed). It is a live probe, so it runs after the pre-gate (ADR 1454), alongside the board-lag probe (ADR 1974), and its result is archived in `probes.json`.
- **FR-004**: `bedWebhooksEnabled` reports the leg's effective event source rather than only `FABRIK_WEBHOOKS`/`webhooks:`, so webhook-dependent tests run on a webhook leg and skip on a poll leg.
- **FR-005**: The poll interval on a webhook leg is the production-shaped one the transport is meant to support, and the chosen value and rationale are documented in `tests/e2e/README.md`, so that an engine effect arriving quickly is evidence of the webhook path and not of a short poll.
- **FR-006**: `tests/e2e/README.md` documents bed setup for webhook delivery: tailnet, auth-key provisioning and rotation, App webhook URL, webhook secret, and how to check a bed's delivery by hand.
- **FR-007**: `TestWebhookChangeSurvivesReconcile` no longer cites #2408 in its skip message, and its registry entry's `skip_ok_legs` and `auth_reason` describe the real condition (legs with no webhook transport, including all PAT legs) instead of the obsolete "GitHub App auth refuses webhooks (ADR 1752)" and `app/*`.

### Key Entities *(if applicable)*

- **Bed**: a provisioned engine instance with its own directory, `.env`, repositories, App identity and board. Gains its own tailnet node identity (hostname, auth key) and App webhook URL.
- **Leg**: one gate cell (auth × train today) run on a bed. Gains an effective event source: poll-only or webhook (Tailscale).
- **Event source (per leg)**: the launch-time input that decides whether the bed daemon attaches a webhook transport.
- **Webhook delivery preflight**: a live probe, run per webhook leg, with a named-step result recorded in `probes.json`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a provisioned bed, a webhook leg passes the delivery preflight and `TestWebhookChangeSurvivesReconcile` ends PASS (not SKIP) in the gate's recorded outcome.
- **SC-002**: On the same bed, a poll leg shows no transport attached in the daemon's startup banner and health, and `TestWebhookChangeSurvivesReconcile` skips.
- **SC-003**: With a bed's Funnel down, and separately with its App hook URL pointing elsewhere, the gate exits 4 before any test runs, and the output names the bed and a different, correct cause for each.
- **SC-004**: Every webhook-leg preflight run, passing or failing, leaves a record of its result and failed step in that leg's `probes.json`.
- **SC-005**: Nothing in the repository's live-test skip messages or registry references #2408 as the reason webhook tests skip.
- **SC-006**: An operator who has not set up a bed before can provision one and confirm delivery by hand using only `tests/e2e/README.md`.

## Assumptions

- **Preflight placement.** The delivery preflight needs the bed daemon running with the transport, so it cannot run before the bed is built like the board-lag probe. It runs after the pre-gate and after the webhook leg's bed start, before that leg's tests spend live budget. It stays "beside" the board-lag probe in that it is a live probe, follows ADR 1454's pre-gate-spends-nothing ordering, and is archived in `probes.json`. A bed already running a poll leg is never probed.
- **Selecting the event source until #2427.** The matrix axis and ledger keys are out of scope, so this issue adds only the minimal selection input: an operator-supplied gate input (e.g. an `E2E_EVENT_SOURCE`-style variable) choosing poll-only or webhook for a run, defaulting to poll-only so existing gate behavior is unchanged. #2427 replaces this with a real axis. The gate and the harness restart path read the same input.
- **Webhook legs are App-auth legs.** The Tailscale transport is App-auth only, so a PAT leg is always poll-only, whatever the input says.
- **Observing delivery.** The daemon's local API already reports per-transport last-delivery time in health; the preflight reads that, comparing against a baseline taken before its write. The harness label write targets a scratch issue in a bed repository; the App must be subscribed to the event it triggers.
- **Poll interval on a webhook leg.** Poll legs keep the current 60 s bed cadence. A webhook leg uses a materially longer, production-shaped cadence (the value is chosen at Plan time, with its rationale in the README) so that a quick effect cannot be explained by polling. It remains overridable through the existing bed-poll override.
- **Bed provisioning is manual and per-bed.** Creating tailnet nodes, auth keys, App webhook URLs and secrets is operator work documented in the README; the gate only consumes the result and verifies it through the preflight.
- **Registry vocabulary.** `skip_ok_legs` currently names auth × train legs. Expressing "legs with no webhook transport" for this test may need a minimal extension of the leg pattern vocabulary here, pending the full axis in #2427; Plan decides the smallest accurate form.
- **Blocking dependency.** #2143 (the embedded Tailscale receiver) is the transport this issue builds on; this spec assumes it is available to the beds.

## Out of Scope *(optional)*

- The gate matrix axis and ledger keys for event source (#2427).
- New webhook coverage tests (#2427).
- Hookdeck or `direct` transports on the beds, including behind a system proxy; Tailscale is the bed transport. Mentioned for context: #2081.
- gh-forward delivery or any PAT-leg webhook delivery.
- Automating tailnet node, auth-key or App webhook configuration; those stay manual, documented steps.

## Source References *(optional)*

- ADR 1454 (sim pre-gate is not a replacement; live probes run after the pre-gate), ADR 1974 (environment probes, `probes.json`), ADR 1752 (App auth refused webhooks; premise now obsolete), ADR 2351 (transport-neutral intake), ADR 2142–2144 (direct receiver, Tailscale receiver, delivery replay), ADR 2417 (bed wait timeouts injected at launch)
- `tests/e2e/webhook_reconcile_test.go`, `tests/e2e/mergetrain_coldbase_test.go` (`bedWebhooksEnabled`), `tests/e2e/lifecycle.go` and `tests/gate/bed.go` (bed launch sites), `tests/e2e/registry/registry.json`, `tests/e2e/README.md`
