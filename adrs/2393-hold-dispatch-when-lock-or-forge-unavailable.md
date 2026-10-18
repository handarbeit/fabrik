# ADR 2393: Hold dispatch when the lock can't be written or the forge is unreachable

## Status

Accepted. Supersedes ADR 1413 for the `fabrik:editing` write failure charge only.
Mentioned for context: #2388 (the report); this change does not close it.

## Context

`acquireLockAndVerify` logged `could not add lock label` and carried on, so a worker was
spawned with no lock, no worker record and no `in_progress` label. Its results could not be
recorded, the next poll saw an untouched item and dispatched it again. In #2388 a GitHub
Enterprise Server host was unreachable for ~21 hours while the model API stayed up: one
Research stage ran about every 8 minutes, ~150 runs. The poll kept dispatching from the board
cache. Other gates already knew the forge was down (`feedback-gate: holding advance — live
feedback read failed`); nothing turned that into a decision.

## Decisions

1. **A failed lock write refuses the dispatch (R1).** `acquireLockAndVerify` returns the write
   error and nothing else happens: no worker handle, no heartbeat, no `in_progress` write, no
   `StageAttempted`, no retry counter. `processItem` logs one `[lock]` line naming the label and
   the error and records the admission cooldown `lock-write-failed` (one `PollSeconds`, consumed
   on admission per ADR 2096, listed in `shortAdmissionCooldownReasons` per ADR 2232). Applies to
   every cause, 4xx included. A persistently failing 4xx therefore retries once per poll with no
   cap; capping re-dispatches is a separate issue.
2. **A failed `fabrik:editing` write refuses a comment-review pass, uncharged (R2).** The write
   moved ahead of `recordCommentBreakerInvocation` and the 👀 reactions, so a refused pass leaves
   no side effect. It returns the new `didNotRunEditingWrite` (the reinvoke scaffold refunds the
   pre-charged cycle, comments stay unprocessed) and records `lock-write-failed`. This
   supersedes ADR 1413 for this one write: ADR 1413 charged the breaker so a persistently failing
   setup step could not loop unbounded; the loop is now bounded by the cooldown (one failed label
   write per poll, no model run). The base-branch and worktree setup failures keep the ADR 1413
   charge.
3. **An account-wide forge-unreachable hold (R3).** `forgeHealth` (`engine/forge_hold.go`) tracks
   the unbroken run of transport failures. It engages at ≥ 5 consecutive failures spanning ≥ 2 min
   on `e.now()`; a run with no further failure for 10 min lapses. Any non-transport outcome —
   success or 4xx, either proves the server answered — resets the run and lifts the hold.
   Transport errors (`github.IsTransportError`): timeouts and `context.DeadlineExceeded`,
   `*url.Error`, `net.Error`/`*net.OpError`, `io.ErrUnexpectedEOF`, a typed `*StatusError` ≥ 500
   and the text `GitHub API returned 5xx`. Never: 4xx, rate limits, `forbiddenError`,
   `context.Canceled`. In memory only; thresholds are engine defaults, not settings, so nothing
   needs a `workerenv` classification.
4. **Observation by result observer, not a client decorator.** `github.Client.SetResultObserver`
   is called with the final error from the two HTTP choke points (`doWithHeaders`,
   `graphqlRequest`); `New()` wires it to `Engine.ObserveForgeResult`. The sim bypasses
   `github.Client`, so `simgh.FaultTable.SetResultObserver` plays the same role and the sim env
   wires it behind the opt-in `EnvOptions.ForgeHold`. Both feed one classifier. An ~85-method
   `GitHubClient` decorator would need a reflection guard and risks the `e.client.(…)`
   type assertions for no extra coverage. Limit: the sim exercises the tracker, gates and
   classifier but not the HTTP observer, which has its own `httptest` test.
5. **Hold gates.** Early: `dispatchCandidates` hands every candidate back through the existing
   slot-deferred path (nothing recorded) and `dispatchReinvoke` refunds the pre-charged cycle and
   returns. Late backstops: `processItem` before the lock write, `processCommentsClassified`
   beside the usage-limit check (`didNotRunForgeHeld`) and `resolveConflictWithClaude` before and
   after the slot wait (`errForgeHeldNotAttempted`, which `trainCancelled` reads as the uncharged
   "not attempted" outcome). Running workers are never touched. No late check was added in
   `runInvocationWithExtension`: its error contract is per-error-type handlers, and the check in
   `processItem` precedes it by one worktree setup.
6. **Recovery.** While held, `dispatchCandidates` makes one `FetchProjectUpdatedAt` read per poll
   as a probe (a cache-served poll may make no other live call) and `PollWithBackoff` caps the
   idle interval at the configured poll interval, so the hold lifts within about one poll.
7. **Visibility (R4).** `warnings.Report` type `forge_unreachable`, subject `github`, stable
   fingerprint, resolved on lift. `fabrik_health.forge_hold {since, consecutive_failures,
   last_error}` from the `daemonHealth` mirror. The existing `pipeline-stalled`/`pipeline-resumed`
   events with `scope=forge`, derived from the health mirror only (the channel scan forbids
   GitHub reads), one pair per episode, included in catch-up. No catalogue or subscription change.
8. **Per-Engine neutralisation seams:** `SetLockWriteRefusalDisabledForTest` (R1/R2),
   `SetForgeHoldDisabledForTest` (R3).

## Dispatch-site audit

Exactly three non-test sites invoke the model.

| Site | Claim taken before the worker | Rule |
|---|---|---|
| Stage dispatch: `processItem` → `acquireLockAndVerify` → `runInvocationWithExtension` | `fabrik:locked:<id>` label | Lock-write failure refuses (decision 1). Hold gated early (`dispatchCandidates`) and before the lock write. |
| Comment-review family: `processCommentsClassified`, from `processComments` and `dispatchReinvoke` (CI-fix, review, rebase) | `fabrik:editing` label | Editing-write failure refuses, uncharged (decision 2). Hold gated early (`dispatchReinvoke`) and beside the usage-limit check. |
| Merge-train conflict resolution: `resolveConflictWithClaude` | none: the in-memory `ConflictResolutionSet` plus a worker slot | No label write exists to gate. Covered by the hold only; refusals are the uncharged "not attempted" outcome (no ejection, no `recordTrial`, no dissolve, no pause). |

## Consequences

- **Writes-only failure never engages the hold.** Any success lifts it, so a forge where only
  writes fail (reads keep answering) is handled by decision 1 alone: the item is retried once per
  cooldown and the model is never handed it. Sim scenario A covers this; scenario B (every call
  fails, cache-served poll) covers the hold.
- A lock write that timed out but reached the forge leaves a stray `fabrik:locked:<id>`; the
  existing stale-lock sweeps remove it. No best-effort remove is attempted (it would likely fail).
- Out of scope, unchanged: recording a run that finished during the outage, and capping
  re-dispatches.
