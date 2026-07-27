# ADR 1089: Comment-Processing Circuit Breaker

**Date**: 2026-07-26
**Status**: Accepted
**Issue**: #1089 — Runaway-loop fix (3/4): comment-processing circuit breaker

## Context

Incident #1083: a self-sustaining comment loop ran ~995 times over roughly $210 before a human `kill -9`'d the daemon. Fix 1/4 (#1087, ADR-069) closed the ability of a bot comment to silently lift an operator's `fabrik:paused`. Fix 2/4 (#1088, ADR-070) stopped known bot service-notice shapes from spawning a worker or drawing a reply in the first place. Both fixes are shape-specific: they close the two pumps the incident actually exercised. Neither one counts damage from a loop it doesn't recognize — a differently-worded bot notice, a webhook or integration that re-triggers on Fabrik's own reply through some other mechanism, or any other self-sustaining pattern not yet anticipated would still run unbounded, because nothing on the comment-processing path asks "have we made N invocations on this issue with nothing to show for it?"

## Decision

A per-issue comment-processing circuit breaker, tracked in the event-sourced `itemstate.Store` and enforced by the engine:

**Counter.** `ItemState.CommentBreaker` (`internal/itemstate/itemstate.go`) holds `InvocationsAt []time.Time` and `LastAuthor string`, driven by two mutations — `CommentBreakerInvocationRecorded{Repo, Number, At, Author, Cutoff}` (append, pruning `InvocationsAt` entries older than `Cutoff` first when `Cutoff` is non-zero) and `CommentBreakerReset{Repo, Number}` (clear; a no-op, no `Change` emitted, when already empty). It is scoped to the item as a whole, not per-stage, because the #1083 incident stayed on a single stage throughout the entire run, and a stage-keyed counter would let a legitimate stage transition mid-window quietly reset the count to a number lower than what actually happened. Window math (threshold N, window T) lives in the engine (`engine/comment_breaker.go`), not in `itemstate` — the Store stores raw timestamps only and prunes only against a caller-supplied `Cutoff`, mirroring the existing `mergeTrainTrials` runaway-guard precedent (ADR-059 D8) and keeping `itemstate` free of Fabrik-specific business thresholds. `recordCommentBreakerInvocation()` computes `Cutoff` from the current window on every call, so `InvocationsAt` is pruned at write time and doesn't grow without bound for an issue that receives invocations sparser than the window; `commentBreakerCount()` additionally prunes-and-counts on every read as a cheap belt-and-suspenders pass.

**Recording point.** `recordCommentBreakerInvocation()` is called inside `processComments` (`engine/comments.go`) immediately before Claude is actually invoked — after the `fabrik:editing` label and worktree setup, but before the extension loop calls `InvokeForComments()`. An early return before that point (e.g. an editing-label API failure) is not counted as a wasted cycle.

**Threshold and window.** Default **N = 10** invocations within **T = 30 minutes**, both zero-means-default: `engine.Config.MaxCommentCyclesPerWindow` / `CommentCycleWindow`, overridable via `--max-comment-cycles-per-window` / `--comment-cycle-window` flags, `FABRIK_MAX_COMMENT_CYCLES_PER_WINDOW` / `FABRIK_COMMENT_CYCLE_WINDOW` env vars, or `max_comment_cycles_per_window` / `comment_cycle_window` in `config.yaml` — the same flag > env > config.yaml > default precedence, and the same plumbing shape, as `MaxTrainTrialsPerWindow`/`TrainTrialWindowMinutes`.

**Reset triggers** (any one clears the counter before N is reached):

| Trigger | Hook point |
|---|---|
| `stage:*:complete` transition | `handleStageComplete` (`engine/stages.go`) — the single choke point reached on stage completion regardless of whether it originated from a plain stage run or from `finalizeComments`'s completed branch |
| New commit on the branch | Inside `processComments`, comparing `gitHeadSHA(workDir)` before/after the Claude invocation — detects the commit locally, without depending on a webhook-lagged PR head-SHA update |
| PR state change | `CommentBreakerObserver` (`engine/observers.go`), subscribed to the Store, reacting to the `PRStateChanged` flag |
| Issue body edited (`FABRIK_ISSUE_UPDATE`) | Inside `publishCommentOutput`'s existing `extractUpdatedBody(output) != ""` branch — the only forward-progress signal pre-PR stages (Specify/Research/Plan) produce |
| Manual human unpause | `clearFailedStage` (`engine/item.go`), alongside the existing `ReviewCycles`/`CIFixCycles`/`RebaseCycles`/`EnqueueCycles` clears |

**Trip action.** `checkCommentBreaker()` runs at the tail of `processComments` (on both the non-completing-error path and the normal-completion path, after any reset from this same cycle has already applied). Once `commentBreakerCount() >= N`, `tripCommentBreaker()` applies `fabrik:paused` + `fabrik:awaiting-input` through the existing `pauseIssue`/`pauseOpts` helper (ADR-065) — the same helper `pauseForReviewCycleLimit` and the merge-train runaway guard already use — posts an explanatory comment (invocation count, window, last comment author, resume instructions), and emits `tui.CommentBreakerTrippedEvent`.

## Rationale

### Why reuse `fabrik:paused` + `fabrik:awaiting-input` instead of a new label?

Routing the trip through `pauseIssue` gets ADR-069's honorable-pause guarantee — a subsequent bot comment cannot silently lift the pause — automatically and for free, with zero new label semantics to teach the rest of the engine or document in `CLAUDE.md`. A bespoke label would have to re-implement that guarantee (and every other consumer that already special-cases `fabrik:paused`/`fabrik:awaiting-input`) from scratch.

### Why a new `PRStateChanged` flag instead of reusing `LinkedPRChanged`?

The first draft of this design reused the existing broad `LinkedPRChanged` flag, reasoning that its "fires on any genuine field change" semantics were diff-based enough to avoid false trips, and that erring toward more resets (fewer false trips) was the correct trade-off for a last-resort backstop — the same trade-off ADR-070 made explicitly for bot-noise filtering. PR review of this issue caught that this reasoning had a fatal gap: `LinkedPRChanged` also fires on `PRReviewSubmitted`/`PRReviewCommentCreated`/`ReviewThreadCommentAdded` — a new PR review or inline review comment. That is the *same event* that supplies the comment `processComments` is about to process for a review-reinvoke. Keying the reset off `LinkedPRChanged` there resets the counter on the same cycle that records its own invocation, capping the observed count at 1 forever — silently defeating the breaker for exactly the PR-review-comment-loop shape it exists to catch.

The fix adds `PRStateChanged`, a narrower `ChangeFlags` sub-flag set only by `PRDetailsUpdated` (the mutation that carries `LinkedPR.State`/`Merged`/`Draft` — genuine PR-level transitions: merged, closed, draft↔ready). This still doesn't require injecting circuit-breaker-specific diffing into `itemstate.Store`'s core mutation switch — `PRDetailsUpdated` already exists as a distinct mutation type for exactly this purpose (populated from `boardcache/delta.go`'s `pull_request` webhook handling), so the fix is additive: one more `ChangeFlags` bit returned alongside `LinkedPRChanged` at that one call site, not new business logic. `PRReviewSubmitted`/`PRReviewCommentCreated`/`ReviewThreadCommentAdded` continue to set `LinkedPRChanged` only, so other existing consumers (`wakeChFlags`, etc.) are unaffected.

### Why does an issue-body edit count as forward progress?

Pre-PR stages (Specify, Research, Plan) iterate purely via `FABRIK_ISSUE_UPDATE` — there is no commit, no PR, and no stage completion until the human is satisfied with the spec/research/plan. Without this trigger, a long but entirely legitimate Q&A session in one of those stages could accumulate invocations toward the threshold with no other reset available, and eventually false-trip. Counting a body edit as progress closes that gap without needing a separately tuned threshold for pre-PR stages.

### Why is the counter reset on manual unpause?

`clearFailedStage` already resets `ReviewCycles`, `CIFixCycles`, `RebaseCycles`, and `EnqueueCycles` on the same "a human investigated and is giving this another shot" event. Without the same treatment here, a human who manually removes `fabrik:paused` after investigating a (mistaken or since-fixed) trip would be re-tripped after a single subsequent invocation that hadn't yet produced one of the four automatic reset signals — needlessly punishing the exact case a manual resume is meant to handle.

### Why is the invocation recorded right before the Claude call, not at `processComments` entry?

`JobStartedEvent` is emitted at step 0 of `processComments`, before the `fabrik:editing` label is even applied. Recording the breaker invocation there would count invocations that never reached Claude — e.g. an `fabrik:editing` label API failure — as wasted cycles, which they are not; they represent transient infrastructure friction, not a non-advancing comment-processing loop.

## Consequences

**Positive:**
- A new, previously unanticipated self-sustaining comment loop is now bounded to at most N invocations within T before it self-pauses, regardless of what causes it — closing the blind spot that let #1083 run ~995 times.
- No new label, so ADR-069's honorable-pause guarantee applies without any additional wiring, and no `CLAUDE.md` label-table update beyond documentation of the new trigger source.
- All five reset triggers are chosen at points where the signal is already locally and reliably available — no new polling, no new GitHub API calls beyond what `processComments`, `handleStageComplete`, `publishCommentOutput`, and the existing `itemstate.Store` subscription already do.

**Negative / Trade-offs:**
- **`gitHeadSHA` now shells out to `git rev-parse HEAD` twice per comment-processing invocation** (before/after), rather than only when `fabrik:extend-turns` progress detection needs it. The cost is negligible (a single local git call), and the pattern (`detectProgress`) was already established elsewhere in the engine.
- **The breaker is a backstop, not a fix** — it does not prevent the underlying loop from running N times and costing N invocations' worth of tokens before it trips. It bounds damage; it does not eliminate it. Fixes 1/4 and 2/4 remain the primary defenses against the two known pump shapes.
- **`PRStateChanged` is still coarser than a per-transition flag** (it doesn't distinguish merge from close from draft↔ready) — acceptable because the breaker only needs "did the PR's own state move," not which direction.

## Related Work

- Incident #1083 (human incident report — not modified by this ADR or its issue).
- ADR-069 / #1087 — fix 1/4; establishes the honorable-pause guarantee this ADR's trip action depends on.
- ADR-070 / #1088 — fix 2/4; establishes the "false negatives accepted by design" framing this ADR's PR-state reset trigger is judged against.
- #1090 — fix 4/4 of the same runaway-loop remediation series (decoupling Fabrik's own writes from cache invalidation/re-poll), out of scope here.
- ADR-059 D8 — the `mergeTrainTrials` runaway-guard precedent this breaker's window-pruning-in-the-engine design mirrors.

**References:** [docs/state-machine.md §4.6](../docs/state-machine.md)
