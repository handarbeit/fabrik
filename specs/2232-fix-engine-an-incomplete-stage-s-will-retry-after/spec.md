# Feature Specification: An incomplete stage is retried after its backoff, not after the periodic re-evaluation

**Created**: 2026-10-13
**Status**: Specified
**Input**: Seen live on #2210. Its Implement worker was stopped by `max_wall_time` at 15:37:10 UTC. The engine logged `stage "Implement" did not complete — will retry after 1m0s`. The polls at 15:37:18 and 15:38:22 reported `nothing to do`, and the engine then went to `idle backoff: 2x (5m0s)`. The item was not re-dispatched. Its only way back was the periodic re-evaluation, 10 × `poll` = 30 minutes on the dev daemon (`poll: 180`).

## Background

The incomplete-stage branch of the post-run path (`engine/item.go`, the `else` branch that logs `will retry after %v` using `stageRetryBackoff()`) records **no cooldown**. The only place a `retry-backoff` cooldown is recorded is `recordRetryBackoffCooldown` (`engine/poll.go`), called from `dispatchCandidates` only when a later poll evaluates the item inside the backoff window and `itemNeedsWorkRetryAt` refuses it with a positive `retryIn` (#2096, ADR 2096).

An item whose worker just ended incomplete usually is **not** a deep-fetch candidate on the next poll. Its own commits, pushes and label writes are self-writes and raise no change signal. So nothing is recorded, nothing admits it when the backoff expires, and it waits for `periodic-re-eval`, which every poll re-stamps.

Effect: every stage that ends incomplete (a `max_wall_time` stop, a turn-cap time slice, any non-completing exit) is retried after up to `10 × PollSeconds` instead of `RetryBackoff` (default 60 s). That's up to 30 minutes at `poll: 180`. This is exactly the coupling ADR 1831 removed, coming back through a different route. Idle backoff makes it worse: with no other work, the poll interval stretches to 5 minutes, even though a retry the engine itself scheduled is due.

## Requirements

- **FR-001**: When a stage invocation ends without `FABRIK_STAGE_COMPLETE` (and not blocked on input) and the engine logs `will retry after <d>`, record a `retry-backoff` cooldown for the item at that point: `Until = e.now() + d`, the same value as the log line. When it expires, the item is admitted to the next poll's candidate set (consumed once, per ADR 2096), and the stage is re-dispatched if `itemNeedsWork` agrees.
- **FR-001b**: When a worker finishes, by any outcome (complete, incomplete, blocked), and the item has unprocessed feedback by `unprocessedFeedback` (the same predicate as `feedbackGateBlocks`), the item is admitted on the next poll, so the comment pass runs promptly. This is done by recording a short cooldown or by keeping the item in `mayNeedWork`. It covers feedback that arrived and was read into the cache *during* the run, which raises no change signal after the run ends. Seen live on #2210: a human comment arrived at 14:39:29 UTC while Implement was running; when the worker stopped at 15:37:10 it was still unprocessed, and no poll admitted the item for the next 6 minutes, until a daemon restart at 15:43.
- **FR-002**: The same applies to every other "did not run / did not complete, retry later" exit that relies on `stageRetryBackoff` (turn-cap slices, `max_wall_time`, resume failure). Audit them, and cover each one or record why it doesn't need it.
- **FR-003**: Idle backoff must not hold the poll past a known due time. When the poll loop backs off, cap the next interval at the earliest pending `CooldownAt` that will admit an item (`retry-backoff` at least), floored at a sensible minimum. A webhook wake path is unchanged.
- **FR-004**: The log line and the recorded cooldown are computed from the same value, so they can't disagree. This is the ADR 1831 guarantee, extended.
- **FR-005**: `docs/state-machine.md` (dispatch cooldowns / ADR 2096 section) describes the rule, including the post-run feedback admission of FR-001b. Regenerate `docs/llms-full.txt`.

## Success Criteria

- **SC-001**: A sim test has a stage end incomplete with `poll` set high (e.g. 180 s) and `RetryBackoff` 60 s. It asserts the stage is re-dispatched on the first poll after 60 s, not after 10 × poll. Shown non-vacuous by removing the FR-001 record: the test must then fail.
- **SC-002**: A sim or engine test with idle backoff active asserts the next poll fires at the due cooldown, not the 5-minute idle interval. Shown non-vacuous by removing the FR-003 cap.
- **SC-003**: `go test -race ./...` is clean.
- **SC-004**: A sim test posts a human comment mid-run and asserts the comment pass starts on the first poll after the worker ends, at `poll: 180`. Shown non-vacuous by removing the FR-001b admission: the test must then fail.

## Notes

- Critical for 0.0.84: every wall-time or turn-capped stage on a production daemon waits up to 30 minutes for no reason.
- Related: ADR 1831 (retry backoff decoupled from poll), ADR 2096 (admission cooldowns consumed once).
