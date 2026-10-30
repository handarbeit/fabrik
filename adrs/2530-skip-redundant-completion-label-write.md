# ADR 2530: The CI gate clear does not re-add a completion label the item already carries; a periodic-only re-evaluation is not poll activity

**Status:** Accepted
**Date:** 2026-10-30
**Issue:** [#2530](https://github.com/handarbeit/fabrik/issues/2530)
**Related (mentioned for context):** #2528; ADR 1945, ADR 2096, ADR 2180, ADR 2158, ADR 2343.

## Context

An item parked at `stage:Validate:complete` (observed under `fabrik:cruise`, waiting for a human merge) is admitted by the periodic safety net (`periodic-re-eval`, `10 × PollSeconds`, ADR 2096). Catch-up Phase 1 `handleMergeAndCIGates` reaches `checkCIGate` unconditionally; with a clean PR the gate takes the `PRMergeReady` branch and calls `addCompleteLabelAndRemoveCI`, which re-POSTed the completion label that was already on the issue.

GitHub records no timeline event for re-adding a present label but bumps the issue's `updated_at` (verified in this repo: a 6 s move, no event). So every re-evaluation cost a label write, moved `updatedAt`, and — through the poll's `deepFetched > 0` — logged `activity detected — idle backoff reset`, so a daemon whose only work was such items never backed off. On a `poll: 180` daemon one item did this for about 26 hours.

## Decisions

1. **Skip the add in `addCompleteLabelAndRemoveCI`** when `item.Labels` already holds `stage:<name>:complete`. All three clear branches of `checkCIGate` (no PR, merged, ready) go through this function, so one place covers them. The gate itself is *not* skipped: the same function detects CI failure and conflict after completion and dispatches `ci-fix-reinvoke` / `rebase-reinvoke`. The `ApplyLabelAdded` write-through is skipped with the write, because no write happened and an engine-write record would shield a listing for 30 s for nothing. `removeAwaitingCILabel` still runs, so a complete item that also carries `fabrik:awaiting-ci` is still cleared.
2. **The validate completion SHA is still re-recorded on every clear** (FR-004: "keep its current meaning"). A re-evaluation whose head moved while CI is green therefore still re-blesses the new head before `settleSHAInvalidationScan` runs, exactly as before. Whether the SHA should be recorded only when the label is newly added is a separate question (follow-up below).
3. **Presence comes from `item.Labels`** (cache under the ADR 2180 freshness rule, or a live deep fetch). An external removal bumps `updatedAt` past the baseline and the cache goes stale; any doubt runs the add as before. *Rejected: `liveLabels`* — one REST read per re-evaluation to close only a same-second external-removal window, the documented ADR 1945 limit.
4. **The remaining add goes through `addLabelChecked`** (echo reservation, write, `syncLabelAddBy` → `observeSelfWrite`, applied-at), replacing the bare `client.AddLabelToIssue` + `ApplyLabelAdded` idiom, which never raised the self-write baseline (ADR 2180 requires self-writes to go through `observeSelfWrite`). The early return on error that preserves `fabrik:awaiting-ci` is unchanged.
5. **Periodic-only admissions are not poll activity.** `selectDeepFetchCandidatesCounted` also returns how many deep-fetched items were admitted *only* by an expired `periodic-re-eval` (not in `cycleSet`, no bypass label, not cleanup, not a first sighting). `pollResult.Active` uses `activeDeepFetches(deepFetched, periodicOnly)`. Webhook- and observer-driven admissions (`cycleSet`), bypass labels, cleanup items and first sightings still count. A periodic re-evaluation that acts shows up as a dispatch or as a Store change that puts the item in the next poll's `cycleSet`, so the reset can be one poll late. *Rejected: counting GitHub writes* — it would have to cover every mutation.
6. **Seams** (per-Engine): `SetCompleteLabelSkipDisabledForTest`, `SetPeriodicActivityNarrowingDisabledForTest`.
7. **Sim fidelity.** `simgh.AddLabelToIssue` on a present label now bumps `updatedAt` and the write stamp with no event and no applied-at change (`FIDELITY.md`, "Re-adding a present label bumps `updatedAt`"); before this the sim could not reproduce the defect.

## Why drift fired despite the ADR 1945 baseline (requirement 3)

- `AddLabelToIssue` *does* record a GitHub-anchored stamp (the `Date` header). The gap was in the engine: the direct idiom at the gate clear (and in `stages.go`, `pr_terminal_advance.go`) never called `observeSelfWrite`, so `LastSeenSourceUpdatedAt` was not raised (Gap A). Fixed here for the gate clear (decision 4).
- `LightReconcile` compares the cached `UpdatedAt`, Status and label set against the listing; it does not consult `LastSeenSourceUpdatedAt`, and its only discount is the 30 s engine-write shield (`EffectiveShallow`). Cached `UpdatedAt` is moved only by listings and probes (Gap B).
- **Gap B was not reproduced in the sim.** The twin runs a light reconcile after every poll, 45 s after the poll (outside the shield), with the skip neutralised and even with the bare idiom restored: zero drift. Per the plan, no `LightReconcile` change was made; the redundant write that produced the `updatedAt` bump is gone, which is what the issue's log shape needs. If the drift is seen again with no engine write, the next step is a baseline-aware comparison in `LightReconcile` guarded by a test that a later external edit still drifts.

## Audit of label adds on a re-evaluation of an unchanged item (requirement 2)

| Site | Disposition |
|---|---|
| `ci.go` `addCompleteLabelAndRemoveCI` | **Fixed** (decision 1) |
| `ci.go` `awaiting-ci` adds | Guarded by `hasLabel` |
| `reviews.go` `awaiting-review` adds | Guarded |
| `reviews.go` `bot-reprompted` | Bounded by the ladder state |
| `merge_gate.go` `rebase-needed` | Guarded |
| `stages.go` (221–228, 350–359, 800) | Guarded |
| `no_work_needed_settle.go`, `pr_terminal_advance.go:192` | Guarded |
| `dependencies.go` `fabrik:blocked` | Guarded by `!alreadyBlocked` |
| `stages.go` `advanceToQueued` | One-shot advance, not a re-run path |
| `comments.go` rework restore | The label was withdrawn; the add is real |
| `item.go` `handleCleanupStage` `addLabel(completeLabel)` | Returns at the top when the label is in `item.Labels`; terminal items are also skipped by admission. Cannot repeat for an unchanged terminal item |
| Episode-gated (`tools-denied`, `claude-limit`, `api-key-helper-detected`, `toolchain-stale`), spawn, pause, landing-verification, lock/in-progress | Gated on label absence or dispatch-time only |

## Follow-ups (not done here)

- `item.go` `handleCleanupStage` calls `removeLabel(fabrik:extend-turns)` unconditionally (a removal, so out of scope).
- The bare-idiom adds in `stages.go` (228, 265, 359, 802, 817, 899) and `pr_terminal_advance.go:195` skip `observeSelfWrite`. They are not on a re-run path, and routing them changes when the next deep fetch happens.
- Record the validate SHA only when the completion label is newly added (lets the SHA-invalidation scan see a head that moved after completion).

## Consequences

- A parked Validate-complete item costs no label write per re-evaluation, and the idle backoff engages when only such items remain.
- The residual risk is a stale `item.Labels` (an external removal in the same second as a self-write) leaving the item without its completion label until its next real admission.
