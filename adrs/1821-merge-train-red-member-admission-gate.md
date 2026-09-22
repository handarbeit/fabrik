# ADR 1821: Merge-Train Own-PR CI Admission Gate

**Date**: 2026-09-20
**Status**: Accepted
**Issue**: #1821 — merge-train: admits members whose own PR CI is already red, then bisects for hours to rediscover it

## Context

`fetchTrainMembers` validated two things per Queued member: a linked PR exists, and it has a head SHA. It
never read the member's CI. A member whose own PR checks were already red was admitted, assembled into a
trial, and discovered only by that trial's CI or by bisection. On `verveguy/concept-maps` two such members
(both failing the same spec on their *own* PRs, at heads unchanged for hours) blocked eleven Queued items for
93+ minutes with zero landings; clearing one poisoner from an eight-member batch cost about two hours (a 54-minute
assembly, a 24-minute CI cycle, re-bisection), and the second repeated the episode.

`classifyLandingCI` (ADR-1153/ADR-1441) already answers "is this head's CI red", and ADR-1644 already trusts
it for a landing decision — but only on the singleton fast path, never at admission.

## Decision

### 1. A fail-open admission gate at fresh batch formation

`admitTrainMembers` runs in `prepareTrainWorker` after `fetchTrainMembers`. For each member it reads
`e.client.FetchCheckRuns` at the head SHA `fetchTrainMembers` snapshotted and classifies with
`classifyLandingCI`, **unmodified** (ADR-1644 R1.2). Only `TrainCIRed` defers. Pending, green, zero check
runs, a `FetchCheckRuns` error, or context cancellation all **admit**, byte-identical to pre-#1821 behavior.

This is the deliberate inverse of ADR-1644's fail-closed polarity. The fast path *skips* validation, so it
needs positive evidence to act; the gate *removes* a member, so it needs positive evidence of failure to act.
Deferring on ambiguity would silently drain the queue and strand members that have done nothing wrong. The
accepted cost: a red member whose run is pending, whose head has zero runs, or whose read errors is found by
the trial exactly as before.

Live REST (`e.client`), not the boardcache-backed `readClient`: "positively confirmed red" needs current
evidence, and the cache trusts a stale PENDING.

### 2. Placement in `prepareTrainWorker`, not `fetchTrainMembers`

`fetchTrainMembers` has three callers. Two are restart reconstruction (`completeDeferredLanding`,
`resumeTrain`) re-resolving an already-decided batch; gating them would re-litigate admission for members
already committed to a train. `prepareTrainWorker`'s fall-through after `reconstructTrainState` returns `false`
is reached only by a genuinely fresh batch. It also has `state.projectID`, which the reroute needs.
Bisection sub-trials, `landOneAtATime` and `landGreenBatch` are structurally out of reach.

### 3. Precondition: the reroute target must have `wait_for_ci`

A deferred member is rerouted (ADR-1208's `rerouteQueuedMemberOffHolding`) to `stageBeforeHolding` with
`stage:<X>:complete` intact. Phase 1's `handleMergeAndCIGates` then claims it: red checks →
`PRMergeBlocked` → `checkCIGate` → `ciFailure` → `fabrik:awaiting-ci` + `dispatchCIFixReinvoke`. Phase 2
(`attemptMergeOnValidate` → `advanceToQueued`) is never reached for a claimed item, so the member is not
advanced back to Queued even under yolo. But `checkCIGate` is a no-op without `wait_for_ci`. Without it a
deferred member would bounce back to Queued and be re-deferred every poll (yolo) or strand (cruise). The
gate therefore admits every member without a single read when the target lacks `wait_for_ci`, logging once
per formation. This is stricter than "defer on red", and it fits the fail-open spirit: defer only when
re-pickup is guaranteed. The shipped `validate.yaml` sets `wait_for_ci: true`.

### 4. Never paused, never counted

`deferRedMember` does not touch `mergeTrainEjectionCounts` and never calls `pauseMergeTrainMember`, so a
deferral cannot accumulate toward `MaxMergeTrainEjections`. ADR-1545's red-singleton disposition pauses
because that failure was only ever observed on a synthetic trial branch and nothing external would re-detect
it. Here the failure is on the member's own PR check-runs, so the ordinary CI gate has an external signal —
that reasoning does not carry over.

### 5. Ordering and failure behavior (ADR-1208/1545)

Reroute first. On failure: no comment, no signal consumption, no dedupe record — but the member is still
excluded from the current batch, because it is confirmed red; the next poll retries the whole operation. On
success: consume any pending review-eject signal for the member (avoiding double-routing/commenting and a
stale signal firing later on a re-queued member — deliberately not #1557's unconsumed-signal problem), then
post the comment.

### 6. A locally composed comment

`ejectMember(stayInQueue=false)` hard-codes review-finding wording, increments the ejection counter and
pauses at the cap; `ejectRedSingleton`'s wording assumes "combined Validate" and a pause;
`renderDiagnosticBlock`'s header prints `trial <sha>, integration PR #<n>` (misleading for a member's own PR)
and `renderBatchContext(nil, …)` says "moved base branch alone". None fit. `composeCIDeferralBody` composes
the body locally and shares only `renderFailedChecks` (ADR-1420), so failing checks render identically to
every other merge-train diagnostic; a required-context red (no failing runs) falls back to the classifier's
`detail`. The shared renderers' pinned strings are untouched.

### 7. Ping-pong backstop: in-memory SHA-keyed dedupe

`mergeTrainCIDeferred` (`owner/repo#N` → head SHA) suppresses a repeat comment when the same SHA is
re-deferred (reachable only by a live-red vs. cached-green divergence in Phase 1). The reroute still happens;
the entry clears when the member is next admitted non-red. In-memory, consistent with
`mergeTrainEjectionCounts`/`mergeTrainRunawayAlerted`; a restart costs at most one duplicate comment. A
durable label would be disproportionate for comment suppression.

## Consequences

- One `FetchCheckRuns` per member per fresh batch formation, repeating per poll while a trial is pending or
  timing out. `FetchCombinedStatus` (cache-backed) is added only when `required_status_contexts` is
  configured and a required name is missing from the check runs. Every defer/admit is logged with its reason.
- A deferred member holds both `stage:<X>:complete` and `fabrik:awaiting-ci` for the CI-fix cycle — a
  sustained state that was previously only transient. Existing guards cover it (`snap.Worker() != nil`,
  `hasCIGatePauseComment`); `MaxCiFixCycles`/`CIBackstopTimeout` can still pause a slow fix as an ordinary
  Validate pause.
- A crash between the reroute and the comment leaves a rerouted member with no comment (same as ADR-1208/1545).
- A repo whose pre-Queued stage lacks `wait_for_ci` gets no gate (documented, logged).
- A red member inside the first `MaxBatchSize` wastes one slot for one poll.
- `classifyLandingCI`, `singletonFastPathEligible`, and the fast path's fail-closed rules are unchanged.

## Out of scope

The shared-index conflict tax (many conflicts on one file, members ejected on Claude turn limits); changes to
#1688 or #1557 behavior; deferring for pending/missing/unreadable CI, review findings, or conflicts.

See `docs/state-machine.md` §6.25.
