# ADR 2232: Engine-scheduled retries own an admission cooldown

## Status

Accepted. Amends ADR 2096 (item 6) and extends ADR 1831.

## Context

ADR 1831 decoupled the stage retry backoff from `poll`. ADR 2096 made an expired cooldown admit an
item once and recorded a `retry-backoff` cooldown — but only from `dispatchCandidates`, when a poll
that something *else* had admitted refused the item inside the backoff window.

An incomplete stage admits nothing. Its commits, pushes and label writes are self-writes;
`InvocationChanged` is in neither the wake set nor `cycleSet`; `WorkerExited` wakes the poll but is
excluded from `cycleSetFlags` (ADR 039); the lock is deliberately kept through the cooldown. The poll
the exit wakes finds the item behind its `periodic-re-eval` cooldown, reports "nothing to do" and lets
idle backoff stretch the ticker. The retry then waits for `periodic-re-eval` — up to 10 × `poll`, 30
minutes at `poll: 180` (seen on #2210: a `max_wall_time` stop, `will retry after 1m0s`, no retry).
ADR 2096 item 6's premise that "the worker-exit `cycleSet` admission" re-admits the item holds only
for exits that churn a label. The same loss affected feedback cached during a run (#2210: a comment
at 14:39, still unprocessed at the 15:37 exit, no admission for six minutes).

Four earlier issues (#1831, #2096, #2114, #2062) are the same defect: nothing re-admits an item whose
retry the engine itself scheduled, so it waits for the periodic interval.

## Decision

**Every engine-scheduled retry owns an admission cooldown, recorded where the engine decides to retry.**

1. **Post-run record (FR-001, FR-004).** The decision point records
   `CooldownRecorded{Reason: "retry-backoff", Until: e.now() + d}` from the same `d` it logs and hands
   to `emitRetryScheduled`, via `recordStageRetryCooldown`. The dispatch-time record (ADR 2096 item 6)
   stays as a backstop for the clock-domain gap below.
2. **Idle backoff (FR-003).** `PollWithBackoff` caps the interval at the earliest unexpired
   `retry-backoff` / `sentinel-live` / `merge-unsettled` cooldown, floored at 1 s. Long cooldowns do
   not cap (they would shrink the 60-minute webhook-healthy idle cap to 10 × `poll`); expired entries
   do not count (they would pin the poll at the floor); the cap only shortens, and is skipped under
   GraphQL rate-limit backoff.
3. **Post-run feedback admission (FR-001b).** The worker goroutines defer `postRunFeedbackAdmission`,
   which sets `mayNeedWork` when the *cache* already holds unprocessed feedback (`cachedPendingFeedbackOf`,
   the cache-only twin of `unprocessedFeedback`). It runs on the worker goroutine, not in a
   `WorkerExited` observer, so it stays off the store's apply path and guarantees the entry exists before
   the exit's wake poll. `mayNeedWork` rather than a cooldown: it is drained once, bypasses cooldowns, and
   needs neither a reason name nor a duration; the exit wake already guarantees a poll. Conditional on
   known pending feedback, so ADR 039 stands. Only feedback created at or after the worker's start counts
   (`cachedFeedbackArrivedSince`): feedback pending before the run is what the run was for, and a pass
   that ended without clearing it must not re-admit itself past every cooldown; it takes the ordinary
   `stageRetryBackoff` path. (A comment pass's own `fabrik:editing` label write-through already raises
   `LabelsChanged`, a pre-existing re-admission bounded by the comment circuit breakers and unrelated to
   this hook, so this restriction cannot be shown in a sim scenario and is pinned at unit level,
   `TestPostRunFeedbackAdmission*`.)

### FR-002 audit — which exits rely on the retry gate

| Exit | Decision |
|---|---|
| Incomplete branch of `finalizeStageOutcome` (wall-time / inactivity kill, turn-cap slice, resume failure, tools-denied, error) | Record, when `claudeRan` (the only case `StageAttempted` is stamped) |
| Start failure (`claudeRan == false`) | No cooldown; no backoff applies, the retry is the next poll; log says so |
| `handleAPIErrorExit` | Record `now + backoff` |
| `handleUsageLimitExit` | Record until the later of `now + backoff` and the account-wide hold, so an expiry during the hold is not admitted only to be refused |
| PR creation failed below `MaxRetries` | Record `now + backoff` (`StageAttempted` was stamped) |
| apiKeyHelper detected, skill unresolved / load failed, boundary violation, escalations and pauses | Exempt: a human or config fix is needed, or the item is paused |

## Time slices and dispatch order (amendment)

The backoff only protects against a stage that exits incomplete *immediately*. A turn-cap stop or a
`max_wall_time` kill has just run a full slice and is already bounded by `SliceRetries` and `max_retries`,
so waiting `retry_backoff` there only idles the item. `isTimeSlice` classifies them from the engine's own
structural signals (`TurnLimitError`, `KilledError{KillWallTime}`), deliberately not from run duration, so a
fast error exit can never qualify (and an inactivity kill, a hang, keeps the backoff). Such an attempt is
stamped `StageAttempted{Slice: true}` (`StageState.BackoffExempt`, rewritten by every attempt), both retry
gates skip the backoff for it, an already-due `retry-backoff` cooldown is recorded and the log says
`will retry on the next poll (time slice)`; the `WorkerExited` wake runs the poll. A duration threshold
("ran longer than N × `retry_backoff`") was considered and rejected: it is a heuristic, and the structural
signals already cover the cases the cap and the wall-time limit exist for.

Without the backoff, board-order dispatch would let an item near the top of the board retake its own slot,
starving everything behind it. Stage dispatch candidates are therefore ordered by time without a worker,
longest first (`orderDispatchCandidates`: oldest `LastAttemptAt`, never-run first, ties by issue number).
Seams: `SetTimeSliceBackoffDisabledForTest`, `SetDispatchOrderDisabledForTest`.

## Alternatives rejected

- Moving the retry gate onto `e.now()`: `LastAttemptAt` is stamped with real time at many sites
  (ADR 1831 records this as deliberately not done).
- A `WorkerExited` observer for FR-001b: it runs inside `store.Apply`; a live feedback read there would
  put GitHub calls on the apply path.
- A live `unprocessedFeedback` read per exit: a GitHub call for no coverage gain — feedback the cache
  lacks is still caught by `feedbackGateBlocks` at every advance and landing.
- Re-queueing at the `Worker() != nil` skip: it would deep-fetch the item every poll for the length of a run.
- Capping the interval on every cooldown: defeats the webhook idle cap.

## Consequences

- A stage that ends incomplete is retried on the first poll after `retry_backoff`, not after 10 × `poll`.
  A stage failing the same way reaches `max_retries` sooner; ADR 1831 already accepts that trade.
- Clock domains: the cooldown is on `e.now()`, `LastAttemptAt` is wall-clock. If the cooldown expires
  first, the dispatch-time backstop re-records the remaining wait, so the item is re-admitted once per poll
  for the tail of the backoff only.
- Test seams: `SetPostRunRetryRecordDisabledForTest`, `SetPostRunFeedbackAdmissionDisabledForTest`,
  `SetIntervalCapDisabledForTest`. Sim twins in `tests/sim/incomplete_retry_admission_test.go`; the cap is
  covered by `TestPollWithBackoff_IdleIntervalCappedToDueRetry`.
- Documented in `docs/state-machine.md` §7.1, §7.8 and §9.9.2.
