# ADR 1563: Hookdeck Drop Accounting and Signature-Drift Escalation

**Status:** Accepted
**Date:** 2026-08-12
**Issue:** [#1563](https://github.com/handarbeit/fabrik/issues/1563)

## Context

ADR-1254 accepted a real risk: `pruefer/events/hookdeck/source.go` reimplements
Hookdeck's undocumented CLI-session wire protocol, and `processAttempt`'s GitHub
signature verification depends on `attempt.request.data_string` being byte-identical
to the body GitHub actually signed — a property of that protocol, not a guarantee. If
Hookdeck ever re-serializes the forwarded body, every signature check fails (see the R1
amendment to `adrs/1254-*.md` landed alongside this ADR).

That alone would be tolerable — a loud, visible failure with an obvious fix. What #1563
identified is that four individually-reasonable decisions compose into an invisible
one:

1. A failed verification drops the event (correct: never act on an unverified payload).
2. The attempt is still acked `Status: 200` regardless (correct per ADR-1254 D3 — a
   non-200 would just trigger Hookdeck-side retries of a delivery that will never
   verify).
3. The signature-failure warning is rate-limited to one line per 30s
   (`sigFailureLogInterval`, correct — needed to stay visible without flooding the log
   under sustained failure).
4. The reconciliation-fallback ticker keeps polling regardless (correct — ADR-1254 D3's
   entire point).

Composed: a **total** protocol break (every delivery failing signature verification —
a wrong secret, or a Hookdeck wire-format change) degrades Pruefer to poll-only with
zero operator-visible signal beyond a rate-limited log line nothing watches. The
observable difference between "100% of events are being discarded" and "nothing has
happened lately" was, before this issue, indistinguishable without reading the log
file.

Research confirmed the raw signal already existed in full: nine distinct drop points
across `source.go`'s `handleFrame`/`processAttempt` (6) and `pruefer/eventsink.go`'s
`ReviewFromEvent` (4, with one overlap counted once — see Decision 1) already `logf` a
specific reason before dropping. Nothing was missing detection-wise; every drop simply
terminated at a log line instead of anywhere an operator could see it without reading
logs.

## Decision

### 1. `events.DropReason`: a shared 10-value enum, not two package-local ones

`pruefer/events/events.go` gains a `DropReason` string type with ten constants,
covering both origin packages: `DropSignatureMissing`, `DropSignatureInvalid`,
`DropMalformedEnvelope`, `DropMalformedAttempt`, `DropMalformedPayload`, `DropDedupe`
(all raised by `hookdeck.Source`), and `DropUnwatchedOwner`, `DropUnwatchedRepo`,
`DropReviewInFlight`, `DropPRNotOpen` (all raised by `Daemon.ReviewFromEvent`,
`pruefer/eventsink.go`). `pruefer/events` is already mutually visible to both
`hookdeck` and `pruefer` — the same precedent `events.HealthState`/`HealthEvent`
already set for the identical cross-boundary problem (ADR-1254 D1) — so a shared type
here avoids inventing a second cross-package idiom for what is structurally the same
kind of signal (a transport-adjacent operational transition, not a `GitHubEvent`; D1's
"only `GitHubEvent` crosses the boundary" was scoped to domain events, not this).

### 2. Two new `hookdeck.Config` callbacks; direct calls where already in-package

`hookdeck` cannot import `pruefer` (`pruefer` imports `hookdeck`; the reverse would
cycle). `Config` gains `OnDrop func(events.DropReason)` and
`OnSignatureDrift func(active bool)`, mirroring `OnHealth`'s existing shape exactly —
`source.go`'s `emitDrop`/`emitSignatureDrift` helpers are direct siblings of
`emitHealth`. `eventsink.go`'s four drop points call `Daemon.recordDrop` directly: no
callback indirection is needed since `eventsink.go` already lives in `pruefer`.

`Daemon.recordDrop` is therefore the single point where every drop, from either origin,
is counted — `execute.go` wires `OnDrop: daemon.recordDrop` alongside the existing
`OnHealth`.

### 3. Cumulative totals, not deltas, in the TUI event

`pruefer/tui`'s own `Daemon.emit`/`tui_run.go` wrapper drops a TUI message under
channel backpressure rather than blocking the daemon. A delta-based counter
(`DropEvent{Reason, Delta}`) would permanently under-count on any dropped message.
`DropEvent{Reason string, Total int, At time.Time}` instead carries the daemon's
already-mutex-guarded cumulative count for that reason — the canonical count in
`Daemon.dropCounts` is never lossy; a dropped TUI message just means the footer's
display briefly lags, and self-heals on the next delivered `DropEvent` for the same
reason. `FooterComponent.Update` overwrites (`dropCounts[ev.Reason] = ev.Total`), never
increments, on receipt.

`DropEvent.Reason` and `SignatureDriftEvent` carry no dependency on `pruefer/events` —
`Reason` is the `DropReason` rendered as a plain string, matching
`ReviewCompletedEvent.Reason`'s existing "plain string, no domain-type dependency"
convention for `pruefer/tui`.

### 4. Signature-drift threshold: 20 consecutive failures, count-based, unconfigurable

R4 requires escalating "a sustained all-failures window," deliberately left undefined
by the issue for Plan to pin down. Two axes were considered:

- **Count vs. time-window.** A count of consecutive failures with zero interleaved
  successes was chosen over a rolling time-window rate. A count crosses its threshold
  on exactly one delivery, independent of timing — deterministic to test without any
  wall-clock mocking. A time-window rate would need a minimum-sample floor to avoid
  false-positiving during genuinely low-traffic periods, and Pruefer has no existing
  windowed-rate machinery to reuse (Research's own finding) — introducing one here,
  for exactly one consumer, was judged unwarranted complexity.
- **Reset-on-any-success vs. reset-only-on-a-healthy-mix.** Reset-on-any-success was
  chosen: it makes AC4's "a window with a healthy mix does not trigger" true
  unconditionally, for *any* interleaving of failures and successes, rather than only
  for interleavings that happen to fall under some tuned ratio.

`SignatureDriftThreshold = 20` is an exported package constant in `hookdeck`, not new
config — sized to resist ordinary noise (a single bad request, a brief clock skew)
without adding config surface for a value this issue's own scope explicitly warned
against over-engineering. Tracked as a plain `int` (`consecutiveSigFailures`) plus a
`bool` (`sigDriftActive`, guarding one-shot escalation/recovery firing) directly on
`Source` — unsynchronized, matching the exact single-threaded guarantee already
documented for `lastSigWarnAt` (both are touched only from `processAttempt`, itself
only ever called off `Source`'s own WebSocket read loop).

Deliberately scoped to signature failures only, not folded together with
`malformed_envelope`/`malformed_attempt`/`dedupe`: R4's text names signature
verification specifically, and blending it with R3's "expected, benign" categories
(dedupe hits, unwatched repos) would make an actionable signal indistinguishable from
noise — exactly the anti-pattern R3 exists to prevent, just moved one level up.

### 5. Escalation is a loud log line plus a TUI banner, not a mode switch

Poll fallback already always runs alongside event-driven mode regardless of signature
drift (`runEventDriven`, ADR-1254 D3) — there is nothing to switch into, only something
to say plainly. `Daemon.SignatureDriftHandler` returns the callback wired to
`OnSignatureDrift`; its log line's "continuing on poll-fallback only" phrasing
deliberately echoes `runEventDriven`'s own `sourceDone`-exit message, for one
consistent operator-facing vocabulary across both "the event source stopped running"
and "the event source is running but nothing it delivers verifies." The handler stays
transport-agnostic (no "hookdeck" in its own message) — the same convention
`HealthHandler` above it already follows, since `Daemon` has no structural reason to
know which `EventSource` implementation is in play.

The TUI banner (`FooterComponent`'s `driftBannerSegment`, `"⚠ SIGNATURE DRIFT — check
webhook secret"`) reuses the footer's existing `failStyle` idiom — the same visual
language already used for the REST rate-limit gauge's critical tier — rather than a new
component.

### 6. The drop-reason breakdown is a footer segment, appearing only once something has dropped

`FooterComponent.dropBreakdownSegment` renders `"dropped: N (sig S · unwatched U ·
dedupe D · other O)"`, aggregating the ten `DropReason` values into four display
buckets: `sig` (the two signature reasons — the sole actionable category),
`unwatched` (owner + repo), `dedupe`, and `other` (every remaining reason: malformed
envelope/attempt/payload, review-in-flight, PR-not-open). It renders nothing at all
until the first drop is recorded — R2's requirement is distinguishing "quiet" from
"actively dropping," which starts with there being no ambient noise to read when
nothing has actually happened yet. Colored `failStyle` whenever any signature failure
has ever been recorded (regardless of total), `dimStyle` otherwise — a steady trickle
of unwatched-repo/dedupe drops is expected background noise, not a problem.

## Alternatives Considered

**A time-windowed rolling failure rate instead of a consecutive-failure count.**
Rejected — see Decision 4. Requires a minimum-sample floor to avoid false-positiving on
low-traffic repos, needs wall-clock-relative test scaffolding Pruefer's `source_test.go`
suite otherwise avoids entirely, and Pruefer has no existing windowed-rate type to
build on (`events.Dedupe` is capacity-bounded, not time-windowed).

**A single lumped "dropped" counter.** Rejected outright by R3's explicit text: it
would satisfy R2's letter (something is now visible) while defeating its intent — an
operator cannot tell a 100% signature-failure incident from ordinary background
dedupe/unwatched-repo noise without per-category accounting.

**Falling back to poll-only mode explicitly (disabling the WebSocket read loop) on
sustained drift**, one of R4's two illustrative options. Rejected in favor of the
loud-log-plus-banner approach: the WebSocket connection itself is not the problem in a
signature-drift scenario — it stays healthy (`HealthConnected`) throughout, since
`HealthState` only observes transport connectivity, not payload verifiability (this gap
is exactly why R2/R4 exist as a distinct signal from `HealthEvent`). Tearing down a
healthy connection would add real complexity (reconnect-suppression bookkeeping, a
resume condition) to fix a problem that is actually about visibility, not connectivity
— and ADR-1254 D3's poll fallback already runs unconditionally regardless of event
source health, so there is no availability gap for a mode switch to close that logging
plus a banner does not already close.

**Threshold as new YAML config (`hookdeck.signature_drift_threshold`).** Rejected.
Research explicitly flagged the risk of over-engineering a daemon with, by design, no
other windowed/sustained-state machinery anywhere. A fixed, documented, revisitable
constant was judged sufficient — nothing about `SignatureDriftThreshold`'s value is
operator-specific in the way `poll_interval_seconds` or `reconciliation.*` genuinely
are.

**A generic health/metrics framework instead of a purpose-built `DropReason` enum and
two callbacks.** Rejected. Exactly two consumers (drop accounting, signature-drift
escalation) exist today; a general-purpose metrics abstraction would be speculative
generality for a daemon whose entire design philosophy (ADR-1113) favors minimal,
directly-reasoned-about state over frameworks.

## Consequences

- An operator watching Pruefer's TUI in `event_source: hookdeck` mode can now
  distinguish, without reading logs: nothing happening (footer shows no drop segment),
  ordinary background noise (drop segment present, `sig 0`), and an actionable
  signature-verification problem (drop segment `sig > 0`, escalating to the drift
  banner at `SignatureDriftThreshold` consecutive failures).
- `event_source: poll` is untouched — `runPollOnly`/`poll()` gain no new code path; the
  entire feature is inert unless `EventSource` is non-nil, exactly like every other
  ADR-1254-era addition.
- `ack`'s always-`Status: 200` behavior is unchanged (out of scope, ADR-1254 D3's
  accepted trade-off, not reopened) — this issue is purely additive observability and
  escalation, never a change to what Pruefer tells Hookdeck over the wire.
- No new persistence: `dropCounts`, `consecutiveSigFailures`, and `sigDriftActive` are
  all in-memory, reset on restart, consistent with ADR-1254 D4/ADR-1113's
  zero-persistence philosophy.
- `SignatureDriftThreshold` is a fixed constant. A deployment whose genuine
  request volume is unusually bursty, or unusually sparse, may find 20 too fast or too
  slow to fire — accepted as a documented, revisitable trade-off rather than new config
  surface (see Alternatives Considered).
- `Daemon.dropCounts` (guarded by `dropMu`) must tolerate concurrent writers from two
  structurally different sources — `hookdeck.Source`'s single WebSocket read-loop
  goroutine (via `OnDrop`) and `eventsink.go`'s uncapped per-event goroutines (via
  direct `recordDrop` calls) — unlike `source.go`'s own `consecutiveSigFailures`/
  `sigDriftActive`, which stay unsynchronized because they're touched only from that one
  read loop.

## References

- [ADR-1254: Event-Driven Webhook Ingestion via Hookdeck](1254-event-driven-hookdeck-ingestion.md)
  — the design this issue's R1 amends (byte-exactness dependency, silent-degradation
  composition) and R2-R5 follow up on. D1 (only `GitHubEvent` crosses the boundary,
  scoped to domain events), D3 (poll remains the permanent fallback), and D4 (no new
  persistence) all directly constrain this ADR's decisions.
- `docs/events.HealthState`/`HealthEvent` (`pruefer/events/events.go`) — the direct
  precedent for a shared, transport-agnostic cross-package signal type and its
  `OnHealth`-callback wiring pattern, which this ADR's `DropReason`/`OnDrop` and
  `OnSignatureDrift` both replicate.
- `cmd/pruefer/README.md` — operator-facing documentation of the drop breakdown and
  signature-drift banner, updated in the same PR as this ADR.
