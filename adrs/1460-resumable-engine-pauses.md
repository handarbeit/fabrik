# ADR 1460: Resumable Engine Pauses — a Reset Trigger for Cycle-Limit Counters

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1460 — every pause comment Fabrik posts says "remove `fabrik:paused` to resume," but for
four cycle-limit pause sites this was false: the triggering counter was never reset, so the item
re-paused within seconds of the label being removed.

## Context

Every `pauseIssue` call site tells the operator, in the pause comment itself, to remove
`fabrik:paused` to resume. For genuine stage failures (`escalateFailedStage`, `escalatePRCreationFailure`,
`handleBoundaryViolation`, `escalateSettle`) that promise has always been honest: each of these applies
`itemstate.EnginePaused`, which routes the item through `processItem`'s unpause gate
(`engine/item.go`):

```go
wasPaused = snap.PausedByEngine(stage.Name)   // requires itemstate.EnginePaused to have been Applied
if wasPaused || hasFailedLabel {              // hasFailedLabel requires stage:<name>:failed
    e.clearFailedStage(item, stage)
}
```

`clearFailedStage` is the sole caller of `EngineCyclesCleared`, the only mutation that zeroes
`ReviewCycles`/`CIFixCycles`/`RebaseCycles`/`EnqueueCycles` for a stage. Four sites —
`pauseForReviewCycleLimit`, `pauseForCIFixCycleLimit`, `pauseForRebaseCycleLimit`,
`pauseForEnqueueCycleLimit` — pause on exactly this shape (`cycleCount >= maxCycles`) but never applied
`itemstate.EnginePaused`, so `wasPaused` was never true for them and `clearFailedStage` never fired.
Because nothing else zeroes these counters, `cycleCount >= maxCycles` remained true forever once
tripped: the very next time the item was evaluated after a human removed `fabrik:paused`, it
re-evaluated the identical `>=` check and re-paused — reproduced live on #1208, five seconds from
unpause to re-pause, byte-identical repost comment.

PR #1445 (issue #1408, merged a few hours before this issue was filed) had already fixed the
comment-repost half of this shape for `pauseForCITimeout`/`pauseForCIFixCycleLimit`
([ADR-1408](1408-ci-gate-pause-comment-reuse-on-resume.md)) — `hasCIGatePauseComment` +
`reapplyCIGatePauseLabels` stop a re-pause from spamming a duplicate comment. It deliberately did not
touch counter reset (out of scope for #1408); `pauseForCIFixCycleLimit` remained a one-way door after
that merge, confirmed by reading the current code before this issue's Specify stage ran.

### The suggested fix's premise was wrong

The issue body's own suggested R2 approach — "apply `itemstate.EnginePaused`, making `wasPaused` true
so `clearFailedStage` fires on unpause" via `processItem`'s existing gate — assumed that gate is
reachable for these four sites. It is not. All four pauses fire from the Phase 1 catch-up handler
chain (`catchUpPhase1Handlers`, `engine/catch_up_handlers.go` — `handleReviewGate` for review,
`handleMergeAndCIGates`/`handleAutoMergeConvergence` for CI-fix/rebase/enqueue), which only runs for
items that are **already stage-complete** (or carrying `fabrik:awaiting-ci`) with no new comment.
`itemNeedsWork` (`engine/item.go`) returns `false` for such an item — "already completed this stage,"
no new comment — so `processItem` is never dispatched for it after a label-only unpause.
`processItem`'s own unpause gate is structurally unreachable for these four sites.

The actual re-evaluation point is the catch-up loop's admission in `poll.go` and the identical loop
`settleAwaitingCIScan` runs in `ci_settle.go` — both skip an item outright while `fabrik:paused` is
present, then re-run the same `catchUpPhase1Handlers` chain the moment it's removed. Before this fix,
that chain contained no reset logic at all — which is exactly what #1208's live repro shows: no
comment was ever involved in the resume attempt, only a label removal, and the item was re-evaluated
by the catch-up loop, not by `processItem`.

## Decision

### R2 — apply `itemstate.EnginePaused`, but add a second, additive reset trigger

Reuse `itemstate.EnginePaused` directly (not a neutral marker — see "Alternatives Considered"). Each
of the four confirmed sites now applies it in **both** branches (fresh pause and the R4
reapply-existing-comment branch — see below for why both matter).

That alone is insufficient, per the reachability finding above. The actual fix is a new Phase 1
handler, `handleEngineUnpause`, **prepended first** in `catchUpPhase1Handlers`:

```go
func (e *Engine) handleEngineUnpause(pctx *phase1Ctx) bool {
	repoStr := itemOwnerRepoString(pctx.item, e.defaultRepo())
	snap, err := e.store.Get(repoStr, pctx.item.Number)
	if err != nil || !snap.PausedByEngine(pctx.stage.Name) {
		return false
	}
	e.clearFailedStage(pctx.item, pctx.stage)
	return false
}
```

It reuses `clearFailedStage` as-is — no new reset implementation, keeping one reset path for both the
pre-completion (`processItem`) and post-completion (`catchUpPhase1Handlers`) resume cases.
`PausedByEngine` being true here means: the item reached this handler chain (so `fabrik:paused` is
currently absent, per both entry points' admission filters) while still carrying a stale
`EnginePaused` record from before the label was removed — precisely "a human unpaused a cycle-limit
pause." It always returns `false` (never claims), so the rest of the chain evaluates the just-reset
counters in the *same* pass — a genuine resume converges in one poll, not two, satisfying AC2
immediately.

Placement is load-bearing: `handleEngineUnpause` must run **unconditionally, before any handler that
might claim/short-circuit the item**, so the reset always lands before that pass's own cycle-limit
checks read the (freshly zeroed) counters. `escalateFailedStage`/`escalatePRCreationFailure`/
`handleBoundaryViolation`-paused items are never stage-complete when paused, so they never reach the
catch-up loop at all — `processItem`'s gate remains their sole, unchanged resume path (AC5).
`escalateSettle` is untouched: out of scope, and it uses a synthetic `retryStage` string that doesn't
intersect with either gate.

**Why EnginePaused is applied on the reapply branch too, not just the fresh-pause branch:** a resume
clears `PausedByEngine` via `handleEngineUnpause`. If the underlying condition recurs (CI keeps
failing, the conflict keeps recurring, the queue keeps thrashing) and the counter climbs back to the
limit a *second* time, the pause function runs again — but `hasPauseComment` still matches the old
comment, so it takes the reapply branch. If that branch didn't re-apply `EnginePaused`, the *next*
resume attempt would be a no-op again. Both branches must re-arm it uniformly.

### The `Attempts`-reset side effect (accepted, not a defect)

`clearFailedStage`'s full reset sequence — remove `stage:<name>:failed` (harmless no-op; these four
sites never apply it) → `StageRetryCleared` → `EngineUnpaused` → `StageLastAttemptCleared` →
`EngineCyclesCleared` → `resetCommentBreaker` — also zeroes `Attempts(stage.Name)` via
`StageRetryCleared`, the counter `MaxRetries`/`escalateFailedStage` uses. A stage can accumulate
`Attempts` from genuine incomplete/errored invocations independently of its cycle counter. Unpausing a
review-cycle-limited (etc.) item therefore also gives that stage's `MaxRetries` budget a clean slate.

This is accepted as deliberate — "a human unpause is 'try again,' clean slate" — consistent with the
same reset already applying identically to `escalateFailedStage`/`escalatePRCreationFailure` (AC5's
own baseline) and to `pauseForSliceLimit` (#1199, landed on `main` ahead of this issue via a review
finding on PR #1447 — see "Relationship to `pauseForSliceLimit`" below). There is no "unchanged"
`Attempts` baseline to preserve for the four newly-fixed sites — they were broken before this fix — so
this is a net-new, intentional behavior, not a regression.

### R4 — reuse, generalize, and correctly scope PR #1445's no-repost pattern

`hasCIGatePauseComment`/`reapplyCIGatePauseLabels` (`engine/ci.go`) are generalized into
`engine/mutate.go` as `hasPauseComment(item, fragments...) bool` and `reapplyPauseLabels(item)` —
identical bodies, parameterized on the caller's own stable message fragment(s) instead of hardcoding
CI's two. `hasCIGatePauseComment` now delegates to the shared helper; no behavior change for the CI
case.

Each of the three newly-fixed sites gets its own fragment constant/function and reapply branch,
mirroring the CI pair exactly:

| Site | Stable fragment matched |
|---|---|
| `pauseForReviewCycleLimit` | `"The stage **%s** has been re-invoked to address PR review feedback"` |
| `pauseForRebaseCycleLimit` | `"The stage **%s** has been re-invoked to rebase onto the base branch"` |
| `pauseForEnqueueCycleLimit` | `"has been re-enqueued into GitHub's merge queue"` (deliberately omits the stage name — the original sentence puts it earlier: *"The linked PR for stage **%s** has been re-enqueued..."*) |
| `pauseForCIFixCycleLimit` | unchanged — `"The stage **%s** has been re-invoked to fix CI failures"` (from #1445) |

All four functions now return `(escalated bool)` — `true` when a fresh pause comment was posted,
`false` when an existing episode's pause was merely reapplied — matching `pauseForCITimeout`'s
existing shape from #1445. Every call site discards the return value as a bare statement (Go permits
this; no signature changes were needed at any of the six call sites across the four functions),
matching the existing precedent at `merge_gate.go` and `catch_up_handlers.go` for the CI case.

Given R2 zeroes the counter on every genuine resume, this reapply branch rarely fires post-fix for the
three newly-covered sites — once resumed, the counter reads 0, so `cycleCount >= maxCycles` is false
and the item just proceeds normally instead of re-pausing. Its remaining purpose is defense-in-depth:
the same same-poll double-call race [ADR-1408](1408-ci-gate-pause-comment-reuse-on-resume.md)
documents for CI, and the second-episode-after-a-successful-resume case described above.

**Inherited, unscoped-by-time caveat (unchanged, out of scope to fix):** `hasPauseComment`'s match
scans the issue's *entire* comment history for the fragment, not just "this episode." A stage that was
cycle-limit-paused once, resumed successfully, and later hits the same limit again for an unrelated
reason will match the old comment and silently relabel instead of posting a fresh one. This is
identical to `hasCIGatePauseComment`'s pre-existing, already-accepted behavior (documented in its own
comment) — the issue's scope explicitly asks to reuse the pattern, not fix this characteristic.

### `pauseForEnqueueCycleLimit`'s doc comment correction

Before this fix, the function's doc comment asserted: *"The EnqueueCycles counter is cleared by
`clearFailedStage` (`EngineCyclesCleared`) when the user unpauses."* This was never true — the
function never applied `itemstate.EnginePaused`, so `wasPaused` was never true for it via
`processItem`'s gate, and (before `handleEngineUnpause` existed) nothing else called
`clearFailedStage` for it either. The comment is corrected to describe the actual, now-true mechanism
(`handleEngineUnpause`).

## R3 — Classification of Every `pauseIssue`/Cycle-Limit Site

Every site identified in the issue's root-cause sweep, re-verified against the current codebase (not
assumed from the original report):

| Site | Classification | Mechanism |
|---|---|---|
| `escalateFailedStage` | Already correct | Applies `EnginePaused`; resumed via `processItem`'s gate (unreachable-by-catch-up-loop items only, since not yet stage-complete) |
| `escalatePRCreationFailure` | Already correct | Same as above |
| `handleBoundaryViolation` | Already correct | Same as above |
| `escalateSettle` | Already correct | Applies `EnginePaused` keyed on a synthetic `retryStage` string; out of scope, untouched |
| `pauseForSliceLimit` | Already correct (fixed on `main` ahead of this issue) | Applies `EnginePaused`; resumed via `processItem`'s gate — reachable for it because a slice-limit pause fires *mid-stage*, before `stage:<name>:complete`, unlike the four confirmed sites here. See "Relationship to `pauseForSliceLimit`" below. |
| **`pauseForReviewCycleLimit`** | **Confirmed defective → fixed (R2)** | `ReviewCycles`; now applies `EnginePaused`, resumed via `handleEngineUnpause` |
| **`pauseForCIFixCycleLimit`** | **Confirmed defective → fixed (R2)** | `CIFixCycles`; now applies `EnginePaused`, resumed via `handleEngineUnpause` (R4 no-repost already existed from #1445) |
| **`pauseForRebaseCycleLimit`** | **Confirmed defective → fixed (R2)** | `RebaseCycles`; now applies `EnginePaused`, resumed via `handleEngineUnpause` |
| **`pauseForEnqueueCycleLimit`** | **Confirmed defective → fixed (R2)** | `EnqueueCycles`; now applies `EnginePaused`, resumed via `handleEngineUnpause`; doc comment corrected |
| `pauseForReviewTimeout` | Independently resets | `checkAwaitingReviewTimeout` calls `removeAwaitingReviewLabel` (strips `fabrik:awaiting-review` + `fabrik:bot-reprompted`) *before* pausing — its own timing anchor is gone by the time the pause fires. No counter. |
| `pauseForCITimeout` | Condition-driven, no counter | Anchored to `fabrik:awaiting-ci`'s label-applied-at (external CI-completion state, not an attempt budget). Already has R4's reapply-without-repost from #1445. Re-pausing on resume while CI is genuinely still unresolved is correct, not a defect — there is no "give it another try" budget for an external wait condition. |
| `pauseForConvergenceFailed` | Independently resets | `pauseOpts{removeAutoMerge: true}` strips `fabrik:auto-merge-enabled`, its own anchor label (`ConvergenceBudget` is measured from that label's applied-at). Resume naturally starts a fresh budget once `attemptMergeOnValidate` reapplies the label. |
| `pauseForMergeGroupStall` | Independently resets | Same `removeAutoMerge: true` mechanism, explicitly documented in its own doc comment as resetting the convergence flow so the stall check doesn't immediately re-fire on resume. |
| `pauseForPRClosedNotMerged` | Condition-driven, no counter | Removes all three gate labels (`fabrik:awaiting-ci`, `fabrik:awaiting-review`, `fabrik:rebase-needed`) itself; resuming requires a GitHub-side state change (reopen/replace the PR), not a counter reset. |
| `pauseForRequiredNeverRunningCheck` | Condition-driven, no counter | Removes `fabrik:awaiting-ci`; resuming re-derives check-run presence live from GitHub on every call. |
| `handleBrokenReviewLinkage` | Condition-driven, no counter | Re-fetches `FetchLinkedPR`/`FetchPRClosingIssues` live on every call (`item.LinkedPRNumber != 0` short-circuits at the top when linkage is already fine); resolves itself once a human fixes the PR body/branch. No counter, no anchor. |
| `checkAutoMergeConvergence`'s direct pause ("auto-merge disabled by user") | Independently resets | Also uses `pauseOpts{removeAutoMerge: true}` — same mechanism as `pauseForConvergenceFailed`/`pauseForMergeGroupStall`. |
| `pauseForBrokenLinkage` | Condition-driven, no counter | No cycle counter; resolves once the PR is created/relinked. |
| `tripCommentBreaker` | Independently resets | The comment-breaker counter is windowed (time-based), so it self-expires — not gated by `fabrik:paused` removal at all. |
| `checkDependencies` | Independently resets | Cleared when the blocking issues close; no counter, live-derived every call. |
| `ensureRepoReady`, `postSpawnCloneError`, `spawnChildren` | Condition-driven, no counter | Each resolves once the underlying external condition (clone succeeds, spawn retried) changes; no stuck counter. |

None of the 8 originally-unclassified sites needed R2's treatment. Each was re-verified by reading
its current implementation (not taken on faith from Research's read) — every one either strips its own
timing anchor before or as part of pausing, or has no counter/anchor at all and re-derives its
condition live on every call.

## Relationship to `pauseForSliceLimit` (#1199, R5)

#1199/PR #1447 (`pauseForSliceLimit`, the turn-cap/slice-budget pause) landed on `main` *ahead of* this
issue, via a review finding on that PR: *"a slice-limit pause was unrecoverable... `SliceRetries`
stayed pinned at `MaxSliceRetries`."* Its fix applies `itemstate.EnginePaused` — the same mechanism
this ADR formalizes — but reaches resumability differently: a slice-limit pause fires **mid-stage**,
before `stage:<name>:complete` is ever applied, so the item is *not* yet stage-complete when paused,
and `processItem`'s own existing gate remains reachable and sufficient for it (it never needs
`handleEngineUnpause`). This is the same distinction that keeps `escalateFailedStage` et al. off the
catch-up-loop path: whether a pause happens *before* or *after* stage completion determines which of
the two resume gates (`processItem`'s vs. `handleEngineUnpause`'s) actually reaches it.

`pauseForSliceLimit` independently adopted the `EnginePaused` mechanism this issue's R2 also uses,
which satisfies R5's coordination requirement retroactively — no further change to that function is
needed here. It is listed in the R3 table above as "already correct" for completeness.

## The Two Resume Gates

Post-fix, "remove `fabrik:paused` to resume" is honest for every reachable pause, via exactly one of
two paths depending on *when* the pause fires relative to stage completion:

1. **Pre-completion pauses** (stage not yet marked `stage:<name>:complete`) — `escalateFailedStage`,
   `escalatePRCreationFailure`, `handleBoundaryViolation`, `pauseForSliceLimit` — resume via
   `processItem`'s own `wasPaused || hasFailedLabel` gate (`engine/item.go`), unchanged by this issue.
2. **Post-completion pauses** (stage already complete, or in the `fabrik:awaiting-ci` window) — the
   four confirmed sites this issue fixes — resume via `handleEngineUnpause`, the new Phase 1 handler
   in `catchUpPhase1Handlers`.

`escalateSettle` is a structurally separate third case (settle-scan retry, synthetic stage key), left
untouched and out of scope.

## Restart Durability

A review comment on the implementing PR raised a plausible-sounding concern: `handleEngineUnpause`
detects a stale pause purely via `snap.PausedByEngine(stage.Name)`, which is in-memory-only and "does
not survive restart" per its own doc comment (`internal/itemstate/snapshot.go`) — so if the engine
restarts while an item is genuinely cycle-limit-paused, wouldn't that flag come back `false`, leaving
`handleEngineUnpause` unable to detect the stale pause, while the *durable* cycle counter stays pinned
at the limit? That would reproduce this issue's exact bug via a restart instead of a missing
`EnginePaused` apply.

The premise doesn't hold: `ReviewCycles`/`CIFixCycles`/`RebaseCycles`/`EnqueueCycles` are **not**
durable either — they live in the same in-memory `StageState` map structure as `PausedByEngine`, in
the same `itemstate.Store`, which `NewStore(nil)` (`engine/engine.go`) always constructs with empty
maps at process start; nothing in this codebase serializes or rehydrates `itemstate.Store` from any
prior process. There is no persistence layer for any of `StageState`'s fields — the "in-memory only,
does not survive restart" language on `PausedByEngine`'s and `PRCreationFailed`'s doc comments happens
to call this out explicitly, but it applies uniformly to every field in `StageState`, cycle counters
included; those two doc comments are not marking an exception.

Concretely: a restart wipes `PausedByEngine` *and* the triggering cycle counter for that stage
together, in the same moment (a fresh `ItemState` for that key, or the whole `items` map starting
empty). There is no interleaving in which one resets while the other survives — both are gone, or
neither is (trivially, since the process either restarted or it didn't). Tracing the actual sequence:
a restart before a human removes `fabrik:paused` leaves the label in place (durable, on GitHub) while
the in-memory counter and flag both silently zero; when the label is later removed, the item reaches
`handleEngineUnpause` with `PausedByEngine` already `false` — a no-op, exactly as the comment
predicts — but the cycle-limit check downstream reads the *also-already-zeroed* counter, so it doesn't
re-pause either. The net effect of a restart is indistinguishable from an early, free `handleEngineUnpause`
having already fired for every paused item — never the split-brain (flag lost, counter retained) the
comment described. This is unchanged by, and not specific to, this issue's fix: it is a pre-existing
property of every `EnginePaused`-gated site, including the four already-correct ones
(`escalateFailedStage`, `escalatePRCreationFailure`, `handleBoundaryViolation`) and `pauseForSliceLimit`
(#1199), none of which persist `Attempts`/`SliceRetries` across a restart either.

## Alternatives Considered

**A neutral marker instead of reusing `itemstate.EnginePaused`.** Would require a new `Mutation` type
(e.g. `EngineCounterPaused`), a second field in `StageState`, a second checked condition at both
resume gates, and a variant of `clearFailedStage` that skips `stage:<name>:failed` removal/
`StageRetryCleared`/`resetCommentBreaker`. This avoids the `Attempts`-reset side effect but is
materially more code across `itemstate` and `engine`, and introduces a second reset implementation to
keep in sync with the first. Rejected: the `Attempts`-reset side effect is accepted as intentional
(see above), and one reset path is easier to reason about and test than two.

**Relying solely on `processItem`'s existing gate**, as the issue body's own suggested R2 approach
assumed. Rejected because it is provably unreachable for all four confirmed sites — see "The
suggested fix's premise was wrong" above. This is the single most important correction this ADR
records versus the issue's own suggested implementation.

## Consequences

**Positive:**
- Every pause comment's "remove `fabrik:paused` to resume" instruction is now honest for all four
  previously-defective sites, closing the exact defect #1208 demonstrated live.
- One reset implementation (`clearFailedStage`) serves both resume gates — no duplicated reset logic.
- `handleEngineUnpause`'s placement and always-`false` return let a genuine resume converge in a
  single poll pass (reset, then re-evaluate with the fresh counter, in the same call).
- R4's no-repost pattern is now uniform across all four cycle-limit-pause domains (review, rebase,
  enqueue, CI-fix), sharing one implementation in `mutate.go` instead of four near-duplicates.
- `escalateFailedStage`/`escalatePRCreationFailure`/`handleBoundaryViolation`'s existing, correct
  behavior is provably unaffected — none of the three functions, nor `clearFailedStage`, nor
  `processItem`'s gate were modified; they are structurally unreachable by `handleEngineUnpause`
  (never stage-complete when paused).

**Negative / Trade-offs:**
- A manual unpause of a cycle-limit-paused stage also resets that stage's unrelated `Attempts`/
  `MaxRetries` counter (the accepted side effect described above) — a behavior change from "broken"
  (no reset at all) to "broader reset than the issue narrowly asked for," not a regression from any
  prior correct behavior.
- `hasPauseComment`'s fragment matching remains unscoped by time, inherited unchanged from
  `hasCIGatePauseComment` — a second, unrelated cycle-limit episode long after a successful resume
  will silently relabel instead of posting a fresh comment. Pre-existing, accepted, and explicitly out
  of scope to fix here.
- `handleEngineUnpause` adds one more unconditional handler to every Phase 1 catch-up pass (one
  `store.Get` per stage-complete-or-awaiting-ci item, per poll) — negligible cost, no additional
  GitHub API calls (store-only read).

## References

[ADR-1408: CI-Gate Pause Comment Reuse on Resume](1408-ci-gate-pause-comment-reuse-on-resume.md) (the
direct R4 precedent this issue generalizes), [ADR-1270: Awaiting-CI Settle Scan](1270-awaiting-ci-settle-scan.md)
(establishes the `catchUpPhase1Handlers`-shared-loop pattern `handleEngineUnpause` also runs inside),
[ADR-1199: Separate the slice-budget counter from the failure-retry counter](1199-slice-budget-separate-from-failure-counter.md)
(the sibling pause site that independently adopted `EnginePaused`, satisfying R5), issue #1208 (the
live reproduction that prompted this issue), `docs/state-machine.md` §6.2 and the "Resumable Engine
Pauses" section (as-built documentation of both resume gates).
