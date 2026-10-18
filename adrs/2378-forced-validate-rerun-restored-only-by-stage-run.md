# ADR 2378: A forced Validate re-run is restored only by a Validate stage run

## Status

Accepted. Amends ADR 2343 (R3), ADR 2220 (`stageRanOnEntry`) and ADR 2280 (the comment path's non-completing outcomes).

## Context

ADR 2343 R3 re-runs Validate when no completion SHA can be recovered after a restart, by stripping
`stage:Validate:complete` (`handleValidateSHAInvalidation`). The head-moved scan, `fabrik:revalidate` and the
merge train's auto-repair strip the same label for the same reason. Nothing recorded *why* the label was gone:
afterwards the item looked like any Validate that had not completed.

With a human comment pending, the comment-review pass for Validate ran next. `stageRanOnEntry` (ADR 2220) was
true because the earlier run's stage comment still existed (`findStageComment` matches the header only), so the
comment went to the comment path; the pass emitted `FABRIK_STAGE_COMPLETE` and `finalizeComments` →
`handleStageComplete` restored the label (directly, or through `fabrik:awaiting-ci` and the CI gate, which also
records a SHA). The item then landed with no Validate stage run (v0.0.88 gate, bed alpha #8586: stripped
15:47:03Z, a 4-turn "acknowledge only" pass restored it, merged 15:48:18Z).

## Decision

1. **A durable marker, `fabrik:validate-rerun`.** It must survive a restart (FR-004) and be visible to a live
   read, which rules out the Store, `validateSHARerun` and the session-meta `Reset` flag (a resume hint that
   comment passes never consume, bound to a session file). Deriving it from "no label plus a stage comment"
   would also match a Validate that blocked on input and was resumed by a comment pass, which must keep
   completing (FR-006).
2. **Set before the strip.** `markValidateRerun` runs first in `handleValidateSHAInvalidation` and
   `reenterValidate`; a crash between the two writes leaves marker-and-label (safe), not neither. A failed add
   defers the whole strip.
3. **Cleared by a Validate stage run only.** `handleStageComplete` for Validate calls `clearValidateRerun` once
   the completion label is added or `fabrik:awaiting-ci` is armed (never before a durable completion signal,
   the ordering principle of ADR 1802). The comment path never reaches `handleStageComplete` while the marker
   is set (decision 4), so a call that does is a stage run.
4. **A Validate comment pass cannot complete the stage while the marker is set.** `processCommentsClassified`
   reads the labels live (`validateRerunPending`, fail closed on a read error, like the pause guard) and, when
   the pass emitted the completion marker, treats it as not completed: the comments are still reacted to and
   marked processed, `handleStageComplete`/`ensureDraftPR`/`markPRReady` are skipped (so neither the label nor
   `fabrik:awaiting-ci` is armed — one guard covers both restore routes), and a rework bracket opened from a
   stale snapshot is closed without restoring the completion label. Like ADR 2280's blocked-on-input arm it
   bypasses both comment breakers and returns `commentRunCompletionSuppressed`, which `dispatchReinvoke`
   treats as a cycle refund without `opts.after`.
5. **Stage first.** While the marker is set `stageRanOnEntry` is false for Validate (except for an
   engine-paused or resumed item), so a comment arriving after the strip waits for the Validate stage run
   (ADR 2220). The pending comment is processed either way, and landing needs `stage:Validate:complete`, so it
   waits for both; no extra landing guard.
6. **Scope.** The marker is set by every path that strips the label to force a re-run (the issue's
   assumption), not only R3. The SHA rebuild rule is unchanged. `fabrik:validate-rerun` is in
   `transientLifecycleLabels`, so a closed issue does not keep it.

## Consequences

- One new label (`attention.MeaningOf`, `docs/state-machine.md` §1.1/§1.4, `LABELS.md`, CLAUDE.md).
- A Validate comment pass that resumes a blocked Validate while the marker is set will not complete it; the
  next Validate stage run does, at the cost of one extra run. The comment is already processed, so there is no
  loop.
- A transient label-read failure on a Validate comment pass defers its completion (fail closed); the label is
  restored by the rework bracket or the next Validate run.
- Neutralisation seam: `Engine.SetValidateRerunGuardDisabledForTest` (per-Engine). Sim twin:
  `tests/sim/validate_sha_restart_test.go` (`PendingCommentDoesNotSatisfyForcedRerun` and its non-vacuity run).
