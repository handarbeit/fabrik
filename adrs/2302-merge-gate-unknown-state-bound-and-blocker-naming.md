# ADR 2302: Bound an unknown merge state on its own clock, and name the real blocker in the CI-backstop pause

## Status

Accepted (#2302). Follows ADR 2096, which left both halves out of scope. Mentioned for context: #1809, #1943.

## Context

GitHub reports a PR's mergeability as null (or its merge state as `unknown`) while it computes it, normally within seconds to minutes. `settlePRMergeState` returns `PRMergeUnsettled` for that, and for several waits that are legitimately long (checks pending, a suite hold, a pending required context). `checkMergeabilityGate` claims every unsettled result before `checkCIGate` is reached.

Two gaps followed:

1. The only bound on that claim was `settleAwaitingCIScan`'s `CIBackstopTimeout` (4h), and only for an item carrying `fabrik:awaiting-ci`. A Validate-complete item at the landing decision had no bound at all.
2. When the backstop did fire for an item that never reached the CI gate, `pauseForCITimeout` told the reader the CI gate "timed out waiting for checks to pass" and to "address any failures". In the #1809 incident every check had been green for 5h25m.

The engine already recorded what held the item (`noteGateHold` → `ItemState.GateHold`). Nothing read it.

## Decision

**A typed observation, not a `Reason` match.** `PRSettleResult` gains `MergeObs` (`MergeObsNone` zero value, `MergeObsUnknown`, `MergeObsComputed`) and `MergeUnknownValue`. Only the `mergeable == nil` and `mergeable_state == "unknown"` returns are `Unknown`; every return after them is `Computed` (stamped by the `settlePRMergeStateWith` wrapper); a `FetchPRMergeableFields` error never reaches either and stays `None`. Existing `Status`/`Reason`/`MergeableState` values are untouched, since `checkCIGate` and the merge train depend on them.

**The clock lives in the merge gate's claim branch.** `handleMergeAndCIGates`' `mergeBlocked` branch is the one place every route into the gate passes (the main Phase 1 loop and `settleAwaitingCIScan` run the same chain), so one call site covers items with and without `fabrik:awaiting-ci`. The clock is an engine-level in-memory map keyed by item and holding `{head, value, since, lastSeen}`, on `e.now()`. It is not an `itemstate` field: the cause rides in the pause comment's marker, as `daemon-stop` does, so the Store gains no mutation or snapshot-copy path.

**Clock semantics are a stricter reading of the spec.** The spec resets the clock on a computed state or a head change. We additionally (a) lapse it after `wedgeLapseFor("merge")` without an observation and (b) clear it when the pause is applied. Without them a human resume while the PR still reads null would re-pause at once through the #1408 reapply branch — the loop ADR 1813 and ADR 2059 R1 removed for other gates. The lapse also means a pause, a stretch of non-admission and a restart can only lengthen the wait, never shorten it. A clock begins at the first observation in this process, so the bound cannot fire on the first evaluation after a start (ADR 2059 R2) with no separate guard. The pause is deferred while a worker for the item is in flight (ADR 2059 R3).

**Setting.** `--merge-state-unknown-timeout` / `FABRIK_MERGE_STATE_UNKNOWN_TIMEOUT`, minutes, default 30: well above GitHub's recompute time, well below the 4h backstop. Flag and env only, like `CIWaitTimeout` and `CIBackstopTimeout`; the sibling timeouts have no YAML key and adding one for this setting alone would be inconsistent. Classified `tune` in `internal/workerenv.Scrubbed`. No startup ordering warning: a misordered value only makes the existing backstop the effective bound.

**Pause comment and dedup.** `pauseForUnknownMergeState` states the PR, head, elapsed time, the value returned, that the CI gate was never reached, and the check-run state (`gh.SummarizeCheckRuns` over one best-effort live read). Its opening fragment embeds the PR number and head SHA, so the #1408 episode is "this PR head": an old comment for another head cannot suppress a fresh pause, and neither this comment nor a CI-timeout comment matches the other's fragment. It is posted through `pauseIssue`, so the compliance scan and 🏭 prefix hold.

**The backstop names the blocker.** `pauseForCIBackstop` (the backstop's only caller) reads the recorded hold, treating one unconfirmed for more than `wedgeLapseFor(gate)` as absent. CI-gate hold, or a failed check: today's text (with one extra sentence naming the hold in the failed-check case). Anything else — another gate, or no recorded hold — gets a body naming the gate, its reason, how long it was observed holding ("at least": `GateHold.Since` is bounded by daemon uptime) and the check-run state, and neither claiming the wait was for checks nor asking for failures to be addressed. `pauseForCITimeout` keeps serving the gate-reached caller. `hasCIGatePauseComment` recognises the new fragment so dedup and reapply-without-repost are unchanged.

**Observability.** A new cause, `unknown-merge-state`, joins the ADR 2121 vocabulary, established by an explicit recorded fact (the comment marker, via `pausedByMarker`, generalised from `daemonStopPause`) and set on both pause results. `meta.cause` on the pause-family push derives from the same classification, so no new channel event type is needed and `ci-timeout` is never emitted for this pause. The log line names the bound and the elapsed time.

**Research finding: the re-read is uncached.** `CacheImpl.FetchPRMergeableFields` always delegates; `github.Client.FetchPRMergeableFields` uses `restGetJSON`, not `condGetJSON`. #1952's conditional requests are opt-in (`EnableConditionalRequests`) and only Pruefer's clients enable them, so a null is a fresh GitHub answer on every poll, never a stale 304. The bound needs no allowance. `FetchLinkedPR`'s cached head SHA can delay the head-change reset by about one poll, negligible against the bound.

## Consequences

- A persistent unknown merge state pauses the item after about 30 minutes (plus one poll), with or without `fabrik:awaiting-ci`.
- The 4h backstop's comment stops blaming CI when CI was not the holder.
- A repository where GitHub legitimately takes longer than the bound to compute mergeability (a very large PR, a busy base) can be paused spuriously; the setting is raisable.
- A resume followed by a #1408 reapply (comment reused) leaves the marker older than a later eligibility change, so the `unknown-merge-state` cause is absent for that reapplied pause — "absent rather than guessed".
- Out of scope, unchanged: the #2096 re-check cadence, "CI checks pending" holds on items without `fabrik:awaiting-ci` (#1943's broader case), applying `fabrik:awaiting-ci` on merge-gate claims, the CI liveness dwell (#1410), `PRMergeQueued` and merge-train landing waits (#2297).
- The live e2e suite cannot create a persistent GitHub null; coverage is the sim twins over `simgh.SeedMergeableUnknownUntil` (ADR 1454's recorded blind spot).
