# ADR 2604: Count live worker turns as progress in the attention classifier

## Status

Accepted. Amends the progress-anchor notes of ADR 2120 and ADR 2121.

## Context

`worker-no-progress` (`stalled`) fired on healthy workers. The progress anchor
(`internal/attention.progressAnchor`) ignored live turns, so during a run, with no
label, Status or attempt change, it stayed at worker start. With no `max_wall_time`
the worker budget is the 15 minute inactivity timeout, so every run longer than the
30 minute threshold was reported stalled, e.g. a worker at turn 86 of 100 that had
pushed commits. The claim was self-contradictory: a worker past the inactivity
timeout that has not been killed is producing output.

## Decisions

1. **Stamp the turn time in the store apply.** `WorkerHandle.LastTurnAt` is set from
   the store clock (`nowLocked`, wired to `Engine.now` by `SetClock`) when
   `WorkerTurnsRecorded` raises the count. The mutation signature and its call
   sites are unchanged, sim clocks stay deterministic, and the existing early
   return on unchanged fields means a same-count re-report cannot refresh it. A
   budget-only change leaves it; the zero-turn reset clears it. Memory only.
2. **A turn is progress.** `attention.Input.WorkerLastTurnAt` enters
   `progressAnchor`. The ticker heartbeat (`LastSignAt`) stays excluded: it stays
   fresh while Claude is hung; a turn is model output.
3. **Turn-live overdue uses the inactivity timeout.** When the turn time is known,
   `workerOverdue` compares progress age against `Input.WorkerInactivityTimeout`
   (+ the 5 minute margin), not `WorkerBudget`. `max_wall_time` is a total-run
   ceiling enforced by an engine timer and says nothing about gaps between turns.
   Consequence: a worker that overruns `max_wall_time` while still taking turns is
   no longer `stalled`; the wall-time kill owns that case.
4. **Wording.** Turn-live: `… is in flight but no turn for 31m0s (inactivity
   timeout 15m0s did not end it)`, the turn age computed from `Now - LastTurnAt` so
   a poll-recovery floor or label write cannot misstate it. The long-running note
   on a `working` item uses run time (`Now - WorkerStartedAt`) because progress age
   of a healthy run is now small. Unknown turns keep today's strings exactly.
5. **`ProgressAge` meaning.** For an item with a live turn it is the age of the
   latest signal, not time since worker start; this also reaches the read API's
   progress age. Every other row is unaffected (the turn time is zero without a
   worker).
6. **R3 (PR head change as progress) is not adopted.** Turns cover every live worker
   whose turns are recorded; a worker with unrecorded turns is the "unknown" case
   and keeps today's behaviour. `LastCIProgressAt` is already an anchor input; why
   it did not cover the reported item is unestablished here. Adding
   `LinkedPR.LastHeadSHAUpdate` would also mix a host-clock stamp with the store
   clock. It can be added later behind the same `Input` pattern.
7. **Known residual.** The zero-turn reset at each invocation's start clears the
   turn time, so a worker handle spanning several invocations has no turn time until
   the next invocation's first tool result and falls back to the start-anchored
   verdict. This is today's behaviour, not a regression; stamping "now" at the reset
   would close it but contradicts the requirement that the reset clears the time.
8. **Seam.** `Engine.SetTurnProgressDisabledForTest` (per-Engine, in
   `channelTestSettings`, copied by `CopyChannelTestSettingsFrom`, per ADR 2210)
   drops the turn time from the attention input, so the non-vacuity tests show the
   healthy long run reads `stalled` again without it.

## Consequences

Long healthy runs stay `working` and send no `stalled` push; a real stall is
reported after 30 minutes with no turn, with a reason that names the inactivity
timeout. A genuinely stuck worker is delayed (30 minutes since the last turn rather
than 30 minutes of runtime), but the inactivity kill should have ended it at 15
minutes, so what remains is the "kill did not work" case. One theoretical
false "did not end it" remains: a worker emitting assistant output but no tool
results, which resets the inactivity watchdog without counting a turn. No retry,
kill, dispatch or admission decision reads the turn time.
