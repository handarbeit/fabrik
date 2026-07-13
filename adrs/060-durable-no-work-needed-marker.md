# ADR 060: Durable No-Work-Needed Marker

**Date**: 2026-07-13
**Status**: Accepted
**Issue**: #981 — fix(engine): no-work-needed Done-advance lost when the move fails (rate limit)

## Context

ADR-045 introduced `handleNoWorkNeeded`: when a stage emits `FABRIK_STAGE_COMPLETE` + `FABRIK_NO_WORK_NEEDED`, the engine skips the remaining pipeline stages and moves the issue directly to Done, without creating a PR. ADR-045 assumed the Done-move (`UpdateProjectItemStatus`) and the subsequent `CloseIssue` call would simply succeed, and did not consider the failure case.

In a clean e2e run (2026-07-13), a GraphQL rate-limit exhaustion caused `UpdateProjectItemStatus` to fail inside `handleNoWorkNeeded`. The error was logged and swallowed. Because nothing durable had been written to record the "no work needed" decision itself — only the emitting stage's `stage:<X>:complete` label existed, and that label alone does not signal "stop here" to the catch-up loop — the issue looked, on the next poll, exactly like an ordinary item that had completed one stage and was ready to advance normally. `fabrik:cruise` was present, so Phase 2 of the catch-up loop (`poll.go`) advanced it through the *entire remaining pipeline* — Plan, Implement, Review, Validate, merge-train landing — for real. This wasted ~16 minutes of wall-clock, four stages of Claude spend, and produced a PR/branch/merge that should never have existed.

The codebase already has a working precedent for exactly this class of problem: `advanceToNextStage`, the plain per-stage board move used by `handleStageComplete` and the Phase 2 catch-up loop, is safe by construction — it is only ever called *after* `stage:<X>:complete` has been durably written, so a failed move is naturally retried on the next poll (the label survives regardless of the move's outcome). `handleNoWorkNeeded` did not follow this shape: it interleaved "record the decision" and "execute the one-shot mutations" in a single uninterruptible sequence, with the closest thing to a durable record (the emitting stage's completion label) written *partway through*, not as the first, unconditional action.

## Decision

Write a new durable label, `fabrik:awaiting-done`, as the **very first mutation** in `handleNoWorkNeeded` — before the `fabrik:awaiting-input` clear, before the emitting stage's completion label, before anything else. This closes the observed failure window directly: even a fully rate-limited invocation (every subsequent call failing) leaves this one write durably recorded on GitHub.

While `fabrik:awaiting-done` is present:

- `itemMayNeedWork` and `itemNeedsWork` (`engine/item.go`) return `false` for every non-cleanup stage, checked immediately after each function's `stage == nil` guard — independent of `item.Status`, since the outstanding board move is exactly what may be failing, so the item could be observed sitting at any column while the marker is active.
- A new unconditional settle scan in `poll.go`, `runNoWorkNeededSettle` (added immediately after `runValidatePRTerminalAdvance`, mirroring its "single-owner, `item.Status`-agnostic" shape — ADR-056 D2 / ADR-057), retries the outstanding work every poll via `settleNoWorkNeeded`.

`handleNoWorkNeeded` itself is split into two functions:

- `handleNoWorkNeeded` — writes the marker (idempotently), then delegates.
- `settleNoWorkNeeded` — does the rest of the original work (label/comment sub-steps, the Done move, the close), with every sub-step checking current state (`hasLabel`, a new `hasSkippedComment` helper, `item.Status`, `item.IsClosed`) before mutating, so it is safe to call repeatedly with a fresh item snapshot on each poll. It only attempts the higher-risk `UpdateProjectItemStatus`/`CloseIssue` calls once the label/comment sub-steps have all succeeded in that pass, preserving the existing invariant that `CloseIssue` is never called when the status move has not succeeded.

Repeated settle failures are counted against the existing `itemstate.StageRetryIncremented`/`Attempts`/`e.cfg.MaxRetries` mechanism — the same counter family `escalatePRCreationFailure` and `escalateFailedStage` already use — but keyed by a dedicated constant, `"__no_work_needed__"`, rather than the emitting stage's real name. Once `MaxRetries` is reached, `escalateNoWorkNeededFailure` fires: `fabrik:paused` is added, `fabrik:awaiting-done` is removed, an explanatory comment with manual recovery steps is posted, and `itemstate.EnginePaused` is applied — mirroring `escalatePRCreationFailure`/`escalateFailedStage` exactly.

`settleNoWorkNeeded` skips its label/comment sub-steps (the `fabrik:awaiting-input` clear, the emitting stage's completion label, and the per-stage skip loop) entirely once `item.Status == "Done"`. This is not merely an optimization: on a retried call, `stage` is re-derived by the settle scan via `stages.FindStage(e.cfg.Stages, item.Status)`, and once the board has moved to `"Done"`, that resolves to the **cleanup stage itself**, not the original emitting stage. Running the label steps with that wrong `stage` would add a spurious `stage:<Done-stage-name>:complete` label — before the real Done-stage worktree cleanup has ever run for the item — which `itemNeedsWork`'s `CleanupWorktree` branch reads as "cleanup already complete," permanently short-circuiting normal cleanup dispatch. The guard is safe because the Done move (step 6) is only ever attempted after all label/comment sub-steps have already succeeded in that same pass — so `item.Status == "Done"` is itself proof there is nothing left for them to do.

## Rationale

### Why a durable GitHub label, not an `itemstate.Store` mutation?

`itemstate.Store` is confirmed in-memory-only — it does not survive an engine restart. The existing `PRCreationFailedRecorded` flag (§5.5 of `docs/state-machine.md`, the "R5" pattern) is explicitly documented as acceptable *in-memory only*, because a Claude re-run triggered by a restart is safe and conservative: the worktree's commits from the prior run are idempotent, so re-running Claude just reproduces the same state.

That reasoning does not transfer to the no-work-needed case. There are no commits to safely redo — a no-work-needed decision has no artifact except the decision itself. If the marker were in-memory only and the engine restarted while the Done-move was still outstanding, the decision would be lost exactly as if the marker never existed, and the full pipeline would run for real. The marker must therefore be the one kind of state that does survive a restart in this engine: a GitHub label.

### Why the first mutation, not merely "before `UpdateProjectItemStatus`"?

The observed failure was rate-limit exhaustion, which is not scoped to a single call — `handleNoWorkNeeded` makes upward of ten sequential GraphQL calls in its original form (label clear, completion label, N skip labels, N skip comments, status move, close), any of which can be the one that first hits the exhausted limit. Writing the marker only immediately before the status move would still lose the decision if the limit was hit earlier, during the skip-label/comment loop. Writing it as the unconditional first call is the only placement that survives every observed and plausible failure point.

### Why generalize `fabrik:awaiting-ci` / `runValidatePRTerminalAdvance` rather than build something new?

The engine already has two working instances of "durable label suppresses dispatch while an out-of-band condition resolves, retried by an independent per-poll pass, escalated on a bounded counter": `fabrik:awaiting-ci` (CI gate) and `fabrik:rebase-needed` (merge-conflict gate, with `RebaseCycleIncremented`/`pauseForRebaseCycleLimit`). `runValidatePRTerminalAdvance` (ADR-057) additionally establishes the "single-owner, `item.Status`-agnostic, idempotent fill-only-what's-missing" scan shape needed here, since the no-work-needed item's board column cannot be trusted to reflect its true state while the move itself may be what's failing. This fix is a third instance of the same pattern family, not a new abstraction — no new `itemstate.Mutation` type, no new configuration surface, no new escalation shape.

### Why a dedicated retry-counter constant instead of the emitting stage's real name?

The emitting stage's own `Attempts` counter is deliberately cleared (`StageRetryCleared`) immediately before `handleNoWorkNeeded` is ever called (`processItem`'s `completed && noWorkNeeded` branch) — it is safe to reuse without collision. It was rejected anyway: reusing it would conflate two different failure semantics under one counter — "Claude failed to complete this stage" and "the Done-move failed after Claude already succeeded" — which would be confusing to read in logs and in any future debugging of `MaxRetries` behavior. `"__no_work_needed__"` is deliberately unrepresentable as a real YAML stage `name:` (the double-underscore wrapping), so it can never collide with a configured stage's own counter.

### Why is the marker never removed by the closed-issue defensive sweep (`cleanupClosedIssueTransientLabels`)?

Most gate labels (`fabrik:awaiting-ci`, `fabrik:awaiting-review`, `fabrik:rebase-needed`, `fabrik:awaiting-input`, …) are swept unconditionally from closed issues, because they have no meaning once an issue is closed and leaving them behind risks confusing a reopened issue. `fabrik:awaiting-done` is different: an issue can be closed (by GitHub's own mechanics, or manually) *before* the settle scan has finished moving it to the Done column, while retries are still below `MaxRetries`. If the sweep stripped the marker at that point, the decision would be lost exactly as in the original bug — the item would fall out of dispatch suppression while still sitting in a non-Done column, and would be picked up by ordinary dispatch on the next poll. `settleNoWorkNeeded` is already closed-issue-aware (its `item.IsClosed` check skips the now-redundant `CloseIssue` call and still completes the status move), so it is the sole self-healing path for this label — the marker is deliberately excluded from `transientLifecycleLabels`.

## Consequences

**Positive:**
- A rate-limited (or otherwise interrupted) no-work-needed decision can no longer silently fall back into the normal pipeline. The decision, once made, is durable from its very first byte on the wire.
- The fix is a pure generalization of already-proven machinery: no new abstractions, no new `itemstate.Mutation` type, no new configuration surface, no new escalation shape.
- A permanently broken board/API state (rather than a transient rate limit) still surfaces to a human via the existing `fabrik:paused` + comment escalation pattern, instead of retrying invisibly forever.

**Negative / Trade-offs:**
- `settleNoWorkNeeded` re-evaluates `hasLabel`/`hasSkippedComment` against the *passed-in* item snapshot on every call. Because all skip comments for one decision carry identical text (naming the emitting stage, not the individual skipped stage — an existing behavior, unchanged by this fix), `hasSkippedComment` is a per-decision check, not per-stage: if a partial pass posts some skip comments and then fails before posting all of them, a differently-partial pass could in principle observe "at least one skip comment exists" and treat the set as fully posted before every skipped stage actually has one. This is an accepted, pre-existing minor gap (the same ambiguity exists in the un-fixed code for any retry of the comment loop) — not introduced by this change, and bounded in practice because skip-comment failures are rare (much rarer than the observed rate-limit failure mode, which tends to fail every call in a tight window, not just some).
- The settle scan adds one label-set membership check per poll for every item carrying `fabrik:awaiting-done` — negligible cost, bounded by the (small, transient) number of issues mid-no-work-needed-decision at any time.

## Sibling Audit

Two structurally similar one-shot-mutation-without-durable-marker sites were found during this issue's research and are **explicitly out of scope for this change** (per the issue's scope-creep guidance) — tracked as follow-up issues instead:

- `engine/spawn.go` (`preImplement`): a spawned child issue's initial `UpdateProjectItemStatus` call (placing it on the board) is logged-and-swallowed on failure with no retry. Since `itemMayNeedWork`/`itemNeedsWork` short-circuit to `false` for any item whose column has no matching configured stage, a child stranded in an unmatched column (e.g. a generic "Backlog") is never dispatched, ever — arguably a worse bug class than this issue's, since the child is never revisited at all.
- `engine/merge_train.go` (merge-train landing routine): the final `CloseIssue` call for a batch member, after the item's status has already durably advanced to Done, has no retry if it fails. Lower severity — GitHub's own `Closes #N` normally auto-closes on merge into the default branch, so this only matters for a non-default base branch.

Both require their own durable-marker-shaped design work and are filed as follow-up issues rather than folded into this change.

**References:** [ADR-045: No Work Needed Marker](045-no-work-needed-marker.md), [ADR-056: Consolidate Convergence Gate Recovery](056-consolidate-convergence-gate-recovery.md), [ADR-057: Single-Owner Validate PR Terminal Advance](057-validate-pr-terminal-advance.md)
