# ADR 1413: Comment Circuit Breaker Records Setup Failures

**Date**: 2026-08-08
**Status**: Accepted
**Issue**: #1413 — Comment-path setup failures bypass the #1089 circuit breaker and re-invoke unbounded

## Context

The #1089 comment-processing circuit breaker (ADR-1089) exists to bound exactly one failure mode: an issue that undergoes N comment-processing invocations within a rolling window T with no forward progress. Its recording point, however, was placed immediately before the Claude invocation — deliberately, per ADR-1089's own rationale ("Why is the invocation recorded right before the Claude call, not at `processComments` entry?"): recording earlier would count an `fabrik:editing` label API failure as a wasted cycle, which ADR-1089 judged to be "transient infrastructure friction, not a non-advancing comment-processing loop."

That judgment turned out to be wrong in the case that matters. `processComments` (`engine/comments.go`) has three setup steps between comment-slice finalization and the Claude invocation — the `fabrik:editing` label add, `baseBranchForItem` (base-branch resolution), and `EnsureWorktree` (worktree setup) — each of which returns early on error, before `recordCommentBreakerInvocation` ever runs. None of the three is instantaneous-and-rare in practice: `AddLabelToIssue` is a GraphQL mutation, and a persistently exhausted GraphQL budget (or any other persistent infrastructure condition) can make it fail on every single poll, indefinitely. When that happens, the comment is never marked processed (`finalizeComments` is never reached), so `findNewComments` returns it as new on the next poll, and the cycle repeats — with the breaker counting none of it, because none of it ever reaches the recording point.

@bdueck reported (community report #1382, discussion #1386) 141 invocations in ~31 minutes (~11s apart) on one issue — stopped only by removing the item from the board. Restarting the daemon, removing the arming label, and applying `fabrik:paused` manually all had no effect, because none of those interventions address a loop that the breaker was never counting in the first place. (A related but independent hypothesis — #1414's shared `<Stage>.session` file and comment review's unconditional session resume — could produce a similar-looking symptom through a different mechanism; #1413 does not depend on, and is not superseded by, #1414's outcome. Both are worth fixing regardless of which explains the #1382 incident specifically.)

## Decision

Move `recordCommentBreakerInvocation()`'s call site in `processComments` from immediately before the Claude invocation to immediately after the working comment slice is finalized — before the step 0 `JobStartedEvent` emission and every subsequent setup side effect (👀 reactions, editing label, base-branch resolution, worktree setup). Add `checkCommentBreaker()` calls to each of the three setup-failure early-return paths, so a setup failure is now both recorded and evaluated against the trip threshold, exactly as an invocation-time failure already was.

**Reason threading, not persisted state.** `checkCommentBreaker` and `tripCommentBreaker` gain a `reason string` parameter, empty on the two pre-existing call sites (the non-completing invocation-failure path and the success-tail path) and populated with a step-specific message (e.g. `"the fabrik:editing label add failed: %v"`) on each of the three new setup-failure call sites. When `tripCommentBreaker` fires with a non-empty reason, the pause comment gains one additional sentence naming the specific failing step and its error, so a human reading the pause isn't left with an undiagnosable "no forward progress" notice for what was actually a label-add or worktree failure. `reason` is a call parameter, not a new `itemstate.CommentBreakerState` field: `checkCommentBreaker` is always invoked synchronously in the same call as the failure it should explain, on every path, both before and after this change — there is no scenario where the trip happens on a later cycle than the failure that caused it, so durable storage would be unused complexity. This mirrors the existing `LastAuthor` field's "only the triggering cycle's context matters" precedent rather than extending it.

**`preInvokeSHA` capture is unchanged and stays decoupled.** The pre-invocation `gitHeadSHA(workDir)` snapshot (used by the "new commit" reset trigger) remains at its original call site, right before the Claude invocation, since `workDir` only exists once `EnsureWorktree` has succeeded. Recording the invocation and capturing the pre-invoke SHA never needed to be co-located — only `lastCommentAuthor(comments)` is needed to record, and that is available as soon as the comment slice is final.

This supersedes ADR-1089's "Why is the invocation recorded right before the Claude call" rationale subsection: the specific judgment that a setup failure shouldn't count as a wasted cycle is exactly what produced the unbounded loop this issue closes. ADR-1089 is left unedited as a historical record of the reasoning at the time; this ADR documents the reversal.

## Rationale

### Why not add `StageAttempted`/`StageRetryIncremented` tracking to the comment path instead?

That would give the comment path `max_retries`-based dispatch cooldown, a second, independent bound alongside the circuit breaker. It's a reasonable defense-in-depth idea, but it introduces stage-retry semantics to a path that has never had them — a materially larger architectural change than moving an existing recording call earlier. Left as a candidate follow-up issue, not bundled into this fix.

### Why does a one-off transient setup blip not need special-casing?

Recording earlier means a single transient failure (e.g. one `AddLabelToIssue` 5xx that succeeds on the next poll's retry) now consumes one slot in the rolling window it previously didn't. This is accepted rather than mitigated with new logic: the existing window-based threshold (default N=10 within T=30min) and the five pre-existing reset triggers (stage completion, new commit, PR state change, issue-body edit, manual unpause) already exist precisely to absorb intermittent noise without false-tripping on a healthy issue. A dedicated regression test (`TestCommentBreaker_SetupFailureThenSuccess_NoPoisonedCounter`) demonstrates this empirically — one setup failure followed by one forward-progressing cycle leaves the counter at zero, not one.

### Why is the pinned-trip-count test needed, not just the three setup-failure trip tests?

The three setup-failure trip tests each drive their scenario to the configured threshold and assert a trip occurred, which confirms the mechanism works but not that it works with the *default* recording point. If a future refactor moved the record call back down below setup — reintroducing exactly this bug — those three tests could still pass (they'd just take longer, bounded by whatever loop they run), silently regressing back toward the #1382 shape. `TestCommentBreaker_PinnedTripCount_SetupFailure` asserts the exact invocation count at each step (not paused at N-1, paused at N), so a regression that moves the record point back down fails loudly and immediately rather than only showing up as "slower to trip" in production.

## Consequences

**Positive:**
- Any comment-processing setup-failure loop — not just the specific `fabrik:editing` label-add case observed in #1382 — is now bounded to at most N invocations within T before the issue self-pauses, closing the placement gap regardless of which setup step is the one that's failing.
- The pause comment names the actual failing setup step and error when applicable, making a setup-failure trip immediately diagnosable rather than a bare "no forward progress" notice a human has to investigate from scratch.
- No new label, no new `itemstate` field, no change to the threshold/window configuration surface — the fix is contained to the recording/evaluation call sites in `engine/comments.go` and the two function signatures in `engine/comment_breaker.go`.

**Negative / Trade-offs:**
- A single transient setup blip now counts toward the window that previously didn't — accepted, per the Rationale above, as within the breaker's existing tolerance for intermittent noise.
- This is containment, not a cure, for the #1382 incident specifically: the version of Fabrik running during that incident is not yet confirmed (the #1089 breaker shipped in v0.0.76 / `771af8f6`), so whether this placement gap or #1414's session-poisoning hypothesis (or both) actually produced the 141-run observation remains provisional. This issue's fix is worth doing on its own merits — the placement gap is real and independently verified regardless of that answer — but landing it does not confirm or rule out #1414's mechanism, and #1414 should not be closed on the strength of this ADR alone.

## Related Work

- ADR-1089 / #1089 — origin of the comment-processing circuit breaker; its "Recording point" and dedicated rationale subsection asserted the literal opposite of this ADR's decision. Left unedited as a historical record; this ADR supersedes that one subsection by reference.
- ADR-069 / #1087 — establishes the honorable-pause guarantee (`fabrik:paused` cannot be silently lifted by a bot comment) that `tripCommentBreaker`'s reuse of `pauseIssue` depends on. Unaffected by this change.
- ADR-065 — the shared `pauseIssue`/`pauseOpts` helper `tripCommentBreaker` calls; only the message string composed before calling it grows.
- Community report #1382, discussion #1386 — the 141-invocation observation motivating this fix.
- #1414 — a related, independently-tracked hypothesis (shared `<Stage>.session` path and comment review's unconditional session resume) that could produce a similar-looking symptom through a different mechanism. Neither issue is a prerequisite for the other; #1414 is not resolved or ruled out by this ADR.

**References:** [docs/state-machine.md §4.6](../docs/state-machine.md)
