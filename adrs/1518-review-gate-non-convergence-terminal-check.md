# ADR 1518: Review-gate non-convergence gets its own terminal check and counter

**Status:** Accepted
**Date:** 2026-08-10
**Issue:** [#1518](https://github.com/handarbeit/fabrik/issues/1518)

**Amends:** [ADR-1375: `review_authority: authoritative` reinvokes on unresolved
feedback — it never pauses first](1375-review-authority-reinvoke-not-pause.md)

**Interacts with:** [ADR-1045: review-body comments are actionable regardless of
state; a no-op reinvoke doesn't spend the cycle budget](1045-review-body-comment-actionability-and-noop-budget.md)

## Context

`handleReviewGate` (`engine/catch_up_handlers.go`) is supposed to bound a
non-converging authoritative review gate at `MaxReviewCycles` and terminate in
`pauseForReviewCycleLimit` — a pause whose comment names the cycle ceiling and
tells the operator the loop was bounded deliberately, distinct from
`pauseForReviewTimeout`'s "nobody has responded" diagnosis. In practice it was
terminating in `pauseForReviewTimeout` instead, confirmed live in the e2e bed
(`TestReviewAuthorityCycleLimitPauses`, two consecutive full-run failures) and
independently by static tracing plus the existing unit suite.

Two independent, code-confirmed mechanisms produced the same outward symptom
(repeating "still blocking" log line, no reinvoke dispatched, eventual
`pauseForReviewTimeout`):

**Mechanism 1 — dedup-empty fallthrough.** `pauseForReviewCycleLimit` was
reachable only from inside `handleReviewGate`'s `len(syntheticComments) > 0`
branch, via `dispatchWithCycleLimit`. The moment the currently-outstanding
review/thread comment had already been addressed by an earlier reinvoke
(`snap.CommentProcessed(id)` set) but the underlying verdict (e.g.
`CHANGES_REQUESTED`) had not yet changed — the steady state immediately after
the `MaxReviewCycles`-th reinvoke completes, before any *new* review lands —
`syntheticComments` was empty on that poll even though the persisted cycle
count was already at or past `MaxReviewCycles`. Control fell into the
pre-existing `blocked`/`timedOut` tail, which had no cycle-count awareness at
all: a genuinely blocked, not-yet-timed-out state did nothing that poll, and
once `timedOut` became true, `pauseForReviewTimeout` fired unconditionally.
`TestHandleReviewGate_AuthoritativeBlocked_AlreadyProcessed_FallsThroughToCooldown`
already proved this branch was real and correct at `cycleCount == 0` (nothing
has ever been spent, correctly falls through) — the untested, unreachable gap
was the same shape at `cycleCount >= MaxReviewCycles`.

**Mechanism 2 — refund masking.** `dispatchReviewReinvoke`'s `after` hook
applies `itemstate.ReviewCycleDecremented` (ADR-1045) whenever a reinvoke
lands no new commit, floored at 0 in the store, unconditionally — regardless
of whether the gate was still blocking when the reinvoke was dispatched. In a
loop where every reinvoke happens to be a no-op with respect to `HEAD`,
`ReviewCycles` could never accumulate past 1, so it never reached
`MaxReviewCycles` via the existing cycle-limit check either. This is a real
defect, not merely hypothetical, but it is not a general license to narrow the
refund: ADR-1045's own motivating scenario
(`TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed`)
depends on the refund forgiving an *unbounded* number of no-op reinvokes on
non-blocking feedback (advisory-mode `COMMENTED` junk overviews that never
held up the gate at all) — that forgiveness must remain unconditional for that
case.

The two scenarios are structurally distinguishable at the exact point
`handleReviewGate` decides to dispatch: `checkReviewGate`'s own `blocked`/
`timedOut` return (computed *before* `syntheticComments` is even built) is
true only when the gate itself is still failing to clear. A no-op reinvoke
dispatched while `blocked || timedOut` is genuine non-convergence evidence; a
no-op reinvoke dispatched while `!blocked && !timedOut` (ADR-1045's shape) is
harmless junk that must keep being forgiven forever.

## Decision

**A structural terminal check, not a special-cased fallback (R1/R2).** When
`syntheticComments` is empty on a given poll but the gate is still
`blocked || timedOut`, `handleReviewGate` now reads the persisted cycle count
from the store *before* falling into the pre-existing
`CooldownRecorded`/`pauseForReviewTimeout` tail. If it is already at or past
`MaxReviewCycles`, `pauseForReviewCycleLimit` fires directly and the item is
claimed — the terminal decision becomes a pure function of persisted state and
the gate's own blocked/timed-out verdict, never of whether this particular
poll happened to find fresh, undeduped feedback. The check is gated on
`blocked || timedOut`, so the naturally-cleared case (`blocked == timedOut ==
false`, which returns `false` unclaimed) is untouched, and a structurally-zero
cycle count (nothing has ever been spent — the only shape a genuine "nobody
has ever reviewed" block can have) can never satisfy the `>=` comparison, so
R4 holds without an explicit special case.

**An additive, never-refunded `ReviewBlockedCycles` counter, not a narrowed
`ReviewCycleDecremented` (R3).** `internal/itemstate/itemstate.go`'s
`StageState` gains `ReviewBlockedCycles map[string]int`, mirroring the
existing `ReviewCycles` per-stage counter shape. `handleReviewGate` increments
it — via a new `ReviewBlockedCycleIncremented` mutation, applied alongside the
existing pre-dispatch `ReviewCycleIncremented` — exactly when a reinvoke is
about to be dispatched *and* `blocked || timedOut` is true at that moment:
only when the gate itself had something real to lose by not converging. It is
never decremented; unlike `ReviewCycles` it has no compensating mutation at
all. Both the existing `syntheticComments > 0` cycle-limit check and the new
fallback-tail terminal check above compare against
`max(ReviewCycles, ReviewBlockedCycles)`, so a loop where every reinvoke nets
to zero via ADR-1045's refund can no longer stay below `MaxReviewCycles`
forever *when it was genuinely blocked at dispatch time*. A reinvoke
dispatched while `!blocked && !timedOut` — ADR-1045's own scenario — never
increments `ReviewBlockedCycles`, so it remains forgivable indefinitely,
exactly as ADR-1045 requires.

`EngineCyclesCleared` (the reset `handleEngineUnpause`/`clearFailedStage`
apply on resume from any of the four cycle-limit pauses, ADR-1460) now also
clears `ReviewBlockedCycles` — otherwise a resumed item would immediately
re-trip the new terminal check on its very next pass, reproducing the exact
"unpause → re-pause in one poll" failure ADR-1460 already fixed for the other
three counters. `copyStageState` (`internal/itemstate/snapshot.go`) adds the
new map to its deep-copy enumeration, matching every other `StageState` map
field.

## Alternatives Considered

**Narrow `ReviewCycleDecremented` itself** by threading `blocked`/`timedOut`
into `dispatchReviewReinvoke`'s `after` hook and skipping the decrement when
either was true at dispatch time. Smaller diff, but it touches the exact
mutation and code path
`TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed`
(ADR-1045) pins byte-for-byte, for no structural benefit over the additive
counter — and makes the two ADRs' logic more entangled to reason about
independently. Rejected in favor of the additive counter: it leaves every
existing ADR-1045 mutation, test, and code path completely untouched, and
composes for free with the merge-train eject/re-queue path exactly as
`ReviewCycles` already does (confirmed during Research: `merge_train.go`
applies no `ReviewCycles`/`ReviewBlockedCycles` mutations of its own — it
relies entirely on the ordinary review-reinvoke loop re-evaluating a
re-queued member).

**Cap consecutive no-op refunds outright**, independent of `blocked`/
`timedOut` at dispatch time. Rejected outright: this is exactly what ADR-1045
requires *not* happen for its own five-consecutive-no-op scenario — a blanket
cap would break that test by design, not just risk regressing it.

## Consequences

**`pauseForReviewCycleLimit`'s pause comment can report a different, more
honest cycle count when the two counters diverge.** In the refund-masked
scenario (mechanism 2), the comment now shows
`max(ReviewCycles, ReviewBlockedCycles)`, which will be higher than
`ReviewCycles` alone once the refund has been masking non-convergence. This is
the intended, more accurate number — the operator should see how many
genuinely-blocked attempts actually occurred, not the refund-suppressed
figure — but it is a user-visible text change worth calling out explicitly
rather than mistaking for an unrelated regression.

**Both fix components must land together.** The terminal check alone (without
`ReviewBlockedCycles`) would still leave mechanism 2 able to mask the
`syntheticComments > 0` branch's own cycle-limit check indefinitely; the new
counter alone (without the terminal check) would still leave the dedup-empty
fallthrough with no check at all. Splitting them across separate changes
would leave one mechanism unfixed.

**No new label, no new engine-visible state beyond the counter itself.**
`ReviewBlockedCycleIncremented` mirrors `ReviewCycleIncremented`'s shape
exactly and is consulted nowhere except through the new
`Snapshot.ReviewBlockedCycles` accessor, the same read path every other
cycle-limit check already uses. `pauseForReviewCycleLimit`'s message format
itself is unchanged — only the cycle-count argument it is called with can
differ.

## Related

- [ADR-1375](1375-review-authority-reinvoke-not-pause.md) — the
  reinvoke-not-pause principle this ADR's terminal check still respects: a
  reinvoke always gets first crack at unresolved feedback; only a genuinely
  exhausted loop pauses.
- [ADR-1045](1045-review-body-comment-actionability-and-noop-budget.md) — the
  `ReviewCycleDecremented` no-op-cycle refund this ADR's new counter composes
  alongside without narrowing; `TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed`
  remains the non-regression pin for that refund's own unconditional
  forgiveness.
- [ADR-1460](1460-resumable-engine-pauses.md) — `EngineCyclesCleared`'s reset
  pattern on resume, which `ReviewBlockedCycles` now participates in
  identically to `ReviewCycles`/`CIFixCycles`/`RebaseCycles`/`EnqueueCycles`.
- [ADR-1250](1250-review-authority-orthogonal-to-autonomy.md) —
  `review_authority` governs merging, never working; unaffected by this ADR.
