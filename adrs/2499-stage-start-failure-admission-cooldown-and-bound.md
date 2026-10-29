# ADR 2499: Stage start failure records its retry's admission cooldown, and a persistent one is bounded

## Status

Accepted. Amends ADR 2232 (FR-002 audit row "Start failure"). Source: community report #2453, issue #2499.

## Context

When a stage's Claude process cannot be launched (`*exec.Error`, `*os.PathError`, `exec.ErrNotFound`) `finalizeStageOutcome` sets `claudeRan = false`. The retry branch logged `did not start — will retry on the next poll` but recorded no admission cooldown (`recordPostRunRetryCooldown` was gated on `claudeRan`) and stamped no `StageAttempted`. Since #2232 nothing else admits the item, so the retry waited for the periodic re-evaluation (10 × `PollSeconds`); on `totalslacker/chronicle` #491 that was ~5.5 minutes. The log line was false, and `emitRetryScheduled` reported a due time (`stageRetryBackoff()`) that matched neither.

Recording the instant alone is unsafe. The dispatch gates (`itemNeedsWorkRetryAt`, `processItem`) read only `LastAttemptAt`, which a start failure leaves zero, so any admission — including an already-due cooldown or a `cycleSet` signal — re-dispatches at once. With a missing binary that would turn today's accidental slow loop into a poll-rate loop. `max_stage_dispatches` (ADR 2395) counts only runs where Claude ran, so it does not bound this either.

## Decision

1. **Do not stamp `StageAttempted`.** It flips "has run before", which drives the worktree-sync skip and the session-resume decision, and the stage never ran. `LastAttemptAt` stays zero (pinned by `TestProcessItem_ClaudeError`).
2. **A separate in-memory tracker** (`Engine.startFailures`, `engine/start_failure.go`): per item `{consecutive, holdUntil}` in the `e.now()` domain, plus a daemon-wide consecutive count. A restart resets it (the spec's assumption).
3. **Delay schedule** (`startFailureDelay`): the first failure is `0` (already due, like a time slice); the n-th (n ≥ 2) is `min(stageRetryBackoff() × 2^(n-2), max(stageRetryBackoff(), githubRecheckInterval()))`. No new flag, YAML key or env var; the cap is at most the periodic re-evaluation, so that safety net is never the effective retry again.
4. **One value everywhere.** `recordStartFailure` returns the delay once; the log line, `emitRetryScheduled` (new cause `start-failure`, replacing a false due time) and `recordPostRunRetryCooldown` all use it (#2232 FR-004).
5. **The hold gates dispatch.** `startFailureRetryIn` is consulted in `itemNeedsWorkRetryAt` (returned as `retryIn`, so the existing dispatch-time `recordRetryBackoffCooldown` re-admits the item when the hold lapses) and in `processItem`, so no admission route can re-dispatch inside it.
6. **Per-item hold, daemon-wide signal.** `*os.PathError` can be item-specific (a bad working directory), so a usage-limit-style account-wide hold could stall healthy items; it is rejected. A missing binary instead costs each item one launch per backoff step.
7. **Operator signal.** From the second consecutive daemon-wide start failure `warnings.Report` raises `claude_start:claude` (stable fingerprint; the newest error text in the detail). One entry per daemon. A single transient failure raises nothing.
8. **Resolution.** `noteClaudeStarted` runs after every stage and comment-review invocation whose outcome proves a process launched (`claudeProcessStarted`: not a start failure, not a pre-spawn apiKeyHelper/skill refusal). It clears the item's hold, resets the daemon count and resolves the warning only if it was reported, so healthy invocations never touch the warnings file.
9. **Classification is shared, not changed.** `isClaudeStartFailure` is the former inline test extracted verbatim, so the success hook cannot drift from it (`TestIsClaudeStartFailure`).
10. Never counted against `max_retries`, never `stage:<name>:failed`, never a pause: those remain gated on `claudeRan`, which is unchanged.

## Consequences

- A transient start failure is re-admitted on the next poll, as the log says; a persistent one backs off 0, 1×, 2×, 4×… up to the cap and is visible in the warnings panel without reading logs.
- A crash-looping daemon with a missing binary restarts the schedule each time; the panel entry and logs are its persistent signals.
- Each retry still records an errored invocation and a `Last run` write (existing behaviour, out of scope).
- Test seams: `SetPostRunRetryRecordDisabledForTest` (the cooldown), `Engine.SetStartFailureGateDisabledForTest` (the hold). The sim twin (`tests/sim/start_failure_test.go`) shows the transient retry, the bounded persistent case and recovery; the hold's non-vacuity is proven in `engine/start_failure_test.go` because the sim offers no extra admission signal.
- Mentioned for context: #2232, #2096, #1831, #2395.
