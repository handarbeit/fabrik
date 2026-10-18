# Feature Specification: Log the cause of Hookdeck reconnects in the engine and Pruefer

**Feature Branch**: `fabrik/issue-2384`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description: "## Problem

When the Hookdeck event source loses its connection, the error that caused it is never logged, in either the engine or Pruefer.

`internal/events/hookdeck/source.go`'s `Run` calls `emitHealth(HealthReconnecting, err)` after each failed or dropped `runOnce`, and `emitHealth` copies `err.Error()` into `events.HealthEvent.Err`. Neither consumer reads it:

- `engine/hookdeck.go` `handleHealth` reads only `ev.State`. The only log line is `transitionHealthState`'s `health state: X → Y`, and it is written only on a **state change**. So while the stream is already unhealthy, repeated reconnect failures log nothing at all, and the first one logs the transition without its cause.
- Pruefer's `HealthHandler` (`pruefer/daemon.go`) logs only the reconnect (`event source reconnected — running a reconciliation poll`), never the error.

An operator who sees `health state: healthy → unhealthy` cannot tell a DNS failure from an auth rejection, a TLS error or a server close without attaching a debugger.

## Requirements

1. When a `HealthEvent` carries a non-empty `Err`, log it once with the transition, and include it on the first unhealthy transition.
2. Do not log one line per reconnect attempt forever. Repeated identical errors during one unhealthy episode should be rate-limited or coalesced, for example: log the first, then log again when the error text changes or every N minutes with a count.
3. Apply the same rule in Pruefer's health handler. Share the helper if that is natural; the two daemons already share `internal/events`.
4. Sanitise nothing beyond what `err.Error()` already gives. The error comes from the WebSocket and HTTP client, not from webhook payload content. Confirm no secret (connection token, signing secret) can appear in it.

## Scope

- In scope: `engine/hookdeck.go` (`handleHealth` / `transitionHealthState`) and `pruefer/daemon.go` (`HealthHandler`). Optionally a small helper in `internal/events`.
- Out of scope: changing health states, the warnings panel, or reconnect/backoff behaviour.

## Acceptance

- A test that feeds `HealthEvent{State: HealthReconnecting, Err: \"dial tcp: …\"}` sees the error text in the log on the first unhealthy transition.
- A test that feeds the same error repeatedly sees it coalesced, not logged once per event.
- A clean `HealthConnected` event logs no error."

## Background

When the Hookdeck event source loses its connection, the error that caused it is never logged, in either the engine or Pruefer.

The Hookdeck source reports every failed or dropped connection attempt as a reconnecting health event. That event carries the cause as text in an error field. Neither daemon reads it.

- **Engine.** The health handler looks only at the event's state. The only log line is the state-transition line, and it is written only when the combined state changes. While the stream is already unhealthy, repeated reconnect failures log nothing. The first failure logs the transition without its cause.
- **Pruefer.** The health handler logs only the reconnect (`event source reconnected — running a reconciliation poll`), never the error.

An operator who sees `health state: healthy → unhealthy` cannot tell a DNS failure from an auth rejection, a TLS error or a server close without attaching a debugger.

Findings from reading the source, which inform Requirement 4:

- The API key is sent in an `Authorization` header and is not placed in the URL. Go's HTTP and WebSocket client errors name the URL, host and failure reason, not request headers.
- When session creation is rejected, the error text includes up to 4 KiB of the HTTP response body from Hookdeck. That text can be long or multi-line. It is the server's own reply, not webhook payload content.
- Errors come from session creation, the WebSocket dial and the read loop. None of them carries webhook payload content or the signing secret.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See why the stream went unhealthy (Priority: P1)

An operator running the engine sees the stream drop. The log line for the first unhealthy transition says why (for example a DNS failure, an auth rejection, a TLS error or a server close).

**Why this priority**: This is the core gap. Without it, diagnosing a lost connection needs a debugger.

**Independent Test**: Feed the engine's Hookdeck health handler a reconnecting event with a non-empty error while the stream is healthy. Check that the log shows the transition and the error text.

**Acceptance Scenarios**:

1. **Given** the stream is healthy, **When** a reconnecting event with error `dial tcp: lookup api.hookdeck.com: no such host` arrives, **Then** the log contains the `healthy → unhealthy` transition and that error text.
2. **Given** the stream is healthy, **When** a reconnecting event with an empty error arrives, **Then** the transition is logged as it is today, with no error text.

---

### User Story 2 - Repeated failures do not flood the log (Priority: P1)

While the stream stays unhealthy, reconnect attempts keep failing, often with the same error. The operator sees the cause once, then sees a new line only when the cause changes or after a reminder interval, with a count of suppressed repeats.

**Why this priority**: Without coalescing, logging every error would produce one line per reconnect attempt for as long as an outage lasts.

**Independent Test**: Feed the same error many times during one unhealthy episode. Check that the log contains it once, not once per event. Feed a different error and check that it is logged.

**Acceptance Scenarios**:

1. **Given** an unhealthy episode where the error was already logged, **When** the same error text arrives again within the reminder interval, **Then** nothing new is logged.
2. **Given** the same state, **When** an error with different text arrives, **Then** it is logged.
3. **Given** the same error text keeps arriving past the reminder interval, **When** the next occurrence arrives, **Then** one line is logged stating the error and the number of occurrences since the last line.
4. **Given** the stream recovered and a new unhealthy episode begins, **When** the first error arrives, **Then** it is logged even if its text equals an error from an earlier episode.

---

### User Story 3 - Pruefer behaves the same way (Priority: P2)

An operator running Pruefer gets the same diagnostics from its health handler, with the same coalescing rule.

**Why this priority**: Pruefer has the same gap, but the engine is the larger deployment and Story 1 delivers the main value.

**Independent Test**: Feed Pruefer's health handler the same event sequences as Stories 1 and 2 and check the log output.

**Acceptance Scenarios**:

1. **Given** Pruefer's event source is connected, **When** a reconnecting event with an error arrives, **Then** the error text appears in the log.
2. **Given** an unhealthy episode, **When** the same error repeats, **Then** it is coalesced as in User Story 2.
3. **Given** a connected event, **When** it is handled, **Then** the existing reconnect message and reconciliation poll are unchanged.

---

### Edge Cases

- A connected event carries no error and must log no error line.
- Multiple events with an empty error during an unhealthy episode log nothing about an error.
- A very long or multi-line error (such as an HTTP response body echoed in a session-creation failure) must not produce an unbounded or multi-line log entry.
- Other conditions can also make the engine's combined state unhealthy (signature drift, cache drift). A reconnect error arriving while one of these is active is still logged under the same coalescing rule, and it does not change which reason the transition line reports.
- Events can arrive from the source's goroutine while other health inputs change state, so the coalescing state must be safe under concurrent use.
- A daemon restart resets the coalescing state, so the first error after a restart is logged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a Hookdeck health event with a non-empty error text arrives and it starts an unhealthy episode, the engine MUST log the error text on, or with, the `health state: … → unhealthy` transition.
- **FR-002**: Within one unhealthy episode, a repeat of the most recently logged error text MUST NOT be logged on every occurrence. The engine MUST log it again only after a defined reminder interval, as a single line that states the number of occurrences since the last line.
- **FR-003**: Within one unhealthy episode, an error whose text differs from the most recently logged one MUST be logged when it arrives.
- **FR-004**: A recovery (a connected event) MUST end the episode and reset the coalescing state, so the first error of the next episode is logged. A connected event MUST NOT log an error line.
- **FR-005**: Pruefer's health handler MUST apply the same rules (FR-001 to FR-004) to the same health events and log the cause alongside its existing reconnect message.
- **FR-006**: The coalescing rule MUST be implemented once and shared by both daemons, in the package they already share for events, unless that proves unnatural.
- **FR-007**: Logged error text MUST be the event's error text and nothing more, except that, for logging only, it is collapsed to a single line and truncated to a fixed maximum length. No other redaction or rewriting is applied.
- **FR-008**: The change MUST NOT alter health states, reconnect or backoff behaviour, the warnings panel, or the event source's emission of health events.
- **FR-009**: Tests MUST cover: the error text appearing on the first unhealthy transition; repeated identical errors being coalesced; a changed error being logged; the reminder line with a count after the interval; a clean connected event logging no error; and an equivalent set for Pruefer's handler.

### Key Entities *(if applicable)*

- **Health event**: A transport-health report from an event source: state (connected or reconnecting), time and optional error text.
- **Unhealthy episode**: The period from the first reconnecting event after a connected state until the next connected event. Error coalescing is scoped to one episode.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every unhealthy episode that begins with an error, the engine log contains that error text at least once, on or next to the transition line.
- **SC-002**: During an outage of any length with an unchanging error, the number of log lines carrying that error is bounded by one plus the number of reminder intervals elapsed, not by the number of reconnect attempts.
- **SC-003**: An error whose text changes mid-outage appears in the log within one event of the change.
- **SC-004**: A healthy connect, and a recovery, add no error lines.
- **SC-005**: The Pruefer log shows the same cause and the same coalescing behaviour for the same event sequence.
- **SC-006**: No credential (API key, connection token, signing secret) appears in any logged error text for the error paths of session creation, WebSocket dial and the read loop.

## Assumptions

- The reminder interval is a fixed value chosen in planning (the issue suggests "every N minutes"; a value in the range of a few minutes to ten is reasonable). It is not operator-configurable.
- "Repeated identical errors" means exact error-text equality. Errors that differ only in volatile detail (such as a port number) are treated as different and logged. This errs on the side of showing information.
- The issue says to sanitise nothing beyond `err.Error()`. The one exception is the logging-only collapse to a single line and length cap in FR-007. This is needed because session-creation errors can embed up to 4 KiB of a multi-line HTTP response body, and it does not alter what a reader learns about the cause.
- No credential can appear in these errors: the API key travels only in an `Authorization` header, which Go client errors do not print, and the URLs involved carry no secret. FR-009's tests include a check on that point.
- The same reasoning covers Pruefer's use of the shared source. If Pruefer constructs the source with a different URL or credential arrangement, the planning stage will verify it.

## Out of Scope *(optional)*

- Changing health states, adding states, or changing how the combined state is computed.
- The warnings panel and any operator-warning entries for connection errors.
- Reconnect, backoff and timeout behaviour.
- Metrics or counters beyond the count carried in a reminder line.
- Event sources other than Hookdeck, unless they use the same health handler path.

## Source References *(optional)*

- `internal/events/hookdeck/source.go` (`Run`, `runOnce`, `emitHealth`), `internal/events/hookdeck/client.go` (`createSession`)
- `internal/events/events.go` (`HealthEvent`, `HealthState`)
- `engine/hookdeck.go` (`handleHealth`, `recomputeHealthState`, `transitionHealthState`)
- `pruefer/daemon.go` (`HealthHandler`)
