# ADR 1545: Red-Singleton Reroute Before Pause

**Date**: 2026-08-11
**Status**: Accepted
**Issue**: #1545 — merge-train: a member paused for its own validation failure is stranded in Queued, unreachable by any stage

## Context

When a merge-train trial fails and the engine concludes the failure is the member's **own** — a red
combined Validate on a true singleton, or a member that still fails combined Validate even in isolation
via the one-at-a-time fallback — `ejectRedSingleton` (#1440) paused the member **in place, inside the
`Queued` holding column**, and posted a comment instructing a human to fix it and remove `fabrik:paused`.

An item in that state was unreachable by the engine in three independent ways: `itemMayNeedWork`
unconditionally excludes `HoldingStage` items from dispatch; the comment-driven unpause path lives in
`processItem`, which a `HoldingStage` item never reaches; and `settleQueuedReviewFindings` applies the
same closed/`fabrik:paused` exclusion `routeQueuedGroup` does, so a paused member is skipped there too.
The pause instruction therefore had an unstated precondition: it assumed a human would edit the board
column by hand, since the engine could not fix its own reported failure — fixing requires a stage, and a
stage requires leaving the holding column.

`ejectQueuedMemberForReviewFindings` (ADR-1208) had already solved the structurally identical problem for
a different trigger (an unresolved PR review-thread finding arriving while a member sits Queued): it
reroutes the member off Queued to `stageBeforeHolding` (normally Validate) via
`rerouteQueuedMemberOffHolding`, *before* posting its ejection comment or incrementing any counter, so a
transient board-mutation failure produces neither a duplicate comment nor a double count.

## Decision

### 1. Reuse `rerouteQueuedMemberOffHolding` and the reroute-before-side-effects ordering, unchanged

`ejectRedSingleton` now calls `rerouteQueuedMemberOffHolding(projectID, m.item)` as its first action. On
failure it logs and returns — no comment, no pause — mirroring `ejectQueuedMemberForReviewFindings`
exactly (R2). A failed reroute looks like nothing happened; the very next poll's train re-forms the same
singleton and re-hits this same disposition, retrying the whole operation. `rerouteQueuedMemberOffHolding`
itself is untouched: it remains a plain board-Status move that never touches `stage:Validate:complete` or
any `ReviewCycles` counter, a contract #1208 already pins with its own tests. `ejectRedSingleton` and
`landOneAtATime`'s red-singleton branch (both call sites) already have `state.projectID` in scope, so
threading it through is mechanical — no new plumbing, no new concurrency hazard (both call sites already
run inside the worker goroutine that owns the relevant state).

### 2. R3: reroute + pause, not reroute-without-pause

This is the one place this issue's design **diverges** from #1208's precedent, and the divergence is
deliberate, not an oversight.

`ejectQueuedMemberForReviewFindings`'s "reroute without pause" works because a review-thread finding has
an external, persistent signal independent of Fabrik's own actions: once rerouted, `handleReviewGate`
(completely unmodified by #1208) finds the same still-unresolved thread on the very next poll and
dispatches a review-reinvoke through the existing `MaxReviewCycles`-bounded path — "for free."

A standalone combined-Validate failure has no equivalent signal. The failure was observed only on the
merge-train's synthetic combined trial branch — assembled and torn down by `assembleAndValidate`/
`cleanupTrialArtifacts` — never persisted anywhere the catch-up loop reads from. The member's own PR CI
already passed (that's how it reached Queued in the first place), so `handleMergeAndCIGates`'s CI-failure
branch finds nothing wrong on the member's real, already-green checks. The ejection comment itself is
also invisible to comment-driven redispatch: `findNewComments` skips any comment whose body starts with
`🏭 **Fabrik`, and both `ejectRedSingleton`'s and `ejectMember`'s comments carry that prefix.

Consequently, rerouting without pausing would not self-heal the way the review-findings case does — it
would land the member inertly on Validate, with `stage:Validate:complete` already present, no unresolved
review thread, no CI regression, and no non-bot comment. Every Phase 1 handler would fall through and
nothing would ever claim it. This is a **quieter** stranding than the pre-#1545 bug: no `fabrik:paused`
signal at all, the item silently absent from all future Queued batches, and only the human-readable but
machine-invisible ejection comment as a trace.

The chosen design therefore keeps the pause — the human gate the original #1440 design already relied on
— but now on `stageBeforeHolding` (normally Validate), a column where clearing the gate actually has
somewhere to act.

### 3. R4: point the recovery instruction at `fabrik:revalidate`, not a bare `fabrik:paused` removal

Even with the reroute, "remove `fabrik:paused` to resume" does not work as stated, independent of this
issue's reroute fix. `itemNeedsWork`'s paused branch only resumes dispatch when a *human comment* is also
present; bare label removal with no comment falls through to the "already completed this stage" check,
which — since `stage:Validate:complete` is still set (the member's Validate genuinely completed before it
ever reached Queued) — returns `false` forever. `pauseMergeTrainMember` also does not apply
`itemstate.EnginePaused`, unlike the shared `pauseIssue` helper other `pauseFor*` sites use, so the usual
`handleEngineUnpause` reset does not apply here either.

Rather than migrate `pauseMergeTrainMember` onto `pauseIssue`/`itemstate.EnginePaused` — a change to
shared, already-tested code, for a benefit this fix doesn't need — the ejection comment now names the
reroute target stage and instructs applying `fabrik:revalidate` instead. `fabrik:revalidate`'s existing
handler (`handleRevalidateLabel`) already clears exactly the label set that blocks re-entry
(`stage:Validate:complete`, `stage:Validate:failed`, `fabrik:paused`, `fabrik:awaiting-input`,
`fabrik:awaiting-ci`, `fabrik:auto-merge-enabled`) together, resets the store's retry/cycle counters, and
is itself poll-retried on partial failure — reusing it is strictly safer than inventing new
label-clearing logic inside `ejectRedSingleton`.

### 4. Target stage name resolved locally, not by widening `rerouteQueuedMemberOffHolding`'s signature

`rerouteQueuedMemberOffHolding` still returns a plain `bool`. Giving it a second return value to expose
the resolved target stage's name would touch its own existing tests and its other caller
(`ejectQueuedMemberForReviewFindings`) for no benefit to that caller. `ejectRedSingleton` instead calls
`stageBeforeHolding(e.cfg, holdingStage(e.cfg))` a second time, purely to name the target in its own
message — cheap and pure, no new state.

### 5. R5 audit: only `ejectRedSingleton` needed a code change

Every other site that applies `fabrik:paused` to an item while it sits in a `HoldingStage` column was
audited and is recorded in `docs/state-machine.md`'s "Pause-in-Holding-Column Audit (#1545)" section:

- **`fireRunawayGuard`** — exempted. Its recovery path is already reachable without a reroute:
  `groupQueuedByRepo`/`routeQueuedGroup` re-admit an unpaused Queued member directly from `board.Items`
  on the very next poll, entirely independent of the `HoldingStage` dispatch exclusion (that exclusion
  only blocks per-item stage dispatch, not the batch handler's own board read). Rerouting the whole
  paused column to Validate would instead force every member through a redundant re-run for a cause
  (persistent infra failure — billing, a broken base branch, all required checks erroring) no code stage
  can fix, and would make each member re-earn its way back into Queued via a fresh stage completion
  instead of simply resuming the batch it had already validated into.
- **`ejectMember`'s cap-reached pause** (`MaxMergeTrainEjections`) — exempted, out of scope per this
  issue's own Scope section. It governs the counter-driven bisection/conflict ejection ladder, where
  `stayInQueue=true` for every cause it serves — the member was always meant to stay in Queued for a
  future differently-composed train regardless of pause state, so rerouting it would change that
  design's own semantics, which this issue does not touch.
- **`checkAutoMergeConvergence`'s `fabrik:paused` sites** (`engine/merge_gate.go`) — exempted, confirmed
  structurally unreachable while an item's board Status is Queued. Its only call site,
  `handleAutoMergeConvergence`, runs from the Phase 1 catch-up loop, which unconditionally excludes
  `HoldingStage` items before it is ever reached. An ADR-058-enqueued item that stays at
  `Status == "Queued"` is drained by `settleClosedItemsToDone` once closed (regardless of column), or
  simply sits, unpaused, in Queued until GitHub's native merge queue merges and closes it.

## Consequences

**Positive:**
- A member paused for its own standalone validation failure is now reachable: a human can act on the
  pause via the normal pipeline, from a column `itemMayNeedWork`/`processItem` actually admit.
- The recovery instruction is now literally true — applying `fabrik:revalidate` genuinely re-triggers
  Validate, unlike the previous "remove `fabrik:paused`" instruction, which silently no-op'd.
- Reuses #1208's reroute primitive and ordering exactly, rather than inventing parallel machinery — the
  only new code is the `projectID` parameter, the reroute call, and the corrected message.
- The poison-well guard (`groupQueuedByRepo`'s `fabrik:paused` exclusion) is no longer load-bearing for
  this cause: once the member leaves Queued, it is excluded from every future batch snapshot by Status
  alone, whether or not the pause is later cleared incorrectly (re-admission requires completing Validate
  again).

**Negative / Trade-offs:**
- Unlike #1208's cause, this fix does **not** make the rerouted member self-heal "for free" — see
  Decision point 2. The pause remains a genuine human gate; there is no bound analogous to
  `MaxReviewCycles` for this specific failure mode, because nothing auto-retries it. This is a deliberate,
  documented choice, not an oversight: a future contributor extending this pattern to a third
  pause-in-holding-column cause should not assume `MaxReviewCycles`-style composition transfers by
  default — it depends on the cause having an external, persistent re-detection signal, which this one
  does not.
- `pauseMergeTrainMember` remains a bespoke, non-`itemstate.EnginePaused`-aware pause helper, unlike every
  other `pauseFor*` site in the codebase. This ADR does not close that gap; `fabrik:revalidate`'s reset
  already covers what a migration to `pauseIssue`/`EnginePaused` would have bought for this specific
  cause.
- Misclassification risk (a genuinely batch-poisoning interaction wrongly classified as standalone) is
  unchanged by this fix — the arity guard's classification logic itself is out of scope. Rerouting only
  changes *where* a misclassified member is paused, not whether misclassification happens.

## References

- ADR-1208: Queued Review-Finding Ejection (the reroute primitive and reroute-before-side-effects
  ordering this issue reuses verbatim; the "for free" composition property this issue's cause explicitly
  does not share)
- ADR-1420: Merge-Train Ejection Diagnostics (`diag` threading through `ejectRedSingleton`, unchanged by
  this issue)
- ADR-059: Internal Merge Train (`ejectRedSingleton`'s #1440 origin; the runaway guard exempted by the R5
  audit)
- `docs/state-machine.md` §"Merge-Train Red-Batch Bisection" (as-built spec, updated by this issue) and
  its "Pause-in-Holding-Column Audit (#1545)" subsection (the full R5 audit table)
- Issue #1545, Issue #1440, Issue #1208
