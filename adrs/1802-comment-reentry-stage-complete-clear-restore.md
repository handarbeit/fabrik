# ADR 1802: Clear and restore stage:<Stage>:complete for the duration of a comment re-entry

**Status:** Accepted
**Date:** 2026-09-18
**Issue:** [#1802](https://github.com/handarbeit/fabrik/issues/1802)

## Context

`processComments` re-enters a stage to process a steering comment (a plain new comment,
an awaiting-input unblock, a paused unpause, or one of the three catch-up-loop reinvoke
dispatchers — review, CI-fix, rebase). All six call sites funnel through this one
function. For the entire duration of a rework, the issue carries `fabrik:editing`
alongside a **stale `stage:<Stage>:complete`** left over from the stage's previous run —
reported in #1746 (@verveguy), observed mid-rework:

```
fabrik:yolo, fabrik:editing, stage:Specify:complete ... stage:Validate:complete
```

That issue was actively being reworked, and nothing on the board said so except
`fabrik:editing`. This is explicitly an observability defect, not a correctness one: the
engine gates correctly on unresolved review comments regardless of the stale label, and
would not have merged past them even under `fabrik:yolo`. The defect is that a reader
cannot distinguish *validated, finished, waiting for the train* from *validated earlier,
currently being reworked*, except by already knowing that `fabrik:editing` silently
invalidates every adjacent `stage:*:complete` — tribal knowledge, and it reads backwards,
since a `:complete` label looks like the more specific, more authoritative signal.

A precedent already exists, narrower in trigger and scope: the `stage:Validate:complete`
SHA-invalidation scan clears that label when the linked PR's HEAD SHA changes after
Validate completed, and `advanceValidateTerminalItem` later re-adds `stage:<N>:complete`
for every gate-checked stage once the PR merges — "clear now, let the normal completion
flow re-populate." This issue generalizes the same idea (a stage's completion claim can
become stale and must be correctable) to any stage's comment re-entry.

## Decision

**A marker label — `fabrik:reworking:<Stage>` — brackets the window a comment re-entry
has cleared the re-entered stage's own `stage:<Stage>:complete`.** The reworked stage's
name is encoded directly in the label suffix, not resolved from the item's board `Status`
at recovery time (`itemstate.Snapshot.Status()`) — see "Amendment" below for why the
original design (a single, fixed, non-suffixed label) was corrected before merge.

**The bracket is nested strictly inside the pre-existing `fabrik:editing` bracket** (Step
2 add → Step 9 remove in `processComments`), which is the one and only gate
(`itemNeedsWork`, `engine/item.go`) governing dispatch admission during rework. Because
every dispatch-admission decision that reads `stage:<Stage>:complete` is unreachable
while `fabrik:editing` is present, clearing/restoring the label strictly inside that same
window cannot, by construction, change any gating decision (R4). This is a structural
invariant of the implementation, not an incidental consequence — moving the clear before
`fabrik:editing` is added, the restore after it's removed, or letting the bracket leak
outside it in a future refactor would reopen a real (if narrow) race where a concurrent
poll could observe the stale-cleared label while dispatch is actually admissible.

**Mark-first-then-clear ordering.** `beginStageRework` adds `fabrik:reworking:<Stage>`
*before* removing `stage:<Stage>:complete`; `endStageRework` restores
`stage:<Stage>:complete` (or defers to the normal completion flow) *before* removing
`fabrik:reworking:<Stage>`. This makes the marker's presence the single, unambiguous
crash-recovery trigger: whichever of the two mutations in a pair actually landed before a
crash, restoring is always safe —
`AddLabelToIssue` is a documented no-op on an already-present label. The reverse ordering
would leave a window where a crash between the clear and the mark leaves no durable
signal that a restore is owed — exactly the "silently re-run a finished stage" failure R2
says is worse than the pre-fix stale-label lie.

**No-op when there is nothing to clear.** `beginStageRework` only acts (and only returns
`true`) when `stage:<Stage>:complete` is present on entry. The common mid-flight-rework
case — the stage hasn't completed yet, or a prior rework already cleared it — has nothing
to lie about, so no `fabrik:reworking:<Stage>`/clear pair is ever applied for it.

**Restore via the normal completion flow when the re-entry itself completes, not a direct
re-add.** When `completed == true` this cycle, `endStageRework` removes only
`fabrik:reworking:<Stage>` — `finalizeComments`'s existing, unmodified call to
`handleStageComplete` re-derives `stage:<Stage>:complete` from scratch, including its
`wait_for_ci` deferral (no `:complete` is added at all until CI passes). Restoring it
directly here too would be redundant and would duplicate the single source of truth for
"what does complete mean" in two places. Only the non-completing exits (generic error,
context cancellation already excluded by its own early return, tools-denied, the two
setup-failure early returns, and an exhausted extension loop) restore
`stage:<Stage>:complete` directly, since no other flow will restore it for those.

**`fabrik:reworking:<Stage>`'s removal on a completing exit happens strictly *after*
`handleStageComplete` runs, not before it.** `finalizeComments` calls
`e.handleStageComplete(...)` and only then calls `endStageRework(..., completedThisCycle:
true)` to drop the marker — never the other way around. `handleStageComplete`'s own
completion decision is itself made across several network calls (label writes, an
optional draft-PR/PR-ready path for `CreateDraftPR`/`MarkPRReadyOnComplete` stages, the
`wait_for_ci` deferral, or — for a Validate `yolo` merge failure — no label write at all,
identical to the non-rework dispatch path). Removing `fabrik:reworking:<Stage>` *before*
that decision is durably written, rather than after, would reopen exactly the crash window
R3 exists to close: a crash landing anywhere in that span leaves the marker gone and no
completion label recorded, so a genuinely-completed rework looks like the stage was never
attempted at all on restart — silently re-running a finished stage (R2) after all, just
relocated to a different, still-real window instead of the immediately-obvious one this
issue set out to fix. See the doc comments on `finalizeComments` and `endStageRework`
(`engine/comments.go`) for the call-site detail.

**Crash recovery is not "restore unconditionally" — it checks for an already-settled
outcome first.** Deferring the marker's removal to after `handleStageComplete` opens a
second, narrower crash point: the completion decision (`stage:<Stage>:complete`, or
`fabrik:awaiting-ci` for a `wait_for_ci` stage) can land successfully and *then* the
process crashes before the marker itself is removed. `runStartupCleanup`'s third pass
(`engine/worker_liveness.go`) checks whether `stage:<Stage>:complete` or
`fabrik:awaiting-ci` is already present (for the stage named in the marker — see
"Amendment" below) before restoring anything — if either is found, the rework already
settled correctly and only the now-stale marker needs cleaning up. An unconditional
restore here would be merely redundant in the `:complete` case (`AddLabelToIssue` is
idempotent) but actively wrong in the `fabrik:awaiting-ci` case: it would add
`stage:<Stage>:complete` on top of a stage the engine deliberately parked pending CI,
bypassing that gate on restart.

**Fail-open on the mutation itself.** If the `fabrik:reworking:<Stage>` add or the
`stage:<Stage>:complete` remove/restore fails transiently, the existing `addLabel`/
`removeLabel` idiom logs a warning and the cycle proceeds regardless — consistent with
this issue's own framing ("observability defect, not a correctness one"). Aborting a
rework cycle over a failed cosmetic label mutation would be a worse regression than one
cycle keeping the stale label.

**Startup-only crash recovery, not a per-poll settle scan.** This condition can only
arise from a crash landing exactly mid-bracket — an in-process, synchronous event, unlike
the async external conditions the `fabrik:awaiting-*` settle-scan family exists to poll
for (ADR-1270). `fabrik:editing`'s own restart-safety precedent (`runStartupCleanup`'s
"stale label + no active `Worker()`" scan, startup-only, no retry/escalation) is the
correct structural match. A third pass, added to the same function right after the
existing `fabrik:editing` pass, scans each unworked item's labels for the
`fabrik:reworking:` prefix, restores `"stage:" + <suffix> + ":complete"` using the stage
name read from that prefix, and removes the marker; a malformed marker (empty suffix)
logs loudly and is left in place rather than silently dropping the only remaining signal
that a restore is owed (mirroring ADR-1533's precedent for an unresolvable alert marker).

## Amendment (pre-merge, #1802)

The original implementation used a single, fixed, non-stage-suffixed `fabrik:reworking`
label, on the reasoning that only one stage is ever "current" for an item, so recovery
could resolve which stage's `:complete` to restore from the item's board `Status`
(`itemstate.Snapshot.Status()`) rather than from the label name.

A Pruefer bot review caught that this reasoning does not hold at *recovery* time. A
completing rework that also auto-advances the board (`fabrik:yolo`/`fabrik:cruise`/
`auto_advance: true`, non-`wait_for_ci`) moves `Status` to the *next* stage synchronously
inside `handleStageComplete` — before `finalizeComments`'s deferred call to
`endStageRework` ever removes the marker (deliberately deferred, per the ordering
invariant above). A crash landing in that window leaves the item with `Status` already
pointing at the *next* stage, the marker still present, and `stage:<ReworkedStage>:complete`
already correctly landed — but *no* `stage:<NextStage>:complete`, because the next stage
never ran. The original `Status`-keyed recovery would read `Status` (the next stage),
find its `:complete` absent, conclude "not yet settled," and stamp
`stage:<NextStage>:complete` onto a stage that was never actually dispatched.
`itemNeedsWork` then reads that label as "already done," silently and permanently
skipping the next stage's real work — precisely the failure mode R2 says is worse than
the original stale-label observability defect.

The fix encodes the reworked stage's name directly in the marker label
(`fabrik:reworking:<Stage>`) instead of inferring it from `Status`. This removes the
ambiguity structurally: the label always names the stage that was actually reworked,
regardless of how far `Status` has since advanced. Every other invariant in this ADR
(mark-first-then-clear ordering, deferred marker removal, checking for an already-settled
outcome before restoring, fail-open on the mutation itself, startup-only recovery) is
unchanged — only the source of the stage name changed, from `Status` to the label itself.

## Consequences

- The board now always reflects whether a stage's most recent claim of completion is
  live or is currently being reworked, without requiring a reader to know that
  `fabrik:editing` silently overrides an adjacent `stage:*:complete`.
- No gating decision changes: merge gating, review gating, and auto-advance are
  byte-identical, since the entire clear/restore window is contained inside the
  pre-existing `fabrik:editing` admission gate.
- The mark-first-then-clear/restore-first-then-unmark ordering is a load-bearing
  invariant of this fix, not a stylistic choice — a future edit to `processComments`
  that reorders these calls, or lets them escape the `fabrik:editing` bracket, would
  silently reintroduce either the original observability defect or the narrow race R4
  depends on not existing. Both risks are covered by a dedicated regression test.
- `fabrik:reworking:<Stage>` is purely additive to `fabrik:editing`'s own restart-safety
  precedent (`runStartupCleanup`) — it does not replace or modify that scan's existing
  behavior for `fabrik:locked:<user>`/`fabrik:editing`.
