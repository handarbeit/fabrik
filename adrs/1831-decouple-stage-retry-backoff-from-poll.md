# ADR 1831: Decouple Stage Retry Backoff from the Poll Interval

**Date**: 2026-09-23
**Status**: Accepted
**Issue**: #1831 — Stage-retry backoff is derived from poll interval, so a rate-limit fix silently makes retries 6x slower

## Context

Ten call sites in `engine/` computed the same expression, `10 × PollSeconds`, for two unrelated purposes:

1. **Stage retry backoff** — how long to wait before re-dispatching a stage after an attempt that did not complete. Three sites: the dispatch gate in `itemNeedsWork`, `processItem`'s own gate, and the `"did not complete — will retry after"` message in `finalizeStageOutcome`. Each computed its own copy, so the gate and the message only agreed by coincidence.
2. **GitHub re-check cadence** — how often to re-read an item we will not be notified about (a dependency closing, a silent bot reviewer, a terminal item's periodic re-eval), and how long to back off after a failed read. Seven sites: deep-fetch failure cooldown, `dep-blocked`, `review-blocked` (#495), `periodic-re-eval` (twice), and both spawn live-re-read failures.

The second purpose is genuinely about GitHub API cost, so scaling it with poll is correct. The first is not: a retry costs no API calls beyond the retry itself. Coupling them meant that raising `poll` to protect a shared rate limit — in the reporting deployment, four daemons authenticating as one user raised it from 30 to 180 to stay inside the 5,000 points/hour GraphQL budget — silently turned a 5-minute stage retry into a 30-minute one. An Implement stage that was making real progress (8 commits, ~2,300 insertions) and simply exhausted `max_turns` sat idle for an hour across two attempts, with nothing indicating anything was wrong.

The issue as filed cited `catch_up_handlers.go`'s `review-blocked` site. That site is a re-check, and correctly stays coupled; the reported symptom came from the three retry sites above.

## Decision

Name the two concepts and route every site through exactly one of two helpers (`engine/retry_backoff.go`):

- **`stageRetryBackoff()`** reads a new, independent setting, `retry_backoff` (`--retry-backoff` / `FABRIK_RETRY_BACKOFF` / `config.yaml`), **default 60 seconds**.
- **`githubRecheckInterval()`** keeps `10 × PollSeconds`, unchanged.

The re-check sites are a pure refactor with no behaviour change. No `PollSeconds*10` expression remains in non-test engine code, so a future site has to choose one of the two named concepts rather than copying an expression.

**Why 60 seconds.** The retry cooldown's only job, per `processItem`'s own comment, is to stop a stage that exits incomplete immediately from hot-looping. It is not a runaway or fairness control: `max_turns` bounds a single attempt (and the worker slot is released when the invocation ends, which is what lets other issues run), and `max_retries` bounds attempts. At typical poll intervals a 60-second floor amounts to "retry at the next poll", while still covering webhook-triggered wakes that would otherwise re-poll immediately.

**Minimum 1 second.** Values below 1 are rejected with a warning — from the env var and `config.yaml` in `resolveRetryBackoff`, and from an explicit `--retry-backoff`, which bypasses that resolution. A zero floor would allow a hot loop, and a zero reaching the engine would read as "unset".

**Zero in the engine means unset.** The CLI always supplies a value. A `Config` built directly (tests, embedders) leaves `RetryBackoff` zero and falls back to `githubRecheckInterval()`, preserving their existing timing and avoiding churn across tests that pin it.

## Consequences

- Operators can raise `poll` to protect a rate limit without slowing retries. At poll 180, retry latency drops from 30 minutes to roughly one poll.
- Faster retries mean a stage that fails the same way repeatedly reaches `max_retries` sooner. That is the intended trade: the retry budget, not wall-clock delay, is what bounds a failing stage.
- `docs/state-machine.md` §7.1 previously stated that the retry cooldown was stored as `CooldownAt("periodic-re-eval")`. That was already stale since #504 moved dispatch suppression to `LastAttemptAt`, and it is the same conflation this ADR removes; it is corrected in the same change.
- **Not addressed, noted for follow-up:** the retry gate compares `LastAttemptAt` against real `time.Since`, not the `e.now()` clock seam (ADR-1449), because `LastAttemptAt` is also stamped with real time at several sites. Moving only the read side would mix clocks. The sim bed therefore still spends real wall-clock time on retry cooldowns (`retryCooldownPolls` in `tests/sim/failure_shapes_test.go`).
- **Not addressed:** varying the backoff by *why* a stage did not complete (turn-limit exhaustion with progress, as against an error). A single small backoff removes the reported harm; distinguishing outcomes can build on this if needed.
