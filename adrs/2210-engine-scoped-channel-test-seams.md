# ADR 2210: Channel test seams are per-Engine, and sim waits are drain-driven

## Status

Accepted (2026-10-13)

## Context

`tests/sim` reached the 10-minute CI per-package timeout again three days after #2069 doubled it (270–300 s on 2026-10-09, 551–600 s by 2026-10-12). The existing tests had not slowed down; 34 new MCP channel tests (#1966 family) ran serially, for two compounding reasons:

1. Their seams were process-global variables: `channelDebounce`/`channelTick`, `channelReadyTimeout`, `decisionAnchorGrace`, `situationMinOverride` and a set of `*Disabled` toggles behind the `Set*ForTest` functions (plus `reviewReinvokeDisabledForTest`). A non-vacuity test that disables a fix globally would corrupt a parallel positive test, so the channel tests omitted `t.Parallel()` and relied on Go running serial tests to completion first.
2. `settleWait()` was a real 150 ms `time.Sleep`, called from inside `AdvanceUntil` conditions (68 call sites), so one wait of up to 80 polls could spend 12 s asleep.

Sharding the package and per-PR test selection are out of scope (ADR 1857).

## Decision

1. **Seams are per-Engine.** `engine/channel_settings.go` defines `channelTestSettings`, held as `Engine.chanTest`. Durations and flags are atomics; the zero value is the production behaviour, so no constructor changes and production pays one atomic load per read. Every former package-level setter is now a `*Engine` method returning the same restore function. Settings live on the `Engine`, not on `channelEvents`, so hooks that run before a hub exists read the same state. Timing, ready-timeout and situation-minimum values are read when the hub starts and must be set before `StartChannelEventsForTest`; neutralisations are read at use. `CopyChannelTestSettingsFrom` carries them across `tests/sim`'s `RestartEnv`. **A new channel test seam must be a field of `channelTestSettings`, never a package-level variable.**
2. **Waits are drain-driven.** `Engine.DrainChannelEventsForTest(timeout)` makes every debounced item due, waits until the consumer is idle (empty queue, no dirty item, no nudge, no settle candidate, no `drain()` pass in flight — `busy` is set under the same lock that takes the queue), runs a pending restart readiness pass, then waits until the hub owes no attached subscriber a deliverable batch. The last step uses the new `Hub.Deliverable`, not `Hub.Queued`: `Queued` counts digest entries the hub deliberately holds until their due time and would block forever. The wait is bounded and its error names the stage that did not drain. A poll's observers run synchronously inside `Store.Apply`, so after `PollOnce` returns everything it produced is already enqueued.
3. **Time-driven effects keep a documented real-time wait.** Scheduled digests, reminders, anomaly detectors and stall events are driven by the deriver's ticker and have no drain point. `tests/sim`'s `waitRealTime(t, d, why)` requires a reason at every use.
4. **A guard keeps the package parallel.** `tests/sim/parallel_guard_test.go` parses every `*_test.go` and fails, naming `file.go:TestName`, when a top-level test has no `t.Parallel()` in its own body (a call inside a `t.Run` closure does not count) and neither it nor its file is on the reasoned `parallelExempt` list. It is per test (tighter than the file-granular guard the issue asks for, so a serial test cannot hide in a parallel file), ignores comments and strings, and rejects stale or reasonless exemptions.

5. **The sim prefers the real git on macOS (R6).** `tests/sim`'s `TestMain` resolves `xcrun -f git` once and prepends its directory to `PATH` (darwin only, and only when `PATH`'s git is Apple's `/usr/bin/git` shim or absent, so a developer's own git is never swapped; any failure leaves `PATH` alone; `SIM_REAL_GIT=off` opts out), avoiding the shim's per-call developer-directory lookup across thousands of git starts.

## Consequences

- Channel sim tests run in the parallel pool; two engines in one test binary cannot see each other's seams (`TestChannelSeamsAreScopedToOneEngine`).
- The engine's own unit tests no longer write package variables, so they could also become parallel.
- Non-vacuity rests on each neutralised test asserting the opposite outcome with its seam set on its own engine, and on `TestChannelSeamsAreScopedToOneEngine` pinning that a toggle reaches only that engine. Demonstrating each test failing with its seam made a no-op is a review step recorded in the PR, not a property this ADR certifies.
- `RunPoll`'s fixed 100 ms `workerYield` per empty poll is unchanged. It is a real-time floor on `AdvanceUntil`-heavy tests and cooldown tests rely on it; it is the next lever if the package needs more margin.
- The guard checks each `Test` function's own body; a test that parallelises only through its subtests is reported and needs a top-level `t.Parallel()`.
