# ADR 1387: Closed Items Are Never Dispatched — Board-Source the Validate Settle-Owner

**Date**: 2026-08-03
**Status**: Accepted
**Issue**: #1387 — a closed issue parked at Validate was re-dispatched as a full Claude stage invocation on every poll cycle, indefinitely

## Context

A closed issue parked at a gate-checked stage (Validate) was re-dispatched as a real Claude stage
invocation **every poll cycle, indefinitely**. Field evidence (`handarbeit/fabrik-test-alpha#4246`)
showed 87 Validate invocations over ~14 hours, every single one after the issue was closed — each a
real invocation (one sampled run consumed 12 turns, 127k input tokens, 307k cache-read tokens), each
posting an engine-authored comment to the closed issue. The loop terminated only by chance: 85 of 87
runs emitted `FABRIK_STAGE_COMPLETE` (inert here), 2 emitted `FABRIK_NO_WORK_NEEDED`, which happens to
route through `handleNoWorkNeeded` and force an advance to Done, bypassing the CI gate entirely.
Termination depended on the model happening to choose the second marker.

### Why it looped

Four individually-reasonable mechanisms composed into a cycle:

1. Validate is `wait_for_ci: true`, so the conjunctive gate (`handleStageComplete`) withholds
   `stage:Validate:complete` on `FABRIK_STAGE_COMPLETE` and applies `fabrik:awaiting-ci` instead —
   the only thing suppressing re-dispatch during the CI wait.
2. `cleanupClosedIssueTransientLabels` (#617) strips `fabrik:awaiting-ci` from closed issues every
   poll, treating it as stale operational residue rather than load-bearing gate state.
3. The item is left with **neither** the completion label (deferred) **nor** the suppressing label
   (swept) — still sitting at Validate.
4. `itemMayNeedWork`/`itemNeedsWork` admitted it anyway, via a `!stageIsGateChecked(stage)` disjunct
   added in commit `7311a14e` — returning to step 1.

Steps 1 and 2 are each correct in isolation. The sweep removed the suppressor without ever supplying
the completion it was deferring.

### The regression point, and why its intent was correct

Commit `7311a14e` ("fix(convergence): admit closed gate-checked items so settle-owner heals paused
merges") added `!stageIsGateChecked(stage)` to the closed-issue admission guard specifically so
`runValidatePRTerminalAdvance` (ADR-056 D2) — the settle-owner responsible for healing a closed item
at Validate carrying `fabrik:paused`/`fabrik:awaiting-review`/no label at all (the #874 class) — could
observe and advance it. That goal was, and remains, correct: a merge that closes an issue while it
sits at Validate under any gate state must still be healed to Done.

**The mechanism was wrong.** `runValidatePRTerminalAdvance` was fed exclusively from
`deepFetchCandidates`, itself filtered by `itemMayNeedWork`. The only way to make a closed item
observable to the settle-owner was therefore to admit it to dispatch — conflating "let the
settle-owner see this item" with "let this item receive a real Claude invocation." The same `true`
that let the settle-owner reach the item also let ordinary stage dispatch reach it. The item was
healed *and* re-invoked, every poll, for as long as the deferred-completion/swept-label state
persisted.

### Why this had not been seen in production

The trigger requires an issue to be closed out-of-band while parked at a gate-checked stage with its
completion deferred. In normal operation, the PR merge that closes an issue also means CI passed, so
the gate clears, `stage:Validate:complete` lands, and the item advances to Done in the ordinary flow
— the window barely exists. The e2e harness (`fabrik-test-alpha#4246`) created the state
deliberately by closing the issue mid-Validate during teardown. Production issues close *through* the
gate; this scenario closes *around* it.

### The invariant the codebase already stated, for every other column

`settleClosedItemsToDone` (ADR-064) already implements the correct rule for every non-gate-checked
column: a closed issue at a non-terminal stage is itself the complete, sufficient, durable trigger —
sourced directly from `board.Items`, never conditioned on any label, never gated by dispatch
admission. It already excludes gate-checked stages specifically to defer to
`runValidatePRTerminalAdvance` — the exclusion was already correct; it just deferred to an owner that
was, at the time, unreachable except through dispatch.

## Decision

**A closed item is never dispatched to a Claude stage invocation.** The sole exception is a stage
with `cleanup_worktree: true`, where dispatch legitimately performs worktree reaping — a local,
non-computational operation.

Two changes make this hold, plus a third that closes the interaction that made the loop
self-sustaining:

### 1. Simplify the closed-issue admission guard (R1, R3)

`itemMayNeedWork`/`itemNeedsWork`'s closed-issue block (`engine/item.go`) is reduced from a five-way
admission (`stage:complete` OR cleanup-stage OR `fabrik:awaiting-ci` OR `fabrik:auto-merge-enabled` OR
`!stageIsGateChecked(stage)`) to exactly two conditions:

```go
if !stage.CleanupWorktree && !hasLabel(item.Labels, fmt.Sprintf("stage:%s:complete", stage.Name)) {
    return false
}
```

Dispatch admission is no longer the settle-owner's delivery mechanism, so the widenings that existed
solely to reach it are removed. This block is entirely inside `if item.IsClosed { ... }` in both
functions — the change is structurally inert for open items (R7).

### 2. Give the settle-owner a board-sourced feed, independent of admission (R2, R4)

`runValidatePRTerminalAdvance`'s existing per-item logic is extracted, unchanged, into a shared
helper, `advanceValidateTerminalItem(board, item, advancedItems)`. Two thin callers wrap it,
partitioned on `item.IsClosed`:

- `runValidatePRTerminalAdvance` — the **open-item** owner, unchanged sourcing
  (`deepFetchCandidates`), with a `continue` added for any `item.IsClosed` item as an explicit,
  unit-testable ownership boundary (redundant for correctness given `advancedItems` dedup and
  `poll()`'s single-threaded sequencing, but not for clarity).
- `settleClosedValidateAdvance` (`engine/pr_terminal_advance.go`) — the **closed-item** owner, a new
  function sourced directly from `board.Items`, mirroring the `settleAwaitingCIScan`/
  `settleClosedItemsToDone` pattern (ADR-1270, ADR-064): iterate the raw board snapshot, filter to
  `item.IsClosed`, delegate to the shared helper.

Neither function's admission (whether a closed item reaches it) depends any longer on dispatch
admission. `settleClosedItemsToDone`'s existing exclusion of `stageIsGateChecked` stages needed **no
code change** — it was already correctly deferring to whichever function owns closed Validate items;
it only needed that function to be reachable independent of `itemMayNeedWork`.

### 3. Stop the transient-label sweep from racing the settle-owner (R6)

`cleanupClosedIssueTransientLabels` (#617) previously stripped `fabrik:awaiting-ci`,
`fabrik:awaiting-review`, and `fabrik:rebase-needed` off every closed issue unconditionally — the
mechanism behind step 2 of the loop. It now resolves each closed item's stage and, when the item
resolves **specifically to `stage.Name == "Validate"`**, leaves those three labels alone — they are
cleared atomically by the settle-owner pair as part of a real merge/pause transition, not
independently by the generic sweep. Every other transient label, including
`fabrik:auto-merge-enabled` (which per `attemptMergeOnValidate`'s design always coexists with
`stage:Validate:complete` and so was never part of the stranding mechanism), continues to sweep
unconditionally. Closed items at any other column — gate-checked or not — are entirely unaffected.

The "cleared atomically by the settle-owner pair as part of a real merge/pause transition" claim
requires the pause branch to clear all three gate labels, not just the one (`fabrik:awaiting-ci`) it
originally cleared — otherwise an item carrying `fabrik:awaiting-review` or `fabrik:rebase-needed`
that hits the closed-and-not-merged path is paused with that label intact, and every subsequent poll
short-circuits on `fabrik:paused` before ever reaching the clearing code again, stranding it
permanently now that the sweep no longer catches it either. Caught by Pruefer review on PR #1388;
`pauseForPRClosedNotMerged` (`engine/ci.go`, shared by this settle-owner and the open-item `checkCIGate`
catch-up path) now clears `fabrik:awaiting-ci`, `fabrik:awaiting-review`, and `fabrik:rebase-needed`
unconditionally (each removal is itself a no-op when the label is absent).

**Why `stage.Name == "Validate"`, not `stageIsGateChecked(stage)`, for this exclusion.** A first
implementation used `stageIsGateChecked(stage)` here, matching the general convention used elsewhere
in this codebase (`itemMayNeedWork`'s pre-fix guard, `settleClosedItemsToDone`'s exclusion). It was
caught and corrected during implementation: `advanceValidateTerminalItem` — and therefore both
settle-owners — only ever processes `stage.Name == "Validate"` (see "R1 Follow-up: Closed Item Stranded at a Non-Validate Gate-Checked Stage" below). The
shipped default `Review` stage (`stages/examples/review.yaml`) is independently configured with
`wait_for_reviews: true`, making it gate-checked too. Had the exclusion used `stageIsGateChecked`
generally, a closed item stranded at Review with no `stage:Review:complete` would have had
`fabrik:awaiting-review` excluded from the sweep with no settle-owner left to ever clear it —
trading the loop this issue fixes for a silent, permanent strand at a different stage. The exclusion
is scoped to match exactly what the settle-owner pair actually processes.

## Rationale

### Why two owners, not one consolidated function?

`settleClosedItemsToDone` is deliberately a pure board/label-move function with no live PR fetch —
that is the entirety of ADR-064's contract ("closed items need reconciliation, not computation").
Folding Validate's PR-fetch-and-decide logic into it would blur that contract and roughly double its
complexity for no benefit: the two domains are already disjoint by construction
(`stage.Name == "Validate"` partitions them in `settleClosedItemsToDone` — see the "R1 Follow-up: Closed Item Stranded at a Non-Validate Gate-Checked Stage" section below
for why this is `stage.Name`, not `stageIsGateChecked`), so consolidation would not improve
race-safety, only increase blast radius. Keeping `settleClosedValidateAdvance` as a small,
symmetric sibling to the existing open-item owner — sharing one extracted helper, not duplicating
logic — is the smaller change and satisfies "single authoritative owner" (ADR-056 D2, ADR-057) in
spirit: one union of healing logic, split only by feed and cost, never two independently-reasoning
implementations that could drift apart.

### Why is dispatch admission the wrong seam for settle-owner reachability?

Dispatch admission answers one question: "is this item eligible for a real Claude invocation right
now?" A settle-owner answers a structurally different question: "does this item's board/label state
need reconciling?" Before this fix, both questions were answered by the same boolean — conflating
"observable by the settle-owner" with "eligible for computation." Every other settle scan in this
codebase (`settleClosedItemsToDone`, `settleAwaitingCIScan`, `settleChildPlacements`,
`settleMergeTrainMemberCloses`, `settleNonDefaultBaseCloses`) already answers its own question from
`board.Items` directly, independent of dispatch admission. `runValidatePRTerminalAdvance`'s
`deepFetchCandidates` sourcing was the one holdout — not because it needed dispatch's filtering, but
because no board-sourced closed-item sibling existed yet for it to defer to. This issue removes the
last consumer that needed the admission gate widened for a non-dispatch reason.

### No double-advance, by construction (R5)

`advancedItems[iKey]` is checked before the live `FetchLinkedPR` call inside the shared per-item
helper, and `poll()` is fully single-threaded/sequential — so even without the explicit `IsClosed`
partition, whichever of the two callers ran second for the same item would find it already marked and
no-op. The partition is added anyway as a unit-testable ownership boundary, not because correctness
depends on it.

### Why not generalize `advanceValidateTerminalItem` to `stageIsGateChecked` while touching this code?

Doing so would let the settle-owner pair heal a closed item at *any* gate-checked stage, not just
Validate — one way to close the gap the "R1 Follow-up: Closed Item Stranded at a Non-Validate Gate-Checked Stage" section below describes. It is deliberately not taken:
Validate is the only gate-checked stage today with real PR-merge/close authority (the standard
pipeline shape merges PRs at or after Validate, never at Review), so the PR-fetch-and-decide logic
`advanceValidateTerminalItem` implements (merged → fill labels/advance; closed-unmerged → pause) has
nothing to check for a closed item at any other stage — that logic is Validate-specific, not a
generic "any gate-checked stage" concern. The gap this would have closed is closed a different, smaller
way instead: `settleClosedItemsToDone`'s exclusion is scoped to `stage.Name == "Validate"` rather than
`stageIsGateChecked`, so a closed item at any other gate-checked stage (e.g. Review) gets the same
plain "move to Done" treatment already correct for Specify/Plan/Implement — see the "R1 Follow-up:
Closed Item Stranded at a Non-Validate Gate-Checked Stage" section below. Generalizing `advanceValidateTerminalItem` itself remains unnecessary: there is no PR-state
nuance at Review for it to add.

## Consequences

**Positive:**
- A closed item is never dispatched to a Claude stage invocation (R1) — the invariant now holds
  unconditionally, not merely "unless the settle-owner happens to observe it first in the same poll."
- `runValidatePRTerminalAdvance`'s healing of the #874 class (paused / awaiting-review / no-label
  merges at Validate) is *more* reliable than before, since it no longer depends on the item having
  survived dispatch admission (R4).
- The regression this fix targets — an admitted-and-dispatched closed item at Validate — is directly
  reproduced and asserted against, with zero `Invoke` calls across two poll cycles, verified to fail
  against the pre-fix engine before this fix and pass after.
- No change to conjunctive-gate semantics, marker semantics, or `settleClosedItemsToDone`'s behavior
  for non-gate-checked columns — all explicitly out of scope and confirmed untouched.

**Negative / Trade-offs:**
- `advanceValidateTerminalItem`'s pre-existing `stage.Name == "Validate"` hardcoding (rather than
  `stageIsGateChecked`) is now load-bearing for three call sites instead of one — `settleClosedItemsToDone`'s
  exclusion (see the "R1 Follow-up: Closed Item Stranded at a Non-Validate Gate-Checked Stage" section
  below) independently keys on the identical string comparison, for a
  related but distinct reason. Any future generalization work (e.g. supporting a second real
  gate-checked, PR-merging stage) touches all three.
- **Unresolved-PR polling cost, pre-existing and unchanged by this fix:** for a closed item at
  Validate whose linked PR is neither merged nor closed (a human closed the issue without touching the
  PR — an unusual, out-of-band action, same trigger class as the loop this issue fixes),
  `advanceValidateTerminalItem` returns immediately with no state change. `settleClosedValidateAdvance`
  re-evaluates that item every poll — one `FetchLinkedPR` call, indefinitely, until the PR resolves —
  with no timeout/escalation comparable to `settleAwaitingCIScan`'s `CIWaitTimeout` backstop (ADR-1270).
  This exact no-escalation behavior already existed in `runValidatePRTerminalAdvance` before this issue
  (verified against the pre-fix code); this fix does not introduce it, and only lowers its cost — the
  same item pre-fix was *also* being fully re-dispatched to Claude every poll (the R1 bug), so the
  post-fix steady-state cost (one lightweight, read-only API call per poll) is strictly cheaper, not a
  new failure mode. Adding a stuck-PR escalation path is unrelated to R1–R7 and left out of scope.

## Sibling Audit

This is the sixth instance of the "dedicated `board.Items`-sourced settle scan" pattern in this
codebase (`fabrik:awaiting-done`/ADR-060, spawned-child placement/ADR-062, merge-train member-close
retry/ADR-061, non-default-base close retry/ADR-1097, `fabrik:awaiting-ci`/ADR-1270, now closed items
at Validate). Unlike four of those five, this is not a durable-marker-write-on-failure mechanism —
closer in shape to ADR-1270's `settleAwaitingCIScan`, whose marker (`fabrik:awaiting-ci`) is a normal,
expected steady-state condition the scan evaluates continuously, not a rare failure branch. The
distinguishing feature here is the *trigger* is not a label at all but `item.IsClosed` itself,
matching `settleClosedItemsToDone`'s own "deliberately not conditioned on any label" design (ADR-064)
— a closed issue at a non-terminal, non-cleanup, gate-checked column is itself the complete,
sufficient, durable signal.

## R1 Follow-up: Comment-Driven Dispatch on a Closed, Stage-Complete Item

Caught by Pruefer review on PR #1388, after the fixes above landed. R1's admission-guard simplification
retains one exception beyond cleanup stages: a closed item carrying `stage:<X>:complete` is still
admitted by `itemMayNeedWork`/`itemNeedsWork` (see
`TestItemMayNeedWork_ClosedAtValidate_StageComplete_StillAdmitted`), so the catch-up loop can still act
on it. But `itemNeedsWork`'s "new comments are always worth processing (even on completed stages)"
fast path, and its mirror in `processItem`, had no `item.IsClosed` check — so a closed, stage-complete
item that received a fresh comment was still routed to `processComments`, a real Claude invocation.
This is a narrower, **pre-existing** instance of the same class of bug this ADR fixes (a closed item
reaching real dispatch), reachable independently of the CI-gate/`fabrik:awaiting-ci` mechanism that
was this issue's original trigger — it predates commit `7311a14e` and is not a regression this PR
introduced.

R1 as stated is unconditional ("a closed item is never dispatched … the sole exception is a
`cleanup_worktree` stage"), so this is closed on the same terms as the rest of the invariant: every
comment-triggered path out of `itemNeedsWork` now skips when `item.IsClosed`.

**A first pass guarded only the plain new-comment fast path, which was not sufficient** — caught by a
second Pruefer review on PR #1388. That pass rested on the reasoning that the "already completed this
stage" check just below rejects a closed+`stage:complete` item once the fast path stops pre-empting
it. That holds for the fast path itself, but not for the two *comment-triggered resume* branches
above it: `awaitingInput` and `isPaused` each return `true` on a new human comment and never reach
that check. Their `processItem` counterparts call `processComments` directly. So a closed item
carrying `stage:<X>:complete` **together with** `fabrik:paused` / `fabrik:awaiting-input` was still
dispatched on a human comment. That combination is reachable, not hypothetical: the pause paths
(`comment_breaker.go`, `reviews.go`) apply the pause labels without touching the completion label,
and `settleClosedItemsToDone` deliberately treats closed+paused as a normal state
(`TestSettleClosedItemsToDone_IgnoresLabelState`).

The guard is therefore placed **once, ahead of all three branches** — `if item.IsClosed &&
!stage.CleanupWorktree { return false }` — rather than repeated per branch, so a future branch added
above the fast path cannot reintroduce the same gap. The cleanup-stage exclusion is R1's stated
exception: that dispatch exception exists for worktree reaping, which the fall-through below reaches,
and is not a licence to route a closed item into comment processing — hence the `!item.IsClosed`
condition on the fast path is retained as well, covering the one closed case the guard lets past.

In `processItem`, the mirrored guard is redundant-but-explicit (itemNeedsWork's gate already prevents
`processItem` from being invoked at all for a closed item outside a cleanup stage) — the same
ownership-boundary idiom used for `runValidatePRTerminalAdvance`'s own `IsClosed` skip earlier in this
ADR. A human comment on a closed, completed issue is no longer actionable by Fabrik at all — consistent
with "a closed issue has no computable work left" (this ADR's own framing) applying to comment
processing exactly as it applies to stage re-invocation.

**References:** [ADR-056: Consolidate Convergence Gate Recovery](056-consolidate-convergence-gate-recovery.md) (D2), [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md), [ADR-064: Closed-Item-At-Any-Stage Advance To Done](064-closed-item-any-stage-advance-to-done.md), [ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md), commit `7311a14e` (the regression point whose intent this ADR preserves, only changing its mechanism), issue #617 (the transient-label sweep whose interaction with the conjunctive gate is closed by this issue's R6)

## R1 Follow-up: Closed Item Stranded at a Non-Validate Gate-Checked Stage

Caught by human review (issue comment, PR #1388), after the fixes above landed and were themselves
documented as a "known limitation." `settleClosedItemsToDone` (ADR-064) originally excluded any
`stageIsGateChecked` stage — a broader category than "Validate" — on the theory that gate-checked
stages were categorically deferred to the Validate-specific settle-owner pair. But
`runValidatePRTerminalAdvance`/`settleClosedValidateAdvance` only ever process `stage.Name ==
"Validate"`. Both shipped default stage templates, `stages/examples/review.yaml` and
`stages/examples/validate.yaml`, set `wait_for_reviews: true` — so a deployment using the defaults has
a second gate-checked stage (Review) that is neither Validate nor a plain non-gate-checked stage. For a
closed item stranded there lacking `stage:Review:complete`: dispatch admission correctly refuses it
(R1), no settle-owner processes anything but Validate, and `settleClosedItemsToDone`'s `stageIsGateChecked`
exclusion skipped it too. **Zero remaining owners** — the item was never advanced, its worktree never
reaped, never archived. The only signal was a per-poll warning log, which is not an owner.

This was initially accepted as a documented, believed-rare trade-off. On review it was correctly
rejected: closing a superseded, duplicate, or abandoned issue mid-pipeline is ordinary operator
behavior, not a rare edge case, and it is exactly what produced the field evidence this whole ADR is
built on (`fabrik-test-alpha#4246`, closed mid-Validate by `"Closing: e2e test teardown"`). Before this
issue's fixes, such an item was at least admitted to dispatch and could still reach `stage:complete` and
advance — a loud, wasteful, but *moving* bug. A silent permanent strand is worse: harder to notice,
with no path to resolution short of a human manually fixing board state.

**Fix:** narrow `settleClosedItemsToDone`'s exclusion from `stageIsGateChecked(stage)` to `stage.Name ==
"Validate"` — matching the settle-owner pair's actual scope by name rather than by the broader category
that happens to also catch Review. This is safe because the two functions do fundamentally different
things: `advanceValidateTerminalItem` (the Validate-specific settle-owner logic) fetches the linked PR
and decides merge-vs-pause because Fabrik's own PR merge action (`attemptMergeOnValidate`) only ever
runs as part of Validate completing — no other stage has that nuance to get wrong. `advanceClosedItemToDone`
(what `settleClosedItemsToDone` calls) never inspects the PR at all — it only moves the board column,
exactly as it already does, unconditionally, for a closed item at Specify/Plan/Implement. Extending its
reach to Review (or Backlog, or any future non-Validate stage) is not a new capability requiring new
reasoning; it is the existing, already-correct "closed = move to Done" behavior no longer being
incorrectly withheld from one particular stage.

With the fix, the per-poll "no settle-owner" warning previously added to `cleanupClosedIssueTransientLabels`
was removed along with the condition that produced it — the label sweep and the board-move now happen
independently and safely regardless of ordering, since `advanceClosedItemToDone` doesn't care about
label state (ADR-064's "deliberately not conditioned on any label" property, confirmed still holding here).

**References:** [ADR-064: Closed-Item-At-Any-Stage Advance To Done](064-closed-item-any-stage-advance-to-done.md) (the scan whose exclusion this narrows), [ADR-056: Consolidate Convergence Gate Recovery](056-consolidate-convergence-gate-recovery.md) (D2, why Validate's PR-merge nuance doesn't generalize), `handarbeit/fabrik-test-alpha#4246` (the field evidence showing this is ordinary operator behavior, not a rare edge case)

## R1 Follow-up: `fabrik:auto-merge-enabled` Without `stage:Validate:complete` Delayed Healing by a Poll Cycle

Caught by Pruefer review on PR #1388, after the fixes above landed. `advanceValidateTerminalItem`
defers entirely to `checkAutoMergeConvergence` (Phase 1) whenever `fabrik:auto-merge-enabled` is
present, on the assumption that `attemptMergeOnValidate`'s auto-merge/enqueue/direct-merge branches
never apply the label before `stage:Validate:complete` already exists — true for both callers today.
An initial implementation deferred unconditionally on the label alone and, when that assumption was
observed violated, logged a warning and returned. The warning's own wording claimed this "may
permanently strand the item" — the same "zero remaining owners" framing correctly used for the
Review-stage strand above. On closer review that framing was itself wrong here (a second Pruefer
finding, on the first fix): `fabrik:auto-merge-enabled` is not among the labels
`cleanupClosedIssueTransientLabels` withholds from its closed-item sweep
(`gateSettleOwnedTransientLabels`, R6 above) — so for a *closed* item, the very same poll's later sweep
step removes the label unconditionally, and the next poll's call into this function then falls through
normally and heals it. The actual prior cost was a one-poll-cycle delay for a closed item, not a
permanent strand. (An *open* item in this state has no such fallback — the sweep only ever runs on
closed issues — but this state is believed genuinely rare for either: a labeling race, or a future
caller of the label, not ordinary operator behavior like the Review-stage case above.)

**Fix (unchanged by the corrected rationale):** narrow the defer-to-Phase-1 skip to the
invariant-respecting case only — `fabrik:auto-merge-enabled` **and** `stage:Validate:complete` both
present. When the label is present without the completion label, log the same warning but fall through
into the ordinary merge-vs-pause flow immediately below instead of returning, healing the item via the
identical logic already used for every other Validate terminal advance in this function. This remains
worth doing even though the closed-item case would have self-healed on its own: it heals inline instead
of depending on sweep ordering and a wasted poll cycle, and it is the only fix available for the
open-item case, which has no sweep to fall back on at all. It does not risk racing
`checkAutoMergeConvergence`: that handler cannot reach the item in this state either (same `hasComplete`
gate), so the two code paths remain mutually exclusive by construction, exactly as they are in the
invariant-respecting case.

**References:** [ADR-056: Consolidate Convergence Gate Recovery](056-consolidate-convergence-gate-recovery.md) (D2), [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md)
