# ADR 032: CI Gate Conjunctive Completion Label (Approach A')

**Status**: Accepted  
**Date**: 2026-04-26  
**Supersedes**: ADR 027 (Approach A — partially; the two-prong CI gate structure is retained)

## Context

ADR 027 implemented a two-prong CI gate controlled by `wait_for_ci: true`. Under "Approach A," `handleStageComplete` adds `stage:<X>:complete` immediately when Claude emits `FABRIK_STAGE_COMPLETE`, then defers CI evaluation to the catch-up loop. The intent was that the completion label would block re-dispatch (`itemNeedsWork` returns false for completed stages) while the catch-up loop polled CI on every tick.

### The Problem With Approach A

In practice, the combination of `fabrik:yolo` + CI-fix reinvoke + repeated commit pushes produced a re-dispatch loop:

1. Claude emits `FABRIK_STAGE_COMPLETE` → `handleStageComplete` adds `stage:Validate:complete`.
2. Catch-up loop runs; CI is still pending; no-op (R10c: no label for pending).
3. CI fails; catch-up loop adds `fabrik:awaiting-ci`; dispatches CI-fix reinvoke.
4. CI-fix reinvoke pushes commits → triggers new CI run → removes the old `stage:Validate:complete` label (GitHub removes it as part of SHA-refresh state management in some configurations, or a race condition in the catch-up loop removes it).
5. Next poll: `stage:Validate:complete` absent → `itemNeedsWork` returns true → dispatcher re-invokes the full Validate stage.
6. New Validate invocation emits `FABRIK_STAGE_COMPLETE` → cycle repeats.

Observed in `verveguy/liminis #705`: **27 full Validate stage invocations** during a single CI-await window (~80 minutes), costing ~$10–20 for work that should have been free REST polls.

The root cause is a semantic layering problem: `stage:Validate:complete` claims "Validate is done" when in fact only the Claude verdict condition has been met. The remaining conditions (CI green, PR mergeable) are still being evaluated by the catch-up loop.

### Why Approach B Still Doesn't Apply

ADR 027 rejected "Approach B" (poll CI in `handleStageComplete` before adding the completion label) because `handleStageComplete` runs in a semaphore-held goroutine. Polling CI repeatedly while holding the semaphore would starve other workers.

**The new approach (A') avoids this problem entirely:** CI polling stays in the catch-up loop. The only change is that `stage:X:complete` is added by the catch-up loop (in `checkCIGate`) instead of by `handleStageComplete`. No new polling is introduced in the worker path.

## Decision

**Approach A' (conjunctive gate):** Defer `stage:<X>:complete` to `checkCIGate`. `handleStageComplete` adds `fabrik:awaiting-ci` as the durable in-flight marker when a `wait_for_ci: true` stage emits `FABRIK_STAGE_COMPLETE`. `stage:<X>:complete` is only added after `checkCIGate` confirms CI is green.

### Changes From Approach A

#### `handleStageComplete` (stages.go)

For `wait_for_ci: true` stages, the function now:
1. Adds `fabrik:awaiting-ci` (idempotent — skipped if already present).
2. Also adds `fabrik:awaiting-review` if `wait_for_reviews: true` (idempotent).
3. Logs "deferring stage:X:complete until CI gate clears".
4. Returns immediately — does NOT add `stage:<X>:complete`.

The previous code path that added `stage:<X>:complete` immediately and then returned early from the `shouldAdvance` block (for `wait_for_ci: true` stages) is removed. The `fabrik:awaiting-ci` durable marker makes the separate "return early without completing" path unnecessary.

#### `checkCIGate` (ci.go)

When the CI gate clears (gate-cleared return paths: no check runs (R5) or all checks green), `checkCIGate` now calls `addCompleteLabelAndRemoveCI`, which:
1. Adds `stage:<X>:complete` to GitHub.
2. Removes `fabrik:awaiting-ci`.

This is the **only place** `stage:<X>:complete` is added for `wait_for_ci: true` stages. The conjunctive invariant: `stage:<X>:complete` is only ever set after CI has been verified green by `FetchCheckRuns`.

#### Catch-up loop entry guard (`poll.go`)

The guard is broadened from:
```go
if !hasComplete {
    continue
}
```
to:
```go
isWaitForCI := stage.WaitForCI != nil && *stage.WaitForCI
if !hasComplete && !(hasAwaitingCI && isWaitForCI) {
    continue
}
```

Items with `fabrik:awaiting-ci` on a `wait_for_ci: true` stage are admitted to Phase 1 (CI gate evaluation) even without `stage:<X>:complete`. Phase 2 (advancement/merge) is unaffected — it proceeds naturally once `stage:<X>:complete` is added by `checkCIGate`.

#### `itemNeedsWork` (item.go)

A new guard added after the completion-label check: if `fabrik:awaiting-ci` is present, return `false`. This prevents the dispatcher from re-invoking the stage during CI await (R3). The closed-issue guards in `itemNeedsWork` and `itemMayNeedWork` are updated to also admit `fabrik:awaiting-ci` items, so the catch-up loop can complete the CI gate transition after a PR merge closes the issue.

#### Dead code removed: `wait_for_ci + stage:X:complete` bypass in `itemMayNeedWork`

The bypass at `item.go` that forced a deep-fetch for `wait_for_ci: true + stage:X:complete` items is removed. With Approach A', `fabrik:awaiting-ci` is always present from the start of the CI-await window. The existing `fabrik:awaiting-ci` bypass already handles deep-fetch forcing for these items.

### Semantic Expansion of `fabrik:awaiting-ci`

Under ADR 027, `fabrik:awaiting-ci` meant "CI confirmed failed." It was explicitly NOT applied for pending/queued checks (R10c — no label churn for transient states).

Under ADR 032, the semantics broaden to **"CI gate active"** — the label is present from the moment Claude signals FABRIK_STAGE_COMPLETE on a `wait_for_ci: true` stage. It covers both sub-states:
- CI checks pending (running or queued)
- CI checks failed (one or more `failure/timed_out/action_required` conclusions)

R10c's "no label for pending" invariant is intentionally reversed for the CI-await-after-stage-completion scenario. The label's timeout tracking (via `FetchLabelAppliedAt`) now measures the entire CI-await window from stage completion, which is more accurate than measuring only from the first confirmed failure.

**The timeout machinery is unchanged** — `pauseForCITimeout` continues to operate on `FetchLabelAppliedAt` on `fabrik:awaiting-ci`. The only difference is the label is applied earlier, so the timeout starts from stage completion rather than from first CI failure. This is the correct semantics: the user is waiting for CI from the moment Validate finishes, not from the moment CI first reports a failure.

## Consequences

### Resolved

- **Re-dispatch during CI await eliminated**: `fabrik:awaiting-ci` is durable from stage completion to CI clearance. `itemNeedsWork` returns false for this entire window. The dispatcher will not re-invoke the stage.
- **`stage:<X>:complete` semantics correct**: The label now genuinely means "stage completed and all post-completion gates cleared." No more "optimistic" or "premature" labeling.
- **Idempotent re-invocation**: If a CI-fix reinvoke emits `FABRIK_STAGE_COMPLETE`, `handleStageComplete` finds `fabrik:awaiting-ci` already present and returns immediately without adding the completion label. The fix is safe for multiple FABRIK_STAGE_COMPLETE emissions.

### Tradeoffs

- **Label semantics change is user-visible**: Users who monitored `fabrik:awaiting-ci` to mean "CI failed and engine is retrying" will now see it appear earlier (immediately at stage completion). The GitHub label description is updated to reflect the broader semantics.
- **`stage:X:complete` delayed until CI clears**: For `wait_for_ci: true` stages, `stage:<X>:complete` is absent until CI passes. Tooling, dashboards, or users that interpret `stage:Validate:complete` as "ready to merge" now receive that label at the correct time (after CI green), not at the optimistic time (after Claude verdict).
- **One extra poll cycle for `wait_for_ci + wait_for_reviews` stages**: After reviews clear, the catch-up loop must run one more time to evaluate the CI gate and add `stage:X:complete`. This introduces a ~15–30s delay compared to Approach A. Acceptable given that Approach A's timing was incorrect anyway.
