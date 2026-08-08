# ADR 1208: Queued Review-Finding Ejection

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1208 — merge-train `Queued` members process no PR feedback for a whole batch cycle

## Context

While an item sits in the `Queued` holding stage awaiting a merge-train batch, Fabrik processes no PR
feedback at all — not review findings, not human comments. The admission guard shared by both catch-up
phases in the poll loop skips any item whose stage is `HoldingStage: true` before either phase runs:
correct for *dispatch* (a batched member must not run its stage prompt individually while queued), but
too broad — it also suppresses *feedback processing*, a different concern from running a stage.

This is worse than #1207 (the sibling Validate-side race, closed 2026-07-29): #1207 lost a finding that
landed in the seconds between Validate completing and the merge firing. This loses **everything** that
arrives across a whole batch cycle — batch formation, trial-branch assembly, CI, possibly inline
conflict resolution and bisection — easily tens of minutes, and a live merge-train worker can block
synchronously inside `pollTrainCI` for up to `CIWaitTimeout` (default 30 min). It is an active window,
not a quiet one: Pruefer re-reviews on every new head SHA, and train activity produces new SHAs, so it
is precisely when a queued member is most likely to attract review comments that Fabrik is guaranteed
not to read them.

Processing feedback in place is the wrong shape: addressing a finding means pushing a commit, which
changes the member's head SHA mid-batch — invalidating a trial branch the train may have already
assembled, CI'd, or bisected, racing the train's own state machine. The train already knows how to
remove a member (`ejectMember`, called on fetch failure, unresolvable conflict, and bisection
isolation) — this issue is a new *trigger* for that existing mechanism, not new removal machinery, with
one behavioral difference: the three existing causes deliberately leave the member in `Queued` to retry
in a future train, because nothing about those causes requires the member to leave. A review-finding
ejection is different — the whole point is to get the finding in front of the normal reinvoke path,
which does not run on `Queued` items, so this ejection must also move the item's board Status off of
`Queued`.

## Decision

### 1. A dedicated settle scan, not a fold-in to existing merge-train batch logic

`settleQueuedReviewFindings` (`engine/queued_review_settle.go`) is the sixth instance of the ADR-1270
"durable condition invisible to the shared, admission-gated catch-up path, recovered by a dedicated
`board.Items`-sourced settle scan" pattern. It runs every poll, independent of merge-train worker state,
which is the only design that can catch a finding arriving during a live worker's `pollTrainCI` wait —
the scenario this issue's motivation centers on. Folding detection into `routeQueuedGroup`/
`fetchTrainMembers` (only evaluated at "train idle" moments) was considered and rejected: it is
structurally incapable of seeing the mid-CI-wait window, which is the majority of a batch cycle's
duration.

It deep-fetches every Queued internal-merge-train member itself (`FetchItemDetails`) — `deepFetchCandidates`
never does this for a `HoldingStage` item — and checks `currentHeadReviewThreadComments`, the
ADR-1207-canonical, current-head-scoped detection primitive, reused rather than forked so a thread
against a superseded commit never triggers a spurious ejection and the two detection definitions never
drift apart.

### 2. `ejectMember` gains a `stayInQueue bool` parameter, not a sibling function

The issue's Requirements explicitly ask to reuse the existing `ejectMember` mechanism (comment posting,
`MaxMergeTrainEjections` counter, pause-at-cap). Rather than fork a parallel ejection function, `ejectMember`
gained one boolean parameter controlling its closing sentence. All five pre-existing call sites pass
`true` (unchanged wording, unchanged behavior); the new cause passes `false`, producing the textually
distinguishable opposite: "This issue has left the Queued column so the unresolved review-thread finding
above can be addressed via the normal review pipeline. Once addressed and Validate completes again, it
will re-queue and join a later batch." This satisfies the issue's requirement that an operator can tell
from the ejection comment alone which of the four causes fired, at the cost of touching the shared
function's signature — mitigated by keeping the diff to one added parameter and one conditional branch,
not a restructuring, since a mistake there would alter the wording of the three unaffected causes' own
tested comment content.

### 3. Reroute target derived structurally (`stageBeforeHolding`), not hardcoded `"Validate"`

`stageBeforeHolding(cfg, hs)` returns the non-`Unmanaged` stage with the highest `Order` strictly less
than the holding stage's — mirroring the existing `holdingStage`/`cleanupStage` order-based-lookup idiom
already in `engine/stages.go` — rather than a literal `"Validate"` string. This survives a custom stage
config whose pre-`Queued` stage isn't literally named `Validate`, at negligible extra cost.

### 4. Reroute happens before the ejection comment/counter, not after

`ejectQueuedMemberForReviewFindings` calls `rerouteQueuedMemberOffHolding` first; if the status move
fails, nothing is posted and nothing is counted. Doing it in the other order would risk a duplicate
ejection comment (and a double-counted `MaxMergeTrainEjections` increment) on a transient board-mutation
failure, since the settle scan would simply re-detect the same unresolved thread on a member still
sitting in `Queued` and retry the whole operation next poll — reroute-then-eject makes a failed attempt
indistinguishable from "nothing happened yet."

### 5. The reroute is a plain status move — no label mutation, no `ReviewCycles` reset

`rerouteQueuedMemberOffHolding` only calls `UpdateProjectItemStatus` (plus the usual cache
write-through/`SelfWriteObserved`/webhook-echo bookkeeping `advanceToQueued` already uses for the
reverse move). It deliberately never touches `stage:Validate:complete` (already present from the
original Validate completion, and never removed by `advanceToQueued`/`advanceToNextStage` on the Queued
visit) and never calls `EngineCyclesCleared` (the only mutation that zeroes `ReviewCycles`, otherwise
reachable only via manual-intervention paths). This is what makes `MaxReviewCycles` apply across the
eject/re-queue cycle "for free": the very next poll admits the rerouted item to Phase 1 with
`hasComplete=true`, `handleReviewGate` — completely unmodified by this issue — finds the same
still-unresolved thread comment, and dispatches through the existing `dispatchWithCycleLimit`/
`MaxReviewCycles` gate exactly as ordinary CHANGES_REQUESTED reinvocation does. This is a negative
constraint that is easy to violate accidentally (e.g. by copying a manual-unpause/`fabrik:revalidate`-style
reset), so it is pinned by a dedicated test
(`TestEjectQueuedMemberForReviewFindings_MaxReviewCyclesComposesAcrossCycle`) asserting the counter
keeps incrementing — never resets — across a full eject → reroute → reinvoke → re-Queue cycle, and
eventually escalates via `pauseForReviewCycleLimit`. `MaxMergeTrainEjections` (a separate, pre-existing
bound reused via `ejectMember`) fires independently of `ReviewCycles` from the same repeated-ejection
counter every other cause already increments — the two bounds are not substitutes for each other, and a
second dedicated test confirms `MaxMergeTrainEjections` alone can pause a member purely from repeated
review-finding ejections before `MaxReviewCycles` would ever fire.

### 6. Concurrency: a checkpoint signal consumed by the worker itself, not the settle scan mutating a live batch

The three pre-existing `ejectMember` call sites all run from inside the merge-train worker goroutine
that owns the batch state — no concurrent-mutation hazard, since the same goroutine that decides to
eject also owns the slice being mutated. This issue's new trigger fires from the poll loop, which can
run concurrently with a worker blocked up to `CIWaitTimeout` inside `pollTrainCI`. Reaching into that
goroutine's own in-memory batch slice from outside it would race the worker's own assemble/validate/land
sequence.

The settle scan therefore only ejects directly when `!mergeTrainWorkerActive(repoKey)` — safe, nothing
else is touching the member. When a worker IS in flight, it records a pending-eject signal instead
(`markPendingReviewEject`/`takePendingReviewEject`, a mutex-guarded `map[string]map[int]int` on `Engine`
keyed `owner/repo` → issue number → finding count, cleared on read for one-shot semantics) rather than
mutating anything. The worker consumes the signal itself, from inside its own goroutine, at two
checkpoints (`applyPendingReviewEjects`):

- **`runMergeTrainWorker`'s re-form loop**, immediately after `assembleAndValidate` returns and the
  existing Hook 1 runaway-guard check — mirroring that guard's own established "poll writes a signal,
  worker checks it at a checkpoint" shape (`isRunawayTripped`/`mergeTrainTrialsMu`) rather than inventing
  a new coordination primitive.
- **`landOneAtATime`'s per-singleton loop**, right before its green/red/pending outcome switch, using
  the natural 1-element slice.

At both checkpoints, a flagged member's current trial is discarded regardless of its own CI result — a
green trial containing a flagged member must never reach `landGreenBatch`, since "checkpoint-only,
eventually consistent" is not acceptable for that one property even though it is acceptable for
reaction latency (see Consequences). The loop re-forms with the reduced membership; an empty remainder
falls through to the existing zero-survivors return, needing no special-casing.

### 7. Scope: mid-flight ejection only, no admission-time filtering

The issue's own Scope section explicitly deferred to Plan whether to also refuse a member with
unresolved threads admission into a *forming* batch, in addition to ejecting mid-flight members. This
was left out: mid-flight ejection alone satisfies the Definition of Done, and admission-time filtering
would touch `groupQueuedByRepo`/`routeQueuedGroup`'s batch-composition logic for a case the issue's own
motivation (a finding landing *after* the member is already Queued, since #1207 already guards the
moment Validate hands off) narrows to a secondary concern.

### 8. Native-merge-queue members are skipped

An item on the GitHub-native queue path (`MergeQueue != "off" && LinkedPRIsMergeQueueEnabled`, or
already carrying `fabrik:auto-merge-enabled`) is not an internal-train member — `ejectMember`/
`MaxMergeTrainEjections` have no meaning for it — so the scan skips it, mirroring `routeQueuedGroup`'s
own FR-3 precedence exactly.

## Consequences

**Positive:**
- Closes the blackout window #1207 explicitly named as its own follow-up: a Queued member's PR feedback
  is no longer silently ignored for the duration of a batch cycle, including the multi-minute CI-wait
  window a live worker blocks inside.
- Reuses every existing mechanism (`ejectMember`, `currentHeadReviewThreadComments`,
  `dispatchWithCycleLimit`/`MaxReviewCycles`, the runaway guard's checkpoint-signal shape) rather than
  building parallel machinery — the only genuinely new code is the settle scan, the reroute function, the
  `stayInQueue` parameter, and the pending-eject signal.
- `MaxReviewCycles` composes across the eject/re-queue cycle without any new counter or plumbing, closing
  the issue's central stall risk by construction rather than by a new bespoke bound.
- An operator can distinguish this ejection cause from the other three from the comment text alone.
- Establishes the pending-eject-signal pattern as a second, independently-verified instance of "poll
  writes a signal, worker consumes it at its own checkpoint" — a template for any future
  externally-triggered merge-train mutation that needs to reach a live worker goroutine safely.

**Negative / Trade-offs:**
- Checkpoint granularity, not real-time preemption: a pending eject flagged mid-bisection is only applied
  at the next outer-loop `assembleAndValidate` checkpoint, not immediately. Acceptable — the runaway guard
  has the identical granularity — but a future reader should not assume sub-second reaction time.
- Extra GraphQL cost: deep-fetching every Queued member every poll (to see fresh `reviewThreads`) adds API
  load proportional to batch size × poll frequency. `settleAwaitingCIScan` already accepted this same
  tradeoff for CI state; no cheaper signal (e.g. skipping when `updatedAt` hasn't moved) was added here,
  consistent with that precedent.
- `ejectMember`'s signature change touches all five pre-existing call sites' shared code path — a future
  regression there risks altering the wording of the three unrelated ejection causes, which have their
  own existing tests asserting on comment content. Mitigated, not eliminated, by keeping the diff minimal.
- This is additive to ADR-059's ejection mechanism, not a rewrite: a fifth future ejection cause would
  need to decide for itself whether it belongs inside `ejectMember`'s `stayInQueue` dichotomy or needs a
  third state — this ADR does not generalize the parameter beyond the two values it currently needs.

## References

- ADR-059: Internal Merge Train (`ejectMember`'s original three causes, the runaway guard's two-hook
  precedent this issue's pending-eject signal mirrors)
- ADR-1420: Merge-Train Ejection Diagnostics (`diag`/`otherMembers` contract — this new cause passes
  `nil, nil`, consistent with the three pre-1420 call sites)
- ADR-1270: Awaiting-CI Settle Scan (the direct template for this settle scan's admission-independence
  shape — the fifth occurrence of the pattern; this issue is the sixth)
- ADR-1207: Yolo-Merge Review-Thread Guards (the sibling Validate-side race; explicitly hands off "the
  merge-train `Queued` blackout window... tracked separately in #1208"; establishes
  `currentHeadReviewThreadComments` as the canonical detection primitive this issue reuses)
- ADR-067: Merge-Train Centralized In-Flight Cleanup (`finishTrain` as the single clear point for
  `mergeTrainInFlight`/`Store.ExitRepoWorker`; the worker-checkpoint early-return paths added by this
  issue still route through it)
- `docs/state-machine.md` §6.16 (as-built specification for this settle scan)
- Issue #1208, Issue #1207
