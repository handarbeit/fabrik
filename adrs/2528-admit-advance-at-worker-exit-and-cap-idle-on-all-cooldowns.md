# ADR 2528: Admit a column move at worker exit, and cap the idle interval on every admission cooldown

## Status

Accepted. Amends ADR 2232 (item 2, "long cooldowns do not cap the idle interval") and ADR 039
(consequence 3, "self-advance is unaffected").

## Context

A stage worker finishes by moving the item to the next column. The Status write raises
`StatusChanged`, which puts the item in the next poll's `cycleSet` exactly once. If that poll runs
while the worker is still registered (it is still finishing the advance; `WorkerExited` is
deferred), `dispatchCandidates` skips the item at `snap.Worker() != nil` and the `poll()` defer
stamps `periodic-re-eval` at 10 × `PollSeconds`. `WorkerExited` wakes a poll but is excluded from
`cycleSetFlags` (ADR 039), so the item is not admitted again. With a healthy transport the idle
interval stretches to `webhookIdleCap` (60 min), past a 30-minute cooldown, so no poll runs when the
cooldown falls due either. Observed on a production daemon: Research → Plan at 09:38, next stage at
10:11, started by a human comment. The board shows nothing.

Two independent gaps: the lost admission, and a backstop that idle backoff can push out.

## Decision

1. **Column-change admission at worker exit.** `postRunAdvanceAdmission`
   (`engine/post_run_admission.go`) is deferred beside `postRunFeedbackAdmission` in the stage-dispatch
   goroutine and in `dispatchReinvoke`, so it runs before the slot release and the `WorkerExited`
   wake. The goroutine captures the item's Status from the Store when it starts
   (`statusAtWorkerStart`, not the poll-start copy, so a stale copy cannot admit falsely). At exit,
   if the Store's Status differs, it sets `e.mayNeedWork[key]`; the next poll admits the item through
   `cycleSet`, bypassing the new stage's cooldown. It is cache-only (`Peek`), writes the map directly
   (no `store.Apply`, no extra wake), is skipped on shutdown and for PR, closed, paused and
   cleanup-stage items, and is one-shot per run (an item that moved twice is admitted once).
   A completion label needs no new code: `stage:X:complete` is consumed by the catch-up chain from
   board items, not by the `cycleSet` dispatch path.
2. **ADR 039 is kept.** An early-return exit (dependency-blocked and the like) leaves Status
   unchanged, so nothing is admitted. Consequence 3 of ADR 039 held only when no poll drained the
   entry while the worker was in flight; that case is now covered.
3. **The idle cap covers every admission cooldown.** `earliestLongCooldown` returns the earliest
   unexpired cooldown of any reason that is neither short (`shortAdmissionCooldownReasons`) nor
   `archive-eligible-at` (an absolute "eligible since" time, not a gate). `PollWithBackoff` caps the
   interval at **due + one configured interval** (floored at 1 s), in addition to the unchanged
   exact-due cap on short cooldowns. Same exclusions as the short scan: expired entries, closed and
   cleanup-stage items; skipped under GraphQL rate-limit backoff.
4. **Loop safety.** Only unexpired entries count. Admission consumes an expired entry and the
   `poll()` defer re-stamps it a full `githubRecheckInterval` later, so a re-armed cooldown is always
   at least 10 × `PollSeconds` away. The one-interval grace lets dues stamped by different polls
   expire in the same window, so the cap yields at most about one poll per configured interval.

## Alternatives rejected

- **Record an already-due short cooldown at the `Worker() != nil` skip.** `dispatchCandidates` does
  not see `cycleSet`, so it needs plumbing, and the cooldown would be consumed and re-recorded on
  every poll for the length of the run, deep-fetching the item each time. ADR 2232 rejected that
  shape.
- **A "dropped admission" flag consumed at exit.** Sound, but broader than needed: it also
  re-admits after the worker's own lock and in-progress label churn.
- **Cap on `periodic-re-eval` alone.** Saves nothing: `dep-blocked`, `review-blocked`,
  `feedback-pending` and `spawn-*` are the same length and are stamped alongside it.
- **Cap at the bare due.** Staggered dues would each cost a poll.

## Consequences

1. **The webhook-healthy idle interval shrinks** from 60 min to about 10 × `PollSeconds` +
   `PollSeconds` whenever an in-pipeline item holds a cooldown, which is almost always. At
   `--poll 30` that is about 5.5 min, so most of the webhook idle benefit is gone; at `poll: 180` it
   is 33 min. This reverses ADR 2232 item 2 and is what the requirement asks for: the periodic
   re-evaluation is the backstop for missed admissions and must not be deferred by idle backoff. Each
   such poll costs one board read plus deep fetches of items whose cooldown expired.
2. A diagnostic `skipping dispatch: worker in flight` line is logged at the drop site, so a
   repeat is visible in the log.
3. Seams (per-Engine, so sim tests run in parallel): `SetPostRunAdvanceAdmissionDisabledForTest`,
   `SetLongCooldownCapDisabledForTest`, `SetAfterAdvanceHookForTest`. Tests:
   `engine/post_run_admission_test.go`, `tests/sim/advance_in_flight_admission_test.go`.
