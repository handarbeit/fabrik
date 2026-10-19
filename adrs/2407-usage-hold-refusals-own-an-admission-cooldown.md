# ADR 2407: A usage-limit refusal of a comment review or reinvoke owns an admission cooldown

## Status

Accepted. Extends ADR 2232 to the comment-review and re-invocation family; closes a gap left by ADR 2096.

## Context

On 2026-10-19 the dev daemon hit the Claude account usage limit three times. Two items showed opposite failures of the same omission:

- **#2350 was stranded.** An expired `periodic-re-eval` cooldown admitted it; ADR 2096 consumed that cooldown. Catch-up Phase 1 then dispatched a review reinvoke (`dispatchWithCycleLimit`), which marks the item in `advancedItems`, so the `poll()` defer re-stamps no `periodic-re-eval`. The ADR-1120 gate in `processCommentsClassified` refused the reinvoke and recorded nothing. The item was left with no cooldown and no change, so nothing ever admitted it again. It sat from the hold's lift at 08:29Z until an unrelated board edit at 08:49Z, and again after the 10:26Z lift until a human comment.
- **#2351 looped.** It was in `settleAwaitingCIScan`, which is board-sourced and runs every poll. Each poll dispatched a review reinvoke and the hold refused it inside the worker goroutine. Each `WorkerExited` woke the poll, so the daemon re-dispatched and re-refused about every five seconds: 565 times in one hour-long hold.

The stage path already followed ADR 2232: its usage-limit exit records `retry-backoff` until the later of the backoff and the hold's end. The #2393 forge hold, one branch below the usage-limit gate, records `lock-write-failed`. The comment and reinvoke family recorded nothing.

## Decision

1. **Refuse a reinvoke before it becomes a worker.** `dispatchReinvoke` checks the account-wide hold beside the forge hold, before `WorkerEntered` (`usageHoldReinvokeRefused`, `engine/usage_hold_admission.go`). It refunds the caller's cycle through `refundDidNotRunCycle` (kind `account-usage-limited`) and logs once per item through `logfThrottled`. No worker means no `WorkerExited`, so nothing wakes the poll.
2. **The refusal owns a cooldown expiring at the lift.** It records `retry-backoff` until the hold's deadline (`recordUsageHoldCooldown`). `PollWithBackoff` already caps the idle interval at the earliest short cooldown, so the first poll after the lift admits the item. The same cooldown is recorded by the comment path's pre-invocation gate and by its mid-run usage-limit exit.
3. **Every did-not-run refund keeps an admission path.** `refundDidNotRunCycle` re-arms `periodic-re-eval` (`rearmPeriodicReEval`). This is the third "admitted, then dropped" case ADR 2096 R4 requires, next to its two drop sites, and it covers every did-not-run kind (usage limit, API error, unresolved skill, `apiKeyHelper`, forge hold) without a kind-specific rule.
4. **The forge-held reinvoke refusal records its cooldown too** (`recordLockWriteCooldown`), matching the comment path's forge gate.

An awaiting-ci item's settle scan still reaches the refusal once per poll during a hold. That costs one cheap refusal and the landed-PR read in `dispatchWithCycleLimit`, but no worker and no wake.

## Consequences

- No item stays unadmitted after a usage hold lifts, and a hold no longer drives a poll loop.
- The `DidNotRunReinvokes` tally grows by one per poll during a hold, not per wake.
- Seam: `Engine.SetUsageHoldReinvokeGateDisabledForTest` restores the pre-#2407 behaviour (worker entered, nothing recorded, no re-arm).
- Tests: `TestReviewReinvoke_UsageHold_NoWorkerAndAdmissionPathKept` (engine, both arms) and the sim twin `TestUsageHold_RefusedReviewReinvokeRunsAfterTheLift` (both arms; the neutralised arm reproduces the #2350 stranding).
