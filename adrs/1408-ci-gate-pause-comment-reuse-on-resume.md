# ADR 1408: CI-Gate Pause Comment Reuse on Resume

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1408 — an item that once hit the CI wait timeout could never be resumed by the remedy its own pause comment instructs

## Context

`settleAwaitingCIScan`'s unconditional `CIWaitTimeout` backstop (ADR-1270, issue #1303) exists to
bound the whole class of silent claims that could make `checkCIGate`'s own internal timeout guard
unreachable: once `fabrik:awaiting-ci`'s applied-at timestamp exceeds `ciWaitTimeout()`, the backstop
calls `pauseForCITimeout` and `continue`s, skipping the rest of the handler chain for that poll.
`pauseForCITimeout`/`pauseForCIFixCycleLimit` (`engine/ci.go`) both guard on `hasCIGatePauseComment`
— an unscoped scan of the issue's entire comment history for the stable prose fragment each posts —
so a redundant call within the same poll (the two-call label-swap race described in ADR-1270) does
not post a duplicate comment.

Field evidence (`verveguy/liminis-context-graph#342`, 2026-08-04) showed this combination makes an
item that ever hits the timeout **permanently unresumable**. The linked PR (#344) was
`CLEAN`/`APPROVED`/`MERGEABLE` with every check green since 13:25:44Z. The issue sat stranded from
20:00:57Z onward — carrying `fabrik:awaiting-ci`, no `stage:Validate:complete`, no `fabrik:paused` —
for at least 4h23m. A human had removed `fabrik:paused`, exactly as the pause comment (posted once,
at 13:21:56Z) instructs, to resume the item.

### Root cause

`fabrik:awaiting-ci`'s applied-at timestamp is set once, at `handleStageComplete` time, and is never
reset for the label's lifetime (ADR-1314) — neither pause function removes or reapplies
`fabrik:awaiting-ci` itself; only `checkCIGate`'s classify helpers do, on a confirmed gate-clear or a
confirmed timeout. So once an item ages past `CIWaitTimeout` once, every subsequent
`FetchLabelAppliedAt`-based check still reads it as timed out, regardless of live CI state, until the
label is genuinely removed or reapplied.

Before this fix, `pauseForCITimeout`/`pauseForCIFixCycleLimit` treated any `hasCIGatePauseComment`
match as "nothing left to do" — they returned immediately, touching no labels and reporting nothing.
The backstop's `continue` treated its own call into either function as unconditionally meaning "this
item was just escalated," regardless of whether the call actually posted anything. A human resuming
the item by removing only `fabrik:paused` — the pause comment's own, correct instruction — never
touches the comment or `fabrik:awaiting-ci`. So the very next settle pass re-derived "timed out" from
the same stale timestamp, matched the same old comment, no-op'd, and `continue`d past the handler
chain — forever. `stage:Validate:complete` was never applied and `fabrik:awaiting-ci` was never
cleared, even though live CI on the PR was green the entire time.

### Why the no-op itself was not the bug

Suppressing a duplicate pause comment within a single timeout/cycle-limit *episode* is correct and
must remain — it is exactly what protects the two-call label-swap race
(`TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment`) from posting twice
inside one poll. The defect was conflating that suppression, at the backstop's call site, with
"escalated." Both a genuine same-poll duplicate and a resumed item with a stale comment hit the
identical `hasCIGatePauseComment == true` branch; only the caller's context distinguishes them, and
before this fix nothing did.

## Decision

Split the fix by whether a live CI read has already happened this call — two coordinated pieces,
neither sufficient alone (see "Rejected designs" below).

### 1. The backstop decides for itself, before ever calling `pauseForCITimeout` (`engine/ci_settle.go`)

`settleAwaitingCIScan`'s `CIWaitTimeout` backstop now evaluates `hasCIGatePauseComment(item, stage)`
itself, ahead of the pause call:

- **No comment yet** (a genuinely fresh timeout): call `pauseForCITimeout` and `continue`, exactly as
  before this fix.
- **A comment already exists** for this episode: do **not** call `pauseForCITimeout`, and do **not**
  `continue`. Log that the item is deferred to the live-data-informed handler chain, and fall through
  to `RefreshCheckRunsLive` and `catchUpPhase1Handlers` unchanged.

This is what lets a resumed item with green CI clear the gate cleanly: `checkCIGate`'s
`PRMergeNoPR`/`PRMergeTerminal`/`PRMergeReady` cases short-circuit to `addCompleteLabelAndRemoveCI`
before any timeout check runs at all, so no pause-label churn happens for a green PR.

### 2. The pause functions stop no-opping when a comment already exists (`engine/ci.go`)

`pauseForCITimeout`/`pauseForCIFixCycleLimit` both now return `escalated bool`. When
`hasCIGatePauseComment` matches:

- Reapply `fabrik:paused` + `fabrik:awaiting-input` via a small `reapplyCIGatePauseLabels` helper
  (two idempotent `applyLabelAdd` calls — GitHub's add-label endpoint no-ops harmlessly if a label is
  somehow already present).
- Do **not** post a new comment — the existing episode's comment is reused.
- Return `false` (not a fresh escalation).

When no comment exists, behavior is unchanged: post via `pauseIssue`, return `true`.

Every caller reaching the reused-comment branch has, by construction, already done a live CI read
this poll: `handleMergeAndCIGates` (`engine/catch_up_handlers.go`) only calls either function after
`checkCIGate`/`classifyCIFrom*` has re-derived `ciTimedOut`/`ciFailure` (which still requires a live
settle result) for a still-blocked item. This is exactly what makes it safe to reapply
`fabrik:paused` unconditionally here but nowhere upstream of a live check — see "Why the split is
necessary" below. `handleMergeAndCIGates` itself needed **zero changes**: its existing unconditional
calls into both pause functions already do the right thing once the functions stopped no-opping.

### 3. Corrected wording (R6)

`hasCIGatePauseComment`'s doc comment and both pause functions' log lines previously said "already
posted this poll — skipping duplicate." This was always slightly wrong (the match is unscoped by
time, not scoped to a poll) and became actively misleading once the reused-comment path does
something (reapplies labels) rather than truly skipping. Both are reworded to describe the unscoped,
full-episode match and the reapply-without-repost behavior it now enables.

## Rationale

### Why the split is necessary — two rejected single-piece designs

**Rejected: "just don't `continue` when suppressed" (fall through unconditionally, no change to the
pause functions).** Satisfies R1 (green CI clears via `checkCIGate`'s early-return cases, before any
timeout logic runs) but fails R4: for still-failing CI, the handler chain reaches
`classifyCIFromCheckRuns` (or a sibling), which runs the *identical* stale-`appliedAt` check,
concludes "timed out" again, removes `fabrik:awaiting-ci`, and calls back into `pauseForCITimeout` —
which, under this design alone, still no-ops on the same old comment. Net effect: `fabrik:awaiting-ci`
disappears (so the item becomes dispatch-eligible again via `itemNeedsWork`) but `fabrik:paused` is
never reapplied — the item is silently redispatched as a full fresh stage invocation instead of
re-escalated to a human. That directly violates R4's language and the Acceptance criterion's
"re-escalated, not silently stranded."

**Rejected: "make the pause functions always reapply labels when suppressed," applied uniformly at
every call site including the backstop's own.** If the *backstop* itself reapplies `fabrik:paused`
the moment it observes a stale `appliedAt` and an existing comment, it does so **before any live CI
read has happened this poll**. A resumed item whose CI actually went green in the meantime would be
re-paused with a stale "timed out" comment instead of advancing — directly failing R1 and the
Acceptance criterion's first bullet.

**Accepted: split by call-time context.** The backstop (pre-live-check) only ever *skips* calling the
pause functions when suppressed — it never itself performs the "reapply" side effect. The pause
functions (reached, in the suppressed case, only from a post-live-check caller) perform the reapply.
Neither piece alone satisfies both R1 and R4; together they do, because each side effect only ever
fires in the context where it's provably safe.

### Why reuse the comment rather than post a fresh one (R4)

`hasCIGatePauseComment`'s unscoped, full-history match is exactly what makes reuse possible without a
new time-scoping mechanism. Two alternatives were considered and rejected:

- **A per-poll dedup set.** Does not survive
  `TestSettleAwaitingCIScan_RaceWithMainLoop_CycleLimitPause_NoDuplicateComment`, which calls
  `settleAwaitingCIScan` twice as two separate top-level calls (standing in for "main loop pass, then
  dedicated scan pass," per that test's own doc comment) — a set scoped to one invocation would not
  catch that race.
- **Comment-recency matching** (e.g. "only treat a match as same-episode if posted within the last N
  minutes"). Requires realistic `CreatedAt` stamping in test mocks that do not currently provide it,
  and an arbitrary "how recent counts as this-poll" threshold with no natural value.

The fix repurposes the existing, already-correct-for-its-original-purpose check instead of adding a
new one. `hasCIGatePauseComment`'s matching logic itself is unchanged by this issue.

### R5 — `pauseForCIFixCycleLimit` gets the identical fix

Unlike `pauseForCITimeout`, `pauseForCIFixCycleLimit` has two call sites — `handleMergeAndCIGates`'s
cycle-limit callback (`engine/catch_up_handlers.go`) and `checkAutoMergeConvergence`'s queue-repo
ejection-recovery path on `PRMergeBlocked` (`engine/merge_gate.go:465`) — and both are always reached
only after a live CI classification this poll (`ciFailure` in the first case, `settle.Status ==
PRMergeBlocked` in the second). Neither is a pre-live-check caller, so neither needs a caller-side
change; the identical internal reapply-without-repost fix covers both. `autoMergeConvergence` and
`mergeAndCIGates` are mutually-exclusive handlers in `catchUpPhase1Handlers`, so the two call sites
can't double-fire for the same item in the same poll.

The strand is independently reachable, not hypothetical: `clearFailedStage()` only resets
`CIFixCycles` when `stage:<X>:failed` is present or `snap.PausedByEngine(stageName)` is true — a
CI-fix-cycle-limit pause sets neither (`pauseIssue` never applies `itemstate.EnginePaused`). So
`CIFixCycles` survives a resume unreset, and a human resuming an item paused at the cycle limit
*before* `CIWaitTimeout` has separately elapsed lands back at `cycleCount >= maxCycles` with the old
comment already present — hitting this exact defect shape independent of the backstop entirely.

### Why no caller needed changes

`handleMergeAndCIGates`'s two calls into the pause functions (`ciTimedOut` branch, cycle-limit-reached
callback) and `checkAutoMergeConvergence`'s single call (`PRMergeBlocked` cycle-limit branch) are all
unconditional statements that ignore the return value. They already do the right thing — post fresh
on a genuine new episode, reapply-without-repost on a resumed one — purely because the functions they
call now do the right thing internally. No caller-side branching was needed or added at any of the
three sites.

## Consequences

**Positive:**
- An item resumed after hitting the CI wait timeout is genuinely reprocessed: advanced when CI is
  green (R1), re-escalated when it is not (R4), and the same holds for the CI-fix cycle limit (R5).
- Both pause functions now report which outcome occurred (`escalated bool`, R2) rather than silently
  doing nothing distinguishable from "posted."
- The two-call label-swap race this mechanism was originally built for
  (ADR-1270) is unaffected — its regression test passes unmodified, along with both existing
  `CIWaitTimeout` backstop tests (R3, R7).
- No change to `checkCIGate`'s return-tuple semantics, `ciTerminated` claim ordering (ADR-1223), or
  dispatch admission (`engine/item.go`, `engine/poll.go`) — all confirmed unaffected and out of scope.

**Negative / Trade-offs:**
- A resumed item whose CI is merely still pending past the deadline (not confirmed-failed) is still
  re-paused, identically to a confirmed failure — `classifyCIFromCheckRuns`'s own independent
  `appliedAt` check does not distinguish the two once the deadline has passed. This is intentional
  per R4's own wording ("still failing **or** still pending past the deadline") and required no new
  code; flagged here so it is not mistaken for a gap in a later review.
- `hasCIGatePauseComment` still cross-matches both comment types (`timeoutWant OR cycleWant`,
  regardless of which function is calling) — pre-existing, unaffected by and out of scope for this
  fix. Once *either* type of pause comment exists, both functions treat themselves as "already
  escalated" for dedup purposes.

## Sibling Audit

This is not a new instance of the "dedicated `board.Items`-sourced settle scan" pattern
(ADR-060/061/062/1097/1270/1387) — `settleAwaitingCIScan` already is that scan for
`fabrik:awaiting-ci`. This issue is instead a correctness fix *within* that scan's own escalation
path: the scan's admission was already correct (ADR-1270); its downstream handler-chain reachability
was already correct (ADR-1303, §6.14.1); this fix corrects a divergent-outcome bug in the escalation
call itself, matching the "zero remaining owners" shape [ADR-1387](1387-closed-items-never-dispatched.md)
names for a structurally different mechanism (closed-item dispatch), applied here to a resumed item's
pause/re-escalation call instead.

**References:** [ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md) (the backstop and
`hasCIGatePauseComment`'s original two-call-race rationale), issue #1303 / #1325 (why the unconditional
backstop exists — `docs/state-machine.md` §6.14.1/§6.14.3), [ADR-1387: Closed Items Are Never
Dispatched](1387-closed-items-never-dispatched.md) (structural precedent for framing a "zero remaining
owners" defect), `docs/state-machine.md` §6.14.4, `verveguy/liminis-context-graph#342` (field evidence)
