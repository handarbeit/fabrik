# ADR 2491: Every automatic route out of a `wait_for_reviews` stage consults the review verdict

## Status

Accepted

## Context

Community report #2453 found items leaving a `wait_for_reviews` + `review_authority: authoritative` stage while a change request stood: after a base conflict and an in-stage rebase, or seconds after a fix round, with the previous poll's log still saying "authoritative gate still blocking".

Tracing every route showed one that skips the verdict. `handleReviewGate` runs `checkReviewGate` only when `stage:X:complete` was present before the Phase 1 handler chain (`hasComplete` is frozen, #617). On a `wait_for_ci` stage that label is added by `checkCIGate` later in the same chain, so on the pass where CI clears the verdict is never asked, and `runCatchUpPhase2` calls `advanceToNextStage` for a non-Validate stage. ADR 1216 closed the same hole for the Validate landing by putting the gate inside `attemptMergeOnValidate`; the non-Validate advance never got the equivalent. The base-conflict route is not a separate bypass: `checkMergeabilityGate` only dispatches an in-stage `rebase-reinvoke`; the bypass is what follows it (rebase pushes, CI clears, Phase 2 advances).

## Decision

1. **Gate the advance in `runCatchUpPhase2`**, after the pause guard and the feedback gate and before `advanceToNextStage`. Both the main catch-up loop and `settleAwaitingCIScan` converge there, so one placement covers both, and it mirrors ADR 1216. Letting `checkReviewGate` run when `!hasComplete` would reopen #617's stale-snapshot concern.
2. **Reuse `reviewGateHold`** (through `reviewGateHoldFor`), the read-only core the landing gate and `fabrik_advance` already share, so no third verdict implementation can disagree with the others. It fails closed on an unreadable PR or review state. `checkReviewGate` is not refactored onto it: it owns the bot re-prompt ladder and that is a larger change than this issue needs.
3. **A hold is a plain hold.** It applies `fabrik:awaiting-review` (the anchor of the review wait clock, ADR 2467) and records a `review-blocked` cooldown, like `handleReviewGate`. No pause, failure state or counter is added: `stage:X:complete` is present, so the next poll's `checkReviewGate` owns the timeout and escalation, and `MaxRebaseCycles` still bounds a base-conflict loop.
4. **The log names the decision taken.** `reviewGateHoldFor` takes a route noun: the landing and overseer callers keep `holding landing decision …` byte for byte, Phase 2 logs `holding advance on PR #N — …`, and a cleared gate logs `review gate clear — advancing`, so an advancing poll never carries a "blocking" line.
5. **The rule cannot silently regress.** `TestEveryAdvanceRouteIsReviewGatedOrAudited` pins every `advanceToNextStage` caller: gated in its function, or audited with a reason (merged/converged PR, no-work, closed item, awaiting-advance retry, merge-train landing).

Stages without `wait_for_reviews` pay no read and log nothing; `advisory` stages behave as the verdict rule already makes them. The verdict rule (#2465), the declined-finding pause (#2466) and the wait clock (#2467) are consumed unchanged.

## Consequences

An item no longer leaves a `wait_for_reviews` + `wait_for_ci` stage on the pass CI clears while the verdict blocks. Cost: one live review read on a non-Validate advance of a review-gated stage. Seam: `SetReviewGateAdvanceDisabledForTest` (per-Engine). Tests: `engine/review_advance_gate_test.go`, `engine/advance_routes_scan_test.go`, sim twins `tests/sim/review_gate_advance_test.go` (each as shipped and neutralised). The reporter's data could not be replayed verbatim; the twins reproduce the structure of the post-fix-round and not-mergeable shapes.

Related: ADR 1216, 1270, 1250, 2126, 2341, 2465, 2466, 2467; `docs/state-machine.md` §6.6.6.
