# ADR 2124: Pipeline liveness and stall events for the MCP overseer

**Status:** Accepted
**Issue:** #2124
**Builds on:** ADR-1966-a (read API, `daemonHealth` mirror), ADR-1966-b (channel events), ADR-2114 (the cause fix)

## Context

On 2026-10-11 the poll loop blocked twice (16 and 11+ minutes) behind a full worker semaphore (#2114). Every per-poll step stalled with it. An overseer reading `fabrik_health` saw a growing `since_last_poll_success_seconds`, a frozen `last_poll_attempt_at` and a `graphql_backoff.remaining` that looked current but was hours stale; nothing was pushed, and an item switched to yolo mid-stall got a `stalled` push that blamed the item. The merge train showed only `members` and `worker_in_flight`.

#2114 removed the cause. This ADR is the instrument: an overseer must see that the pipeline stopped whatever the cause (a hung GitHub call, a quiet worker that holds a slot, a future regression).

## Decision

1. **The poll lifecycle is recorded in `poll()`**, not in its two callers (`PollWithBackoff`, `PollOnce`). One `beginPoll` at entry, one `endPoll` in a `defer`: an error or early return cannot leave `in_progress` stuck, and the sim seam is covered with no extra code. *Completed* means returned; `last_success_at` keeps its meaning. Everything is published to `daemonHealth`, so API and channel goroutines still read only the mirror.
2. **Overdue is measured from the engine's own next-poll-due time**, not from the configured interval. A poll in progress is overdue after K × interval; with none in progress it is overdue K × interval after `next_due_at`, which includes idle back-off (up to 60 min with healthy webhooks), the GraphQL back-off and the REST pause. Using the configured interval would report every idle daemon as stalled. `next_due_at` falls back to `end + interval` (error path, `PollOnce`) and to the daemon start before the first poll. K = 2 is one constant, not a setting; it is echoed as `overdue_after_seconds`. An unknown interval or baseline makes the verdict unknown, never healthy.
3. **Steps are phase names.** #2114 made slot acquisition non-blocking, so `dispatch: waiting for a worker slot` can no longer occur. About ten `setPollStep` calls name the phase; the step is empty when unknown and is never invented. Finer "waiting on X" steps would mean instrumenting every client call.
4. **Gauge ages**: `as_of` on `graphql_backoff` is set only inside `setGraphQLBackoff`; `rest_paused_as_of` by `setRestPaused`, which is now called on every attempt (a refresh) rather than only on a transition. Neither moves with time alone.
5. **Attribution is a classifier post-pass fed with plain values** (`Input.PollStall`, `Input.PollRecoveredAt`). `internal/attention` keeps no engine imports and, with the input zero, no behaviour change. A `stalled` verdict an overdue poll explains becomes `waiting` / `waiting-for-poll`, unranked; `waiting` and `idle` keep their state with the cause prepended; verdicts that need a human or a worker are untouched. Because `pauseFamilyKey` maps only `stalled` to the `stalled` event, no item push fires. Rewriting only the summary was rejected: it leaves the `stalled` push.
6. **Recovery grace.** After an overdue episode every blocked item has a huge progress age and would flip to `stalled` and push the instant the poll returns. `daemonHealth` stamps the end of each overdue interval (`beginPoll`/`endPoll`) and the progress anchor is floored at it (a known anchor only).
7. **Events** `pipeline-stalled` / `pipeline-resumed`: account-wide, immediate, one pair per episode, for the poll (`scope=poll`) and per merge-train partition (`scope=merge-train`). They are evaluated by the channel deriver's tick on its own goroutine, so a blocked poll is still detected; they read the health mirror, the Store, `TrainRunPhases()` and a non-mutating cooldown peek, so the no-GitHub scans stay valid and the events stay observation-only. `daemonHealth` also counts ended episodes (`stall_epoch`), so an episode that ends and restarts between two ticks is two episodes.
8. **Dedup keys carry the episode anchor and a per-process sequence.** The hub persists dedup keys across restarts, so a static key would suppress every later episode; the anchor alone would still collide when a partition's run dies and the same members stay queued. Episode memory is in memory only; a restarted daemon that is still stalled raises a fresh event.
9. **Partition liveness** (`oldest_queued_since`, `phase` from `TrainRunPhases()`, `idle_reason`) uses a pure function shared by health and the deriver. *Active run* is an open durable run **or** an in-flight worker (the async driver lets the worker exit with a run still open, so `worker_in_flight` alone would blame the poll for a partition that has a run). The idle reason is a closed vocabulary — `poll-overdue`, `infra-abandon-cooldown`, `unknown`. Other holds (overlap deferral, the runaway guard, a base contradiction) are `unknown`: copying dispatch gating into the read path would be worse than saying so. The partition stall is not raised inside the infra-abandon cooldown (an intentional hold), with `merge_train` off, or while the engine is deliberately backing off.
10. **Clock consistency.** `markInfraAbandon`/`infraCooldownRemaining` read `e.now()` (identical in production), and `SetClock` re-stamps the daemon start on the injected clock, so the clock-seam tests control both the cooldown and the "no poll started" baseline.
11. **New-subscriber catch-up** includes an open pipeline stall (poll or partition): unlike an item's time-driven `stalled`, it is current account state.

## Consequences

- Detection latency in production is at most K × interval plus the 30 s deriver tick; a stall shorter than that may never be seen (the epoch counter handles re-stalls, not sub-tick ones).
- A partition held for a reason the engine does not name is reported stalled with idle reason `unknown` after K × interval. That is the intent (queued members and no run), at the price of some noise for holds such as overlap deferral; the reason string makes it diagnosable.
- `oldest_queued_since` is daemon-observed (ADR-1833), so a restart restarts the clock, and without the board cache it is unknown and no partition stall is raised.
- No operator knob: K and the partition threshold are constants.
- The poll stall cause is stated once, on the account-wide event; items carry only "waiting for the poll".
