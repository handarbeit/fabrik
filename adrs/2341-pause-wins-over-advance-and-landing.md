# ADR 2341: A Pause Wins Over Every Automatic Advance and Landing

## Status

Accepted

## Context

The engine state audit (`docs/design/engine-state-audit-2026-10.md` §1.1) found that `fabrik:paused` did not stop a landing. `handleStageComplete` and `runCatchUpPhase2` refresh labels live, and `attemptMergeOnValidate` re-reads the whole item live, but none of them, nor `landValidated`, `advanceToNextStage` or `advanceToQueued`, looked at the pause. The only filter was the poll loop's snapshot check, so a human pausing a yolo item during its Validate run, or before the listing showed the label, did not prevent the merge or the move to Queued. A merge-train member paused after its batch formed was still landed, because members are fixed at formation.

## Decision

1. **Caller-side checks, primitive untouched.** A shared `refuseIfPaused` (`engine/pause_guard.go`) is checked at the pre-merge decision sites: `handleStageComplete`, `attemptMergeOnValidate`, `landValidated`, `advanceToQueued` and `runCatchUpPhase2`'s non-Validate advance. `advanceToNextStage` is not guarded: it is also the Done move of an already-merged item (`recordAdvanceOutcome`, the train landing paths), and refusing there would strand a landed item in Validate or Queued. `tests/sim/paused_merged_pr_recovery_test.go` and a unit test pin the exemption.
2. **A refusal is a clean no-op.** One `[#N pause-guard]` log line; no label write or removal, no comment, no cooldown, no retry increment. Re-admission is the pause label's removal (ADR 1813), after which the retained `stage:<X>:complete` lets Phase 2 or the settle scan decide again (R4). A refused landing returns the existing `deferred=true` shape so `handleStageComplete` never falls through to an advance; `advanceToQueued`, which returns only an error, uses the sentinel `errLandingPaused`.
3. **Two reads in `attemptMergeOnValidate`.** The `FetchItemDetails` re-read replaces the REST-fresh `item.Labels` with GraphQL ones that can lag a REST write in either direction. The check runs before the re-read (REST-fresh labels) and again after a successful one, so a pause visible in either live source refuses. No extra API call is made. `landValidated`'s direct-merge fallback checks the labels of its own last re-read before `MergePR`.
4. **Merge-train landings refuse the whole batch.** A batch lands as one integration PR containing every survivor's changes, so a member paused after formation cannot be left out of an assembled PR. `refuseIfMembersPaused` runs strictly before the merge in `landMergeTrainBatch`, `landSingleton` and `trySingletonFastPath`, and is skipped when the PR is already merged (restart safety: a resumed landing finishes its Done move). Members stay Queued and nothing is ejected, counted toward `MaxMergeTrainEjections`, recorded as a trial, paused or commented. The paused member is already excluded from new batches by `groupQueuedItemsByRepoUnordered`, so the next poll re-forms a batch without it; a paused member therefore delays its batch by at most one re-form. Re-using the pending-eject signal or dissolving the batch was rejected: both need a mutation R2 forbids or risk charging the ejection counters.
5. **Live per-member reads, snapshot fallback.** Members hold only the labels snapshotted at formation, so the train check reads `e.client.FetchLabels` (never the boardcache-backed `readClient`), one call per member per landing. When a read fails it falls back to the member's snapshot labels: a transient error leaves the check no weaker than before this change instead of stalling every landing on it, and the landing is re-evaluated on the next poll either way. This differs from `refuseIfBaseContradictsMembers` (ADR 1773), which fails closed because it guards against landing on the wrong base; here the cost of a false refusal is a stall and the guard is an operator stop button, not a data-integrity check.

## Consequences

- No atomic guarantee. A pause applied between the last read and the merge call cannot be prevented; "the labels the site already read" is the contract (R1).
- `reenableAutoMergeAfterRebase` and `reEnqueueOrPause` can still merge or enqueue after a worker that ran while a pause was applied. They are reached only through the paused-item poll filter and were outside the issue's named sites; recorded as a follow-up.
- A live e2e case, which the audit also asks for, is not added here; the sim twins (`tests/sim/pause_mid_validate_test.go`, `tests/sim/mergetrain_pause_test.go`) carry the non-vacuity proof through `SetPauseGuardDisabledForTest`.
- Refines ADR 1813 (what a pause blocks) and ADR 1953-a (the same fail-before-write ordering for gates that record cooldowns).
