# ADR 2549: Route the App webhook receiver's HTTP server errors through the engine log

## Status

Accepted.

## Context

The App webhook receiver (`event_source: direct` and `tailscale`) shares one `http.Server`
(`engine/app_webhook.go`). It had no `ErrorLog`, so `net/http` wrote its server errors through
the std `log` package to stderr. Behind the public, TLS-terminated Funnel hostname, certificate
transparency scanners abort handshakes continuously and every abort produced
`http: TLS handshake error from …: EOF` over the TUI, never reaching `.fabrik/fabrik.log`
(community report #2502, mentioned for context). Delivery was unaffected.

## Decision

1. **The server gets an `ErrorLog`** backed by a transport-owned writer (`serverErrorWriter`), built in
   `Start` from the transport's `logFn`/`name`. Nothing is written to stderr.
2. **Classification is by exact prefix** `http: TLS handshake error from `, the single handshake call site
   in `net/http`. A looser match ("contains TLS") would swallow genuine faults. Everything else is an
   ordinary server error, forwarded whole (panic stacks stay one message) and unthrottled as
   `http server: <line>`.
3. **A handshake failure is a counter only** (`tls_handshake_failures` in
   `fabrik_health.webhook.transports[]`), an atomic touched by nothing else: no warning, no transport
   state, no route-liveness input. It is expected internet background noise needing no action.
4. **A transport-owned goroutine logs one summary per fixed 60 s interval** that saw failures, none for an
   empty interval, and flushes a pending count once on `Stop`. It is joined in `Stop` (WaitGroup + done
   channel) like the other transport goroutines.
5. **Count-only summary** — scanner addresses are not logged. No configuration knob; operators who need the
   exact rate read the counter.

## Alternatives rejected

- *Lazy summary on the next failure after the interval* (no goroutine): never reports a trailing burst.
- *Rate-limit on first event, like `warnBadSignature`*: drops the suppressed count and fires at the start,
  not the end, of an interval.
- *Discard the error log (`io.Discard`)*: would hide genuine server faults (R3).
- *A warnings-panel entry*: handshake aborts are not an operator condition (ADR 2135).
- *A configurable interval*: no demand; can follow.

## Consequences

- If a future Go release changes the handshake line, handshake failures would be logged one per line (noise,
  not data loss). `TestErrorLog_RealHandshakeFailureIsCountedAndNotPrinted` makes a real handshake failure
  and fails on such a change.
- Non-handshake server error volume is unbounded by design; a hostile client could cause repeated lines.
  The same bounded-summary mechanism could be applied to a second class if that ever matters.
- The counter is in memory and resets on restart, like its sibling counters.
