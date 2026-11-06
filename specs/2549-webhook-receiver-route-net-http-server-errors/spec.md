# Feature Specification: Webhook receiver — route net/http server errors through the engine log and count TLS handshake failures

**Feature Branch**: `fabrik/issue-2549`
**Created**: 2026-11-04
**Status**: Specified
**Input**: User description: "## Problem

Under `event_source: tailscale`, the App webhook receiver's `http.Server` (`engine/app_webhook.go`, `appWebhookTransport.Start`) has no `ErrorLog`. The Funnel listener is TLS-terminated, so `net/http` logs every failed handshake (`http: TLS handshake error from …: EOF`) through the standard `log` package to stderr. Certificate-transparency scanners probe the new `*.ts.net` name continuously, so these lines arrive every few seconds to minutes and draw over the TUI. They never reach `.fabrik/fabrik.log`. Delivery is unaffected.

Reported in community report #2502 (mention by number only).

## Requirements

- R1: The receiver's `http.Server` gets an `ErrorLog` that never writes raw to stderr.
- R2: TLS handshake failures are counted per transport (e.g. `tls_handshake_failures` in `fabrik_health.webhook.transports[]`, next to the existing malformed/signature counters) and logged to the engine log at a bounded rate (one summary line per interval, not one per failure).
- R3: Every other `net/http` server error stays visible, routed through the engine logger.
- R4: Handshake noise raises no warning and does not affect the route-liveness verdict.
- R5: `event_source: direct` uses the same server and gets the same `ErrorLog`.

## Acceptance

- A test that drives a failed TLS handshake (or calls the `ErrorLog` writer with the handshake line) shows nothing on stderr, the counter incremented, and at most one rate-limited log line.
- A test shows another server error line still reaches the engine log.
- Non-vacuity: removing the `ErrorLog` assignment fails the stderr test.

## Linkage

Mention #2502 in the PR body; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) with it. The community report is closed by hand when a release ships."

## Background

With `event_source: tailscale`, the webhook receiver is reachable on a public Funnel hostname under `*.ts.net`. The listener terminates TLS itself. Certificate-transparency scanners find a new hostname quickly and keep probing it, and most of those probes abort the TLS handshake.

The receiver's HTTP server has no error logger of its own. Go's `net/http` therefore reports each failed handshake (`http: TLS handshake error from …: EOF`) through the standard library logger, which writes directly to the process's stderr. When Fabrik runs with its terminal UI, those lines are drawn over the display every few seconds to minutes. They never reach the engine log (`.fabrik/fabrik.log`), so an operator watching the log sees nothing and one watching the TUI sees garbage.

Delivery is unaffected: no webhook is lost and no verdict changes. The harm is operator-facing noise and a missing record. Handshake failures are not an engine fault and need no action, so they should be counted and summarized, not printed one by one.

The same HTTP server serves `event_source: direct` (App auth, operator-supplied listener). Its error logging is equally unconfigured, so the fix covers both modes. This was reported in community report #2502 (mentioned by number only).

The receiver's health row (`fabrik_health.webhook.transports[]`) already carries per-transport counters for rejected signatures, malformed requests and oversized requests. A handshake-failure counter belongs next to them.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A quiet terminal under Funnel probing (Priority: P1)

An operator runs Fabrik with `event_source: tailscale` and the TUI open. Internet scanners hit the Funnel hostname and abort TLS handshakes. The operator wants the TUI to stay clean and the engine log to show that probing is happening, without a line per probe.

**Why this priority**: This is the reported defect. The visible harm is the corrupted terminal display, and the missing log record comes with it.

**Independent Test**: Make the receiver's server see a failed TLS handshake (a real aborted handshake against a TLS listener, or the `net/http` handshake error line fed to the server's error writer) and capture stderr. Nothing may reach stderr, and the transport's handshake counter must increase.

**Acceptance Scenarios**:

1. **Given** a running App webhook receiver, **When** a client opens a TCP connection and aborts the TLS handshake, **Then** nothing is written to the process's stderr and the transport's handshake-failure count increases by one.
2. **Given** many handshake failures within one logging interval, **When** the interval ends, **Then** the engine log holds at most one summary line for that interval, and the summary reports how many failures occurred.
3. **Given** no handshake failures in an interval, **When** the interval ends, **Then** no summary line is logged.

---

### User Story 2 - Other server errors stay visible (Priority: P1)

An operator needs real HTTP-server faults (accept errors, panics in handlers, protocol errors) to stay diagnosable, now in the engine log instead of raw stderr.

**Why this priority**: Silencing only the noise must not hide genuine faults. Without this the fix trades one problem for a worse one.

**Independent Test**: Feed the server's error writer a non-handshake `net/http` error line. It must appear in the engine log and not on stderr, and the handshake counter must not change.

**Acceptance Scenarios**:

1. **Given** a running receiver, **When** `net/http` reports a server error that is not a TLS handshake failure, **Then** the line is written to the engine log (not stderr), is not rate-limited into a summary, and the handshake counter is unchanged.

---

### User Story 3 - Handshake noise is not a health signal (Priority: P2)

An operator relies on the warnings panel and the route-liveness verdict (`reachable`/`unreachable`/`unknown`) to tell when the receiver is really broken. Scanner noise must not trip either.

**Why this priority**: A false warning or a wrongly `unreachable` route would send operators chasing a non-problem, which is worse than the log noise being fixed.

**Independent Test**: Record a burst of handshake failures, then read the warnings panel and the transport's route verdict. Neither may change.

**Acceptance Scenarios**:

1. **Given** a healthy receiver whose route verdict is `reachable`, **When** a burst of handshake failures is recorded, **Then** no warning is raised and the verdict and the transport state are unchanged.

---

### User Story 4 - Direct receiver gets the same behaviour (Priority: P2)

An operator using `event_source: direct` gets the same error routing and the same counter as the tailscale transport.

**Why this priority**: The two transports share one server, so a divergence would be a regression. The risk is low because the mechanism is shared.

**Independent Test**: Run the Story 1 and Story 2 checks against a receiver configured as `direct`.

**Acceptance Scenarios**:

1. **Given** a receiver started as `direct`, **When** the Story 1 and Story 2 scenarios are exercised, **Then** the outcomes are identical to the tailscale receiver.

---

### Edge Cases

- A handshake failure line arrives while no interval summary is pending: the counter increments at once, and the summary appears when the interval elapses.
- Receiver shutdown with failures counted but not yet summarized: the final pending count is either logged at shutdown or dropped. The counter itself is never lost while the process runs.
- Error line formats that only resemble a handshake failure (for example an unrelated error that mentions "TLS"): only the `net/http` handshake error form is counted; anything else goes to the log as an ordinary server error.
- Extremely high failure rates: log volume stays bounded by the interval, not by the failure rate.
- The counter is in-memory and resets on restart, like the neighbouring transport counters.
- A receiver with no TLS (plain HTTP listener under `direct`) produces no handshake failures; the counter stays at zero.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The App webhook receiver's HTTP server MUST have an error logger configured, and that logger MUST NOT write to the process's stderr.
- **FR-002**: A TLS handshake failure reported by the HTTP server MUST increment a per-transport handshake-failure counter.
- **FR-003**: The handshake-failure counter MUST be exposed in `fabrik_health.webhook.transports[]` for the transport, alongside the existing rejected-signature, malformed and oversized counters, and MUST read as unknown for transports that cannot report it, like those counters.
- **FR-004**: Handshake failures MUST NOT be logged one per failure. The engine log MUST receive at most one summary line per interval, naming the number of failures in that interval, and no line for an interval with none.
- **FR-005**: Every HTTP-server error that is not a TLS handshake failure MUST be written to the engine log, through the engine's logger and not stderr, without rate limiting into a summary.
- **FR-006**: Handshake failures MUST NOT raise a warning in the operator warnings panel, MUST NOT change the transport's state, and MUST NOT affect the route-liveness verdict.
- **FR-007**: `event_source: direct` and `event_source: tailscale` MUST share the same error-logger behaviour (FR-001 to FR-006).
- **FR-008**: Automated tests MUST cover: a failed handshake writes nothing to stderr and increments the counter; at most one summary line is logged for a burst; a non-handshake error line reaches the engine log; and a neutralising check — removing the error-logger assignment makes the stderr test fail.
- **FR-009**: The new health field and the log behaviour MUST be documented in the canonical docs that describe the transport health row and the webhook receivers, and the `docs/llms-full.txt` bundle MUST be regenerated with them.

### Key Entities *(if applicable)*

- **Transport health row**: The per-transport entry in `fabrik_health.webhook.transports[]`; gains the handshake-failure count.
- **Handshake-failure counter**: A per-transport, in-memory, monotonically increasing count of failed TLS handshakes seen by the receiver's HTTP server.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With the receiver exposed on a public Funnel hostname for 24 hours, zero bytes from handshake failures appear on the process's stderr or in the TUI.
- **SC-002**: The handshake-failure count in the health row increases once per failed handshake, and the engine log carries at most one handshake summary line per interval, however many failures occur.
- **SC-003**: A non-handshake HTTP-server error is visible in the engine log within the same poll cycle in which it occurs, and absent from stderr.
- **SC-004**: A burst of handshake failures changes neither the warnings panel nor the route verdict.
- **SC-005**: The new tests fail when the error-logger assignment is removed and pass when it is present.

## Assumptions

- The counter is named `tls_handshake_failures` (snake_case, matching the sibling counters). The issue gives this name as an example, and it is adopted.
- The summary interval is a fixed value on the order of a minute, not operator-configurable. Operators who need the exact rate read the counter. A configuration knob can follow if anyone asks.
- Only the standard `net/http` TLS handshake error line is classified as a handshake failure. Everything else is treated as an ordinary server error.
- Handshake failures are expected internet background noise and need no operator action, so they get a counter and a summary, never a warning.
- The counter is not persisted across restarts, like the existing transport counters.

## Out of Scope *(optional)*

- Blocking, rate-limiting or otherwise responding to probing clients.
- Changing the Funnel listener, certificate handling or the route-liveness logic.
- Other transports' logging (the Hookdeck and PAT-mode webhook managers).
- Configurable summary intervals or log levels.
- Fixing delivery behaviour, which is unaffected.

## Source References *(optional)*

- Community report #2502 (mentioned by number only; in the PR body, do NOT use a closing keyword — `Closes`/`Fixes`/`Resolves` — with it; the report is closed by hand when a release ships).
- Existing transport health row: `fabrik_health.webhook.transports[]` (rejected signatures, malformed and oversized requests).
- `docs/state-machine.md` §7.8b (webhook intake and receivers).
