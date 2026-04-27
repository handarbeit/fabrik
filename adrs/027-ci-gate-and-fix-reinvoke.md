# ADR 027: CI Gate and CI-Fix Re-invocation Loop

**Status**: Accepted  
**Date**: 2026-04-16

## Context

Fabrik's yolo/cruise auto-advance paths could silently merge or advance issues whose PR CI was broken. The Review and Validate stages lacked any mechanism to:

1. Block auto-merge when CI checks are failing on the PR head.
2. Re-invoke the stage agent to fix CI failures before advancing.

The review gate (ADR 026) established the pattern for re-invocation loops. The CI gate mirrors this pattern.

## Decision

Implement a two-pronged CI gate controlled by `wait_for_ci: true` in the stage YAML.

### Prong 1 — Merge Guard (`attemptMergeOnValidate`)

The `yolo` auto-merge path already called `MergePR` directly. A CI gate is embedded directly in `attemptMergeOnValidate`:

- Fetch the PR's head SHA via `FetchLinkedPR` (REST — avoids extending the existing large GraphQL query).
- Fetch check runs via `FetchCheckRuns`.
- **R5**: No check runs → gate clears (repo has no CI). The post-push registration-window guard based on `prHasHadChecks[issueKey]` applies to the catch-up loop (`checkCIGate`, Prong 2) only — not here.
- **R4**: All checks green → clear `ciMergePendingSince`; clear `fabrik:awaiting-ci`; proceed to merge.
- **R3**: Any check failed → add `fabrik:awaiting-ci`; return error (caller logs and skips advance).
- **R2**: Any check pending → track start time in `ciMergePendingSince`; return error (no label added — R10c).
- **R6**: Pending elapsed ≥ `CIWaitTimeout` (default 30 min) → post comment; add `fabrik:paused` + `fabrik:awaiting-input`; return error.

The `ciMergePendingSince` map is keyed by `issueKey` (owner/repo#N). It tracks the first time a pending-CI observation is made for a given issue, so the timeout starts from when pending was first seen, not from when the issue was advanced.

### Prong 2 — Catch-up Loop CI Gate and Fix-Reinvoke

For stages with `wait_for_ci: true`, `handleStageComplete` uses **Approach A**:
- Add the `stage:<X>:complete` label immediately.
- Skip `attemptMergeOnValidate` (for Validate+yolo).
- Return without advancing; the catch-up loop handles the CI gate.

The catch-up loop Phase 1 (`poll()`) now calls `checkCIGate(board, item, stage)` after the review gate:

- `(false, false, false)` — gate clear; fall through to Phase 2 (advancement/merge).
- `(true, false, false)` — CI still pending, **or** post-push registration delay (R5: zero check runs but `prHasHadChecks[issueKey]` is true); skip to next item. **`fabrik:awaiting-ci` is NOT applied (R10c)** — label churn from transient pending states would produce noise and mislead users. The item's `fabrik:awaiting-ci` label presence triggers `itemMayNeedWork` to bypass the `updatedAt` cache, ensuring re-evaluation on every poll.
- `(true, true, false)` — CI failed; `fabrik:awaiting-ci` applied (idempotent); dispatch `dispatchCIFixReinvoke` or pause if cycle limit exceeded.
- `(false, false, true)` — Timeout: `fabrik:awaiting-ci` already present ≥ `CIWaitTimeout` (checked via `FetchLabelAppliedAt`); pause with `fabrik:paused` + `fabrik:awaiting-input`.

### Timeout Strategy: Two Different Approaches

The two prongs use different timeout strategies due to their different lifecycles:

- **Prong 1** (merge guard): Uses an in-memory `ciMergePendingSince` map. Acceptable because merge-guard state is transient — if the engine restarts, it simply re-evaluates CI on the next poll.
- **Prong 2** (catch-up loop): Uses `FetchLabelAppliedAt` on `fabrik:awaiting-ci`. The label is durable across restarts. `FetchLabelAppliedAt` makes a REST API call, but only when CI has already failed and the label is present — a rare, high-signal path.
- **Post-push registration delay guard**: The in-memory `prHasHadChecks map[string]bool` (keyed by `issueKey`) records whether `FetchCheckRuns` has ever returned a non-empty result for a given issue in this process lifetime. When R5 fires (zero check runs), `checkCIGate` consults this flag: if true, the gate blocks (post-push window); if false, the gate clears (no CI configured). On engine restart the flag resets — the worst case is one poll cycle where a newly-registered post-push SHA gets a false "no CI" read, after which the next poll will re-block correctly.

### CI-Fix Re-invocation

When `checkCIGate` returns `ciFailure=true`, the catch-up loop mirrors `dispatchReviewReinvoke` exactly:

1. Guard: if a goroutine from a previous poll is still in-flight for this item, skip dispatch.
2. Cycle check: if `ciFixCycleCount[stageKey] >= MaxCiFixCycles` (default 5), pause with `pauseForCIFixCycleLimit`.
3. Otherwise: increment cycle count, call `dispatchCIFixReinvoke`.

`dispatchCIFixReinvoke` spawns a goroutine that:
- Marks `inFlight`; acquires semaphore; calls `ensureRepoReady`.
- Calls `buildCIFixComment` to construct a synthetic `gh.Comment` (DatabaseID: 0) with a structured CI failure report comparing PR failures against base-branch failures (to classify NEW REGRESSION vs pre-existing).
- Calls `processComments` with the synthetic comment and the `ci_fix_skill` (falls back to `comment_skill` if not set).
- Emits `JobStarted`/`JobCompleted` TUI events.

The `DatabaseID: 0` guard (already in place from ADR 026) skips 👀 and 🚀 reactions for synthetic comments.

The stage agent should fix NEW REGRESSION failures, commit, push, and **not** emit `FABRIK_STAGE_COMPLETE` — the engine re-evaluates CI on the next poll and advances once all checks pass.

### YAML Defaults

`wait_for_ci: true` is added to `stages/examples/validate.yaml` and `.fabrik/stages/validate.yaml`. The `WaitForCI *bool` field uses nil-means-false semantics. Only Validate has `wait_for_ci: true` in the defaults; other stages opt in explicitly.

### `itemMayNeedWork` Cache Bypass

Items with `fabrik:awaiting-ci` bypass the `updatedAt` cache in `itemMayNeedWork` — same as `fabrik:blocked`. CI results change independently of the issue's GitHub `updatedAt`, so without this bypass, items would be skipped once their `updatedAt` settles.

## Alternatives Considered

### Approach B: Check CI in `handleStageComplete` Before Adding Completion Label

Gate in `handleStageComplete`: poll CI, return early (without adding completion label) if CI is pending or failed.

Rejected because:
- `handleStageComplete` runs synchronously in the worker goroutine, which holds the semaphore. Polling CI repeatedly in a semaphore-held goroutine starves other workers.
- The catch-up loop already evaluates per-item state at the right cadence. Deferring to it (Approach A) is architecturally consistent with how `wait_for_reviews` works.

### Extending the GraphQL Query for Head SHA

Adding `pullRequest { headRefOid }` to the board GraphQL query would avoid a separate REST call for the head SHA.

Rejected because the existing query is already large. The head SHA is only needed on CI-gate paths (stages with `wait_for_ci: true`, which is a subset of all items). A targeted REST call via `FetchLinkedPR` has lower cost for the common case (no CI gate).

### A Single Timeout Strategy for Both Prongs

Using `FetchLabelAppliedAt` for the merge guard (Prong 1) would make both prongs consistent. Rejected because the merge guard runs in a frequently-called hot path (every poll, for every yolo Validate item). `FetchLabelAppliedAt` is a REST call and would add latency for every merge attempt when CI is pending.

Using `ciMergePendingSince` for the catch-up loop (Prong 2) would require persisting the map across restarts to avoid losing timeout context. The label-based approach is naturally durable with no extra machinery.
