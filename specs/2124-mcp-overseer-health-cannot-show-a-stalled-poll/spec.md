## Problem

When the daemon's poll loop stops making progress, neither `fabrik_health` nor any push tells the overseer.

This was observed in an in-situ test against the live dev daemon on 2026-10-11:

- **The poll blocked twice:** from 12:38:25Z to 12:54:30Z (about 16 min), and again from 13:09:47Z for 11+ min.
  - The cause is the engine bug in #2114: `dispatchCandidates` blocks on `e.sem` while every slot is busy.
  - Every per-poll step stalled behind it: settle scans, the merge train, landing verification and dispatch.
  - #2113, which had just been put on cruise, was dispatched about 17 min late.
- **What `fabrik_health` showed:** `since_last_poll_success_seconds: 475`, then 743, with `last_poll_attempt_at` frozen at the start of the blocked poll. It gives:
  - no configured poll interval;
  - no "overdue" flag;
  - no way to tell "a poll has been **in progress** since X" from "**no poll** since X".
- **Stale gauge:** `graphql_backoff.remaining` stayed at 4673 from 12:29Z onward, because only the poll updates it, but it was presented as current with no age.
- **No push:** none fired. `daemon-unreachable` covers only the socket.
- **Items blamed for the engine's stall:** while the poll was blocked (13:26:48Z onward, 923 s, 5 of 5 workers busy), #2096 was switched to yolo. Its PR stayed open because no poll ran to land it. The `stalled` push it then received did not say the engine itself was blocked.
- **Merge-train liveness has the same blind spot:** #2096 sat in Queued, "waiting for batch", for 16+ minutes behind the blocked poll. `fabrik_health.merge_train` showed only `members` and `worker_in_flight`. Nothing said how long each member had been queued, what phase the train was in, or why no train had started.

#2114 fixes the cause. This issue is about the instrument: **an overseer must be able to see that the pipeline has stopped, whatever the cause.** A worker that goes quiet but stays alive, a hung GitHub call or a future regression must all show up.

## Requirements

- **R1 — Poll liveness in health.** `fabrik_health.poll` reports:
  - `interval_seconds` (configured);
  - `in_progress` (bool) and `current_started_at`;
  - `last_completed_at` and `last_success_at`;
  - an `overdue` verdict: true when a poll has been in progress longer than K × interval, or no poll has started within K × interval. Document K, e.g. 2.

  When the poll is in progress and the engine knows the step, also report the step it is in, e.g. `dispatch: waiting for a worker slot`. These are published through `daemonHealth`, so local-API goroutines still read only the mirror.
- **R2 — Gauges carry their age.** Every health value that only the poll refreshes, such as `graphql_backoff`, carries its as-of time.
- **R3 — `pipeline-stalled` / `pipeline-resumed` events.** Account-wide, immediate, published on the overdue edge and its recovery. They carry:
  - the interval;
  - how long the poll has been in progress or absent;
  - the current step when known.

  They are deduped per episode. The stall condition in R6 (a Queued partition with members and no run) also raises `pipeline-stalled`, with its own cause carried in the event.
- **R4 — Docs.**
  - `docs/state-machine.md` §7.13/§7.14 as applicable.
  - The `docs/USER_GUIDE.md` MCP section.
  - Regenerate `docs/llms-full.txt`.
- **R5 — Item-level verdicts and pushes attribute a poll stall to the poll, not the item.** When the poll is overdue (R1), every per-item verdict or push whose next step needs a poll says so instead of blaming the item. This covers attention summaries, `next_action`, and `stalled` events.
  - The wording is, for example: "waiting for the poll: blocked for N s, in step <step>".
  - An item whose only obstacle is the stalled poll is not reported as `stalled` itself, and gets no item-level `stalled` push.
  - The account-wide `pipeline-stalled` event (R3) carries the cause once.
- **R6 — Merge-train liveness in health, per partition.** `fabrik_health.merge_train` adds, for each (repo, base) partition:
  - the oldest member's queued-since time;
  - the current train phase, from the durable `TrainRunPhases()`;
  - an idle reason when members are queued and no run is active, for example "poll blocked or overdue" or the infra-abandon cooldown.

  A Queued partition with members and no active run past a threshold raises `pipeline-stalled` (R3) and recovers with `pipeline-resumed`. The threshold and the treatment of known idle reasons are open questions below.

## Scope

In scope: health fields, gauge ages, the stall event pair, poll-stall attribution in item verdicts and pushes (R5), and per-partition merge-train liveness fields and the train-stall trigger (R6).

Out of scope:
- Fixing the poll's blocking dispatch, which is #2114.
- A dedicated event for an item joining the Queued column. It was noted as a gap in the in-situ test but not requested; R6 covers the train that does not start.

## Acceptance

All of the following are shown non-vacuous:
- **Overdue while in progress:** an engine test holds a poll in progress past K × interval, using the clock seam. `fabrik_health` reports `in_progress: true`, `overdue: true`, and the start time. Exactly one `pipeline-stalled` is published, and one `pipeline-resumed` when the poll completes.
- **Overdue with no poll:** a test where no poll starts within K × interval reports `overdue`.
- **Gauge ages:** the `graphql_backoff` as-of time is present and moves only when the poll refreshes it.
- **Poll-stall attribution (R5):** with the poll held overdue, using the clock seam, an item that is otherwise ready to advance reports "waiting for the poll" and gets no item-level `stalled` push. The test also shows that the same item with the poll healthy and no progress still classifies as `stalled`.
- **Merge-train liveness (R6):** with members queued, no run active and the poll held overdue, the partition reports the oldest member's queued-since time, the phase and an idle reason naming the blocked poll. Past the threshold, exactly one `pipeline-stalled` is published for the partition, and one `pipeline-resumed` when a run starts or the members leave Queued.
- **No-GitHub guarantees:** the local-API and channel no-GitHub scans stay green.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966.
