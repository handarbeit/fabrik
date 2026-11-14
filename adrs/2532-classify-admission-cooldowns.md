# ADR 2532: Classify admission cooldowns as timers, event-replaceable re-checks or the safety net, and schedule each accordingly

## Status

Accepted. Amends ADR 2528 (decision 3, "the idle cap covers every admission cooldown"). Keeps ADR 2096,
ADR 2232/2499, ADR 039 and ADR 2530 unchanged; reads ADR 2452's route verdict as the transport-health input.

## Context

Since ADR 2528 every admission cooldown other than the short ones is treated alike: it is stamped
`githubRecheckInterval()` (10 × `PollSeconds`) ahead and the idle interval is capped at due + one
configured interval. They are three different things:

1. **Timers**: a deadline that no GitHub event announces. Today they run only when the item happens to be
   re-admitted through a cooldown, so they fire anywhere between the deadline and the next re-admission.
2. **Event-replaceable re-checks**: conditions GitHub already delivers events for. With healthy ingestion they
   are redundant.
3. **The safety net**: `periodic-re-eval`, which exists only to catch an admission the engine missed (the #2528
   shape) or a lost delivery.

An idle, webhook-healthy daemon with any in-pipeline item therefore polled about every 11 × `PollSeconds` mostly to
re-check conditions an event would have announced. A coupling hid behind that: items awaiting review or CI have
**no cooldown of their own while the gate waits**, so `periodic-re-eval` and `review-blocked` were the de facto
wakes for the CI backstop, the CI dwells and the review ladder. Lengthening the safety net without giving those
deadlines their own wake would have made them late.

## Inventory and classification

Every reason found by scanning every `CooldownRecorded` literal in non-test engine code, with the evidence for its
class. `decision-pending` and `sha-blocked`, named in the issue, are not cooldown reasons anywhere in code
(`decision-pending` is an attention code and a channel event). The table is the typed `cooldownClasses` map in
`engine/cooldown_class.go`; `TestEveryCooldownReasonIsClassified` (a `go/parser` scan, `engine/cooldown_class_scan_test.go`)
fails on an unclassified reason, a stale entry, or a reason it cannot resolve that is not on an audited list.

| Reason | Recorded by | Class | Evidence |
|---|---|---|---|
| `periodic-re-eval` | poll defer, `rearmPeriodicReEval`, cleanup stage | safety net | Catches a missed admission or a lost delivery. The cleanup-stage retry keeps `githubRecheckInterval()`. |
| `dep-blocked` | `checkDependencies`, `fabrik_set_blocked_by` | event-replaceable | `issue_dependencies` invalidates and admits the item; `DependentRefreshObserver` covers a blocker closing. The observer is cache-only, so the re-check is stretched only when every blocker is cached and ingestion is reliable for its repo. |
| `review-blocked` | review gate, review advance gate | event-replaceable | A review event admits the item through `cycleSet`. Its silence deadline is a timer (below). |
| `feedback-pending` | feedback gate | event-replaceable | `issue_comment`, `pull_request_review`, `pull_request_review_comment` and `pull_request_review_thread`, including edit and delete (#2352). |
| `feedback-read-failed` | feedback gate | short | Retry of a failed live read (ADR 2498). |
| `feedback-dispatcher-gap` | dispatcher blindness repair | short | Recorded at `PollSeconds/2` to refetch a cache the dispatcher cannot see through. It was capped as long only because it was missing from the short list; this ADR adds it (it tightens a cap only). |
| `merge-unsettled` | merge gates (five sites) | short | Transient GitHub merge state. |
| `retry-backoff` | stage retry | short | ADR 2232/2499. |
| `sentinel-live` | dispatch refusal | short | #1779. |
| `lock-write-failed` | lock refusal, orphan reclaim | short | ADR 2393. |
| `spawn-resume-deferred`, `spawn-recovery-deferred` | `spawnChildren` | timer (cooldown) | Retry of a failed live read; no event announces it. Keeps today's due + one interval cap. |
| `archive-eligible-at` | archive settle | exempt | An absolute "eligible since" time, not a gate. |

Time-based checks that run on admission, with deadlines anchored on label applied times, comment times or head
arrival. Each is a **timer**: nothing in GitHub's event stream announces the deadline passing.

| Check | Anchor | Hook |
|---|---|---|
| Review-wait timeout and Phase 1 re-prompt | `fabrik:awaiting-review` applied time, restarted by head arrival (ADR 2467) | `checkAwaitingReviewTimeout` |
| Phase 2 bot-reprompt | `fabrik:bot-reprompted` applied time | `checkBotPhase2Timeout` |
| `CIBackstopTimeout` | `fabrik:awaiting-ci` applied time, later of latest pause removal (`effectiveAnchor`) | `settleAwaitingCIScan` |
| `CIWaitTimeout` mergeable-state and never-checked dwells, liveness-stall dwell | same, or `LastCIProgressAt` | `classifyCIFromMergeableState`, `classifyCIFromCheckRuns` |
| Merge-queue stall dwell | `fabrik:auto-merge-enabled` applied time | `merge_gate.go` |
| Unknown-merge-state bound | in-memory clock | `checkUnknownMergeBound` |
| Lock-orphan grace | in-memory clock | `settleOrphanedLocks` |
| Review-wait head-time memo back-off (1 min) | a failed read | a short retry with no deadline of its own |

Merge-train deadlines use the train's own persisted run state, not admission.

## Decision

1. **One typed table.** `cooldownClasses` replaces `shortAdmissionCooldownReasons` and `gateCooldowns`; both are
   derived from it. Every reason is a constant, including the two `spawn-*` reasons and the feedback gate's
   variable-valued one (the scan's single audited exception).
2. **Timers get an in-memory deadline registry, not new cooldown reasons** (`engine/timer_wake.go`). Each gate
   records its own deadline from the "not yet elapsed" branch it already has, using the anchor and timeout
   variables that branch computed, so no anchor logic is re-implemented and no decision can drift. The registry
   decides nothing: `PollWithBackoff` caps the idle interval at the **exact** due (the short-cooldown shape, floor
   1 s), and `poll()` re-queues a due item through `mayNeedWork` (the `requeueDeferredDispatch` route) so the gate
   runs even when the item holds no bypass label. A re-note is floored at `mergeGateRecheckInterval()`, so a gate
   that still says "not yet" at the deadline cannot hot-loop the poll. Entries for closed items are pruned.
   New cooldown reasons (`ci-deadline`, ...) were rejected: they would have to follow admission semantics (ADR 2096,
   the pre-filter skip) and could hide an item.
3. **Exact due for registry timers, due + one interval for the safety net.** The issue text says both "the
   short-cooldown cap shape keyed on the deadline" and "the ADR 2528 cap applies to timers"; "about one poll
   interval" holds either way. Timers are the stricter case and get the exact due; cooldown-class timers
   (`spawn-*`) and the safety net keep ADR 2528's due + one configured interval. This amends ADR 2528 decision 3.
4. **Event-replaceable re-checks follow transport health, strictly.** `ingestTransport` gained `HealthyFor(repo,
   eventType)` and `transportSet.ReliableFor`. It reads the existing state, adds no signal, and is stricter than
   `IsHealthyOrStartingUp`: connected and past start-up (the gh-forward manager's `starting_up` does not count),
   not disabled, no tracked drift, repo covered, event subscribed; for App receivers the ADR 2452 route verdict
   must be `reachable` (`unknown` does not count) and installation coverage must report no missing repo. Unknown
   health, a missing transport and poll-only are all not reliable, and keep today's cadence. While reliable for
   the item's repo **and** every replacing event (and, for `dep-blocked`, every blocker), an event-replaceable
   reason is not counted by `earliestLongCooldown`. The aggregate `Healthy()` still drives `computeEffectiveInterval`.
5. **The safety net has its own interval.** `periodic_reeval_interval` (minutes; `--periodic-reeval-interval`,
   `FABRIK_PERIODIC_REEVAL_INTERVAL`, YAML; flag > env > YAML; negative refused at startup; `0` or unset = auto;
   minimum 1; `workerenv.Scrubbed`). While ingestion is reliable for the repo the poll defer and
   `rearmPeriodicReEval` stamp `periodic-re-eval` that far ahead (**default 30 minutes**); otherwise
   `githubRecheckInterval()` as today (an explicit value below that also applies). Stretched event-replaceable
   re-checks reuse this interval, so stretching and dropping are equivalent: the safety net re-evaluates the item.
6. **Health flips are acted on within about one poll** (`engine/health_watch.go`, FR-007). A tracked goroutine
   (stopped and joined in `Run()`, no GitHub call) snapshots per-repo reliability every `max(5 s, one poll)`. When a
   repo stops being reliable it pulls that repo's `periodic-re-eval` entries back to `githubRecheckInterval()` and
   makes a non-blocking `wakeCh` send (a buffered channel coalesces a flapping transport). Event-replaceable
   entries stay at 10 × poll and are simply counted again by the cap, so only the safety-net stamp needs the
   re-clamp.
7. **Cap-time exclusion, no new `ItemState` field.** Rejected: stamp-time stretching for every reason (every
   stretched entry would need a re-clamp), and a recorded-at field (a new Store field for a scheduling hint).

### Safety-net default (30 minutes)

The repository holds no quantitative delivery-loss rates. What it does show:

- Lost deliveries are healed by the 3-minute light reconcile (ADR 2351: overflow, crash and cache-pause drops),
  replay on the App receivers (ADR 2144) and the route-liveness check (ADR 2452). The safety net is the second line
  behind them, so a lost event costs at most one reconcile plus its interval.
- PAT-mode gh-forward has no replay, but `HealthyFor` is false for any repo or event it does not subscribe to,
  and the reconcile still heals cache drift.
- The one observed missed admission (#2528: Research → Plan at 09:38, next stage 10:11 by a human comment) was a
  dropped admission now fixed at the source; `periodic-re-eval` was the backstop that would have caught it.
- 30 minutes is the cooldown ADR 2528 already accepted at `poll: 180`, and is far above today's 5 minutes at
  `poll: 30`, which is the saving.

## Consequences

- An idle, webhook-healthy daemon holding only event-replaceable cooldowns polls about every 30 minutes (bounded
  by the 60-minute webhook idle cap) instead of every 11 × `PollSeconds`. Poll-only, unhealthy and unknown-health
  daemons are unchanged.
- Timer deadlines are acted on within about one poll interval of the deadline on an idle daemon, which they were
  not before.
- A dropped event for a stretched re-check is recovered by a poll within the safety-net interval. Any poll finds a
  GitHub-side change through the probe; what the safety net guarantees is that a poll happens.
- ADR 2096 holds: an expired entry still admits once and the drop sites re-arm through `periodicReEvalDue`. ADR 2232 /
  2499 hold: every retry keeps its short cooldown. ADR 039 holds: no wake flag was added; the registry re-queues
  through `mayNeedWork`, the same route as a deferred dispatch. ADR 2530 holds: a periodic-only admission is still
  not poll activity, which is what lets a stretched cadence take effect.
- No gate decides differently; only the time it is re-evaluated changes. The one decision-adjacent edit
  (`headRestartRemaining`) returns the time left in the window beside the unchanged boolean.

## Seams

Per-Engine: `SetEventReplaceableStretchDisabledForTest` (event-replaceable stretch and the safety-net stamp revert),
`SetSafetyNetDisabledForTest` (periodic-re-eval effectively never), `SetTimerWakeDisabledForTest` (no registry),
`AttachStubTransportForTest` with the exported `StubTransport` (scripted health, covered repos, subscriptions),
`HealthWatchTickForTest`. Sim twins: `tests/sim/cooldown_classes_test.go`.
