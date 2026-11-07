# Feature Specification: Pruefer `event_source: tailscale` and `direct` (one App per installation)

**Feature Branch**: `fabrik/issue-2476`
**Created**: 2026-11-06
**Status**: Specified
**Input**: User description: "## Problem

Pruefer has no supported event-driven mode once Hookdeck is removed (0.0.92). The receiver Fabrik uses (`event_source: tailscale`, `direct`) will be in a shared package after the previous issue in this chain.

**Constraint:** a GitHub App has exactly one webhook URL. Today one Pruefer App (`handarbeit-pruefer`) is installed on several orgs, with one daemon per installation, so the daemons cannot each receive that App's webhook.

**Decision (2026-10-25): one GitHub App per installation.** Each Pruefer daemon gets its own App, and therefore its own webhook URL, secret and Funnel node. No fan-out component is needed.

## Requirements

- Pruefer `event_source: tailscale` and `event_source: direct`, built on the shared receiver. Config follows Pruefer's existing conventions: flag > `PRUEFER_*` env > YAML, restart-only, secret values named through `*_env` indirection. The new names stay out of the review environment, which is an allowlist.
- A verified delivery is handed to Pruefer's existing event sink and dispatch, the path Hookdeck deliveries take today. Pruefer's own delivery-ID dedupe applies. Replay of failed deliveries uses Pruefer's dedupe as the \"seen\" check and never dispatches a review directly.
- A delivery whose `installation.id` is not the daemon's own installation is ignored with one log line. This is a backstop for a misconfigured shared App.
- The reconciliation fallback poll and startup sweep keep their current behaviour.
- Health: the TUI footer and status show the transport state, tailnet and Funnel state, last delivery and route liveness. Listen failures and signature drift are reported as for Fabrik.
- Docs in `cmd/pruefer/README.md`:
  - operator setup, including registering one App per installation (the manifest flow creates one App per run; set its webhook URL, secret and content type `application/json`);
  - access-policy `nodeAttrs`, HTTPS certificates and a tagged auth key;
  - a migration runbook from the shared App.

## Scope

Out: moving the existing Pruefer instances. That is an operator step, done in a quiet window per instance.

## Acceptance

- A signed `pull_request` delivery to the receiver dispatches a review (test through the real handler into Pruefer's sink).
- A delivery for another installation is ignored.
- Restart reuses the node from its state directory without an auth key.
- Verified once live on one Pruefer instance with its own App; recorded in the PR.
- Docs; `go test -race ./...` green."

## Background

Pruefer, the review bot, runs as a separate program from the Fabrik engine. Today its only event-driven mode is Hookdeck, which is deprecated (0.0.90) and removed in 0.0.92. After that, Pruefer has only its poll loop. Fabrik's engine already has two first-party ways to receive GitHub App webhooks: `direct` (a plain listener) and `tailscale` (an embedded Tailscale Funnel node). The preceding issue (#2475) moved that receiver into a shared internal package that Pruefer can import. This issue wires Pruefer to it.

A GitHub App has exactly one webhook URL. Today a single Pruefer App (`handarbeit-pruefer`) is installed on several orgs, with one daemon per installation. Those daemons cannot each receive the App's one webhook. Decision of 2026-10-25: **one GitHub App per installation**. Each Pruefer daemon gets its own App, and so its own webhook URL, secret and Funnel node. No fan-out component is needed.

The result is that a Pruefer operator can run an event-driven daemon with no third-party relay, using the same receiver, security properties and health reporting as Fabrik. Poll-based reconciliation stays as the safety net.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A signed delivery triggers a review (Priority: P1)

An operator configures a Pruefer daemon with `event_source: direct` or `event_source: tailscale`. When GitHub delivers a signed `pull_request` event (opened, reopened, synchronize, ready for review), the daemon reviews that PR without waiting for the next poll.

**Why this priority**: This is the whole point of the feature: a supported event-driven mode to replace Hookdeck.

**Independent Test**: Send a signed `pull_request` delivery to the receiver through the real HTTP handler, wired into Pruefer's real event sink, and observe a review dispatched for that PR.

**Acceptance Scenarios**:

1. **Given** a daemon on `direct` or `tailscale`, **When** a correctly signed `pull_request` delivery for its own installation arrives, **Then** a review is dispatched through Pruefer's existing event sink, the same path a Hookdeck delivery takes today.
2. **Given** the same delivery arrives twice (same delivery ID), **When** the second copy is received, **Then** Pruefer's existing delivery-ID dedupe drops it and only one review is dispatched.
3. **Given** a delivery with a bad or missing signature, an oversized body, a wrong method or path, or invalid JSON, **When** it arrives, **Then** it is rejected exactly as the shared receiver rejects it for Fabrik, and nothing is dispatched.

---

### User Story 2 - Deliveries for another installation are ignored (Priority: P1)

If an App is misconfigured so that a delivery meant for another installation reaches this daemon (for example the old shared App still points at one daemon), the daemon does not act on it.

**Why this priority**: The shared-App topology is exactly what is being retired; during migration a stray delivery is the most likely failure, and acting on another account's PR would be a cross-tenant mistake.

**Independent Test**: Send a signed delivery whose `installation.id` differs from the daemon's own; verify no review is dispatched and one log line is written.

**Acceptance Scenarios**:

1. **Given** a daemon whose own installation is A, **When** a validly signed delivery for installation B arrives, **Then** it is ignored and one log line names the delivery and the foreign installation.
2. **Given** a delivery for the daemon's own installation, **When** it arrives, **Then** it is processed normally.

---

### User Story 3 - Restart without re-authenticating (Priority: P2)

A daemon on `tailscale` is restarted. It reuses the Funnel node identity stored in its state directory and does not need the auth key again.

**Why this priority**: Auth keys are short-lived and tagged; requiring one on every restart would make the mode fragile to operate.

**Independent Test**: Start a tailscale-mode daemon with an auth key, stop it, restart it with no auth key set, and verify the node comes up from stored state and receives a delivery.

**Acceptance Scenarios**:

1. **Given** a state directory holding a logged-in node, **When** the daemon restarts with no auth key configured, **Then** the node comes up from stored state and the transport reports healthy.
2. **Given** an empty state directory and no auth key, **When** the daemon starts, **Then** it fails to listen, reports the failure as a listen failure, and keeps working in poll mode.

---

### User Story 4 - Operators can see transport health (Priority: P2)

An operator looking at the Pruefer TUI footer and status sees the transport state, the tailnet and Funnel state, the time of the last delivery and route liveness. Listen failures and signature drift are reported the way they are for Fabrik.

**Why this priority**: An event-driven mode that silently stops receiving is worse than polling, because nobody notices.

**Independent Test**: Drive the transport into listen failure, signature drift and an unreachable route; verify each appears in the footer/status and in the same kind of operator warning Fabrik raises.

**Acceptance Scenarios**:

1. **Given** a healthy daemon on `tailscale`, **When** the operator views status, **Then** transport state, tailnet state, Funnel state, last delivery time and route liveness are shown.
2. **Given** a bind or Funnel failure at startup, **When** the daemon runs, **Then** it continues in poll mode and the failure is reported as a listen failure.
3. **Given** repeated deliveries failing signature verification, **When** the drift threshold is reached, **Then** signature drift is reported.

---

### User Story 5 - Failed deliveries are recovered without bypassing dedupe (Priority: P2)

While a daemon was down, GitHub recorded failed deliveries. After startup, the shared replayer asks GitHub to redeliver them.

**Why this priority**: Without replay, events during downtime wait for the fallback poll.

**Independent Test**: Seed GitHub's delivery log with failed deliveries (fake API), start the daemon, and verify redelivery is requested only for deliveries Pruefer has not seen, and that the replayer itself never dispatches a review.

**Acceptance Scenarios**:

1. **Given** failed deliveries in GitHub's log that Pruefer's dedupe has not seen, **When** the replayer runs, **Then** it requests redelivery and the redelivered copies arrive through the normal handler.
2. **Given** a delivery Pruefer's dedupe has already seen, **When** the replayer runs, **Then** it does not request redelivery of it.
3. **Given** any replay pass, **Then** the replayer never hands a delivery to the sink or dispatches a review itself.

---

### User Story 6 - Operator documentation and migration (Priority: P2)

An operator moving a Pruefer instance off the shared App follows `cmd/pruefer/README.md` to register a dedicated App, configure the receiver, set up the tailnet and Funnel prerequisites, and cut over.

**Why this priority**: The code is useless to operators without a way to adopt it, and the migration touches live reviews.

**Independent Test**: A reader who has not seen the design can follow the README from a fresh App to a working delivery, and from the shared App to the per-installation App.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator follows the setup section, **Then** they register one App per installation, set its webhook URL, secret and content type `application/json`, and configure Pruefer to match.
2. **Given** the README, **When** an operator prepares a tailscale-mode host, **Then** the access-policy `nodeAttrs`, HTTPS certificate requirement and tagged auth key are explained.
3. **Given** an instance on the shared App, **When** the operator follows the migration runbook, **Then** there is a step order that avoids a gap in review coverage, and the poll/sweep fallbacks cover the cutover window.

---

### Edge Cases

- Config names (listen address, webhook path, secret env name, Tailscale hostname/auth-key env/state dir) are restart-only; a config reload (SIGHUP) that changes them is reported as needing a restart, as for other restart-only keys.
- An empty webhook secret, a missing listen address for `direct`, or a non-absolute webhook path is refused at startup with a clear message.
- A bind failure or Funnel failure degrades to poll-only rather than stopping the daemon.
- `ping` deliveries are acknowledged and not dispatched.
- Event types Pruefer has no use for are acknowledged and not dispatched (as today).
- Installation lifecycle events (`installation`, `installation_repositories`) still trigger Pruefer's existing re-derivation when they belong to the daemon's own installation (an `installation` `created` event is the one exception; see Assumptions).
- A delivery with no `installation.id` in its payload cannot be matched to the daemon and is treated as not belonging to it (ignored with one log line, with a message saying the delivery carries no `installation.id` rather than blaming a foreign App). There is no exception by event type: once the daemon knows its installations, an App-level event that carries no installation ID (for example `github_app_authorization`) is ignored too, which is harmless because Pruefer's sink has nothing to do for those event types. `ping` never reaches this check; the shared receiver acknowledges it first.
- A daemon whose installation set is not yet known (very first start before derivation completes) cannot judge ownership; see Assumptions.
- The new configuration names (secret env name, auth key env name, listen address, path, hostname, state dir) must not reach the environment of the review subprocess.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Pruefer MUST accept `event_source: direct` and `event_source: tailscale` in addition to its existing values, built on the shared receiver package from #2475 (no second implementation of the handler, listener or Funnel node).
- **FR-002**: Receiver configuration (event source, listen address, webhook path, secret env-var name, Tailscale hostname, auth-key env-var name, Tailscale state directory) MUST follow Pruefer's existing conventions: flag > `PRUEFER_*` environment variable > YAML, restart-only, with secret values read through `*_env` indirection (the configuration holds the variable name, never the secret).
- **FR-003**: The new variable names and their resolved secret values MUST NOT appear in the environment of the review subprocess (the review environment remains an allowlist).
- **FR-004**: A verified delivery MUST be converted to Pruefer's existing normalized event and handed to Pruefer's existing event sink and dispatch, the path Hookdeck deliveries take today. Pull-request actions that trigger a review and installation events that trigger re-derivation behave as they do now.
- **FR-005**: Pruefer's own delivery-ID dedupe MUST apply to deliveries from the new receiver, so a redelivered or duplicated delivery dispatches at most once.
- **FR-006**: Replay of failed deliveries MUST use Pruefer's dedupe as its read-only "seen" check, MUST NOT record a delivery as seen by checking it, and MUST NOT dispatch a review or call the sink itself; redelivered copies re-enter only through the normal handler.
- **FR-007**: A delivery whose `installation.id` is not the daemon's own installation MUST be ignored with exactly one log line, and MUST NOT be dispatched, deduped-as-seen, or counted as a signature failure. The one exception is an `installation` event with action `created` (see Assumptions, ADR 2476 decision 5).
- **FR-008**: The reconciliation fallback poll and the startup sweep MUST keep their current behaviour and cadence in the new modes.
- **FR-009**: The Pruefer TUI footer and status MUST show the transport state, tailnet state, Funnel state, last delivery time and route liveness.
- **FR-010**: Listen failures, signature drift and (for `tailscale`) hook-configuration mismatch MUST be reported to the operator the way Pruefer reports other operator-level conditions, equivalent to how Fabrik reports them. A listen failure MUST degrade to poll-only rather than stop the daemon.
- **FR-011**: On restart, a `tailscale`-mode daemon MUST reuse the node stored in its state directory without an auth key; the auth key is required only when the state directory holds no logged-in node.
- **FR-012**: Startup MUST refuse an unusable receiver configuration (missing secret, missing listen address for `direct`, non-absolute path, unsupported combination) with a clear message.
- **FR-013**: `cmd/pruefer/README.md` MUST document operator setup, including registering one GitHub App per installation (the manifest flow creates one App per run; the operator sets its webhook URL, secret and content type `application/json`).
- **FR-014**: `cmd/pruefer/README.md` MUST document the Tailscale prerequisites: access-policy `nodeAttrs` for Funnel, HTTPS certificates, and a tagged auth key.
- **FR-015**: `cmd/pruefer/README.md` MUST include a migration runbook from the shared App to per-installation Apps, for one instance at a time.
- **FR-016**: The `internal/appwebhook` package MUST remain free of any import of `engine` or `pruefer`; Pruefer-specific behaviour is supplied through the package's existing interfaces.

### Key Entities

- **Delivery**: a signed GitHub App webhook request, identified by its delivery ID, carrying an event type and a payload with an installation ID.
- **Installation**: the single GitHub App installation a Pruefer daemon serves (in the target topology, each daemon has its own App and one installation).
- **Event sink**: Pruefer's existing component that turns normalized events into review dispatches or re-derivation.
- **Dedupe record**: Pruefer's existing delivery-ID record, used both to drop duplicates and as the replayer's "seen" check.
- **Transport health row**: transport, tailnet, Funnel, last-delivery and route-liveness state shown in the footer and status.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A signed `pull_request` delivery sent through the real receiver handler into Pruefer's real sink results in exactly one dispatched review, shown by an automated test.
- **SC-002**: A signed delivery for a different installation results in zero dispatched reviews and exactly one log line, shown by an automated test.
- **SC-003**: A tailscale-mode daemon restarted with no auth key set comes up from its state directory and reports a healthy transport, shown by a test of the start path and once live.
- **SC-004**: The change is verified live once on one Pruefer instance running with its own dedicated App, receiving a real delivery and dispatching a review; the result is recorded in the PR description.
- **SC-005**: `go test -race ./...` passes, including tests that the replayer never dispatches and that the new configuration names are absent from the review environment.
- **SC-006**: An operator can go from "no App" to "first event-driven review" and from "shared App" to "per-installation App" using only `cmd/pruefer/README.md`.

## Assumptions

- **Dedicated App per daemon**: each Pruefer daemon runs under one App with one installation, so "the daemon's own installation" is the single installation that daemon derives for its App. A daemon holding several installations (a misconfigured shared App) treats a delivery as its own if its installation ID is among the installations the daemon currently serves, and ignores it otherwise.
- **Before ownership is known**: if the daemon has not yet derived any installation, it cannot judge ownership and does not drop deliveries on that ground; the filter is a backstop, and the first derivation happens at startup before reviews run.
- **Installation lifecycle events** are subject to the same ownership check, since each carries an installation ID; an event for the daemon's own installation still triggers re-derivation. One deliberate exception (ADR 2476 decision 5): an `installation` event with action `created` bypasses the check, because it is how a daemon holding several installations of its App learns of a new one, and it only triggers the re-derivation a timer tick would do anyway. `installation_repositories`, every other `installation` action and every other event type use the strict check.
- **Hookdeck code is untouched here**: removing Pruefer's Hookdeck mode is separate work tracked with the 0.0.92 removal. This issue only adds the two new values alongside the existing ones.
- **Shared receiver behaviour is unchanged**: Pruefer inherits the receiver's size cap, signature verification, Funnel-only listening, route-liveness and replay semantics as they stand after #2475.
- **Pruefer's replay "seen" check** is read-only against its existing dedupe, mirroring how the engine's replayer uses the ledger.
- **Live verification** needs a real Tailscale tailnet, a real tagged auth key and a dedicated App; this is an operator-assisted step done once, not part of CI.

## Out of Scope *(optional)*

- Moving the existing Pruefer instances off the shared App. That is an operator step, done in a quiet window per instance.
- Any fan-out or relay component to share one App across daemons.
- Removing Pruefer's Hookdeck mode.
- Changes to the shared receiver's behaviour beyond what Pruefer wiring needs.
- Any change to what Pruefer reviews or how a review is performed.

## Source References *(optional)*

- #2475 (shared App webhook receiver package, closed)
- #2143, #2144, #2452 (engine tailscale transport, delivery replay, route liveness) — mentioned for context
- `adrs/1113-*` (the engine/Pruefer import boundary)
