# ADR 2486: Resume issues paused by a daemon clean stop on the next startup

## Status

Accepted. Amends ADR 1393 (the clean-stop pause is lifted only by a human). Builds on ADR 2300 (session
resume), ADR 2487 (pause cause labels) and ADR 2259 (SIGHUP drains instead of pausing).

## Context

A SIGINT/SIGTERM clean stop pauses every in-flight issue (`fabrik:paused`, `fabrik:awaiting-input`,
`fabrik:pause:daemon-stop` and a comment carrying `<!-- fabrik:pause-cause=daemon-stop -->`). Nothing but a
human lifted that pause, so a deliberate stop/start (a config change, an upgrade, a reboot) left every
interrupted issue parked until someone noticed. The motivating report (#2453) measured 17 issues losing 39
hours in one month, and five restarts in one day pausing three issues for 2.3-3.3 hours each. SIGHUP already
drains rather than pauses; a stop followed by a start is common and legitimate.

## Decision

1. **A one-shot, poll-driven scan resumes the stop's own pauses.** `resumeCleanStopPauses` runs from
   `poll()` before deep-fetch selection. The first poll with a populated board freezes the candidate set
   (open, non-PR items already carrying `fabrik:paused`); up to 10 are evaluated per poll and a candidate
   whose reads or writes fail gets 3 attempts. It is not hosted in `RunStartupCleanup`, which is skipped for
   the whole process lifetime when the first poll fails and runs after the labels it would need are cleaned.
2. **Durable GitHub state only, failing closed.** The decision reads live labels, the full comment thread and
   the label event times; never the board snapshot or the memory-only Store. A read error, a missing marker
   or a later event leaves the item paused with no write.
3. **Timestamp ordering, not actors.** The stop writes its labels before its comment, so every engine
   pause-label event is at or before the comment's `created_at`. The pause is resumed only if the newest
   pause-bearing Fabrik comment carries the marker (the same walk `pausedByMarker` uses, now shared as
   `newestPauseMarkerComment`) and no `fabrik:paused`/`fabrik:awaiting-input` `labeled`/`unlabeled` event is
   *strictly later*. In PAT mode the engine and the operator share a login and the sim's label events carry
   no actor, so actor matching was rejected. A tie is treated as the engine's own write (second resolution)
   and resumes: treating it as doubt would leave most real stops unresumed. The residual case is a human
   pause in the same second as the stop comment. A human *comment* never supersedes (the pause is not the
   comment); a human removing and re-adding the labels does, and so does a later stage or pause comment.
4. **The cause label is a veto, not the authority.** A `fabrik:pause:*` label other than `daemon-stop`
   keeps the item paused; no cause label at all is accepted (pauses written before ADR 2487). The marker
   remains the evidence of the cause (ADR 2487 decision 5 is unchanged for every human-driven resume path).
5. **A third lift path.** `unblockAwaitingInput` and `clearFailedStage` call `markSessionReset` and reset
   counters; either would start a fresh session and contradict "the stage is redispatched as a continuation".
   The resume removes `fabrik:paused` first (a failure leaves the item paused; a removed label ends
   candidacy, so one pause is resumed at most once), then `fabrik:awaiting-input` (failure tolerated, the
   orphan scan mops up), the cause label, logs one audit line, requests admission on the next poll, and last
   posts one `🏭 **Fabrik — resumed after restart**` comment. A crash between removal and comment loses the
   comment and never duplicates it. The interrupted run's `.session.meta` reads `closed: false`, a
   continuation, so the session resumes (ADR 2300).
6. **Opt-out.** `resume_after_clean_stop` (`--resume-after-clean-stop`, `FABRIK_RESUME_AFTER_CLEAN_STOP`,
   YAML), default on. Off restores stop-means-stay-paused byte for byte.

## Rejected alternatives

- Actor/login matching of the engine's events: indistinguishable in PAT mode, absent in the sim.
- A bulk label-event accessor on `GitHubClient`: cleaner, but it touches the interface, `simgh`, the
  instrumented wrapper and the wire fixtures for a one-shot scan of a handful of items. Four existing
  label-time reads per candidate are bounded by the per-poll cap.
- Treating a same-second tie as doubt: leaves nearly every real stop paused.
- Reusing `unblockAwaitingInput`: it resets the session.

## Consequences

- A deliberate stop/start no longer parks interrupted issues; time lost drops to roughly the restart.
- A pause the stop wrote is no longer lifted only by a human; operators who relied on that set the opt-out.
- Crash recovery (no marker), every other pause cause and SIGHUP are unchanged.
- Not covered: a human pause in the same second as the stop comment (tie rule), and a pause whose events the
  timeline cannot show (the item stays paused).
