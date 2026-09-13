# ADR 1716: Wake-Path Rate-Limit Backoff Gate

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1716 — wake signal bypasses GraphQL rate-limit backoff, turning a throttle into a
self-sustaining, account-wide outage

## Context

`Engine.Run()`'s main loop has two ways to trigger a poll cycle: a `time.Ticker` and a
`wakeCh` signal (sent by `newWakeChObserver` on board-state changes, and by webhook delivery
under `--webhooks`). `PollWithBackoff` computes a GraphQL rate-limit-aware effective interval
(`computeEffectiveInterval`, `engine/backoff.go`) and applies it via `ticker.Reset(...)` —
but that interval only ever reached the *ticker*. The `case <-e.wakeCh:` branch called
`doPollCycle()` unconditionally, with no reference to `e.backoffRateLimitLow` or any other
backoff state.

This was invisible in isolation — GraphQL hysteresis worked correctly, and did engage (the
"rate limit low — activating backoff" log line fired as designed). The gap only mattered on
the wake path, and under `--webhooks` that path is fed by inbound GitHub events, which are
**not themselves rate-limited**. Once GraphQL exhaustion started, Fabrik's own mutations
(label changes, comments) generated webhook events that woke the loop again, which polled
again, which failed on rate limit and often generated further label/comment churn on the
already-existing `fabrik:awaiting-*` labels — a self-feedback loop (already documented in
§7.9 for other reasons) that, for the first time, had nothing bounding its rate.

Production impact (2026-09-13, private deployment, verified): 39 log lines/second sustained,
1,448 wake-requests and 4,372 rate-limit errors in one 20,000-line sample, the GraphQL
counter observed going from 0 to 4294 within nine seconds of an hourly reset. Because all of
a user's PATs draw on one shared 5,000-points/hour GraphQL bucket, the looping daemon starved
every other project sharing that token — a single misbehaving process denying service to
unrelated deployments, with the symptom presenting on the *victims* (unexplained exhaustion)
rather than the culprit.

A second, independent defect compounded the incident's visibility: `runProbeAndDeepFetch`
(`engine/terminal.go`), on a rate-limited `ProbeProjectBoard` failure, logged `"rate limited —
polling suspended, ..."` and emitted a TUI alert — but this function runs inside `poll()`,
called from *inside* `PollWithBackoff`, before that method's own hysteresis block computes
for the current cycle. It has no suspension mechanism of its own and could not see whether
the (nonexistent, at the time) real suspension was active. The message named a stand-down
that was not happening, actively misdirecting an operator reading the log or the TUI banner
away from the actual running fault.

This is the same defect shape as #1676/#1687/#1694: a correctness guard (GraphQL hysteresis)
that is correct in isolation but unreachable on the path that actually matters in production.
It is the first in that family whose consequence escapes the process — it degrades every
sibling deployment sharing the account's token, not just the one that loops.

## Decision

### R1 — gate the wake path on existing backoff state, don't add new state

`Engine.wakeBlockedByRateLimitBackoff() bool` (`engine/backoff.go`) returns
`e.backoffRateLimitLow || e.backoffRestPaused` — both fields `PollWithBackoff` already
persists on `Engine` (promoted from closure locals by ADR-1592, specifically so
cross-call state would exist). `Run()`'s `case <-e.wakeCh:` branch checks this before calling
`doPollCycle()`:

```go
case <-e.wakeCh:
    if e.wakeBlockedByRateLimitBackoff() {
        e.logfThrottled("wake-dropped-rate-limit-backoff", 0, "poll", "wake requested — dropped, rate-limit backoff active\n")
        continue
    }
    select {
    case <-ticker.C:
    default:
    }
    e.logf(0, "poll", "wake requested — polling immediately\n")
    if err := doPollCycle(); err != nil { ... }
```

A blocked wake is **dropped, not deferred**: no `pendingWakeDuringBackoff` flag, no explicit
re-arm on recovery. The already-backed-off ticker — reset to the correct `NextInterval` by
the prior completed `PollWithBackoff` call — is what eventually re-polls. This gives the
issue's own R1 text ("coalesce it so it fires when the backoff interval expires") a precise,
already-existing mechanism to point at, rather than inventing new state.

**Rejected alternative: explicit re-arm.** A `pendingWakeDuringBackoff` bool, set when a wake
is blocked and consumed the instant `backoffRateLimitLow` clears, would deliver a
webhook-driven change the moment backoff ends rather than waiting out the ticker's full
backed-off interval. Rejected because a dropped-with-no-re-arm bug in *that* design is silent
and easy to introduce in a later refactor — exactly the "hang if the gate is mishandled" risk
the issue's own Risks section calls out — for a benefit (up to one ticker-interval of latency
on a change that arrives mid-backoff and produces no further wake) the issue's own Research
explicitly accepted as "the same bound the ticker itself already has."

The gate intentionally does **not** drain `ticker.C` when a wake is dropped (unlike the
non-blocked branch below it) — an already-pending tick must still fire on the next loop
iteration rather than being silently discarded along with the wake that triggered this branch.

The gate covers REST backoff too (`backoffRestPaused`), even though REST's own hard gate
inside `PollWithBackoff` already made a REST-blocked wake harmless in effect (no board work
happens once inside). Checking it in `Run()` as well avoids wasting the "wake requested" log
line and a `RateLimitStats()` call on a call that was always going to no-op — a strict
improvement with no behavior change to the REST gate itself.

### The legitimate recovery self-wake needs no special-casing

`PollWithBackoff`'s own GraphQL hysteresis block sends a self-wake
(`e.wakeCh <- struct{}{}`) the moment it observes GraphQL recovering from a near-zero state,
to trigger a prompt follow-up probe. At the instant that send executes, `e.backoffRateLimitLow`
is still `true` — the reassignment to the post-recovery value happens a few lines later, in
the same synchronous call. `PollWithBackoff` runs to completion (including that reassignment)
before `doPollCycle` returns, and `doPollCycle` returns before `Run()`'s `select` loop can
re-evaluate and consume the queued wake. So by the time the gate actually evaluates this wake,
`backoffRateLimitLow` has already settled to `false`, and the gate passes it through — with no
tagged payload, no separate channel, nothing distinguishing this wake from an ordinary one on
`wakeCh`, which still only ever carries `struct{}{}`.

This is an **ordering property of the current single-goroutine code** (`Run()`'s select loop
and every `PollWithBackoff` call in production both execute on `Run()`'s own goroutine), not
an enforced invariant. A future change that made either asynchronous would break it silently.
`TestPollWithBackoff_RecoverySelfWake_NotBlockedByBackoffGate` (`engine/poll_test.go`) exists
specifically to catch that regression: it drives `PollWithBackoff` through a near-zero →
recovered transition and asserts both that a wake was queued and that
`wakeBlockedByRateLimitBackoff()` reads `false` immediately after.

### R3 — an unconditional poll-rate floor, independent of R1

`PollWithBackoff` refuses to reach `e.poll()` more than once per `minPollInterval` (a fixed,
unexported `500 * time.Millisecond`, `engine/backoff.go`), checked before even the REST hard
gate. This is defense-in-depth, not the fix for the wake-path bug — R1 already closes the
specific gap this incident exploited. R3 exists so that *any* future bypass of R1's gate (a
refactor that adds a third poll-triggering path, a mistake in a later change to the gate
itself) degrades into a bounded-rate leak rather than reproducing the unbounded 39/sec outage.

Placed inside `PollWithBackoff` rather than in `Run()`'s wake branch specifically because,
unlike R1's gate, the floor does not need to know *why* it was called — it is a pure "was the
last attempt too recent" check, identical for a ticker-triggered or wake-triggered call. This
mirrors the REST hard gate's own placement and rides the same `PollWithBackoff` seam into
`tests/sim` (ADR-1592) for free.

500ms was chosen with roughly a 2× safety margin below the lowest realistic `--poll` value
(1s, also `tests/sim`'s own default) — a ~78× reduction from the observed 39/sec bug rate. Not
exposed as a CLI flag: it is a guard against a defect class, not an operational knob a
deployment should ever need to tune. If a future deployment legitimately needs `--poll` below
roughly 1s, this constant needs revisiting.

### R2 — reword, don't wire backoff state into `runProbeAndDeepFetch`

Two options existed for `runProbeAndDeepFetch`'s misleading "polling suspended" claim: (a)
soften the wording at this call site, since it structurally cannot see the real backoff state
(computed later in the same poll cycle, in `PollWithBackoff`, and enforced one layer up in
`Run()`); or (b) thread an "is backoff currently active" signal down into this function so it
could make an accurate claim from its own vantage point.

Chose (a). `runProbeAndDeepFetch`'s job is the probe/deep-fetch cache refresh, not
backoff-state reporting; threading that state down would create a second, redundant source of
truth for something `PollWithBackoff` already owns, for a call site R1 already makes
irrelevant to the actual suspension. The log line and the one TUI banner string making the
same over-claim (`tui/alert.go`'s GraphQL-only case) both now describe only what happened —
one probe attempt was rejected for rate limit, or polling is *backed off*, not suspended — and
leave the `RateLimitAlertEvent{Bucket: GraphQL, Exhausted: true}` emission itself untouched
(it remains the sole source of that TUI signal). REST's hard-gate wording ("polling
suspended") is correct as-is and unchanged — REST exhaustion genuinely skips all poll work.

### R4 — a shared, general-purpose log-throttle helper

`Engine.logfThrottled` (`engine/logthrottle.go`) wraps `logf` with a dedup check: a message
logs on first occurrence under a given key, on any occurrence where the message text differs
from the last emission under that key, or once a fixed interval has elapsed since the last
emission — whichever comes first. Applied to `runProbeAndDeepFetch`'s rate-limited log line
and the three unconditional per-poll rate-limit stats/warning lines inside `poll()`, none of
which previously had any repeat-suppression (unlike `PollWithBackoff`'s own REST/GraphQL
pause/resume logs, which were already transition-gated on `backoffRestPaused`/
`backoffRateLimitLow` flips — an existing in-repo model this generalizes rather than
replaces). This collapses the alternating-line spam a sustained rate-limited condition
produces from once-per-poll-cycle down to once per throttle window, addressing the 14 MB/hour
log-growth component of the incident independent of the wake-path fix itself.

## Consequences

- A wake arriving during active GraphQL or REST backoff no longer triggers an immediate poll;
  it is dropped, and the next poll happens when the ticker's already-backed-off interval
  elapses. A webhook-driven board change that arrives mid-backoff and produces no further wake
  therefore waits out the full backed-off interval rather than firing the instant backoff
  clears — an accepted, pre-existing bound (see "Rejected alternative" above).
- The legitimate rate-limit-recovery self-wake is unaffected, by the ordering property
  described above — but that property is now load-bearing and only guarded by a regression
  test, not a structural invariant. A future change to make `PollWithBackoff` or wake delivery
  asynchronous must re-verify this.
- `PollWithBackoff` now has a floor on how often it will do real work, independent of caller.
  `tests/sim`'s `runPollWithBackoff` helper already advances its injected clock by
  `env.PollInterval` (default 1s) before every call — comfortably above the 500ms floor — so
  no existing scenario needed adjustment; confirmed by running the full sim suite.
- `runProbeAndDeepFetch`'s rate-limited log line and the GraphQL-only TUI banner string no
  longer claim a suspension neither controls. Any future call site that wants to report a
  *real* suspension should say so plainly — "suspended" is not banned repo-wide, only
  inaccurate uses of it near this incident's call sites were corrected.
- Repeated identical rate-limit log lines are now throttled. An operator watching a live,
  still-rate-limited daemon will see periodic re-statements of the same condition rather than
  a per-poll repeat — intentional; total silence while a real condition persists was
  considered worse than the previous per-poll spam.

## Rejected Alternatives

See R1's "Rejected alternative: explicit re-arm" above for the wake-handling design; no other
end-to-end alternative (e.g., a tagged-payload wake channel to distinguish recovery wakes
explicitly, or moving R3's floor into `Run()` rather than `PollWithBackoff`) was pursued far
enough to warrant separate documentation — both were considered and set aside during Plan in
favor of the ordering-property/shared-method choices described above, for the reasons given
in each corresponding Decision subsection.

## Related

- ADR-1592 (`1592-sim-bed-backoff-and-startup-cleanup-seams.md`) — promoted
  `backoffRateLimitLow`/`backoffRestPaused`/etc. from `doPollCycle` closure locals to `Engine`
  fields, the precedent this fix reads directly. Also the origin of the `PollWithBackoff` seam
  R3's floor rides into `tests/sim` for free.
- ADR-1482 (`1482-rate-limit-banner-bucket-identity.md`) — established REST/GraphQL
  `RateLimitAlertEvent` bucket independence in the TUI. R2's wording change touches only the
  GraphQL-only banner string, not this independence.
- ADR-1120 (`1120-claude-usage-limit-backoff-and-suspension.md`) — a structurally similar
  "account-wide suspension state the dispatch path actually consults" precedent, for a
  different resource (Claude usage limits, not GitHub rate limits).
- §7.8/§7.9, `docs/state-machine.md` — as-built documentation for the wake-path gate, the
  poll-rate floor, and their interaction with idle backoff and the pre-existing self-feedback
  loop.
- #1676/#1687/#1694 — the same "guard correct in isolation, unreachable on the path that
  matters" defect family; #1687 in particular documented the "guards are deletable without
  failing a test" blind spot the neutralization test in this issue's acceptance criteria
  guards against again.
