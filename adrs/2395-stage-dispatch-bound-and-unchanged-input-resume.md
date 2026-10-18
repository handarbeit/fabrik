# ADR 2395: Bound stage dispatches per episode, and do not resume a large session on an unchanged re-dispatch

**Status:** Accepted
**Date:** 2026-10-18
**Issue:** [#2395](https://github.com/handarbeit/fabrik/issues/2395) (mentioned for context: #2388, #1191)

## Context

`max_retries` counts failed runs, `max_turns` bounds one invocation and `max_slice_retries`
(ADR 1199) bounds turn-cap slices. A stage whose every run *succeeds* but leaves nothing
durable behind reaches none of them: the run emits `FABRIK_STAGE_COMPLETE`, the write that
records it (`stage:<X>:complete`, or the `fabrik:awaiting-ci` arm) fails, and the next poll
dispatches the stage again. In #2388 one Research stage ran about 150 times, and because the
defaulted `resume_session: resume` resumed the same session each time, the context carried
into every run grew with the run count. ADR 2300's size-aware rule does not help: a session
whose last run did not close is a continuation and always resumes.

## Decisions

1. **A per-item, per-stage dispatch counter for the current episode**, persisted in a JSON
   sidecar `<Stage>.dispatches` beside the `.session` pointer (the `.session.meta` /
   `.resumefails` precedent: atomic tmp+rename, mode 0600, corrupt = absent, best-effort).
   It is not in `.session.meta` because `updateSessionMeta` is a no-op without the pointer
   and a fresh decision deletes the pointer — exactly when a loop is running. It is not in
   `itemstate`, which is memory only (R3: a restart, including a self-upgrade re-exec, must
   not reset it), and not in a new `.fabrik/state/` registry, which would need a sim seam of
   its own: deriving the path from `sessionFilePathFor` reuses `SetSessionDirForTest` and
   `RestartEnv`.
2. **Counting** happens in `finalizeStageOutcome` at the `claudeRan` site beside
   `StageAttempted`: every dispatch that really ran a worker counts once, whatever its
   outcome. Pre-spawn refusals return earlier and do not count.
3. **Check before the lock, increment after the run.** `dispatchLimitReached` runs in
   `processItem` after the cooldown and before `acquireLockAndVerify`, so the bound is strict
   ("never more than N dispatches without a human"). At the cap `pauseForDispatchLimit`
   pauses with `fabrik:paused` + `fabrik:awaiting-input`, `EnginePaused` and one comment
   (stage, count, cap, last five outcomes, how to resume); no `stage:<X>:failed` label and no
   new label, attention cause or pause-cause marker — the plain pause pair is enough, and
   `pauseForSliceLimit` is the model.
4. **Reset only after durable state was written.** Resetting on the `completed` branch (or
   beside `StageRetryCleared`, which is applied there) would disable the bound for the very
   failure it targets. The reset sites are the successful complete-label write and the
   successful `fabrik:awaiting-ci` arm in `handleStageComplete`, `handleNoWorkNeeded`'s
   durable marker, and the explicit stage resets (`clearFailedStage`, `unblockAwaitingInput`,
   `reenterValidate`, `handleValidateSHAInvalidation`). A column change is detected without a
   hook: dispatching stage X deletes the other stages' records of the item (the known edge
   A→B→A with no dispatch of B keeps the count, which can only fire the cap early). A record
   flagged `paused` that meets an unpaused item was resumed by a human, also across a restart
   that lost `EnginePaused`, so it starts a new episode rather than re-pausing at once.
5. **Default 25, a constant in `cmd`.** A healthy episode can legitimately reach
   `max_slice_retries` (10) slices + `max_retries` (3) failures + the final run, about 14
   dispatches, so 10 would falsely pause it; 25 leaves margin and is about 6× below the
   incident. It is not derived from the other limits, so tuning them cannot silently move it.
   `0` disables. The default is applied in `cmd` because the engine cannot tell an explicit 0
   from unset.
6. **Invalid values are refused at startup**, for the flag, the environment variable and the
   YAML key, as the issue requires. This deliberately differs from the
   `--max-train-auto-repair-attempts` precedent (warn and use the default): a typo in a bound
   whose purpose is to stop runaway cost should not silently fall back.
7. **Unchanged-input resume exemption (R4).** The record also keeps the inputs the previous
   run saw: the worktree head SHA, a hash of the issue body and a hash of the comment set.
   `decideSessionResumeWith` makes a stage dispatch start fresh when its inputs equal the
   previous run's and the saved session's measured size is known and above
   `resume_threshold_tokens`. It is checked after the reset-flag case and before the
   continuation case, so it **overrides the continuation exemption of ADR 2300 decision 4**
   and applies in every `resume_session` mode, including the default `resume`. The reason
   keeps the `fresh session` prefix, which `freshOverSaved` keys on to drop the stale pointer
   and write `stage-<Stage>.md`. The cost, accepted: a turn-capped no-commit slice whose
   session is over the threshold restarts fresh each slice (bounded by `max_slice_retries`
   and the new cap, and the fresh run gets its own previous output as context).
8. **Fingerprint rules.** Comments starting with `🏭 **Fabrik` are excluded — the engine
   posts a fresh stage comment every run, so including them would make every re-dispatch
   look changed and R4 would never fire. Every other comment (human or bot review) is
   included by ID and body, so a new or edited one is a real input change. Anything unknown
   — no previous record, an empty head SHA on either side — reads as changed (fail-safe
   toward the #2300 rule). A restart rebase can move the head of a no-commit branch, so
   across a restart the exemption is best-effort.

## Consequences

- A stage cannot be dispatched more than `max_stage_dispatches` times in one episode without
  a human; the count survives restarts (sim `RestartEnv`).
- Cost accounting is out of scope (`CostUSD` is notional on subscription billing); the
  dispatch count is the bound. A cumulative budget across resumed retries stays #1191.
- The root cause of an unrecorded completion is not addressed here; this only bounds it.
- A sidecar write failure loses the bound for that run (logged; never blocks the dispatch).
- The session janitor ages out `*.dispatches` files (not for in-flight items), so an
  abandoned loop's record cannot keep an issue's session directory alive.

## Seams and tests

`SetStageDispatchCapDisabledForTest` and `SetUnchangedInputExemptionDisabledForTest`
(per-Engine). Unit tests: `engine/stage_dispatch_test.go`, `engine/session_resume_test.go`;
config plumbing: `cmd/cmd_envvar_test.go`; sim twins: `tests/sim/stage_dispatch_cap_test.go`.
See `docs/state-machine.md` §7.21 and `docs/stage-lifecycle.md` "Whether to resume".
