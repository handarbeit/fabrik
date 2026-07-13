# ADR 045: FABRIK_NO_WORK_NEEDED Marker

**Date**: 2026-05-13  
**Status**: Accepted

## Context

When the Plan stage (or any stage) concludes that no implementation work is required — e.g., a docs audit for a pure-internal release, or an issue filed against a problem that no longer exists — the engine previously had no way to short-circuit. It advanced blindly to Implement, which ran Claude, found nothing to do, exited without committing, and then `ensureDraftPR` failed:

```
HTTP 422: Validation Failed — No commits between main and fabrik/issue-N
```

After three retries, the engine escalated and paused the issue. The issue was genuinely complete-with-no-action but was treated as failed. This pattern was observed in issues #728 and #732.

The existing `FABRIK_DECOMPOSED` marker moves an issue directly to Done, but for a different reason — the issue was split into sub-issues. `FABRIK_DECOMPOSED` does not add dummy completion labels for the bypassed stages and does not post "skipped" comments. It is also a standalone marker that does not require `FABRIK_STAGE_COMPLETE` to co-occur.

## Decision

Add a new marker `FABRIK_NO_WORK_NEEDED` that any stage can emit to signal "this issue is complete; no code or documentation changes are required."

Key design choices:

1. **Requires `FABRIK_STAGE_COMPLETE` to co-occur.** Unlike `FABRIK_DECOMPOSED`, this marker does not stand alone. Both `FABRIK_STAGE_COMPLETE` and `FABRIK_NO_WORK_NEEDED` must appear in the same output for the no-work path to fire. This keeps Claude honest: the emitting stage must explicitly declare itself complete before the bypass fires. It also means the timeout/kill recovery path (which scans only for `FABRIK_STAGE_COMPLETE`) does not inadvertently trigger the no-work path.

2. **Adds dummy `stage:<name>:complete` labels for all bypassed non-cleanup stages.** This provides an explicit audit trail that the stages were consciously skipped, not forgotten. It also prevents catch-up advancement logic from re-running those stages on restart.

3. **Posts one-line "skipped" comments per bypassed stage.** Engine-generated metadata comments (no rocket reaction): `_Skipped: no work needed (FABRIK_NO_WORK_NEEDED emitted by <stage>)._`. These are visible in the issue history so the operator understands why nothing ran.

4. **Does not create a PR.** The `ensureDraftPR` and `markPRReady` calls in the normal `completed` branch are skipped — this is why `completed && noWorkNeeded` is checked before the plain `completed` branch in `processItem()`'s dispatch chain.

5. **Moves the issue directly to Done.** Same mechanism as `handleDecomposed`: `UpdateProjectItemStatus` with `e.statusField.Options["Done"]`.

A new `handleNoWorkNeeded(board, item, stage)` function is added to `engine/stages.go` immediately after `handleDecomposed`, following the same file-organization convention.

## Rationale

### Why a new marker instead of reusing `FABRIK_DECOMPOSED`?

`FABRIK_DECOMPOSED` is semantically wrong for this case — the issue was not decomposed into sub-issues. Using it would pollute the "parent decomposed" audit signal and would require callers to know that `FABRIK_DECOMPOSED` has a dual meaning.

More concretely, the behaviors differ:

| | `FABRIK_NO_WORK_NEEDED` | `FABRIK_DECOMPOSED` |
|---|---|---|
| Requires `FABRIK_STAGE_COMPLETE` | Yes | No |
| Adds dummy `stage:<name>:complete` for skipped stages | Yes | No |
| Posts "skipped" comments per stage | Yes | No |
| Moves to Done | Yes | Yes |
| Creates PR | No | No |

### Why require `FABRIK_STAGE_COMPLETE` to co-occur?

`FABRIK_DECOMPOSED` was designed as a standalone marker because decomposition is a categorical outcome — if Plan decomposes, it did not produce a plan and `FABRIK_STAGE_COMPLETE` would be misleading. By contrast, "no work needed" is a valid completion outcome for the emitting stage: Plan ran, assessed the research, and concluded the issue is moot. Requiring `FABRIK_STAGE_COMPLETE` preserves the invariant that `stage:<X>:complete` is only added when the stage genuinely completed its work.

The co-occurrence requirement also prevents a half-finished stage from accidentally triggering the bypass. A stage that emits `FABRIK_NO_WORK_NEEDED` without `FABRIK_STAGE_COMPLETE` routes to the normal cooldown/retry path — the same as if no completion marker were present.

### Why add dummy completion labels for skipped stages?

Without `stage:<Y>:complete` labels on bypassed stages, the catch-up advancement logic would treat those stages as incomplete and potentially try to re-run them. The dummy labels prevent this. They also give human operators a clear view of what happened: the issue's label history shows all stages as complete, with "skipped" comments explaining why.

`FABRIK_DECOMPOSED` does not need this because decomposed issues have sub-issues flowing through the full pipeline — the parent's bypassed stages are understood to be irrelevant.

### Priority in the dispatch chain

`completed && noWorkNeeded` is checked before the plain `completed` branch because the plain `completed` branch calls `ensureDraftPR` and `markPRReady`. Both would fail or behave incorrectly when no commits exist on the branch. The priority ordering ensures these operations are never invoked for the no-work path.

## Consequences

- Any stage can emit `FABRIK_NO_WORK_NEEDED` paired with `FABRIK_STAGE_COMPLETE`. By convention this is expected most from Plan. The skill prompt (`fabrik-plan/SKILL.md`) is the primary enforcement point — it instructs Plan when and how to use the marker.
- Bypassed stages receive `stage:<name>:complete` labels. The catch-up loop will see these and not attempt to re-run them.
- No PR is created when the no-work path fires. The Done stage's cleanup (worktree removal, label cleanup, issue close) runs as normal.
- Existing stuck issues with the "no commits" symptom must be manually closed or recovered — the marker is not backported. Future issues of the same class will be caught by Plan emitting the marker.

**References:** [ADR-017: Decomposed Marker State Machine](017-decomposed-marker-state-machine.md)

**Superseded in part by:** [ADR-060: Durable No-Work-Needed Marker](060-durable-no-work-needed-marker.md), which makes the Done-move/close atomic with the decision (a durable `fabrik:awaiting-done` marker, retried on failure, escalated past `MaxRetries`) — the "moves the issue directly to Done" step above was not originally interruption-safe.
