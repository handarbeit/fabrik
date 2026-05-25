# ADR 050: Convergence Budget + GitHub Native Auto-Merge for yolo Issues

## Status

Accepted

## Context

Issue #829 addresses two related production failures observed on high-churn repos:

1. **Post-Validate convergence is not composable.** Three independent retry mechanisms gate the final merge for yolo issues: rebase reinvoke (gated by `MaxRebaseCycles`), CI-fix reinvoke (gated by `MaxCiFixCycles`), and a polling merge attempt (`MergePR` on every catch-up cycle). In production, the CI-fix limit fired in 90 seconds with five spurious cycles while CI was still running cleanly — Fabrik paused with "CI fix cycle limit reached" when the actual problem was convergence latency, not CI failure. The user received a misleading diagnostic and chased the wrong fix.

2. **Fabrik's poll-merge loop has its own race conditions.** `MergePR` samples CI status and mergeable status separately, then attempts a merge — between sample and attempt, state can change. This produced HTTP 405 log spam and stale-state false positives on multi-PR repos where main moves frequently.

GitHub's native `enablePullRequestAutoMerge` GraphQL mutation makes the merge decision atomically server-side: when CI passes AND required reviews exist AND the PR is mergeable per branch protection, GitHub merges in a single transaction. The race window between "check CI" and "attempt merge" is eliminated.

Three architectural questions drove the design:

1. **What replaces the per-gate cycle counts as the backstop for yolo issues?** The per-gate counts are fine for non-yolo (cruise, manual) workflows where the PR may sit indefinitely waiting for human review. For yolo, the intent is "merge as soon as GitHub says OK" — a wall-clock budget more accurately captures the intended contract: the PR should converge in *N* minutes, or something is genuinely wrong that needs human intervention.

2. **Where does the budget start time live?** It must survive engine restarts. Three options: (a) in-memory `itemstate` field — fast but reset on restart; (b) a dedicated GitHub issue label with its own timestamp — clean but adds label proliferation; (c) the creation timestamp of the existing `fabrik:auto-merge-enabled` label via `FetchLabelAppliedAt`. Option (c) was chosen: it reuses the label we already apply for idempotency, mirrors the exact pattern used by the CI wait timeout (`FetchLabelAppliedAt("fabrik:awaiting-ci")`), and adds zero new API surface.

3. **How should `fabrik:cruise > fabrik:yolo` precedence extend to the auto-merge path?** The existing rule — cruise wins when both labels are present — must extend to `enablePullRequestAutoMerge` enablement. Cruise users choose manual merge; we must not override that choice even when `fabrik:yolo` is also applied.

## Decision

### GitHub Native Auto-Merge for yolo Issues

After Validate completes for a yolo (non-cruise) issue, Fabrik calls `enablePullRequestAutoMerge` with the configured strategy (`MERGE`, `SQUASH`, or `REBASE`; default `MERGE`) instead of calling `MergePR` directly. On success, Fabrik applies the `fabrik:auto-merge-enabled` label. GitHub then merges the PR atomically when all branch-protection requirements are satisfied.

The legacy `MergePR` call path is retained for non-yolo issues. No behavior change for cruise or manual-workflow issues.

### Wall-Clock Convergence Budget

The per-gate cycle counts (`MaxRebaseCycles`, `MaxCiFixCycles`) remain as observability counters and as the backstop for non-yolo issues, but are not consulted as gates when `fabrik:auto-merge-enabled` is present. Instead, a single wall-clock budget (`ConvergenceBudget`, default 30 minutes) governs the total convergence time for yolo issues. When the budget exhausts, Fabrik posts a structured `pauseForConvergenceFailed` comment naming the actual current state (commits-behind, CI status, mergeable status, elapsed time, rebase cycle count) and pauses the issue.

`FABRIK_CONVERGENCE_BUDGET=0` disables the bounded budget and falls back to the legacy per-gate cycle limits, preserving backward compatibility.

### `FetchLabelAppliedAt` as Restart-Durable Budget Storage

The budget start time is read from the GitHub issue events API via `FetchLabelAppliedAt("fabrik:auto-merge-enabled")` on every poll for convergence-active items. This is the same mechanism used for the CI wait timeout (`FetchLabelAppliedAt("fabrik:awaiting-ci")`) and survives engine restarts without additional infrastructure. Cost: one paginated REST call per convergence-active item per poll cycle — acceptable for Fabrik's use case (same as the existing CI timeout check).

### `checkAutoMergeConvergence` as a Phase 1 Catch-Up Handler

A new `checkAutoMergeConvergence` function in `engine/merge_gate.go` is inserted at the start of the Phase 1 catch-up loop in `poll.go`. When `fabrik:auto-merge-enabled` is present, this function handles all convergence logic and returns immediately, bypassing `checkMergeabilityGate`, `checkCIGate`, and Phase 2 entirely. This is consistent with how `fabrik:awaiting-ci` gates the CI path.

Decision tree in `checkAutoMergeConvergence`:
1. PR merged or closed → remove label, advance to Done via existing machinery
2. GitHub auto-merge disabled by user → remove label, post one-liner, treat as cruise
3. Budget exhausted → `pauseForConvergenceFailed`
4. Mergeable = UNKNOWN → wait (GitHub still computing)
5. Mergeable = CONFLICTING, no worker in-flight → dispatch rebase reinvoke (cycle++ for observability, NOT as a gate)
6. Otherwise → wait (GitHub is handling it)

### Auto-Merge Re-Enable After Rebase Push

GitHub automatically disables auto-merge when any new commit is pushed to the PR branch. `dispatchRebaseReinvoke` is extended to call `EnablePullRequestAutoMerge` after a successful rebase push when `fabrik:auto-merge-enabled` is present. Failure to re-enable is logged as a warning and does not abort; the next catch-up cycle will detect auto-merge is disabled and re-enter the convergence flow.

### cruise > yolo Precedence

The `attemptMergeOnValidate` function checks `hasCruiseLabel` before calling `enablePullRequestAutoMerge`. If cruise is present (regardless of yolo co-presence), the function returns without enabling auto-merge. This extends the existing precedence rule to the auto-merge path.

## Alternatives Considered

**Continue using `MergePR` with better retry logic**: Adding smarter backoff and state re-sampling to the `MergePR` path would reduce (not eliminate) the race window. It does not address the fundamental problem: the merge decision is made client-side by sampling two independently-changing states. Rejected: the race is structural, not fixable by retry tuning.

**Replace cycle counts with per-gate timeouts**: Instead of a single convergence budget, replace `MaxRebaseCycles` with a rebase timeout and `MaxCiFixCycles` with a CI-fix timeout. This keeps the two gates independent but adds two new timeout configurations and still doesn't address the root cause (multiple gates composing non-deterministically). Rejected: complexity without correctness gain.

**Store budget start time in itemstate**: `LinkedPRState` already has `CIMergePendingSince time.Time` as a precedent. Adding `ConvergenceBudgetStart time.Time` would be fast (no API call) and clean. Rejected: in-memory state resets on restart; the guarantee "budget starts when label is applied" is only enforceable if the timestamp survives restarts.

**Dedicated `fabrik:convergence-started` label for the timestamp**: Using a separate label solely for its creation timestamp would cleanly separate concerns from `fabrik:auto-merge-enabled`. Rejected: adding a second label adds label proliferation and confusion; the creation timestamp of `fabrik:auto-merge-enabled` already captures "when did convergence start" semantically.

**Pause clock during UNKNOWN mergeability window**: After a rebase push, GitHub transiently returns `UNKNOWN` mergeability. Pausing the budget clock during UNKNOWN would give the PR more effective convergence time. Rejected: the UNKNOWN window is typically <60s; the 30-minute budget makes this negligible; adding clock-pause logic adds complexity for minimal gain.

## Consequences

- One additional REST call per convergence-active yolo item per poll cycle (`FetchLabelAppliedAt` for budget start). Identical cost to the existing CI wait timeout.
- GitHub auto-merge must be enabled at the repository level (Settings → Allow auto-merge). If not, `enablePullRequestAutoMerge` returns an error; Fabrik logs guidance and retries on the next poll.
- Per-gate cycle counts (`MaxRebaseCycles`, `MaxCiFixCycles`) remain in the codebase as observability counters and as the backstop for non-yolo issues. They are not removed.
- `FABRIK_CONVERGENCE_BUDGET=0` is the backward-compatibility escape hatch for operators who prefer the legacy cycle-limit behavior.
- The `GitHubClient` interface gains `EnablePullRequestAutoMerge` and `FetchCommitsBehind` — all implementations (real client, mock, test stubs) must implement both.
- `fabrik:auto-merge-enabled` must be added to `staticLabelDefs` for auto-creation at startup.
- The convergence-failed pause comment names the actual blocking state, replacing the previously misleading per-gate messages.
