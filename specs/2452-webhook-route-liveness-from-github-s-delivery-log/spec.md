# Feature Specification: Webhook route liveness from GitHub's delivery log

**Feature Branch**: `fabrik/issue-2452`
**Created**: 2026-10-25
**Status**: Specified
**Input**: User description: the original issue text, quoted verbatim:

> ## Problem
>
> The Funnel public-DNS probe (#2425, ADR 2425) walks the DNS delegation from the root hints with RD=0. On a network that redirects all port-53 traffic to a local resolver it never gets an authoritative answer, so it can only report `funnel: unknown`. The 0.0.90 gate host is behind a Firewalla router, which does this: the roots, 1.1.1.1, 8.8.8.8, 9.9.9.9 and an unrouted TEST-NET address (203.0.113.7) all answer `REFUSED` with RA set and EDNS 1232, over IPv4, IPv6 and TCP. Corporate networks, a forced Pi-hole and some ISPs behave the same way. Such networks are to be tolerated, not worked around.
>
> The DNS probe also answers a narrower question than the one that matters. What matters is whether GitHub can deliver to the receiver: GitHub's resolvers find the name, Tailscale routes the Funnel, the receiver acknowledges. GitHub records each attempt in the App's delivery log, so it can say this directly.
>
> ## Requirements
>
> - The tailscale transport (and, where it applies, `direct`) reports webhook route liveness from GitHub's own view of delivery: `reachable`, `unreachable` with a reason when one is known, or `unknown`.
> - **Passive signal:** the App delivery log (`GET /app/hook/deliveries`, already read by the replayer, #2144). A recent 2xx attempt to the current hook URL is reachable; a run of failed attempts is unreachable, with GitHub's status text as the reason.
> - **Active signal:** when no recent delivery establishes liveness (startup, a transport reconnect, and a quiet period of N minutes), request a redelivery (`POST /app/hook/deliveries/{id}/attempts`) of a delivery the intake ledger already holds. The intake's dedupe must make it a no-op for the board: it must never be applied. Its arrival (duplicate counter) or the new attempt's status in the log decides liveness. With no eligible delivery the state stays `unknown` until a real delivery arrives.
> - A network that intercepts DNS never produces a warning or an unhealthy webhook mode on its own.
> - Decide whether the authoritative DNS probe stays as a secondary hint or is removed (with its `pending-dns` state, 10-minute grace and the replay hold that depends on it), and record the decision in an ADR.
> - Liveness reaches `fabrik_health.webhook.transports[]` and the warnings panel through the existing paths.
>
> ## Research questions
>
> 1. Does the App webhook have a ping endpoint? Repository hooks do (`POST /repos/{o}/{r}/hooks/{id}/pings`); App hooks are believed not to, which is why redelivery is proposed.
> 2. How long does GitHub keep the App delivery log? This bounds how old a redelivered delivery can be, and whether the intake ledger (24 h) still holds it.
> 3. The exact status text and status code GitHub records for DNS, connect, TLS and 5xx failures, if a reason is to be reported.
> 4. Rate limits or abuse limits on redelivery requests, and a safe cadence for the active check.
>
> ## Scope
>
> - In: tailscale (and direct) transport liveness from the delivery log plus active redelivery; the decision on the DNS probe; sim or engine tests that use the existing replay seams (`SetReplayAPIForTest`).
> - Out: changing anything on the network side; Hookdeck transport health.
>
> ## Acceptance
>
> - On a host behind DNS interception, a healthy bed reports `reachable` within a minute of startup when it holds an eligible delivery, with no warning.
> - With the hook URL pointed somewhere unreachable, the transport reports `unreachable` with GitHub's reason.
> - A redelivered probe delivery is never applied (pinned by a test that fails with the dedupe neutralised).
> - The live transport test (`TestWebhookTransportHealthAndBrokenRoute`) asserts the new liveness field instead of accepting `funnel: unknown`.
>
> Mentioned for context: #2425, #2446.

## Background

With the tailscale transport (and the `direct` transport), the engine needs to tell an operator whether GitHub can actually deliver webhooks to the receiver. Today the only reachability signal for the tailscale transport is the public-DNS probe from #2425 (ADR 2425). It walks the DNS delegation itself, starting at the root servers with recursion off, and trusts only authoritative answers.

That probe fails on networks that intercept port-53 traffic. The 0.0.90 gate host sits behind a Firewalla router that redirects every DNS query, including queries to the roots, public resolvers and an unrouted test address, to a local resolver. That resolver answers `REFUSED`, so the probe never gets an authoritative reply. Corporate networks, a forced Pi-hole and some ISPs behave the same way. The probe can then only report `funnel: unknown`. The operator gets no useful answer, and the 10-minute grace can raise a noisy warning. Such networks must be tolerated, not worked around.

The probe also answers a narrower question than the one that matters. Whether the name resolves publicly says nothing about whether Tailscale routes the Funnel to the node or whether the receiver acknowledges. GitHub's own resolvers do the lookup that matters, and GitHub records every attempt (status code, status text, time) in the App's delivery log. That log is already read by the delivery replayer (#2144), so it can answer the real question directly. When the log has nothing recent, the engine can ask GitHub to redeliver an old delivery the intake ledger already holds. That redelivery is a probe whose result is read back from the log or from the arrival at the receiver, and it must never change board state.

Findings from preliminary research (to be confirmed in the Research stage):

- GitHub's REST docs list the App webhook endpoints as `GET /app/hook/config`, `PATCH /app/hook/config`, `GET /app/hook/deliveries`, `GET /app/hook/deliveries/{delivery_id}` and `POST /app/hook/deliveries/{delivery_id}/attempts`, all authenticated with the App JWT. No ping endpoint is documented for App hooks, which supports redelivery as the active probe.
- The list and get endpoints return `status`, `status_code`, `redelivery`, `delivered_at` and `throttled_at`, and the list can be filtered to success or failure.
- GitHub's troubleshooting docs describe a `failed to connect to host` outcome for names GitHub cannot resolve or reach. The delivery-log retention period and any redelivery rate limit are not stated in the pages consulted and are left to Research.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Healthy receiver behind DNS interception reports reachable (Priority: P1)

An operator runs the tailscale transport on a host whose network redirects all DNS queries. The authoritative DNS walk can never succeed there. The operator wants the webhook route reported accurately and no spurious warning.

**Why this priority**: This is the failure that motivated the issue. The current probe is wrong or silent on exactly these networks.

**Independent Test**: Run the transport with the DNS probe unable to get authoritative answers (every query refused). Provide a replay API fake whose delivery log holds a recent 2xx delivery to the current hook URL. Assert the transport row reports `reachable`, the `webhook.mode` is healthy and no warning is recorded.

**Acceptance Scenarios**:

1. **Given** the delivery log holds a recent 2xx attempt to the current hook URL, **When** the transport starts, **Then** liveness is `reachable` with no warning and no unhealthy webhook mode.
2. **Given** DNS interception makes the authoritative probe return nothing definite, **When** liveness is evaluated, **Then** no warning is raised and the webhook mode is not made unhealthy on that account.
3. **Given** no recent delivery exists but the intake ledger holds an eligible past delivery, **When** the transport starts, **Then** it requests a redelivery of that delivery, and on the redelivered copy arriving at the receiver (counted as a duplicate) or the new attempt showing 2xx in the log, liveness becomes `reachable` within a minute.

---

### User Story 2 - Broken route reports unreachable with GitHub's reason (Priority: P1)

An operator misconfigures the hook URL, or the route breaks (Funnel not granted, name not resolving at GitHub). They want the transport to say deliveries are failing and why, using GitHub's words.

**Why this priority**: Reporting a broken route correctly is the other half of replacing the DNS probe as the reachability signal.

**Independent Test**: Seed the delivery log fake with a run of failed attempts (for example `502` with a `failed to connect to host` status text) to the current hook URL and no later 2xx. Assert liveness is `unreachable` and carries that text.

**Acceptance Scenarios**:

1. **Given** the most recent attempts to the current hook URL are a run of failures with no later success, **When** liveness is evaluated, **Then** it is `unreachable` and the reason is GitHub's status text.
2. **Given** `unreachable` and a later 2xx attempt to the current hook URL, **When** liveness is re-evaluated, **Then** it returns to `reachable` and any warning for the unreachable state clears.
3. **Given** failed attempts that were sent to an earlier hook URL, **When** liveness is evaluated for the current URL, **Then** they do not count against the current route.

---

### User Story 3 - Redelivered probe is never applied (Priority: P1)

The active check redelivers a past delivery. That delivery has already been applied to the board. If the redelivered copy were applied again, it could write stale state.

**Why this priority**: The active signal is only safe if the probe cannot change board state. This is a correctness requirement, not an optimisation.

**Independent Test**: Redeliver a delivery the intake ledger already holds and assert the item store, the board cache and the action counters are unchanged and the duplicate counter increments. Neutralise the intake dedupe in the test and assert the same check then fails, which proves the test depends on the dedupe.

**Acceptance Scenarios**:

1. **Given** a probe redelivery of a delivery the ledger holds, **When** the copy arrives at the intake, **Then** it is counted as a duplicate and applies nothing.
2. **Given** the intake dedupe is neutralised in the test, **When** the same redelivery arrives, **Then** the no-board-change assertion fails.
3. **Given** the ledger holds no eligible delivery, **When** the active check would run, **Then** it requests nothing and liveness stays `unknown` until a real delivery arrives.

---

### User Story 4 - Operators see liveness through existing surfaces (Priority: P2)

An operator or overseer reads `fabrik_health.webhook.transports[]` and the warnings panel to see whether the webhook route works.

**Why this priority**: The value of the signal is realised only when it reaches the surfaces people already watch. No new surface is wanted.

**Independent Test**: Drive the transport through reachable, unreachable and unknown, and assert the transport row and the warnings panel entry change accordingly.

**Acceptance Scenarios**:

1. **Given** liveness changes, **When** `fabrik_health.webhook.transports[]` is read, **Then** the row carries the current liveness value and, when unreachable, the reason.
2. **Given** a sustained `unreachable`, **When** the warnings panel is read, **Then** it holds an entry naming the reason, and the entry is resolved once liveness is `reachable`.
3. **Given** `unknown`, **When** the warnings panel is read, **Then** it holds no entry for this condition.

---

### User Story 5 - Live test asserts the new field (Priority: P2)

The release gate's live test for transport health and a broken route needs to check the real behaviour, not tolerate the old `funnel: unknown`.

**Why this priority**: The gate test currently accepts the DNS-interception outcome and so cannot catch a regression of this feature.

**Independent Test**: `TestWebhookTransportHealthAndBrokenRoute` asserts `reachable` on the healthy leg and `unreachable` with a reason when the hook URL is pointed at an unreachable address.

**Acceptance Scenarios**:

1. **Given** a healthy bed, **When** the live test reads the transport row, **Then** it asserts the liveness field is `reachable` rather than accepting `funnel: unknown`.
2. **Given** the hook URL pointed at an unreachable address, **When** the live test reads the row, **Then** it asserts `unreachable` and a non-empty reason.

---

### Edge Cases

- The delivery log is empty, unreadable, or the App JWT request fails (401, 403, 404, a rate limit, a transport error): liveness stays or becomes `unknown`. A failure to read the log is never reported as `unreachable`.
- Attempts to a hook URL that is not the current one (after a URL change) are ignored for the current route.
- Redelivery attempts that GitHub records with `status_code` 0 or a throttled marker are treated according to what the log actually states, not assumed to be a route failure. The classification is settled in Research.
- A single failed attempt is not a "run of failures". The run length and recency window are fixed constants (see Assumptions).
- A real delivery arriving while a probe redelivery is pending decides liveness at once. The probe result does not overwrite a newer real delivery.
- The only eligible delivery is older than GitHub keeps it, or than the ledger holds it: the active check requests nothing and the state stays `unknown`.
- The intake ledger is cleared or overflowed: eligibility is judged from what the ledger holds now.
- A redelivery request cannot be queued twice for the same delivery inside the probe cadence, so a dead endpoint cannot turn the probe into a request loop.
- The replayer and the liveness check both use redelivery. They must not spend each other's per-delivery request budget in a way that stops the startup replay from recovering missed deliveries.
- The `direct` transport has an externally managed public name. Its liveness comes from the same log and probe, but it never claims anything about DNS.
- A network that blocks all DNS never produces a warning on its own.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The tailscale transport, and the `direct` transport where it applies, report a webhook route liveness of `reachable`, `unreachable` (with a reason when one is known) or `unknown`, derived from GitHub's own record of delivery attempts.
- **FR-002**: Passive signal: liveness is evaluated from the App delivery log (`GET /app/hook/deliveries`). A recent 2xx attempt to the current hook URL makes the route `reachable`. A run of failed attempts to the current hook URL with no later 2xx makes it `unreachable`, with GitHub's recorded status text as the reason.
- **FR-003**: Only attempts to the current hook URL count. Attempts recorded for an earlier URL, or for no determinable URL, are ignored.
- **FR-004**: Active signal: when no recent delivery establishes liveness (at startup, on a transport reconnect, and after a configured quiet period of N minutes) the engine requests a redelivery (`POST /app/hook/deliveries/{id}/attempts`) of a delivery the intake ledger already holds.
- **FR-005**: A probe redelivery is never applied. The intake's dedupe must treat it as a duplicate, and the probe path must neither call the intake sink nor submit anything to the intake. A test pins this and fails when the dedupe is neutralised.
- **FR-006**: The outcome of an active check is decided by either the redelivered copy arriving at the receiver (the duplicate counter) or the new attempt's status in the delivery log, whichever is observed first.
- **FR-007**: When no eligible delivery exists, the active check requests nothing and liveness stays `unknown` until a real delivery arrives.
- **FR-008**: A failure to read the delivery log or to request a redelivery (authorization failure, not found, rate limit, transport error) leaves liveness `unknown` and never reports `unreachable`.
- **FR-009**: A network that intercepts or blocks DNS never, by itself, produces a warning or an unhealthy webhook mode. Neither the authoritative DNS probe's inability to get an answer nor its absence may degrade health or raise a panel entry.
- **FR-010**: The fate of the authoritative DNS probe is decided and recorded in an ADR: kept as a secondary hint, or removed together with its `pending-dns` state, its 10-minute grace panel entry and the startup-replay hold that depends on it. Whichever is chosen, FR-009 holds and the startup replay can never be disabled by a broken DNS path.
- **FR-011**: Liveness reaches `fabrik_health.webhook.transports[]` (value, plus reason when unreachable) and the operator warnings panel through the existing reporting paths. A sustained `unreachable` raises a panel entry that is resolved when the route recovers, and `unknown` raises none. No new surface or command is added.
- **FR-012**: Tests use the existing replay seams (`SetReplayAPIForTest` and the replay wait seam) so no live GitHub call is needed for the unit and sim layers.
- **FR-013**: `TestWebhookTransportHealthAndBrokenRoute` asserts the new liveness field on the healthy and broken-route legs instead of accepting `funnel: unknown`.
- **FR-014**: Probe cadence and request volume are bounded: a fixed minimum interval between active checks and at most one outstanding probe redelivery at a time, so a dead endpoint cannot loop. The exact numbers are set once GitHub's limits are known.
- **FR-015**: The liveness check and the delivery replay (#2144) coexist. Neither may use up the other's per-delivery redelivery budget so as to stop startup recovery of missed deliveries.
- **FR-016**: Docs that describe the transport health values and warnings (`docs/state-machine.md` §7.8b and the `docs/USER_GUIDE.md` webhook section) are updated to match, and `docs/llms-full.txt` is regenerated in the same change.

### Key Entities

- **Webhook route liveness**: A per-transport value, `reachable`, `unreachable` or `unknown`, with an optional reason. It is derived from GitHub's delivery attempts and is distinct from the listener state and from DNS resolvability.
- **App hook delivery attempt**: GitHub's record of one delivery try, with status code, status text, time, redelivery flag and the delivery GUID. Attempts are read from the App delivery log.
- **Probe redelivery**: A redelivery request for a delivery the intake ledger already holds, issued only to learn whether delivery works. Its copy is deduplicated and never applied.
- **Intake delivery ledger**: The persisted, bounded record of deliveries already accepted. It supplies eligible probe candidates and is what makes a redelivered copy a duplicate.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a host behind DNS interception, a healthy bed holding an eligible delivery reports `reachable` within 60 seconds of startup, with zero warnings.
- **SC-002**: With the hook URL pointed somewhere unreachable, the transport reports `unreachable` with GitHub's recorded reason within the quiet-period or probe cadence, and the warnings panel carries one entry naming that reason.
- **SC-003**: Across all unit, sim and live tests, a probe redelivery changes no board state. The pinned test fails when the intake dedupe is neutralised.
- **SC-004**: A network that refuses every DNS query yields no webhook warning and no unhealthy webhook mode attributable to DNS.
- **SC-005**: A single stopped or failing GitHub API call yields `unknown`, never `unreachable`, across the failure kinds in FR-008.
- **SC-006**: The live gate test asserts a specific liveness value on both the healthy and broken-route legs and passes on the gate host.
- **SC-007**: The number of redelivery requests issued by the liveness check stays within the bounds in FR-014 over a 24-hour quiet period on a dead endpoint.

## Assumptions

- GitHub's list endpoint returns enough information per attempt (status code, status text, time, delivery GUID, redelivery flag) and the hook configuration endpoint returns the current URL, so attempts can be tied to the current hook URL. Research confirms the exact fields.
- The App delivery log retention is in the order of days (GitHub documents redelivery for recent deliveries only) and is at least as long as the intake ledger's 24 hours, so an eligible ledger entry is normally still redeliverable. Research confirms the retention and what happens on an expired delivery.
- A "run of failed attempts" is a small fixed number of consecutive recent failures (initially 3, within a recent window), set as constants. A quiet period of N minutes is also a constant (initially 15). All are tunable in the Plan stage. They are not operator configuration in this issue.
- An attempt counts as failed when its status code is outside 200 to 299 (including 0 and 3xx), matching the replayer's rule. Throttled attempts are neither success nor failure evidence.
- Ping is unavailable for App hooks, so redelivery is the only active probe. Research confirms this.
- The probe redelivery goes through the same GitHub App credentials the replayer uses and needs no new permission.
- Liveness is held in memory, like the existing transport health. A restart re-evaluates at startup.
- Hookdeck and the poll-only configuration have no route liveness to report, and their health rows are unchanged.

## Out of Scope *(optional)*

- Any change to the network side (DNS, router, Pi-hole, Tailscale or Funnel settings).
- Hookdeck transport health.
- Making the DNS probe work through interception (an explicit non-goal; such networks are tolerated).
- Changing the delivery replay's recovery logic, beyond what FR-015 requires to coexist.
- Operator configuration for probe cadence or run length.
- A new health surface, MCP tool or CLI command.

## Source References *(optional)*

- Issue #2452; mentioned for context: #2425, #2446, #2144
- ADR 2425 (Funnel public-DNS probe), ADR 2144 (delivery replay), ADR 2143 (embedded Tailscale Funnel receiver), ADR 2142 (direct App webhook transport), ADR 2351 (transport-neutral webhook intake)
- `docs/state-machine.md` §7.8b
- GitHub REST docs, webhooks for GitHub Apps: https://docs.github.com/en/rest/apps/webhooks
- GitHub troubleshooting webhooks (failed to connect to host): https://docs.github.com/en/enterprise-server@3.18/webhooks/testing-and-troubleshooting-webhooks/troubleshooting-webhooks
