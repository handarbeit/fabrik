# ADR 2613: Clear `fabrik:rebase-needed` once the head is mergeable, while CI is pending

## Status

Accepted. Amends the "no label churn on pending" wording of ADR 032 (032-ci-gate-conjunctive-completion-label.md) R10c as built, and the clearing-owner invariant in `docs/state-machine.md` §6.6.1. Restates community report #2608 (mentioned for context: #2608).

## Context

`checkMergeabilityGate` removed `fabrik:rebase-needed` only on `PRMergeReady` and `PRMergeBlocked`. On `PRMergeUnsettled` (checks pending on a freshly pushed head) it claimed the item and left the label, although GitHub already reported `mergeable: true` / `mergeable_state: blocked`. On a ~40-minute CI the item read as "needs a rebase" for most of an hour, and `fabrik_status.attention.reasons` said the engine was dispatching a rebase.

## Decisions

1. **Signal:** `settle.Status == PRMergeUnsettled && settle.MergeObs == MergeObsComputed`. `settlePRMergeStateInner` returns `PRMergeConflicting` before every other unsettled result, so a computed observation on an unsettled result proves mergeable is true and the state is not `dirty`. No new field and no extra GitHub call. `MergeObsUnknown` (R2) and `MergeObsNone` (read error) never clear; `PRMergeQueued` is out of scope.
2. **Only the label changes.** The claim, `noteGateHold`, the `merge-unsettled` cooldown, `checkUnknownMergeBound` and rebase cycle accounting are untouched (R3). Removal goes through `applyLabelRemove`; a failure is logged and retried next poll.
3. **Attention reason (R4)** uses explicit recorded facts, never gate reason text (ADR 2121): `GateHold.NoConflict` (set by the merge claim from `MergeObs`) and `Input.RebaseDispatched` (the stage's rebase-cycle count above zero). With the label present, no worker, not `after-landing`, a merge hold with `NoConflict` and a dispatched rebase, the summary is `rebase pushed; waiting for CI on the new head`. All other cases keep the label text. A new "rebase pushed" store mutation was rejected as coupling to rebase internals for a window R1 already shrinks. Both facts are memory-only. The `PRMergeConflicting` branch drops the recorded merge hold (store only), so a returning conflict never keeps a stale `NoConflict` fact.
4. **Seam:** `SetRebaseLabelClearOnPendingDisabledForTest` (per-Engine) restores the old behaviour for the non-vacuity tests.

## Consequences

- A stale or lagging `mergeable: true` can clear the label, and a later `dirty` read re-applies it. Dispatch stays on `PRMergeConflicting`, so only the label and text flap. The exposure already existed on `PRMergeReady`.
- Clearing drops `RebaseCause`; a returning conflict re-applies the label without the `after-landing` cause.
- The label is a deep-fetch admission bypass; after clearing, re-admission depends on the `merge-unsettled` cooldown recorded in the same claim (covered by the sim twin).
- `validate-settled` can announce, and a `rebase-needed` withdrawal end, sooner.
- Follow-up, not done here: `checkAutoMergeConvergence`'s unsettled branch (yolo, `fabrik:auto-merge-enabled`) can still show a stale label while CI is pending.
