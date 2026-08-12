# ADR 1555: Success-agnostic comment-cycle breaker, and a durable dedup marker for review bodies

**Date**: 2026-08-12
**Status**: Accepted
**Issue**: #1555 — Successful no-op comment re-invocations are unbounded (sibling of #1382)

**Interacts with:**
[ADR-1089: Comment-Processing Circuit Breaker](1089-comment-processing-circuit-breaker.md),
[ADR-1518: Review-gate non-convergence gets its own terminal check and counter](1518-review-gate-non-convergence-terminal-check.md),
[ADR-1045: review-body comments are actionable regardless of state; a no-op reinvoke doesn't spend the cycle budget](1045-review-body-comment-actionability-and-noop-budget.md),
[ADR-1460: Resumable engine pauses](1460-resumable-engine-pauses.md)

## Context

`handarbeit/fabrik#1254`'s Validate stage redelivered the same stale, already-processed
bot review body **seven** consecutive times, each a clean (no-op) cycle: no commit, no
issue-body update, no `FABRIK_STAGE_COMPLETE`. `#1253` showed the identical shape a third
time. Neither issue ever carried `fabrik:paused` — nothing bounded the loop.

Two independent, pre-existing mechanisms both miss this shape by construction:

**The `stage:*:failed`/`max_retries` family never applies.** These invocations exit
cleanly. #1382 (PR #1425, commits #1413/#1414) already closed the *failure*-keyed comment
loop — its own `history.json` evidence showed `IsComment=true, Success=false` running 141
times unbounded before the fix — but every invocation here is `Success=true`. #1382's fix
does not see this loop at all.

**The existing #1089 comment-processing breaker (`engine/comment_breaker.go`) records
every invocation, success or failure — but its counter is a rolling 30-minute window**
(`CommentBreaker.InvocationsAt`, pruned at both write and read time), sized for #1083's
burst-loop shape (~995 invocations in rapid succession). A loop whose invocations are
spaced sparser than 30 minutes apart never accumulates enough entries within any single
window to reach the threshold, no matter how many total invocations occur over the
issue's lifetime. Research traced the actual spacing here to Fabrik's own self-upgrade
restart cadence (`syscall.Exec`, routine and frequent): a review-body synthetic comment
(`reviewBodyCommentID`, `engine/reviews.go`) has **no GitHub reaction endpoint** — GitHub's
REST reactions API has nothing to attach a 🚀 to on a `pulls/.../reviews/{id}` object
itself — so `snap.CommentProcessed(id)`, held entirely in `itemstate.Store`, was the
*only* idempotency guard for it. That store is wiped by every restart. Each restart
re-admits the same already-addressed review body as "new," and the redelivery cadence
this produces (hours to days between self-upgrades) is exactly what the 30-minute window
can never see.

This is structurally the same "windowed/refundable counter can't see a sparse or masked
non-convergence" defect ADR-1518 diagnosed for the review-gate path: `ReviewCycles` could
be refunded to near-zero forever by #1045's no-op-cycle forgiveness
(`ReviewCycleDecremented`), masking genuine non-convergence from `MaxReviewCycles`.
ADR-1518's fix was an additive, never-refunded `ReviewBlockedCycles` counter compared via
`max()` against the refundable one — the direct precedent this issue's R1 half follows.

Two genuinely separate defects, therefore two independent fixes:

1. **R1/R2/R4/R5 — nothing bounds a success-agnostic no-op comment loop.** A safety net
   independent of *why* a cycle keeps producing nothing.
2. **R3 — the review-body redelivery itself is a bug**, not merely a symptom the safety
   net should tolerate. Fixing only (1) caps the damage but leaves the redelivery firing
   on every restart indefinitely.

## Decision

### R1/R2/R4/R5 — `NoOpCommentCycles`, a second, differently-shaped breaker

`internal/itemstate/itemstate.go`'s `StageState` gains `NoOpCommentCycles map[string]int`
— per-**stage**, never-time-pruned, mirroring `ReviewCycles`/`ReviewBlockedCycles`'s shape
(ADR-1518), **not** `CommentBreaker`'s item-scoped rolling-timestamp shape. This is the
structural fix for the windowing defect: a plain counter that increments once per no-op
cycle and never ages out sees a loop regardless of how sparsely its invocations are
spaced.

**Recording point.** `checkNoOpCommentCycle(item, stage, progressed bool, lastAuthor)`
(`engine/comment_noop_breaker.go`) is called from the same five mutually-exclusive exit
points in `processComments` (`engine/comments.go`) that already call
`checkCommentBreaker` — three setup-failure early returns (editing-label add, base-branch
resolution, worktree setup), the invocation-error path, and the success tail. Exactly one
of the five executes per cycle. `progressed` is computed once per cycle, at a single
point, from the three signals R2 names explicitly:

- a new commit (`headChanged`, captured immediately after `runCommentExtensionLoop`
  returns — the same `gitHeadSHA` before/after comparison the old breaker's own reset
  already uses),
- an issue-body update (`extractUpdatedBody(output) != ""`, evaluated against the
  caller's own copy of `output` — `publishCommentOutput` takes `output` by value, so its
  internal marker-stripping never mutates the caller's copy),
- stage completion (`completed`, the same `FABRIK_STAGE_COMPLETE`-derived bool already
  threaded through the rest of `processComments`).

The three setup-failure sites pass `progressed=false` directly — no invocation ran, so
none of the three signals can be true by construction.

**Increment vs. reset**, not increment vs. decrement: `progressed=true` unconditionally
resets the counter to 0 (`NoOpCommentCycleReset`); `progressed=false` increments it
(`NoOpCommentCycleIncremented`). There is no refund/decrement counterpart, deliberately —
unlike `ReviewCycles`, this counter never needs one, because it only ever counts a cycle
that produced *nothing*; a cycle that would qualify for #1045's refund (a reinvoke that
was dispatched while the gate was already clear, landing no commit) is, from this
counter's perspective, indistinguishable from any other no-progress cycle, and correctly
increments it. #1555 and #1045 are answering different questions: #1045 asks "did this
reinvoke change the review-gate verdict," this counter asks "did this comment-processing
cycle change *anything observable*." A junk-overview no-op that #1045 forgives for gate
purposes still correctly counts here — and should, since it is still zero forward
progress on the issue.

**Threshold.** `effectiveMaxNoOpCommentCycles()` applies zero-means-default: default **5**,
configurable via `--max-no-op-comment-cycles` / `FABRIK_MAX_NO_OP_COMMENT_CYCLES` /
`max_no_op_comment_cycles` in `config.yaml` — the same flag > env > config.yaml > default
precedence and plumbing shape as every other `Max*Cycles` config.

**Reset triggers are deliberately narrower than the old breaker's five.** No
`PRStateChanged` reset, no new `Store` observer. R2 enumerates exactly three conditions
(commit, issue-body update, completion); a fourth would be scope creep the issue didn't
ask for, and would require solving a problem the old item-scoped breaker never had to
solve — an observer reacting to `PRStateChanged` has no way to know *which stage's*
per-stage counter to reset. If a future incident shows this is too narrow, that is a
follow-up, not a blocking gap here — the counter is a safety net, not the primary defense
(R3 is).

**R5 — ordering, not exclusion, resolves double-counting.** At each of the five call
sites, `checkNoOpCommentCycle` is evaluated *first*; only when it returns `false` (not
tripped) does `checkCommentBreaker` also run for that cycle:

```go
if !e.checkNoOpCommentCycle(item, stage, progressed, lastCommentAuthor(comments)) {
    e.checkCommentBreaker(item, reason)
}
```

Both counters still **record** every invocation independently, regardless of which one
(if either) trips — `NoOpCommentCycles` increments even when the old breaker is the one
that eventually pauses the issue (confirmed by
`TestNoOpCommentBreaker_OldBreakerStillTripsOnSetupFailure`). What's mutually exclusive is
the **pause escalation**: at most one `fabrik:paused` application and one trip comment per
cycle. The two breakers remain conceptually and textually distinguishable — old breaker's
trip comment opens `🏭 **Fabrik — comment-processing circuit breaker tripped**`; new
breaker's opens `🏭 **Fabrik — no-op comment-processing circuit breaker tripped**` — so an
operator reading the pause comment always knows which mechanism fired.

**Manual unpause resets both.** `EngineCyclesCleared`'s `store.go` apply case gains
`delete(item.StageState.NoOpCommentCycles, v.StageName)` alongside its existing deletes
for `ReviewCycles`/`ReviewBlockedCycles`/`CIFixCycles`/`RebaseCycles`/`EnqueueCycles` —
`clearFailedStage` (`engine/item.go`) already applies `EngineCyclesCleared` on every
manual-unpause path, so no new call site was needed. `copyStageState`
(`internal/itemstate/snapshot.go`) deep-copies the new map, matching every other
`StageState` map field.

**Trip action.** `tripNoOpCommentCycleBreaker` reuses `pauseIssue`/`pauseOpts`
(`fabrik:paused` + `fabrik:awaiting-input`, ADR-069's honorable-pause guarantee) exactly
like the old breaker, `pauseForReviewCycleLimit`, and every other cycle-limit escalation —
no new label, no new resume mechanism. The pause comment names the stage, the consecutive
no-progress count, and the last comment's author (R4).

### R3 — a durable, GitHub-sourced dedup marker for review bodies

`snap.CommentProcessed(reviewBodyCommentID(review))` remains the fast path — nothing
changes for the steady-state case where the store hasn't just been wiped. What's new is a
**second**, durable signal consulted only for candidates the in-memory record doesn't
already know about:

Every review-feedback PR comment `formatReviewFeedbackComment` produces already lists
which review bodies it addressed (`buildThreadEntries`, `addressedReviewIDsFromComments`);
the fix turns that existing artifact into a durable dedup record by appending a
machine-readable trailer:

```
<!-- fabrik:review-ids-addressed: 123,456 -->
```

`buildReviewBodyCommentsFromReviews` (`engine/reviews.go`) now:

1. Filters `reviews` down to **candidates** — the same skip conditions as before
   (`DatabaseID == 0`, `DISMISSED`/`PENDING`, empty body), *and* not already
   `snap.CommentProcessed`.
2. If there are zero candidates, returns immediately — no fetch, matching steady-state
   behavior exactly.
3. Otherwise calls `durablyAddressedReviewIDs(item)`, which resolves the linked PR (from
   `item.LinkedPRNumber`, falling back to `FetchLinkedPR` when zero — the same fallback
   `resolveReviewsForFeedback` already uses for `base:<branch>` items) and fetches its
   comments via the new `GitHubClient.FetchIssueComments` method, scanning each for the
   marker (`parseReviewIDsAddressedMarker`).
4. A candidate whose `DatabaseID` appears in the durable set is treated as already
   addressed — **and backfills** `snap.CommentProcessed` via a fresh `CommentProcessed`
   mutation, so every subsequent call in this process takes the fast, no-fetch path again
   without needing to re-fetch or re-parse.

**Cost.** At steady state (nothing outstanding, the common case), this adds zero API
calls — the candidate-count gate short-circuits before any fetch. Immediately after a
restart, it costs exactly one `FetchIssueComments` call per PR carrying an outstanding
review body — not per poll, since the very next call finds the backfilled in-memory
record and takes the fast path.

**Not part of `boardcache.ReadClient`.** `FetchIssueComments` was added directly to
`engine.GitHubClient`, not the 9-method cached-read subset `ReadClient`/`CacheImpl`
implement. It's a one-shot, lazily-triggered lookup keyed on a rare event (restart +
outstanding review body), not a per-poll cache-worthy read — it doesn't fit the caching
contract `ReadClient` exists for.

**Accepted residual gap.** The durable marker only exists once a review-feedback PR
comment is actually *posted*. A cycle that neither errors nor produces any PR output
(`output == ""` — e.g. every candidate review filtered out upstream by the stage's own
prompt logic) never emits the marker, so it still depends on the in-memory record alone
for that narrower case. This is deliberately not solved here — R1's `NoOpCommentCycles`
counter is the safety net for it, and a broader `itemstate.Store` persistence mechanism
(covering every counter, not just this one) is out of scope for this issue's stated
boundaries (see Alternatives Considered).

## Alternatives Considered

**Persist `itemstate.Store` to disk and reload at startup**, closing the restart-wipe gap
for every counter at once, not just review-body dedup. Correct in principle, and named
explicitly in Research as candidate (a). Rejected for this issue: a serialization format,
load-on-startup path, and migration story is a substantially larger change than the
narrow redelivery bug this issue targets, and every sibling counter
(`ReviewCycles`/`CIFixCycles`/`RebaseCycles`/`CommentBreaker`) already accepts the same
restart-vulnerability as a known, documented systemic property. Fixing it for all of them
at once is a natural follow-up, not a prerequisite for closing #1254's specific defect.

**A much-larger or unwindowed version of `CommentBreaker` itself**, rather than a second,
differently-shaped counter. Rejected: `CommentBreaker` is item-scoped by ADR-1089's
deliberate design (a stage transition mid-window must not silently reset the count), while
R1 explicitly asks for a **per-stage** counter. Reusing the timestamp-list shape would also
forgo `EngineCyclesCleared`'s free reset-on-unpause wiring that the `map[string]int` shape
gets automatically.

**Let both breakers fire independently every cycle**, rather than short-circuiting the old
one when the new one trips. Rejected: would post two pause comments in the pathological
case where both thresholds are reached on the same cycle, and would complicate "did this
trip the old or new mechanism" reasoning for an operator reading the issue, for no
offsetting benefit.

**Narrow the marker to a single addressed ID per comment**, rather than a comma-joined
list. Rejected: a single review-reinvoke batch routinely mixes several review bodies
(e.g. multiple bot reviewers, or several distinct `COMMENTED` submissions), and
`formatReviewFeedbackComment` already aggregates an entire batch into one comment — a
single-ID marker would require either dropping information or posting redundant comments.

## Consequences

**Positive:**
- A comment-processing loop that produces nothing is now bounded regardless of whether
  each individual invocation exits successfully or how sparsely its invocations are
  spaced — closing the exact gap #1382 left open by construction (success-keyed) and
  #1089 left open by construction (windowed).
- The specific #1254/#1253 redelivery is fixed at its root: an already-addressed review
  body is no longer redelivered after a restart once a review-feedback comment carrying
  the marker exists.
- No new label; `fabrik:paused`/`fabrik:awaiting-input` via the existing `pauseIssue`
  helper keeps ADR-069's honorable-pause guarantee for free.
- Both counters remain independently inspectable and distinguishable in their trip
  comments — an operator never has to guess which mechanism fired.

**Negative / Trade-offs:**
- **`NoOpCommentCycles` still inherits the restart-wipe vulnerability** every existing
  cycle counter has — it is in-memory only, like `ReviewCycles`/`CommentBreaker`/etc. If a
  future redelivery loop's cadence happens to be faster than the counter can accumulate
  before a restart clears it, the counter alone would not fully prevent a recurrence of
  *this exact incident shape*; R3's fix (which removes the redelivery trigger itself,
  rather than merely capping its damage) is what actually prevents that recurrence. This
  is why R3, not R1, is called the actual fix in the issue's own framing — R1 is
  explicitly the safety net.
- **The durable marker's accepted residual gap** (no PR comment posted this cycle) means
  R3 is not a complete idempotency guarantee on its own either — it is a durable
  *fallback*, not a durable *replacement*, for the in-memory record. R1 covers the
  narrower remaining case.
- **One additional live `FetchIssueComments` call** whenever a poll has at least one
  candidate review body not already known-processed in memory — bounded by "an
  outstanding review actually exists," which is inherently bursty rather than per-poll,
  but not literally zero-cost the instant a genuinely new review arrives (only
  post-restart redeliveries are the zero-marginal-cost case once backfilled).
- **`NoOpCommentCycles` can now reach its default threshold (5) before `MaxReviewCycles`
  does, on the exact review-reinvoke path ADR-1518/#1045 deliberately built to *tolerate*
  many consecutive no-op rounds via `ReviewCycleDecremented`'s refund.** Both counters
  record the same cycle, but only `NoOpCommentCycles` never forgives — a bot reviewer
  that submits five consecutive no-finding `COMMENTED` overviews before a sixth genuine
  finding lands now pauses on round five by default, even though `ReviewCycles` alone
  would have tolerated an arbitrarily long run of these. This is intentional (see
  Decision, "Increment vs. reset": a cycle #1045 forgives for gate purposes is still zero
  forward progress from this counter's perspective, and should count), but it does narrow
  ADR-1518's "forgive forever" guarantee in practice to "forgive up to
  `MaxNoOpCommentCycles` consecutive rounds." `TestHandleReviewGate_FiveNoOpReinvokes_DoNotExhaustBudget_SixthGenuineFindingAddressed`
  and `TestHandleReviewGate_BlockedNoOpReinvokes_ReachCycleLimitViaReviewBlockedCycles`
  (`engine/review_phase_test.go`) now set `MaxNoOpCommentCycles` explicitly high to keep
  isolating the `ReviewCycles`/`ReviewBlockedCycles` mechanism they each target. An
  operator whose review bots genuinely need more than 5 consecutive no-op rounds to
  converge should raise `--max-no-op-comment-cycles` accordingly.

## Related Work

- #1382 (PR #1425, commits #1413/#1414) — the failure-keyed comment-processing breaker
  this issue's counter is the explicit success-agnostic sibling of.
- ADR-1089 — the existing windowed comment-processing breaker this issue's counter runs
  alongside, never replaces.
- ADR-1518 — the direct architectural precedent for an additive, never-refunded counter
  compared independently of a refundable one, in the sibling review-gate path.
- ADR-1045 — origin of `reviewBodyCommentID`/`buildReviewBodyCommentsFromReviews` and the
  `ReviewCycleDecremented` no-op refund this issue's counter is deliberately orthogonal
  to (see Decision, "Increment vs. reset").
- ADR-1460 — the resumable-pause / `EngineCyclesCleared` convention this issue's manual
  unpause reset follows.

**References:** [docs/state-machine.md §4.6](../docs/state-machine.md), [docs/state-machine.md §6](../docs/state-machine.md)
