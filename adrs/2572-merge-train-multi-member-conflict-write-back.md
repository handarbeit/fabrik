# ADR 2572: Merge-train multi-member trial conflict write-back

## Status

Accepted. Extends ADR 2044 (singleton catch-up) to multi-member trials; reuses ADR 2065 (policy memo) and ADR 2158 (SHA-invalidation exemptions). Context: #2111 (watch item) and #1826 (the original proposal), mentioned by number only.

## Context

When a merge-train trial has two or more members and one conflicts while the trial branch is assembled (`assembleTrialBranch`), Claude resolves the conflict on the disposable trial branch. The resolution survives only if that trial lands. An abandoned, bisected or rebuilt trial, a forgotten rerere resolution (`forgetPoisonerResolutions`) or an ejected member loses it, and the next trial — or the member's own PR — meets the same conflict again. #2111 fired once at the v0.0.92 cut: a trial rebuilt after its base moved, one member conflicting with the new base. The ADR 2047 overlap filter cannot prevent it because the conflict is member-versus-moved-base, not member-versus-member.

## Decisions

1. **Write back from the single assembly point.** After `resolveTrainConflict` resolves a conflict in a trial whose batch has more than one member, `tryWriteBackAndContinue` merges the pinned base into the member's own branch and pushes it. Both train drivers and the `landGreenBatch` rebuild loop reach `assembleTrialBranch`, so no per-driver code exists. One-member batches (including bisection halves) are not written back; singletons keep ADR 2044.
2. **Replay, never derive.** The merge in the member's worktree runs `resolveTrainConflict` with `InvokeOptions.noClaude`: rerere replay and generated-file regeneration only. A conflict that would need Claude is a skip. This writes "the same resolution" and avoids a second Claude call that could produce a different one. The cost: when an earlier member touches the same region the member-versus-base preimage differs from the trial's, and the write-back is skipped in exactly that harder case. Copying the trial's resolved files onto the member branch was rejected as unsafe (they contain other members' changes).
3. **Only write back a conflict against the base.** A clean base merge into the member means the trial conflict was member-versus-member; pushing a pure catch-up then would be churn, so it is a skip.
4. **Same ownership, lease push and policy memo as the catch-up.** `catchUpBusyReason`, `PrepareCatchUp`, `PushCatchUp` (`--force-with-lease` on the verified head), the shape verification and the `GH006`/`GH013` classification are reused unchanged. A policy rejection sets the shared per-repo memo, so write-back and singleton catch-up back off together. The feature follows `singleton_catch_up: off`; no new operator setting exists. Each `(train, member, base SHA)` is attempted once per process, which bounds pushes on a busy base.
5. **Best-effort and uncharged.** Every skip or rejection leaves the trial exactly as today: no ejection, retry, trial-rate entry or pause, and no abort of assembly. Exactly one log line per attempt names member, base SHA and outcome.
6. **Trial content is invariant.** After a push, the trial is rebuilt on the member's new head only if the merge is clean and the tree is byte-identical to the trial's own resolved tree; otherwise the resolved commit is restored and the survivor keeps its original head. The persisted run record, prefix chain and landing therefore stay consistent.
7. **Marker `kind=writeback`, always `pure=false`, no new suppression.** The marker reuses the catch-up format with an optional `kind=writeback` token (plain markers are byte-identical). A write-back contains a conflict resolution, which is by definition not pure, and `catchUpSuppresses` requires `pure=true`; muting reviews of unreviewed resolved content would be unsafe, so reviews of a written-back head stay actionable, as for a conflict-edited singleton catch-up. The marker's job is recognition.
8. **Validate SHA exemption.** `headIsTrainWriteBack` (same trust rules as the pure catch-up: engine-authored marker, exact live head, trailer never trusted) exempts a written-back head from the SHA-invalidation scan. Without it the non-pure head would strip `stage:Validate:complete` and re-run Validate on a member Queued inside a live trial, which can loop with each base move; the trial validates the head. A seam neutralises it.

## Consequences

- The resolution outlives an abandoned, bisected or rebuilt trial in the replayable case, and the next trial assembles that member with no conflict and no Claude call.
- The member's own PR branch carries a Claude-derived resolution that only the trial's CI (and later the member's own CI) validates; reviews of it are actionable.
- In the divergent-preimage case the benefit is lost and the behaviour is today's. Recorded as a conscious trade-off.
- Out of scope: singleton trials, `landOneAtATime`, one-member bisection halves, preventing member-versus-moved-base conflicts.

## Verification

`engine/merge_train_writeback_test.go` (real-git half, gate, marker, one log line, uncharged skips), `engine/sha_invalidation_test.go` (exemption and seam), and the sim twin `tests/sim/mergetrain_writeback_test.go` (sync and asynchronous driver; non-vacuity via `SetTrainWriteBackDisabledForTest`).
