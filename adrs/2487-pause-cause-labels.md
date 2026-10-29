# ADR 2487: Pause Cause Labels; `fabrik:awaiting-input` Keeps Marking Every Engine Pause

**Date**: 2026-10-28
**Status**: Accepted
**Issue**: #2487 — put the pause cause on the issue as a label; reserve `awaiting-input` for real questions (source: community report #2453)

## Context

Every engine pause applied `fabrik:paused` + `fabrik:awaiting-input`, whether a worker asked a question (`FABRIK_BLOCKED_ON_INPUT`) or the engine stopped on a review or CI timeout, a cycle limit, a clean daemon stop, a breaker or a failure. On the board a review-wait timeout looked identical to a worker waiting for an answer, and the reporter had to reconstruct causes from comment text. The cause existed internally (`attention.Result.Cause`, two pause-cause comment markers) but not where people look.

The issue asks for two things and allows either outcome of the second: (1) a cause label from a fixed vocabulary on every engine pause, written with the pause and removed with it; (2) `fabrik:awaiting-input` only on real questions — **or**, if Research shows narrowing it is unsafe, keep it and let the cause label alone carry the distinction.

## Decision

### 1. Keep `fabrik:awaiting-input` on every pause the engine applies today

Research found that narrowing is not a relabel. Resume has two paths:

- `isAwaitingInput` (paused + awaiting-input) → `unblockAwaitingInput`, which removes both labels and **unconditionally** resets the retry, cycle, session, dispatch and comment-breaker state (#1555).
- plain paused → `clearFailedStage`, reached only when the pause recorded `itemstate.EnginePaused` (or left a `stage:X:failed` label).

Only about 13 of the roughly 45 pause sites record `EnginePaused`. The rest — the review timeout, CI timeout, both comment breakers, the push guard, branch sync, the runaway guard, the unknown-merge-state bound, and others — rely on the awaiting-input path's unconditional reset. Dropping `awaiting-input` from them would send their resume down the plain path with nothing to reset, and the item would re-trip the same breaker or limit on the next cycle: the #1460 / #1555 defect, reproduced live once on #1208 within five seconds.

Narrowing safely would first require unifying the two resume paths (one reset for any engine pause whatever its labels), which changes plain-paused behaviour for human-applied pauses and touches `itemNeedsWork`, `processItem`, the classifier, channel events and the overseer guards. That is a change to resume semantics, which this issue rules out (FR-006), so it is **not** done here.

Consequences: `resumeAuthorised` (ADR-1813), `isAwaitingInput`, `unblockAwaitingInput` and every admission decision are unchanged. The cause label is the sole question-vs-not signal. The `fabrik:awaiting-input` label description and `attention` meaning are reworded to say it marks an engine pause, not a question.

**Follow-up (not done):** unify the resume paths, then narrow `awaiting-input` to question pauses (the `question` cause). The cause label makes that a later, separately reviewable change.

### 2. A closed vocabulary under one prefix, `fabrik:pause:`

| Label | Meaning | Sites |
|---|---|---|
| `fabrik:pause:question` | a question was put to a human | `blockOnInput` (`FABRIK_BLOCKED_ON_INPUT`), ungrantable-denial pause, spawn servability |
| `fabrik:pause:review-timeout` | review gate timed out | `pauseForReviewTimeout` |
| `fabrik:pause:ci-timeout` | CI gate, CI backstop or unknown-merge-state wait timed out | `pauseForCITimeoutNoted`, `pauseForCIBackstop`, `pauseForUnknownMergeState` |
| `fabrik:pause:cycle-limit` | a bounded loop reached its limit | review, CI-fix, rebase, enqueue, slice, tools-denied and stage-dispatch limits; merge-train ejection cap |
| `fabrik:pause:daemon-stop` | clean daemon stop interrupted the item | `pauseIssueForDaemonShutdown` |
| `fabrik:pause:breaker` | comment breaker or merge-train runaway guard | both comment breakers, `fireRunawayGuard`, its fallback alert |
| `fabrik:pause:failed` | a stage failed or retries were exhausted | `escalateFailedStage`, PR-creation failures, boundary violation, every `escalateSettle` caller, red merge-train singletons, stage-outcome publication failure |
| `fabrik:pause:other` | anything else | manual TUI stop, spawn failures, dependency cycle, push/branch refusals, skill load failure, clone failure, broken linkage, convergence and merge-group stalls, stranded train member |

The manual TUI stop is `other`, not `daemon-stop`, so the label agrees with `attention.CauseDaemonStop`. Names are well under GitHub's 50-character limit. Labels are created lazily by `AddLabelToIssue`; `github/labels.go` carries static colours and descriptions.

### 3. One choke point and a static scan

`pauseOpts` gains a required `cause`. `pauseIssue` writes `fabrik:paused`, then `fabrik:awaiting-input`, then the cause (`applyPauseCause`). `fabrik:paused` stays first: it is `resumeAuthorised`'s anchor, and a crash between the two writes leaves a pause with no cause — the same state as a pre-upgrade pause. `reapplyPauseLabels` and `pauseInterruptedIssue` take the cause as a parameter. The nine direct `fabrik:paused` writes that bypassed `pauseIssue` go through `applyPauseLabels` (or, for the merge train's raw-client write, `pauseMergeTrainMember`, which takes a cause).

`TestEveryPauseSiteDeclaresACause` is an AST scan over the engine sources: every `pauseOpts` literal, every call to a pause helper, and every raw `fabrik:paused` add must name a `pauseCause*` constant (or pass a `cause` parameter through). The raw-write exceptions are an audited list with a reason each, and the scan fails if an exception is stale. `TestPauseCauseScanFailsForAnUncausedSite` proves it non-vacuous on planted sources. An unset cause at run time logs and records `other`.

### 4. Replace, clear and stale cleanup

- **Replace:** `applyPauseCause` removes every other cause label (from the caller's snapshot and the Store) before adding the new one, so at most one is present. A failed write is logged and non-fatal; the pause stands.
- **Clear:** `clearPauseCause` runs on every engine path that lifts a pause: `clearFailedStage` (plain resume and stale engine-pause records), `unblockAwaitingInput`, `reenterValidate`, the merged-PR terminal advance, and the closed-issue sweep (`transientLifecycleLabels`).
- **Stale:** a human removing `fabrik:paused` in the GitHub UI — the dominant path, since every pause comment says to — is invisible to the engine. `settlePauseCauseScan`, a board-sourced settle scan (ADR-1270 pattern; an item the engine never dispatches still gets cleaned), removes a cause label whose live labels (`liveLabels`, fail-closed on a read error) show no `fabrik:paused`. Because the pause write order is paused-then-cause, a pause in progress is never mistaken for a stale cause. A human-applied `fabrik:paused` gets no cause label.

### 5. The cause is read-only for everything that decides

Nothing in `resumeAuthorised`, `itemNeedsWork`, `processItem`, the pause guards or admission reads the cause label (`TestResumeIsIdenticalWhateverTheCause`).

`attention.Classify` reads it for presentation: `Result.PauseCause` (`fabrik_status` `pause_cause`, channel `meta.pause_cause`). A non-question cause makes a needs-human pause report code `fabrik:paused` rather than `fabrik:awaiting-input`, so the push event is `paused`, not `awaiting-input`, and the summary no longer claims "waiting for a human reply". The state, rank and resume advice are unchanged. A pause with no cause label (pre-upgrade, or human-applied) classifies as before. The label is the primary source of `daemon-stop`; the comment marker remains the source for `unknown-merge-state` (shown as `ci-timeout` on the board) and as the fallback for pre-upgrade pauses.

## Alternatives considered

| Option | Verdict |
|---|---|
| Narrow `awaiting-input` to question pauses now | Rejected: loses the counter and breaker resets for ~30 pause sites (#1460 re-pause loop). Recorded as a follow-up behind a resume-path unification. |
| Separate `pauseIssue` argument for the cause | Rejected: a mass signature change; a required `pauseOpts` field plus the scan gives the same guarantee. |
| Per-site annotations for direct `fabrik:paused` writes | Rejected: easier to forget than a helper the scan can forbid bypassing. |
| Stale cleanup piggybacked on `clearOrphanAwaitingInput` | Rejected: that runs only for items that get processed; an item whose pause a human lifted may never be dispatched. |
| Cause stored only in a comment marker | Rejected: invisible on the board, which is the report's point. |

## Consequences

- Operators can filter the board by `fabrik:pause:*`. Pre-upgrade pauses carry no cause label until re-paused.
- Up to eight labels are created lazily per repository; each pause adds one label write and each lift one removal (webhook echoes are reserved like other engine labels).
- Replacing a cause is two REST calls; the stale scan and the next replace are the backstop for a lost removal.
- `docs/state-machine.md` (label table, pause descriptions), `CLAUDE.md`, `plugin/fabrik-workflows/LABELS.md` and the regenerated `docs/llms-full.txt` describe the family.
