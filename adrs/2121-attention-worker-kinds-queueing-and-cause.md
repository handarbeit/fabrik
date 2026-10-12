# ADR 2121: Attention recognises every worker kind, tells queued from running, never calls dispatchable work idle, and says why

Status: accepted (issue #2121; amends [ADR-1966-a](1966-a-fabrik-mcp-read-api.md) and [ADR-1966-b](1966-b-fabrik-mcp-channel-events.md), parent initiative #1966; builds on [ADR 2114](2114-nonblocking-worker-slot-acquire.md), [ADR 2120](2120-suspended-decision-state.md) and [ADR 2170](2170-channel-anomaly-events.md))

## Context

An in-situ test against the live dev daemon found the attention verdict ("stuck, or waiting correctly?") wrong whenever the worker was anything other than a plain stage run:

- **F4** — a comment-review worker looked like a stage run waiting on CI, because the stage placeholder handle carried only the stage name and `fabrik:awaiting-ci` outranked the worker.
- **F15** — a running review reinvoke was reported `waiting` on `cooldown:feedback-pending`; `fabrik:reworking:<Stage>` had no meaning.
- **F16** — `dispatchReinvoke` applied `WorkerEntered` on the poll goroutine before its goroutine acquired `e.sem`, so a queued reinvoke was "in flight" and `in_use` read 6 of 5.
- **F10** — a cruise item waiting for a slot was `idle`.
- A killed item with a retry pending was `stalled` with no reasons; a feedback-gate hold was `stalled / stage-complete-not-advanced` with no reasons.
- The maintainer-approved requirement: every state change says why.

## Decisions

### 1. The worker record says what a worker is and whether it holds a slot

`WorkerHandle` gains `Kind` (`stage-run`, `comment-review`, `review-reinvoke`, `ci-fix-reinvoke`, `rebase-reinvoke`, `merge-train-conflict`), `Trigger`, `Queued`/`QueuedSince` and live `TurnsUsed`/`TurnsKnown`. The handle **stays non-nil while queued**: about a dozen `Worker != nil` guards (double-dispatch, shutdown pause, janitor, overseer refusals, channel settle code) mean "this item is claimed", and a nil handle would reopen double dispatch. Those consumers were each reviewed; for a queued item they all do the right thing (shutdown pauses it, overseer actions refuse it). Alternative rejected: a separate queue structure — every guard would need to learn it.

`dispatchReinvoke` applies a queued `WorkerEntered` and `WorkerSlotAcquired` immediately after `e.sem` is taken (clearing `Queued`, setting `StartedAt`/`LastSignAt` to the acquisition time). `WorkerSlotAcquired`, kind and turn updates set `WorkerChanged` only — never `WorkerLifecycleChanged` — so they wake nothing and cannot loop. `dispatchCandidates` already took its slot first (ADR 2114); a guard test pins the audited set of `WorkerEntered` sites, so a new site must be reviewed for this ordering. The stale-worker detector skips queued handles (nothing to be stale about; the deferred `WorkerExited` ends the record), and the queue wait is not bounded.

The comment-review kind is applied by `processCommentsClassified` onto the stage placeholder, never over a reinvoke kind, and restored after the pass.

### 2. A merge-train conflict resolution is not a `Worker`

The member has no worker handle today, and the stale-worker detector, shutdown pause set, janitor and overseer refusals all treat a handle as a stage worker; none should act on a train conflict resolution. It is a separate `ItemState.ConflictResolution{Queued, Since}`, set around the slot wait and the invocation in `resolveConflictWithClaude` and cleared in a defer. The classifier treats it as a worker.

### 3. A worker outranks every wait and cooldown (R4)

The worker row moved above the wait labels. This changes existing verdicts (an `awaiting-ci` item with a CI-fix reinvoke running is now `working`); the pinned tests were updated deliberately and the §7.13 table rewritten. While a worker is present `cooldown:*` reasons and deadlines are dropped. The `State` enum is unchanged: queued, waiting-for-dispatch, retry and gate-hold are `waiting` with new codes, unranked, so subscribers and `fabrik_board` filters keep working. Only a slot-**holding** worker is `working`. Alternative rejected: a new `queued` state.

### 4. Waiting for dispatch is a cache-only predicate

`dispatchable` fails closed on anything the cache cannot vouch for and is not conditioned on autonomy (a stage dispatches whatever the autonomy label; autonomy governs advancing). The reason is all slots busy, a cooldown that will really re-dispatch (`retry-backoff`, `sentinel-live`), or the next poll. Gate cooldowns, `dep-blocked` and `periodic-re-eval` are never offered. A sim test pins the predicate against the real dispatcher.

### 5. Retry and gate-hold facts live in the Store

`PendingRetry` and `GateHold` were held only by the channel anomaly detectors, which are absent without a hub, so a hub-less daemon could not explain a starved retry. They are written from the existing hook points (`emitRetryScheduled`, `noteGateHold`, `noteGateClear`) before the hub nil check, and cleared on dispatch, completion, pause and close; a gate hold older than the detector's lapse is ignored. The hub keeps its own episode memory, fed from the same call, so there is one write point.

### 6. The cause vocabulary is fixed and each cause rests on a recorded fact

`wall-time-kill`, `max-turns` (from `PendingRetry.Cause`), `slot-starved` (queued, or waiting for dispatch with every slot busy), `gate-hold:<gate>`, `daemon-stop` and `rebase-conflict-after-landing`. One cause per result; unknown means absent. `inactivity-kill` and `api-error` appear in summaries but are not vocabulary causes. The two inference-prone causes need explicit facts: `daemon-stop` requires a `<!-- fabrik:pause-cause=daemon-stop -->` marker in the shutdown pause comment (it survives a restart because it is on GitHub; a bare stale `in_progress` label, a crash or a later pause comment yields no cause); `rebase-conflict-after-landing` requires the in-memory `RebaseCause` written only when `invalidateConflictingQueued` finds the conflict after a landing. On the wire it is `attention.cause` and, on pushes built from the same classification, `meta.cause` (pause family, `item-wedged`). `retry-starved` keeps its existing `cause` meta (the engine's own retry cause, a superset of the vocabulary, per ADR 2170).

### 7. Health counts held slots

`workers.in_use` is the number of store workers with `Queued == false`, clamped to `max_concurrent` (the clamp covers the instant between a `WorkerExited` and the next `WorkerEntered`); `queued` is the rest. `len(e.sem)` is used only for the "all slots busy" dispatch reason, where it is the truth. Train conflict slots stay in `train_in_use`.

### 8. Live turns are wired through an invocation hook

`InvokeOptions.OnTurn` is called by `turnCountingWriter`; `liveTurnRecorder` applies a `WorkerTurnsRecorded` for the invocation start (zero turns: forget the previous invocation's count, record its effective budget `TurnsMax`) and for every turn, so the count is exact and the budget is the invocation's own (comment-review limit, `fabrik:extend-turns`), not the stage default. The TUI-only `claudeTurnProgress` global (keyed by issue number alone) is untouched. A path that cannot supply a hook leaves turns `unknown`.

### 9. Label meanings are enforced by a scan

`MeaningOf` resolves exact names and prefix families (`reworking:`, `locked:`, `credited-pr:`, `spawned-child:`). There is no single registry of label constants (`staticLabelDefs` is incomplete), so `internal/attention/labels_scan_test.go` scans non-test sources for `fabrik:*` label literals, modelled on `workerenv`'s literal scan, and fails for any without a meaning that is not on an explicit exemption list with a reason; a non-vacuity test proves it fails for a fake label.

## Consequences

- Read paths stay GitHub-free (`TestLocalAPISourceDoesNotReachGitHub`, `TestChannelEventsSourceDoesNotReachGitHub`); every wire addition is optional.
- A queued handle is visible to shutdown and the overseer refusals; both are desirable.
- `docs/state-machine.md` §7.13/§7.14 and the USER_GUIDE "Overseer Tools" section describe the as-built behavior.
