# ADR 1270: Awaiting-CI Settle Scan

**Date**: 2026-07-31
**Status**: Accepted
**Issue**: #1270 — item strands forever with `fabrik:awaiting-ci` after a confirmed CI failure

## Context

An issue could strand **indefinitely** at a `wait_for_ci` stage with `fabrik:awaiting-ci` present and
a confirmed CI failure: no CI-fix reinvoke dispatched, no cycle limit tripped, no timeout fired, no
pause, no comment. Field evidence (`handarbeit/fabrik-test-alpha#3915`) showed the item reliably
reaching `selectDeepFetchCandidates`'s deep-fetch call every poll (`"reading details for #3915 from
cache"` recurring every ~31s) yet producing **zero** `"settle"`-tagged log lines for an 80+ minute
stall, while a sibling item in the same run produced many in the same window.

Before this change, `fabrik:awaiting-ci` items were evaluated by a shared, three-layer admission
pipeline that also gates every other Phase 1 handler (dependency check, review gate, merge-conflict
gate):

1. `itemMayNeedWork` (`engine/item.go`) — a cheap label/status pre-check that unconditionally returns
   `false` for `HoldingStage`/`Unmanaged` stages, with no `fabrik:awaiting-ci` exception (unlike the
   closed-issue branch just above it).
2. `selectDeepFetchCandidates`'s cooldown pre-filter (`engine/poll.go`) — gave `fabrik:awaiting-ci` an
   explicit bypass around the *cooldown* skip, but the bypass only reached as far as
   `itemMayNeedWork` (layer 1 still applied).
3. The main catch-up loop's per-item admission gate (`engine/poll.go`) — the one
   `!hasComplete && !(hasAwaitingCI && isWaitForCI)` guard `docs/state-machine.md` §6.5.1 documented
   as "the" entry mechanism; it was not the only one.

Static reading of the Phase 1 handler chain itself found no bug that would explain #3915's specific
stall — `handleDependencies`/`handleReviewGate`/`handleAutoMergeConvergence` are legitimate, silent
no-ops for a solo item in that state, and `handleMergeAndCIGates`'s `settlePRMergeState` call logs
under `"settle"` on nearly every branch. The precise trigger for #3915 was not root-caused, and the
issue's own scope explicitly did not require it to be: this is the fifth occurrence in this codebase
of the same failure shape — a durable "awaiting-X" marker whose only retry path runs through a shared,
admission-gated pipeline that can silently exclude it, with the marker left with no owner to clear it.
The prior four occurrences (`fabrik:awaiting-done`/ADR-060, spawned-child placement/ADR-062,
merge-train singleton member-close/ADR-061, non-default-base explicit close/ADR-1097) were each fixed
the same way: give the marker's retry evaluation a dedicated `board.Items`-sourced settle scan,
independent of the shared admission pipeline. `fabrik:awaiting-ci` was the one remaining durable marker
without one.

## Decision

Add `settleAwaitingCIScan` (`engine/ci_settle.go`), a dedicated settle scan that is the **sole**
per-poll evaluator of the CI gate for open, not-yet-complete `fabrik:awaiting-ci` items:

- Iterates `board.Items` directly — never `deepFetchCandidates`, never `itemMayNeedWork`. Skips items
  without `fabrik:awaiting-ci`, `fabrik:paused` items, and closed items (closed-issue recovery for a
  merged PR remains `runValidatePRTerminalAdvance`'s job, ADR-056 D2).
- Resolves the item's stage. If it does not resolve to a real `wait_for_ci` stage (no stage found, not
  `wait_for_ci`, or `HoldingStage`/`Unmanaged`/`CleanupWorktree`) — the "gate genuinely cannot be
  evaluated here" case — logs the stray column and retries via the existing generic
  `recordSettleRetry`/`escalateSettle` helpers, keyed by a dedicated synthetic retry-stage constant,
  `awaitingCIOrphanRetryStage = "__awaiting_ci_orphan__"`. After `MaxRetries` failed passes,
  `escalateAwaitingCIOrphanFailure` pauses the issue, removes `fabrik:awaiting-ci`, and posts an
  explanatory comment naming the stray column and the manual recovery step.
- Otherwise, deep-fetches the item (`FetchItemDetails`), re-checks `fabrik:awaiting-ci`/`fabrik:paused`
  post-fetch (guarding against a concurrent clear between the shallow board read and the deep-fetch),
  clears the orphan-retry counter (a transient bounce through a stray column must not accumulate toward
  escalation), and runs the **identical** `catchUpPhase1Handlers` chain the main catch-up loop uses —
  `handleDependencies → handleReviewGate → handleAutoMergeConvergence → handleMergeAndCIGates` — by
  constructing the same `phase1Ctx` the main loop builds. It does not reimplement any gate
  classification or label-mutation logic.
- If no handler claims the item, immediately runs `runCatchUpPhase2` — the gated stage-advancement /
  Validate-landing step (see "Same-poll landing handoff" below).

The main catch-up loop's own per-item admission gate is narrowed to `hasComplete`-only, dropping the
`hasAwaitingCI && isWaitForCI` carve-out entirely (with a diagnostic log line when a
`fabrik:awaiting-ci` item is skipped there, naming the reason: it is owned by the dedicated scan now).
`fabrik:awaiting-ci` is also removed from `selectDeepFetchCandidates`'s cooldown-bypass label list —
the dedicated scan makes that bypass redundant GraphQL cost with no remaining functional purpose.

### Same-poll landing handoff

Phase 2's gated stage-advancement logic (yolo/cruise/`auto_advance` gating; at Validate, the yolo-only
auto-merge-enablement call into `attemptMergeOnValidate`) was extracted from the main catch-up loop's
per-item body into a shared function, `runCatchUpPhase2` (`engine/poll.go`), called from both the main
loop (for `hasComplete` items) and `settleAwaitingCIScan` (for awaiting-ci items whose CI gate clears
on this exact poll pass). This preserves ADR-1216's same-poll CI-gate-clears → landing-decision handoff
(§6.6.6): before this change, an `awaiting-ci` item admitted into the single per-item loop could have
its CI gate clear in Phase 1 and reach the Validate landing decision (`attemptMergeOnValidate`) in the
very same loop iteration, without waiting a full poll cycle. Narrowing the main loop's admission gate
to `hasComplete`-only, without this extraction, would have silently reintroduced the #1216 race this
codebase already fixed once — deferring the landing decision by a full poll every time CI clears,
because the item would no longer reach Phase 2 in the same pass it reaches Phase 1. `runCatchUpPhase2`
makes the two call sites produce the identical outcome; only *which* loop iteration performs it
differs. Verified directly by `TestValidateLanding_ReviewGateBlocks_WhenCIClearsSamePoll` and
`TestValidateLanding_ReviewGateBlocks_MergeTrainOn` (`engine/poll_test.go`), both of which regressed
during implementation before this extraction and pass after it.

## Rationale

### Why reuse `catchUpPhase1Handlers` verbatim instead of a bespoke CI-only settle function?

A bespoke function calling `checkCIGate` directly would skip `handleDependencies`, silently regressing
the "every gate applies uniformly regardless of admission path" guarantee for awaiting-ci items. More
importantly, it would create a **third**, divergence-prone owner of `stage:X:complete` under
`wait_for_ci` — `docs/state-machine.md` §6.5.1 already tracks exactly two ("Clearing-owner invariant"):
the normal path (`addCompleteLabelAndRemoveCI`, via `checkCIGate`) and the PR-merged recovery path
(`runValidatePRTerminalAdvance`, ADR-056 D2). `settleAwaitingCIScan` is a second **call site** into the
first of those two, not a second **owner** — it never mutates `stage:X:complete` or `fabrik:awaiting-ci`
directly, only through the same shared `handleMergeAndCIGates`/`checkCIGate` code the main loop already
uses.

### Why narrow the main loop's admission gate instead of leaving both paths active?

Leaving the old `hasAwaitingCI && isWaitForCI` carve-out in place *alongside* the new scan would let
both paths run `handleMergeAndCIGates` for the same item in the same poll whenever both the main loop's
`deepFetchCandidates` pipeline and the dedicated scan happened to admit it — risking a double-dispatched
CI-fix reinvoke or a double-incremented `CIFixCycles` counter. Since `fabrik:awaiting-ci` and
`stage:X:complete` are mutually exclusive in steady state (the label is cleared in the same call that
adds the completion label), narrowing the gate to `hasComplete`-only makes the partition exhaustive:
every `awaiting-ci` item is `!hasComplete`, so it is *always* routed to the dedicated scan and *never*
to the main loop, until the gate clears. This is a structural guarantee, not a runtime race avoided by
convention — `TestSettleAwaitingCIScan_NoDoubleDispatch` pins it directly.

### Why not fix `itemMayNeedWork`'s `HoldingStage`/`Unmanaged` exclusion instead?

Research confirmed this as a real, code-verified gap (no `fabrik:awaiting-ci` exception, unlike the
closed-issue branch immediately above it) but also confirmed it was **not** the direct cause of
#3915's specific stall trace (the item was reliably passing that check throughout, staying resolvable
to the `Validate` stage). Patching it would have been a narrower, single-trigger fix — exactly what
this ADR's remedy shape (already proven four times) is designed to avoid depending on. Since the
dedicated scan sources from `board.Items` directly and never calls `itemMayNeedWork` for awaiting-ci
items, this gap becomes irrelevant to the CI gate specifically. It is left unfixed for the *other*
labels that still share `itemMayNeedWork` (`fabrik:awaiting-review`, `fabrik:rebase-needed`, etc.) —
deliberately out of this issue's scope.

### Why a dedicated retry-counter constant instead of reusing an existing one?

`awaitingCIOrphanRetryStage = "__awaiting_ci_orphan__"` mirrors `mergeTrainMemberCloseRetryStage` and
`nonDefaultBaseCloseRetryStage`: a double-underscore-wrapped, YAML-unrepresentable name that can never
collide with a real configured stage's own retry counter, and is not conflated with any other failure
class's counter.

### Why is the scan diagnostics-first?

The issue's own root-cause complaint was an 80-minute silence with no log line explaining why the item
wasn't progressing. Every settle pass logs under the `awaiting-ci-settle` tag: a stray-column skip
names the column and the reason it can't host the CI gate; a summary line reports how many
`fabrik:awaiting-ci` items were processed when the count is non-zero. This directly satisfies the
issue's fourth Requirement (a skipped-evaluation log line, and why) independent of whichever structural
fix ultimately closes the gap for a given field recurrence.

## Consequences

**Positive:**
- No terminal state remains reachable in which `fabrik:awaiting-ci` is present, CI has a definitive
  verdict, and the engine takes no further action — the scan is admission-pipeline-independent by
  construction, so whatever silently excluded #3915 from the old shared pipeline cannot exclude it from
  this scan.
- Closes the entire *class* of "shared admission gate silently excludes this durable marker" bugs for
  `fabrik:awaiting-ci`, not just the one confirmed-but-unproven `HoldingStage`/`Unmanaged` trigger —
  consistent with the remedy shape already validated four times in this codebase.
- `fabrik:awaiting-ci` no longer needs a bypass entry in `selectDeepFetchCandidates`'s cooldown-skip
  list, removing a redundant per-poll `FetchItemDetails` call for items the dedicated scan already
  re-fetches itself.
- The ADR-1216 same-poll joint-clearing handoff is preserved exactly (§6.6.6), via the
  `runCatchUpPhase2` extraction shared by both call sites.

**Negative / Trade-offs:**
- `settleAwaitingCIScan` scans all of `board.Items` every poll (a cheap label-membership check before
  the more expensive path), rather than a pre-filtered subset — negligible cost, bounded by the small,
  transient number of issues mid-CI-await at any time, matching every other `board.Items`-sourced
  settle scan in this codebase.
- The exact trigger for #3915's specific stall remains unconfirmed. This ADR's remedy closes the class
  of bug regardless, per its own design rationale, but a future recurrence through a genuinely novel
  mechanism cannot be ruled out by construction — only mitigated by the new diagnostic logging, which
  is what would let it be root-caused in minutes instead of 80 next time.
- `runCatchUpPhase2`'s extraction slightly widens the surface `settleAwaitingCIScan` touches beyond
  pure CI-gate evaluation (it can now also enable auto-merge or advance a non-Validate stage). This is
  a deliberate, necessary consequence of preserving ADR-1216's handoff, not scope creep — the
  alternative (a poll-cycle-deferred landing decision) is the exact race #1216 already fixed once.

## Sibling Audit

This is the fifth instance of the "dedicated `board.Items`-sourced settle scan" pattern in this
codebase (`fabrik:awaiting-done`/ADR-060, spawned-child placement/ADR-062, merge-train member-close
retry/ADR-061, non-default-base close retry/ADR-1097, now `fabrik:awaiting-ci`). Unlike the other four,
this marker is not written-once-on-failure — `fabrik:awaiting-ci` is applied by `handleStageComplete`
on `FABRIK_STAGE_COMPLETE` and is expected to be present for the normal duration of CI-await, not just
a rare failure branch. `settleAwaitingCIScan` is therefore the marker's sole *evaluator*, not merely its
failure-recovery retry path — closer in spirit to how `checkCIGate` itself always worked, just relocated
to an admission-independent call site.

**References:** [ADR-056: Consolidate Convergence Gate Recovery](056-consolidate-convergence-gate-recovery.md), [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md), [ADR-061: Merge-Train Singleton Member-Issue Close Retry](061-merge-train-member-close-retry.md), [ADR-1097: Non-Default-Base Explicit Close Retry](1097-non-default-base-close-retry.md), [ADR-1216: Review Gate Checked at the Landing Decision](1216-review-gate-at-landing-decision.md), [ADR-1223: Gate Pause Terminates, Not Clears](1223-gate-pause-terminates-not-clears.md)
