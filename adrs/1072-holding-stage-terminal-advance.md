# ADR 1072: Holding Stages Are Reachable Only Via Dedicated Code, Not Positional Advance

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1072 — Merge-train (Queued holding stage) items never advance to Done after their PRs merge — 17 closed items stranded, reproduces on v0.0.75

## Context

`stages.NextStage` (`stages/stages.go`) is a purely positional walk: given the current stage name, it
returns the next non-`Unmanaged` entry in `e.cfg.Stages`. It had no awareness of `HoldingStage` at all.
With the shipped pipeline shape (`Validate → Queued → Done`, `Queued` marked `holding_stage: true`),
every *generic* terminal-advance call site that resolves `NextStage("Validate")` — `runValidatePRTerminalAdvance`
(ADR-057, cruise/human-merge/any gate-label convergence) and `advanceConvergedPRToDone` (ADR-058, non-train
yolo convergence) — landed the item on `Queued`, regardless of `merge_train` on/off and regardless of how
the PR actually merged. Neither function has (or should have, per ADR-057's own single-owner design) a
yolo/cruise/merge_train gate; both fire unconditionally on any merged/terminal Validate-stage PR.

Only one call site is *supposed* to land an item in `Queued`: `attemptMergeOnValidate` → `advanceToQueued`
(`engine/stages.go`), gated on `yoloActive && !cruise && merge_train: on`. That path never calls
`NextStage` — it enqueues directly. Every other path reaching `Queued` did so as an unintended side
effect of `NextStage`'s positional blindness.

Once an item landed in `Queued` this way, its PR was *already merged*. With `merge_train: on`, the item
was technically drainable — `handleMergeTrainBatch` scans by column membership, not by enqueue
provenance, so it would eventually pick the item up and attempt to trial-merge its already-landed head
SHA into a fresh trial branch, an interaction the merge train was never designed for or tested against.
With `merge_train: off` (or unset), there is no drain mechanism at all: `handleMergeTrainBatch` only runs
`if e.cfg.MergeTrain == "on"` (`engine/poll.go`), and the existing closed-item backstop,
`settleClosedItemsToDone` (ADR-064), explicitly excluded `HoldingStage` from the columns it would rescue
a closed item from. The item was permanently stranded — visible in production as 17 closed, fully
stage-complete items sitting in `Queued` on one board, and independently reproduced on a second board
with `merge_train` unset and per-issue `fabrik:cruise` (no train involvement at all).

ADR-057 (2026-06-18) predates ADR-059 (2026-06-23, which introduced the `Queued` holding stage) by five
days; its unconditional `advanceToNextStage` call was never revisited when a holding stage was later
inserted into the pipeline. ADR-064 (2026-07-19) repeated the same class of oversight in the opposite
direction: it built a closed-item backstop but excluded holding stages without documenting why, closing
off the one mechanism that could have caught these items after the fact.

## Decision

Two complementary fixes, addressing the entry-point leak and the already-stranded items respectively.

### Fix 1 — `NextStage` skips `HoldingStage`, mirroring its existing `Unmanaged` skip

`NextStage` (`stages/stages.go`) now walks past any stage with `HoldingStage: true` in addition to
`Unmanaged: true`, using the identical skip-loop shape already established (and tested) for `Unmanaged`
by the #973 fix. After this change, `NextStage("Validate")` resolves directly to `Done` whenever `Queued`
sits positionally in between — the dedicated `advanceToQueued` enqueue remains the *only* way an item
reaches `Queued`, since it bypasses `NextStage` entirely.

This is deliberately **unconditional**, not gated on whether the holding stage's drain mechanism (the
merge train) is currently enabled. `NextStage` is a package-level pure function in the `stages` package
with no `Engine` receiver and no access to `e.cfg.MergeTrain` — threading engine configuration into it
would be a materially larger, more invasive change for no behavioral benefit, since the one caller that
*wants* to land in `Queued` never calls `NextStage` in the first place. Treating "is this holding stage
reachable" as entirely the caller's problem — the same posture already taken for `Unmanaged` — keeps the
fix minimal and consistent with existing precedent.

### Fix 2 — `settleClosedItemsToDone` rescues closed items at a Holding stage, guarded by a live-worker check

`settleClosedItemsToDone` (ADR-064) no longer blanket-skips `HoldingStage` columns. A closed item resolved
to a `HoldingStage` is now skipped **only** while `Engine.mergeTrainWorkerActive(repoKey)` — a new helper
reading the existing `Engine.mergeTrainInFlight` per-repo `sync.Map` (ADR-067) — reports a merge-train
worker currently in flight for that item's `owner/repo`. When no worker is in flight (`merge_train: off`,
the repo's train is idle, or the item belongs to a different repo than any active train), the item is
rescued exactly like any other stranded closed item: `item.IsClosed` alone is the trigger, with no
PR-merge re-confirmation, matching the predicate this scan already uses for every other stage.

This closes the immediate 17-item stranding and any future item that reaches `Queued` some other way (a
human `gh pr merge`, an engine restart racing an in-flight assembly, or any as-yet-undiscovered path) —
Fix 1 prevents new recurrences of the *entry-point* leak, but does nothing for items already sitting in
`Queued` today.

## Rationale

### Why guard on "is a train worker in flight," not "is the linked PR confirmed merged"?

The one real race a Holding-stage item poses that no other stage does: it can be closed *without*
merging while still a live batch member mid-assembly or mid-bisection (a train worker may close an issue
as part of landing before the PR itself is confirmed merged in the item's cached state, or an operator
may close it manually mid-batch). Re-confirming PR-merged state via a live API call, mirroring
`runValidatePRTerminalAdvance`'s own `FetchPRMerged` check, would require an extra network call per
closed Holding-stage item per poll and would diverge from every other stage this same scan already
handles — none of which re-check merge state; `item.IsClosed` alone is the accepted, ADR-064-documented
predicate. The actual hazard is fully covered by checking `mergeTrainInFlight`: a batch member can only
be closed-without-merge while the train that owns it is actively running, and once that goroutine exits,
`finishTrain` clears the marker (the single, centralized clearing point per ADR-067) — at that point any
leftover closed item is safe to sweep unconditionally, exactly like any other stage.

### Why is the guard deliberately in-memory rather than a durable label?

`mergeTrainInFlight` reflects only "is a goroutine for this repo running right now" — by construction it
cannot, and does not need to, survive a restart. A freshly restarted engine has no in-flight worker for
any repo, so every closed Holding-stage item is immediately eligible for rescue on the very next poll,
which is exactly the correct behavior (any batch a prior process was assembling is gone with the
process). Adding a durable marker here would solve a problem that does not exist — there is no multi-step
sequence whose loss on restart needs protecting, the same reasoning ADR-064 itself already applied to
this whole settle scan.

### Why not make `NextStage` conditional on whether the holding stage's drain is active, as the issue's "complementary fix" wording suggested?

The issue text offered this as one option ("make holding-stage traversal aware of whether its drain
mechanism ... is actually enabled"). Rejected in favor of the unconditional skip because `NextStage` has
no access to that state without a larger refactor (see Fix 1 above), and because the unconditional skip
is strictly simpler while producing identical observable behavior for every case that matters: the only
caller that ever wants to land in `Queued` already bypasses `NextStage`, so "is the drain active" is
moot from `NextStage`'s point of view — it is never asked to decide that question.

### Why doesn't this ADR touch `cmd/init.go`'s unconditional `queued.yaml` shipping?

Fix 1 makes the concern (a `merge_train: off` install receiving an unreachable-once-entered `Queued`
column) moot: after this change, nothing routes an item into `Queued` unless the dedicated enqueue path
is active, so the column simply sits empty and harmless on such an install rather than acting as a trap.
Addressing *whether* the file should ship by default is a separate, smaller UX concern, deferred as a
follow-up rather than folded into this bug fix.

## Consequences

**Positive:**
- Every generic terminal-advance path (cruise, human-merge, non-train-yolo convergence) now reaches Done
  directly, never `Queued`, closing the entry-point leak regardless of `merge_train` state.
- The 17 already-stranded items (and any structurally similar future item) are rescued by the extended
  backstop without operator intervention.
- Both fixes mirror existing, already-tested precedents (`Unmanaged`-skip in `NextStage`;
  `item.IsClosed`-as-sufficient-trigger in `settleClosedItemsToDone`) rather than introducing new design
  vocabulary.
- The misleading `"...advancing to Done"` log line in `runValidatePRTerminalAdvance` becomes accurate as
  a side effect of Fix 1 — no separate fix needed.

**Negative / Trade-offs:**
- `settleClosedItemsToDone`'s trigger is no longer a pure function of durable board state alone for the
  Holding-stage case — it now also depends on in-memory `mergeTrainInFlight` state. This is a narrower
  exception than it may first appear (see Rationale above: the in-memory-only nature is precisely what
  makes it safe), but it is a genuine, documented deviation from ADR-064's "pure function of durable
  state" framing for every other stage this scan handles.
- A config with a `HoldingStage` positioned somewhere *other* than immediately before the cleanup stage
  is untested (no such configuration exists today), though the `NextStage` fix generalizes correctly to
  it by construction — a holding stage is still never returned as "next" regardless of where it sits.

## Sibling Audit

Both `landSingleton` and `landMergeTrainBatch` (`engine/merge_train.go`) call `advanceToNextStage` *from*
`Queued` itself as their own intentional Queued→Done exit — unaffected by Fix 1, since `Done` is never
itself a `HoldingStage` and `NextStage("Queued")` continues to resolve to `Done` exactly as before. No
other call site invokes `stages.NextStage`.

## Related Work

- [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md) — the
  unconditional, gate-agnostic design this ADR's Fix 1 makes holding-stage-safe without modifying.
- [ADR-059: Internal Merge Train](059-internal-merge-train.md) — defines the `Queued` holding stage and
  the "one column, two landing engines" model this ADR does not change.
- [ADR-064: Closed-Item-At-Any-Stage Advance To Done](064-closed-item-any-stage-advance-to-done.md) —
  this ADR supersedes its blanket `HoldingStage` exclusion with the conditional, live-worker-guarded
  rescue described in Fix 2 above.
- [ADR-067: Centralized Merge-Train In-Flight Marker Cleanup](067-merge-train-centralized-inflight-cleanup.md) —
  the `mergeTrainInFlight`/`finishTrain` machinery Fix 2's guard reads from.

**References:** [docs/state-machine.md §"Queued (Holding Stage)"](../docs/state-machine.md), [docs/state-machine.md §6.11](../docs/state-machine.md)
