# ADR 1223: A Phase 1 gate pause must signal termination, not reuse "cleared"

**Status:** Accepted
**Date:** 2026-07-28
**Issue:** [#1223](https://github.com/handarbeit/fabrik/issues/1223)

## Context

`catchUpPhase1Handlers` (ADR-056 D3) is an ordered list of handlers, each returning a
single `bool`: `true` claims the item (Phase 2 is skipped for it this pass), `false`
passes through. Three handlers wrap a lower-level gate function that returns a richer
value and must be translated into that single claim boolean:

- `handleMergeAndCIGates` → `checkCIGate` (`(blocked, ciFailure, timedOut bool)`)
- `handleReviewGate` → `checkReviewGate` (`(blocked, timedOut bool)`)
- `handleDependencies` → `checkDependencies` (`bool`, no translation — its own return
  *is* the claim signal)

Each of the three had (at least) one branch that calls a pause helper — `pauseIssue`,
which mutates only remote/cache state and never the caller's local `gh.ProjectItem`
value — directly, and then falls through to the same return value used for "nothing is
blocking, gate cleared normally":

- `checkCIGate`'s `PRMergeTerminal`/closed-without-merge branch (`pauseForPRClosedNotMerged`)
  returned `(false, false, false)` — identical to the no-PR/merged/all-green clearing
  returns.
- `classifyCIFromMergeableState`'s R3 branch (`pauseForRequiredNeverRunningCheck`,
  reached through `checkCIGate`) returned the same `(false, false, false)`.
- `checkReviewGate`'s broken-linkage branch (`handleBrokenReviewLinkage`) returned
  `(false, false)` — identical to "all reviewers responded, gate cleared naturally."
- `checkDependencies`'s cycle-detection branch (`detectCycle` finds the item is itself a
  transitive blocker of one of its own blockers) returned `false` — identical to "no
  open dependencies."

Because `pauseIssue` never mutates the caller's local item copy, nothing downstream in
the same call stack can infer "this was just paused" by re-reading `item.Labels` — the
label only appears on the *next* poll's fresh board fetch. The return value was the only
channel available, and it was overloaded: one bit meaning two incompatible things,
"nothing is blocking" and "I already terminated processing for this item."

The consequence: the calling handler read the all-false/false return as "gate cleared,"
did not claim the item, and Phase 1 fell through. If Phase 2's advance gate (yolo,
cruise, or `auto_advance: true`) was open, `advanceToNextStage` or
`attemptMergeOnValidate` ran **in that same poll pass**, moving or merging an item that
had just been paused for human intervention seconds earlier in the same call.

This is a different bug shape than ADR-1216's: that one was a frozen-snapshot ordering
hole (`pctx.hasComplete` computed before the chain runs); this one is an overloaded
*return value* at three independent call sites. The two must not be conflated, but the
same principle governs the fix: add an explicit signal at the existing choke point,
don't restructure the loop.

## Decision

Give each pause branch a **distinct signal**, checked by the caller ahead of every other
branch:

- `checkCIGate` and `classifyCIFromMergeableState` grow a fourth return value,
  `terminated bool`. The `PRMergeTerminal` closed-without-merge branch and R3's
  never-triggers-check branch set it `true`; every other return appends `false`
  (including the two early-return classify calls, `classifyCIFromRequiredContexts` and
  `classifyCIFromCheckRuns`, which never pause directly and are otherwise untouched).
  `handleMergeAndCIGates` checks `ciTerminated` first and claims (`return true`)
  immediately when set.
- `checkReviewGate` grows a third return value, `terminated bool`, set `true` by both of
  `handleBrokenReviewLinkage`'s pause branches (default-branch and `base:<branch>`).
  `handleReviewGate` checks `terminated` first and claims immediately when set — ahead
  of the `blocked`/`timedOut` checks and ahead of the `buildReviewThreadComments` /
  review-reinvoke fall-through, so a just-paused item can never have a reinvoke
  dispatched against it in the same pass either.
- `checkDependencies` needs no signature change: its own `bool` return already **is**
  the claim signal (no translation layer in `handleDependencies`). The cycle-detection
  branch changes `return false` to `return true` — the one-line fix this simpler shape
  allows.

**`terminated` is checked ahead of the other fields, not merely alongside them.** In
practice, today, every pause branch also happens to return all-false for the other
fields, so checking `terminated` last would produce the same claim decision. Checking it
first decouples the claim decision from what the other fields happen to contain — a
future pause branch that also sets, say, `timedOut=true` for logging purposes cannot
silently change Phase 1's claim behavior. Cheap to get right now, expensive to debug
later if it drifts.

## Consequences

**Convention for future gate authors:** any Phase 1 gate handler (or a function it
wraps) that pauses an item directly, via `pauseIssue` or an equivalent, must signal that
termination distinctly in its return value — never reuse the "cleared" shape. A fourth
gate handler written the same way (a helper pauses directly, then falls through to a
`return` reusing an existing value) reintroduces this exact class of bug.

**No `poll()`-level test existed for the Phase 1 → Phase 2 sequence at all**, for any
gate, before this issue — every existing test called `checkCIGate` / `checkReviewGate` /
the handler functions directly. Two new tests
(`TestCatchUpLoop_CIGateTerminated_DoesNotAdvanceInSamePass`,
`TestCatchUpLoop_ReviewGateTerminated_DoesNotAdvanceInSamePass`, `engine/poll_test.go`)
drive a real `eng.poll()` call with an `AutoAdvance` stage and assert no
`UpdateProjectItemStatus` call happened alongside the pause — verified to fail (with a
spurious status-update call) when the `terminated` check is stripped, confirming they
catch the regression.

**`handleAutoMergeConvergence` and `checkMergeabilityGate` are unaffected.**
`handleAutoMergeConvergence` unconditionally returns `true` regardless of what it wraps,
so no overloaded-return ambiguity is possible there by construction.
`checkMergeabilityGate` never calls a pause helper — every branch either clears, blocks,
or applies `fabrik:rebase-needed` (a normal label, not a pause) — so it is not an
instance of this bug class.

## Alternatives Considered

**A shared gate-result struct** (e.g. `gateOutcome{Blocked, Failure, TimedOut,
Terminated bool}`) used by all gate functions for consistency. Rejected: each affected
function has exactly one production call site, so a struct would force touching
unaffected clean gates (`checkMergeabilityGate`) for no behavioral gain, and the issue's
own scope is the narrow "pauses directly, returns the cleared value" shape — not a
general gate-interface redesign. The additive boolean is the smallest diff that
satisfies the requirement.

**Re-reading `item.Labels` after a pause to detect the just-paused case.** Rejected:
`pauseIssue` takes `item gh.ProjectItem` by value and only issues remote label-add
calls — it never mutates the caller's local copy. There is no way to detect the pause
after the fact within the same call without threading an explicit signal through the
return path.

## References

- [ADR-056: Consolidate convergence gate recovery](056-consolidate-convergence-gate-recovery.md) —
  established the ordered-handler-list architecture (D3) this issue's handlers live in.
- [ADR-1216: Review gate checked at the landing decision](1216-review-gate-at-landing-decision.md) —
  precedent for "add an explicit signal at the existing choke point, don't restructure
  the loop," applied here to a different bug shape (overloaded return value vs. frozen
  snapshot).
- `docs/state-machine.md` §6.5.1, §6.1, §6.2 — as-built description of the four-outcome
  `checkCIGate`/`checkReviewGate` signatures and the claim behavior.
