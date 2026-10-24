# Feature Specification: Probe the Funnel name's public DNS before reporting the tailscale transport healthy, and recover deliveries missed while it propagates

**Feature Branch**: `fabrik/issue-2425`
**Created**: 2026-10-23
**Status**: Specified
**Input**: User description: "## Problem

When `event_source: tailscale` (#2143) brings up a **new** tailnet node, its Funnel hostname does not resolve publicly for several minutes. During the #2143 live check it took about 15 minutes: https://github.com/handarbeit/fabrik/pull/2424#issuecomment-6364451352. Throughout that window:

- GitHub's deliveries fail with `502 failed to connect to host`;
- `fabrik_health.webhook.transports` still reports `state: listening`, `tailnet: Running`, `funnel: ok`, so the operator cannot tell that no webhook can reach the daemon;
- deliveries that fail inside the window are never replayed. #2144's replay runs only on start and on the transport's reconnect signal, and both happen before the name resolves, so these deliveries are left to reconcile.

Polling and reconcile (ADR-043) keep running throughout, so the board does not stall; it only falls back to poll latency. The defect is that the health report is wrong and missed deliveries are not recovered once the name resolves.

Two traps for any check:

- **The local resolver answers with the tailnet IP** (MagicDNS) on a host that is itself on the tailnet. That says nothing about the public name.
- **A public recursive resolver caches a negative answer.** An early lookup through 1.1.1.1 returned an answer with no records, and the resolver kept serving that cached negative answer. A probe that asks a public resolver can delay its own success, and can keep the name looking missing after it is published.

Mentioned for context: #2081.

## What already exists (do not rebuild)

- #2142's `appWebhookTransport`, its per-transport health row and its reconnect handler (`SetReconnectHandler` → `requestReconcile`).
- #2143's tsnet listener, tailnet/Funnel health fields and the R7 `/app/hook/config` URL check (re-run on each reconcile tick).
- #2144's replay pass, triggered by the same reconnect signal.

## Requirements

- **R1 — public-resolution probe.** Under `event_source: tailscale`, after the node is up, check whether the node's Funnel hostname resolves publicly. Ask the **authoritative nameservers** for the name's zone directly, discovering them by an NS lookup. Never use the local or system resolver, and never a public recursive resolver. Treat as resolved only an answer that has at least one A or AAAA record outside the tailnet ranges (`100.64.0.0/10`, `fd7a:115c:a1e0::/48`). Retry with backoff (e.g. 10 s growing to 60 s) until it resolves; after that, stop probing for the life of the node.
- **R2 — health.** Until R1 succeeds, the transport's `funnel` field reports `pending-dns` and the transport state is not `listening`; use a distinct value such as `listening (not publicly resolvable)`. The aggregate `webhook.mode` must not read healthy while this holds. Unknown, i.e. the probe itself failed, reports `\"unknown\"`, never `ok`.
- **R3 — warnings panel.** If the name has not resolved within a grace period (10 min default; constant, not config), report a panel entry naming the hostname and the likely causes: propagation still in progress, Funnel not granted to the node's tag, HTTPS certificates disabled. Resolve the entry when the name resolves.
- **R4 — recover missed deliveries.** On the first successful resolution, call the transport's reconnect handler once. That requests a catch-up reconcile and, with #2144, a replay pass, which picks up the deliveries that failed during the window. Never call it when the name already resolved at the first probe, which is the normal case on a restart that reuses the node, and never more than once per node lifetime.
- **R5 — never blocks.** A probe failure (no network, NS lookup error) is logged at most once per backoff level, never fails startup, and never stops the transport serving. Requests that do arrive are handled as usual.
- **R6 — `direct` is out of scope.** Under `event_source: direct` the public name belongs to an external proxy the engine cannot see; no probe.

## Scope

In: the probe (behind an interface seam so tests need no network: stub authoritative answers for NODATA → records, tailnet-only records, NS failure); health fields; the panel entry; the one-shot reconnect call; tests including \"already resolved at first probe → no reconnect call\"; a USER_GUIDE note in the tailscale setup section (#2143's setup steps, replacing its \"redeliver once it resolves\" line); `docs/llms-full.txt` regen; ADR `adrs/<this issue>-funnel-public-dns-probe.md`.

Out: any change to the poll or reconcile cadence; probing for `direct`; changing #2144's replay logic beyond being triggered by the R4 reconnect call.

## Acceptance

- AC1: With a stubbed authoritative server returning no records and then records, health reports `pending-dns` and then `ok`, and the reconnect handler is called exactly once at the transition.
- AC2: Records that resolve only to tailnet addresses count as not resolved.
- AC3: A name that resolves at the first probe never calls the reconnect handler.
- AC4: Past the grace period the panel entry appears; it clears on resolution.
- AC5: Probe errors never fail startup or stop the listener; they are logged and reported as `\"unknown\"`.
- AC6: The probe never queries the system resolver or a recursive resolver (pinned by a test or a static check on the seam).
- AC7: Verified once live with a new node name on a test bed: health shows `pending-dns` until GitHub can reach it, then the replay pass redelivers a delivery that failed in the window. Recorded in the PR.
- AC8: Docs, regen, ADR; `go test -race ./...` green."

## Background

With `event_source: tailscale` (#2143), the daemon embeds a Tailscale node and exposes the GitHub App webhook receiver through a Funnel hostname. A freshly created node's Funnel name does not resolve publicly for several minutes (about 15 minutes in the #2143 live check, see https://github.com/handarbeit/fabrik/pull/2424#issuecomment-6364451352). During that window:

- GitHub's deliveries fail with `502 failed to connect to host`.
- `fabrik_health.webhook.transports` still shows `state: listening`, `tailnet: Running`, `funnel: ok`, so an operator cannot tell that no webhook can reach the daemon.
- Deliveries that fail inside the window are not recovered. The #2144 replay pass runs only at startup and on the transport's reconnect signal, and both occur before the name resolves. Those deliveries are left to the periodic reconcile.

Polling and reconcile (ADR-043) keep running, so the board never stalls; it only falls back to poll latency. The defects are therefore a wrong health report and missed deliveries that are not recovered once the name resolves.

Two traps constrain any check. The local resolver on a tailnet host answers with the tailnet IP (MagicDNS), which says nothing about the public name. A public recursive resolver caches negative answers: an early lookup through 1.1.1.1 returned an empty answer and kept serving it, so a probe that asks a recursive resolver can delay its own success and keep a published name looking missing.

Existing pieces this builds on and must not rebuild: #2142's `appWebhookTransport` with its per-transport health row and reconnect handler (`SetReconnectHandler` → `requestReconcile`), #2143's tsnet listener with tailnet/Funnel health fields and the `/app/hook/config` URL check re-run on each reconcile tick, and #2144's replay pass triggered by the same reconnect signal. Mentioned for context: #2081.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Honest health while the Funnel name propagates (Priority: P1)

An operator starts Fabrik with `event_source: tailscale` on a new node name. Until the Funnel hostname resolves publicly, the health report says so instead of claiming the transport is healthy.

**Why this priority**: The wrong "healthy" report is the defect the operator actually sees; it hides that no webhook can reach the daemon.

**Independent Test**: With a stubbed authoritative nameserver that first returns no records, read the transport health row and the aggregate webhook mode; then make the stub return a public address and read them again.

**Acceptance Scenarios**:

1. **Given** the node is up and the authoritative answer has no A/AAAA records, **When** health is read, **Then** the transport's `funnel` field is `pending-dns`, its state is a distinct not-publicly-resolvable value rather than `listening`, and the aggregate webhook mode does not read healthy.
2. **Given** the name was pending, **When** the authoritative answer gains a public A or AAAA record, **Then** `funnel` reports `ok`, the state returns to `listening`, and the aggregate mode can read healthy again.
3. **Given** the answer contains only tailnet-range addresses, **When** health is read, **Then** the name still counts as unresolved (`pending-dns`).
4. **Given** the probe itself fails (NS lookup error, no network), **When** health is read, **Then** `funnel` reports `"unknown"`, never `ok`.

---

### User Story 2 - Missed deliveries are recovered once the name resolves (Priority: P1)

Deliveries that GitHub failed to make during the propagation window are picked up automatically as soon as the name resolves.

**Why this priority**: Without this, events from the window are only seen at the next periodic reconcile, so work stalls at poll latency or longer.

**Independent Test**: With a stub that moves from no records to public records, count calls to the transport's reconnect handler; repeat with a stub that resolves at the first probe.

**Acceptance Scenarios**:

1. **Given** the name was unresolved at the first probe, **When** it first resolves, **Then** the reconnect handler is called exactly once (requesting a catch-up reconcile and the #2144 replay pass).
2. **Given** the name resolves at the first probe (for example a restart that reuses the node), **When** probing completes, **Then** the reconnect handler is never called.
3. **Given** the name has resolved and the handler has been called, **When** time passes or the node stays up, **Then** no further probing happens and the handler is not called again for the life of the node.

---

### User Story 3 - Operator is told when propagation is taking too long (Priority: P2)

If the name has not resolved after a grace period, the warnings panel names the hostname and the likely causes.

**Why this priority**: Propagation normally ends on its own; a persistent failure is a configuration problem the operator must act on.

**Independent Test**: Advance a test clock past the grace period with an unresolving stub and check the panel; then let it resolve and check the entry clears.

**Acceptance Scenarios**:

1. **Given** the name is unresolved and the grace period (10 minutes) has elapsed, **When** the panel is read, **Then** an entry names the hostname and lists the likely causes: propagation still in progress, Funnel not granted to the node's tag, HTTPS certificates disabled.
2. **Given** that entry exists, **When** the name resolves, **Then** the entry is resolved.
3. **Given** the grace period has not elapsed, **When** the panel is read, **Then** no entry exists yet.

---

### User Story 4 - Probing never gets in the way (Priority: P1)

A probe failure never affects startup, serving, polling or reconcile.

**Why this priority**: The probe is diagnostic and recovery tooling. It must not add a new way for the daemon to fail.

**Independent Test**: Make the probe's NS lookup fail and confirm startup completes, the listener still accepts and handles requests, and the failure is logged at most once per backoff level.

**Acceptance Scenarios**:

1. **Given** the NS lookup errors, **When** the daemon starts, **Then** startup succeeds, the listener serves, and health reports `"unknown"` for `funnel`.
2. **Given** repeated probe failures at the same backoff level, **When** logs are read, **Then** the failure appears at most once per level.
3. **Given** `event_source: direct`, **When** the daemon runs, **Then** no probe is started.

---

### Edge Cases

- Answer with both tailnet-range and public records: counts as resolved (at least one record outside the tailnet ranges).
- Answer with only AAAA records outside `fd7a:115c:a1e0::/48`: counts as resolved.
- NODATA or NXDOMAIN from the authoritative server: unresolved, not an error.
- NS discovery fails or the nameservers are unreachable: probe error, reported as `"unknown"`, retried with backoff.
- Restart on a node that reuses stored state: normally resolves at the first probe, so no reconnect call.
- The first probe errors and a later probe resolves: treated as the first successful resolution (see Assumptions).
- Daemon shutdown while probing: the probe stops with the transport.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Under `event_source: tailscale`, once the node is up, the system MUST probe whether the node's Funnel hostname resolves publicly.
- **FR-002**: The probe MUST query the authoritative nameservers for the name's zone directly, discovering them by an NS lookup, and MUST NOT use the local/system resolver or any public recursive resolver.
- **FR-003**: The name MUST count as resolved only when the answer contains at least one A or AAAA record outside `100.64.0.0/10` and `fd7a:115c:a1e0::/48`.
- **FR-004**: Until resolved, the probe MUST retry with backoff (starting around 10 s, growing to a 60 s ceiling); once resolved it MUST stop probing for the life of the node.
- **FR-005**: Until resolved, the transport's `funnel` health field MUST report `pending-dns`, and its state MUST be a distinct value (for example `listening (not publicly resolvable)`) rather than `listening`.
- **FR-006**: While the name is unresolved the aggregate `webhook.mode` MUST NOT read healthy.
- **FR-007**: When the probe itself fails, the `funnel` field MUST report `"unknown"` and never `ok`.
- **FR-008**: If the name is still unresolved after a fixed grace period of 10 minutes (a constant, not configuration), the system MUST report a warnings-panel entry naming the hostname and the likely causes (propagation still in progress, Funnel not granted to the node's tag, HTTPS certificates disabled), and MUST resolve that entry when the name resolves.
- **FR-009**: On the first successful resolution after one or more unresolved or failed probes, the system MUST call the transport's reconnect handler exactly once, and MUST NOT call it again for the life of the node.
- **FR-010**: If the name resolves at the first probe, the system MUST NOT call the reconnect handler.
- **FR-011**: A probe failure MUST NOT fail startup, stop the transport serving, or change poll or reconcile cadence; failures MUST be logged at most once per backoff level.
- **FR-012**: Under `event_source: direct` the system MUST NOT probe.
- **FR-013**: The probe MUST sit behind an interface seam so tests supply authoritative answers (NODATA, records, tailnet-only records, NS failure) without network access, and a test or static check MUST pin that the production seam never reaches the system or a recursive resolver.
- **FR-014**: The USER_GUIDE tailscale setup section MUST describe the propagation window, the `pending-dns` state and the automatic recovery, replacing the existing "redeliver once it resolves" guidance; `docs/llms-full.txt` MUST be regenerated.
- **FR-015**: An ADR numbered after this issue (`adrs/2425-funnel-public-dns-probe.md`) MUST record the decision.

### Key Entities

- **Funnel hostname**: the node's public `*.ts.net` name that GitHub delivers webhooks to.
- **Transport health row**: the per-transport entry under `fabrik_health.webhook.transports`, carrying `state`, `tailnet` and `funnel`.
- **Reconnect handler**: the transport callback that requests a catch-up reconcile and triggers the #2144 replay pass.
- **Warnings-panel entry**: the operator-facing record naming the hostname and likely causes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a stub returning no records then public records, health shows `pending-dns` and then `ok`, and the reconnect handler is called exactly once at the transition.
- **SC-002**: Tailnet-only answers are never counted as resolved in any test.
- **SC-003**: A name that resolves at the first probe produces zero reconnect-handler calls.
- **SC-004**: Past the 10-minute grace period the panel entry appears, and it clears on resolution.
- **SC-005**: Probe errors never fail startup or stop the listener in any test; they surface as `"unknown"` and are logged at most once per backoff level.
- **SC-006**: A test or static check demonstrates the probe cannot reach the system resolver or a recursive resolver.
- **SC-007**: A live run on a test bed with a new node name shows `pending-dns` until GitHub can reach the name, and the replay pass then redelivers a delivery that failed in the window; the result is recorded in the PR.
- **SC-008**: Docs updated, `docs/llms-full.txt` regenerated, ADR present, and `go test -race ./...` passes.

## Assumptions

- This issue builds on #2143 (tailscale transport), #2142 and #2144, and cannot start Implement until #2143 has landed (it is currently blocked on #2143).
- "Authoritative nameservers for the name's zone" is determined by an NS lookup chosen at the plan stage; how that lookup is bootstrapped without using the system or a recursive resolver is a design matter, not a requirement here.
- The exact state string for the not-publicly-resolvable transport state may differ from the example, as long as it is distinct from `listening`; `funnel` uses exactly `pending-dns`, `ok` and `"unknown"`.
- The grace period is measured from the moment the node is up and the probe begins, and is a fixed 10-minute constant.
- If the first probe errors (unknown) and a later probe resolves, that counts as the first successful resolution and triggers the one reconnect call; a spurious catch-up is harmless, whereas a missed one loses deliveries.
- The "first probe" for AC3 is the first probe that yields a definite answer (records or no records); a name that answers with public records at that point never triggers the handler.
- Probe state is in memory only; a restart starts a fresh node lifetime and probes again.

## Out of Scope *(optional)*

- Any change to poll or reconcile cadence.
- Probing under `event_source: direct` (the public name belongs to an external proxy).
- Changes to #2144's replay logic beyond being triggered by the new reconnect call.
- Making the grace period or backoff configurable.

## Source References *(optional)*

- #2143 live check showing the ~15 minute propagation: https://github.com/handarbeit/fabrik/pull/2424#issuecomment-6364451352
- #2142, #2144, ADR-043; mentioned for context: #2081
