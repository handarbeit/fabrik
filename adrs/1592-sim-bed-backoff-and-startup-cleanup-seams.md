# ADR 1592: Sim Bed Backoff, Observer-Registration, and Startup-Cleanup Seams

**Date**: 2026-08-15
**Status**: Accepted
**Issue**: #1592 — Cover four sequence-shaped engine behaviours the sim bed does not reach

## Context

#1592 set out to write four `tests/sim` scenarios for engine behaviour that only manifests
across multiple polls or across two interacting items. Two of the four (dependency
blocking/unblocking, the review/no-op reinvoke-cycle counters) turned out to be reachable
through the existing `Engine.PollOnce` seam (ADR-1449) with no engine change — every code path
they exercise runs inside `poll()`'s own catch-up-handler chain, which every existing sim
scenario already drives.

The other two, and one seam Research did not anticipate, were not:

- **GraphQL rate-limit backoff** (`engine/backoff.go`): its five pure functions
  (`shouldPauseForRESTRateLimit`, `idleBackoffMultiplier`, `nextRateLimitLow`,
  `isRateLimitNearZero`, `computeEffectiveInterval`) had exactly one caller each, all inside
  `Engine.Run()`'s `doPollCycle` closure — never `poll()` itself. `PollOnce` calls `poll()`
  directly and reaches none of it.
- **Stale-worker-label reaping** (`engine/worker_liveness.go`): the five one-shot startup scans
  (`runStartupCleanup` and four siblings) are unexported and called only from `Run()`,
  immediately after its first successful poll cycle — never from `poll()`/`PollOnce`.
- **`PushUnblockObserver` and every other reactive `itemstate.Store` observer** (discovered
  empirically while writing gap 1's scenario, not anticipated by Research/Plan): the entire
  observer-registration block — `mayNeedWorkObserver`, `InvocationObserver`,
  `StageChangeObserver`, `PushUnblockObserver`, `CommentBreakerObserver`, and the
  `cacheImpl`-gated `WebhookHealthObserver` subscription — also lived only inside `Run()`,
  executed once before entering its poll loop. `NewWithDeps` (the sim harness's engine
  constructor) never registered any of them. Gap 1's blocker-closes-and-unblocks scenario is
  entirely mediated by `PushUnblockObserver`, so without this seam gap 1 would have been
  unreachable too — a gap the original Research pass missed because nothing about
  `PushUnblockObserver`'s own code signals that its registration, not just its logic, is
  `Run()`-only.

All three are the same shape ADR-1449 already established a precedent for: production logic
that is real and correct, sitting in a code path only `Run()`'s long-lived loop reaches, with no
behavior-preserving way to reach it from a single deterministic poll cycle without exposing a
new, minimal, additive seam.

## Decision

### Three new exported `Engine` methods, each a verbatim extraction

**`Engine.RegisterObservers() (unregister func())`** (`engine/poll.go`) moves the entire
observer-registration block out of `Run()` into its own method — same observers, same
construction, same order — and returns an unsubscribe function. `Run()` now calls
`e.RegisterObservers()` and defers the returned function, exactly replacing the block it used to
inline. `tests/sim`'s `NewEnv` and `RestartEnv` each call it once, immediately after
constructing the `Engine`, mirroring where `Run()` calls it in production (immediately before
entering the poll loop). Registering twice on one `Engine` (e.g. a test that both calls this
directly and later calls `Run()`) subscribes every observer twice — never done in production,
and not a scenario this method guards against; a caller that also invokes `Run()` must not call
this itself.

**`Engine.PollWithBackoff(ctx, configuredInterval) (PollBackoffResult, error)`**
(`engine/poll.go`) moves `doPollCycle`'s entire body — the REST/core rate-limit hard gate,
`poll()` itself, idle-timer bookkeeping, GraphQL rate-limit two-threshold hysteresis, and the
resulting effective next-poll interval — into its own method. `PollBackoffResult` carries only
`NextInterval`; `Run()`'s `doPollCycle` closure collapses to calling `PollWithBackoff` and
resetting its own ticker from the returned value. The five pieces of state the closure held as
locals (`prevMultiplier`, `rateLimitLow`, `rateLimitRatio`, `lastRemainingCount`,
`restRateLimitPaused`) become `Engine` fields (`backoffPrevMultiplier`, `backoffRateLimitLow`,
`backoffRateLimitRatio`, `backoffLastRemaining`, `backoffRestPaused`), mirroring the existing
`idleStart` field's precedent — necessary because the method must now be callable repeatedly,
across successive polls, from a test. Unlike `idleStart`'s zero value (a genuine "not idle"
sentinel), `backoffPrevMultiplier` and `backoffRateLimitRatio` have non-zero starting values (1
and 1.0) that both `New()` and `NewWithDeps()` set explicitly.

**`Engine.RunStartupCleanup()`** (`engine/worker_liveness.go`) delegates, in order, to the five
one-shot startup scans `Run()` has always run after its first successful poll cycle
(`runStartupCleanup`, `runStartupOrphanedInProgressScan`, `runStartupBareInProgressScan`,
`runStartupTransientLabelScan`, `runStartupTerminalScan`). `Run()`'s five separate calls collapse
to one call to this method. Deliberately excludes the periodic
`runWorktreeJanitor`/`runLogJanitor`/`runSessionJanitor` calls `Run()` makes alongside these
scans (gated separately on `cfg.JanitorIntervalHours > 0`) — those are a distinct, ongoing
concern, not part of the one-shot startup recovery pass this method names.

All three read as "moved," not "changed": every log line, every TUI event, every mutation is
identical to what `Run()` already did, in the same order, confirmed by the full existing
`go test ./engine/...` suite passing unmodified.

### `PollWithBackoff`'s REST hard gate: `e.now()`, not `time.Now()`

One genuine, minimal behavior widening rides along with the extraction, discovered while writing
gap 2's REST-hard-gate scenario: `shouldPauseForRESTRateLimit`'s `now` argument and the
`NextInterval` computed from `restStats.Reset` both used `time.Now()` directly in the
pre-extraction closure. `restStats.Reset` is computed by the read client relative to whatever
clock it uses — `tests/sim/simgh`'s `RateLimitStats` reads the injected `Clock`, which
`tests/sim/env.go` anchors at 2026-01-01 by default (ADR-1449's `SetClock`/`e.now()` seam).
Comparing that against real `time.Now()` (today's actual date) meant the REST budget always
looked "already reset, ages ago" — the gate could never be observed active in a sim scenario at
all, regardless of how exhausted the seeded budget was.

Both call sites now use `e.now()` instead. This is the same widening ADR-1449 documents for its
own clock seam ("a future contributor adding a new timing gate ... should check whether it is
... engine-local"): `e.now()` falls back to `time.Now()` whenever no `Clock` is injected (see
`engine/clock.go`), so production — where `New()` never calls `SetClock` — is byte-identical
before and after, confirmed by the same full `engine` test suite. Only a scenario that injects a
`Clock` (every `tests/sim` scenario) observes the difference, and only at this one call site.

## Consequences

- `PollBackoffResult` (`engine/poll.go`) and the five `backoff*` `Engine` fields are new,
  additive production surface, alongside the two new exported methods. No existing call site's
  *behavior* changes; both are confirmed byte-identical to prior behavior by the full existing
  `go test ./engine/...` suite.
- `tests/sim/dependency_unblock_test.go` (gap 1), `tests/sim/backoff_test.go` (gap 2), and
  `tests/sim/stale_worker_reap_test.go` (gap 3) are the first sim scenarios to depend on
  `RegisterObservers`, `PollWithBackoff`, and `RunStartupCleanup` respectively — every other
  existing `tests/sim` scenario continues to use `PollOnce` alone and is unaffected by any of the
  three (`RegisterObservers` was silently a no-op for them before this issue, since none of the
  observers it registers gate any assertion those scenarios already made).
- A future contributor writing a new sim scenario that exercises engine behavior gated by a
  reactive `itemstate.Store` observer, GraphQL/REST rate-limit response, or a one-shot
  startup-recovery scan should reach for these three methods directly rather than re-deriving
  `Run()`'s own preamble — the same guidance ADR-1449 gives for `PollOnce`/`SetClock`.
- Mirrors ADR-1449's own closing note: a future timing gate should be checked for whether it
  compares a GitHub-read timestamp against `time.Now()` rather than `e.now()` — this issue found
  a second instance of exactly that gap, in code that predates the Clock seam itself.
