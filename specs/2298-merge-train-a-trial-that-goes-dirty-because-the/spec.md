# Feature Specification: Merge train — a trial made `dirty` by a mid-CI base move is a stale trial to rebuild, not the member's red

**Feature Branch**: `fabrik/issue-2298`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description:

> # Merge train: a trial that goes `dirty` because the base moved mid-CI is a stale trial to rebuild, not the member's red
>
> ## Problem
>
> Every merge-train trial is assembled off a base SHA pinned at batch start (ADR-059 D-b, `EnsureTrainWorktreeAt`). Its draft CI PR targets the live base branch. If the base branch advances while the trial's CI runs, and the advance conflicts with the trial, GitHub reports the trial PR as `mergeable_state = "dirty"`.
>
> `evalTrialCI` (`engine/train_ci_eval.go:39-44`) checks `mergeable_state` before it reads any check run. It returns `TrainCIRed` for `dirty`, with the note "the base moved again after the trial was assembled". `evalTrialCI` is shared by the blocking `pollTrainCI` and the per-poll `settleTrainRuns` driver (ADR 2051), so every trial of a run is affected: main trial, bisection half and one-at-a-time.
>
> The red verdict is then attributed to the members, although nothing is wrong with them:
>
> - **Singleton.** `stepForm`'s red branch (`engine/train_step.go`, "combined Validate RED … with a single member") calls `ejectRedSingleton` (`engine/merge_train.go:3365`).
>   - With auto-repair enabled (default cap 1, #2045), the member is rerouted to Validate and the comment says "#N's own combined Validate is failing against the moved base — this is not a merge-train interaction" (`engine/merge_train_autorepair.go:367`).
>   - At cap 0, or once the cap is spent, `pauseRedSingleton` pauses it with "#N's own combined Validate is failing — this is not a merge-train interaction" (`engine/merge_train.go:3412`).
>
>   Either way the member leaves Queued, its own PR gets a full CI re-run, and a fresh trial follows: two full runs for a stale trial.
> - **Batch of 2+.** The red verdict starts bisection (`stepForm` → `newBisectState`). Every half-trial is assembled off the same pinned base, so against the moved live base each half can go `dirty` too. Bisection then "isolates" a poisoner and ejects it through `ejectMember`, which is counted toward `MaxMergeTrainEjections`, or it degrades to one-at-a-time.
> - The red trial is charged to the runaway guard (`recordTrialIfCounts`, `engine/train_step.go:411-416`).
>
> The train already has the right primitive for a moved base, but only at landing. `landGreenBatch` (`engine/merge_train.go:4795`) checks `trialBehind`, re-pins the base to the current `origin/<base>`, re-assembles with conflict resolution (`assembleAndValidate`, which invokes `resolveConflictWithClaude`) and re-validates. It is bounded by `effectiveMaxTrainRebaseCycles()` (`engine/merge_train.go:445`, default 3), and dissolves the batch back to Queued once that bound is spent. A `dirty` verdict during trial CI never reaches that path.
>
> Reported incident (v0.0.83; the shape is unchanged in v0.0.85 apart from auto-repair):
> 1. A singleton trial's 20–30 minute CI was running.
> 2. An unrelated PR adding a conflicting ADR-index row was merged to `main` by hand.
> 3. The trial went `dirty`, and the member was sent back to Validate and paused with a message blaming its own Validate.
>
> ## Requirements
>
> **R1. Classify base-moved `dirty` as stale.** When a trial PR reads `dirty` and the base has moved since the trial's pinned base SHA, the trial verdict is a distinct *stale* outcome, not `TrainCIRed`.
> - The base-moved check is a live read: the existing `trialBehind`, or a comparison of the live base head with the pinned SHA. Plan chooses.
> - An unreadable base fails toward *stale* (uncharged), never toward red.
> - A `dirty` trial whose base demonstrably has not moved keeps today's red handling, with a diagnostic that says the trial PR reported `dirty` on an unmoved base.
>
> **R2. Rebuild on the new base, within the existing budget.** On a stale verdict the trial is abandoned: PR closed and branch deleted. The base is re-pinned to the current `origin/<base>`, and the same member set is re-assembled on it (conflicts resolved as at assembly) and re-validated. Each rebuild counts against `effectiveMaxTrainRebaseCycles()`. The count is per run, persisted in the run record (ADR 2051, `trainRunRecord`) so it survives a restart, and shared with `landGreenBatch`'s main-moved cycles so one run cannot spend the budget twice.
>
> **R3. Members are not charged.** A stale verdict:
> - never ejects, pauses, reroutes or auto-repairs a member;
> - never starts or advances bisection;
> - never increments `MaxMergeTrainEjections` or the runaway-guard trial counter;
> - never consumes the #2045 auto-repair budget.
>
> Members stay Queued throughout.
>
> **R4. Stale during bisection or one-at-a-time.** A stale verdict on a bisection half-trial or a one-at-a-time trial ends that episode without isolating or ejecting anyone, because its pinned-base premise no longer holds. The run re-forms the current member set on the new base under R2's budget.
>
> **R5. Budget exhausted.** When the rebuild budget is spent, the batch is dissolved back to Queued, uncharged, as `landGreenBatch` does today (`dissolveBatch`). The dissolve comment says the base kept moving under the trial and names the old and new base SHAs.
>
> **R6. Diagnostics.** A stale trial gets its own log line and train-episode outcome (#2050 `trainEpisode`, the status-line phase): trial name, PR number, pinned and live base SHAs, rebuild cycle n/max.
> - No member-facing comment is posted for a rebuild that is still within budget.
> - No message in this path says the member's "own combined Validate is failing", and the auto-repair "failing against the moved base" wording is never reached from it.
> - The channel `merge-train-failed` event is not emitted for a stale trial.
>
> **R7. Both drivers, restart-safe.** R1–R6 hold under the synchronous driver and under the persisted run state machine (`engine/train_step.go`, `engine/train_run_poll.go`). A restart between the stale verdict and the rebuild neither loses the cycle count nor rebuilds twice.
>
> ## Scope
>
> In scope: the `dirty` classification in `evalTrialCI` and its consumers in the run state machine, the rebuild path, budget accounting, diagnostics, tests and docs.
>
> Out of scope:
> - **Part 2 of the report (a deterministic resolver for numbered-record/ADR clashes) is out of scope for this issue.** It is a separate, opt-in feature. With R1–R5, such a clash is still resolved by the existing assembly-time agent conflict resolution.
> - `dirty` on the landing PR in `pollForMergeable` (it already leaves members Queued uncharged).
> - The landing mark-ready race (#2297) and cancelled-check classification (#2296), which are separate work issues.
> - #2047's post-landing invalidation scan, which still runs only after Fabrik's own landings.
>
> Mention #2213 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2213. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - **Unit (`engine`).**
>   - `evalTrialCI` (or its caller) yields the stale outcome for `dirty` + base moved, and red for `dirty` + base unmoved.
>   - Stale is not charged by `recordTrialIfCounts`.
>   - A stale bisection half ends the episode without an ejection.
>   - The rebuild count persists in the run record across a simulated restart (`EnableTrainRunsForTest`).
> - **Sim twins (`tests/sim`).**
>   - Singleton: while the trial's CI is pending, a conflicting commit is pushed to the base. The trial PR goes `dirty`. Assert:
>     - the member is never rerouted, paused or auto-repaired;
>     - no "own combined Validate is failing" comment appears;
>     - a new trial is assembled on the new base;
>     - the member lands from it.
>   - Two-member batch: same setup; no bisection and no ejection.
>   - Repeated base moves past `max_train_rebase_cycles`: the batch dissolves to Queued with no pause.
>   - One scenario runs under `mergeTrainEnvOptions.AsyncTrain` with a `RestartEnv` between the stale verdict and the rebuild.
>   - Confirm `simgh` derives `dirty` for a trial PR when the base advances conflictingly; extend it and note it in `FIDELITY.md` if not.
> - **Non-vacuity.** Each new test fails with the fix neutralised: `dirty` mapped back to `TrainCIRed` unconditionally. Record this in the PR.
> - **Docs.** Update `docs/state-machine.md`: "Merge-Train Serialization + Main-Moved Recovery (ADR-059 D5)" (main-moved now also handled mid-trial-CI), "Merge-Train Run State, Per-Poll Evaluation and Restart Resume" (#2051: new verdict and persisted cycle count), the bisection text that says main-moved is out of scope, and §6.31 (auto-repair is not reached for a stale trial). Regenerate `docs/llms-full.txt` in the same commit.

## Background

The merge train validates a batch of queued items by assembling a "trial" — a combined branch with a draft CI pull request — off a base commit that is pinned when the batch starts. The trial's CI can take 20–30 minutes. If the base branch advances during that time in a way that conflicts with the trial, GitHub marks the trial pull request as `dirty`.

Today that `dirty` signal is treated as a red CI verdict, so the failure is blamed on the members even though nothing is wrong with them:

- A single queued member is sent back to Validate (auto-repair) or paused, with a message saying its *own* combined Validate is failing. It then pays for a full CI re-run of its own pull request and a second full trial run.
- A batch of two or more starts bisection. Every half-trial is built off the same pinned base, so each half can also go `dirty`. Bisection may then "isolate" and eject an innocent member, counted toward the ejection limit, or degrade to one-at-a-time.
- The red trial is charged to the runaway guard.

The train already has the right remedy for a moved base, but only at landing time: it re-pins to the current base, re-assembles with conflict resolution, re-validates, and dissolves the batch back to Queued uncharged once a bounded number of such rebuilds is spent. A `dirty` verdict during trial CI never reaches that path.

In the reported incident (v0.0.83; same shape in v0.0.85 apart from auto-repair), a singleton trial's CI was running, an unrelated pull request adding a conflicting ADR-index row was merged to `main` by hand, the trial went `dirty`, and the member was sent back to Validate and paused with a message blaming its own Validate.

The goal is to recognise this situation as what it is — a stale trial — and rebuild it on the current base within the existing rebuild budget, without charging, moving or blaming any member.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Singleton trial goes stale and is rebuilt (Priority: P1)

An operator has one item in the merge-train queue. While its trial CI is pending, an unrelated change that conflicts with the trial is merged to the base branch. The trial pull request becomes `dirty`.

**Why this priority**: This is the reported incident and the simplest case; today it causes a wrongful Validate re-run or pause and a misleading comment.

**Independent Test**: In the sim bed, with one Queued member and a pending trial, push a conflicting commit to the base so the trial PR reads `dirty`; verify the member is never rerouted, paused or auto-repaired, a new trial is assembled on the new base, and the member lands from it.

**Acceptance Scenarios**:

1. **Given** a singleton trial with pending CI and a trial PR that reads `dirty` after the base advanced conflictingly, **When** the train evaluates the trial, **Then** the verdict is *stale*, the trial is abandoned, and a new trial is assembled on the current base with the same member.
2. **Given** that stale verdict, **When** the train handles it, **Then** the member stays Queued, is not paused, rerouted or auto-repaired, and no comment says its "own combined Validate is failing".
3. **Given** the rebuilt trial passes CI, **When** it is evaluated, **Then** the member lands from it.

---

### User Story 2 - Batch of two or more goes stale without bisection (Priority: P1)

Two or more items are in a batch whose trial goes `dirty` for the same reason.

**Why this priority**: Today this triggers bisection and can eject an innocent member, which is a worse, counted, harder-to-reverse outcome than the singleton case.

**Independent Test**: Same setup as Story 1 with a two-member batch; verify no bisection starts, no member is ejected, and the batch is re-formed on the new base.

**Acceptance Scenarios**:

1. **Given** a two-member batch trial reading `dirty` on a moved base, **When** the train evaluates it, **Then** bisection does not start and no member is ejected or counted toward the ejection limit.
2. **Given** a bisection half-trial or a one-at-a-time trial reading `dirty` on a moved base, **When** it is evaluated, **Then** that episode ends without isolating or ejecting anyone, and the current member set is re-formed on the new base.

---

### User Story 3 - The base keeps moving and the budget runs out (Priority: P2)

The base advances conflictingly again and again, past the allowed number of rebuilds.

**Why this priority**: The rebuild must be bounded so the train cannot loop forever, and the outcome must remain uncharged and explained.

**Independent Test**: In the sim bed, move the base conflictingly more times than the rebuild limit; verify the batch dissolves back to Queued with no pause and a comment naming the old and new base SHAs.

**Acceptance Scenarios**:

1. **Given** the rebuild budget is spent, **When** another stale verdict arrives, **Then** the batch is dissolved back to Queued, uncharged, with no member paused.
2. **Given** that dissolve, **When** the comment is posted, **Then** it says the base kept moving under the trial and names the old and new base SHAs.

---

### User Story 4 - Restart between the stale verdict and the rebuild (Priority: P2)

The engine restarts after a stale verdict has been recorded but before the replacement trial is built.

**Why this priority**: The persisted run state machine must not lose the rebuild count (which would allow unbounded rebuilding) or rebuild twice.

**Independent Test**: In the sim bed under the asynchronous train driver, restart the engine between the stale verdict and the rebuild; verify the rebuild count is preserved and exactly one rebuild happens.

**Acceptance Scenarios**:

1. **Given** a stale verdict recorded and the engine restarted before the rebuild, **When** the run resumes, **Then** the rebuild cycle count is unchanged and exactly one replacement trial is built.

---

### User Story 5 - A `dirty` trial on an unmoved base is still red (Priority: P3)

A trial PR reads `dirty` even though the base has demonstrably not moved since the pinned SHA.

**Why this priority**: This is not the stale case and must not be silently reclassified; it keeps today's handling but gains a clearer diagnostic.

**Independent Test**: Unit test with a `dirty` trial and an unmoved base; verify a red verdict and a diagnostic stating the trial reported `dirty` on an unmoved base.

**Acceptance Scenarios**:

1. **Given** a `dirty` trial and a base head equal to the pinned SHA, **When** the trial is evaluated, **Then** the verdict is red as today, with a diagnostic that the trial PR reported `dirty` on an unmoved base.

---

### Edge Cases

- The live base cannot be read when classifying a `dirty` trial: treated as *stale* (uncharged), never as red.
- A bisection half-trial or one-at-a-time trial goes stale: the whole episode ends, because its pinned-base premise no longer holds; no one is isolated or ejected.
- The rebuild itself hits conflicts: resolved through the same assembly-time conflict resolution used today (this also covers the numbered-record/ADR-index clash from the incident).
- The rebuild budget is shared with landing-time main-moved cycles: a run that already spent cycles at landing has correspondingly fewer available mid-trial, and vice versa.
- Restart between the stale verdict and the rebuild: neither loses the cycle count nor rebuilds twice.
- Both the synchronous and persisted asynchronous drivers must behave the same.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a merge-train trial pull request reads `dirty` and the base branch has moved since the trial's pinned base commit, the trial's verdict MUST be a distinct *stale* outcome, not a red CI verdict.
- **FR-002**: Whether the base has moved MUST be determined by a live read at evaluation time. If that read fails, the verdict MUST be *stale* (uncharged), never red.
- **FR-003**: A `dirty` trial whose base demonstrably has not moved MUST keep today's red handling and MUST carry a diagnostic stating the trial PR reported `dirty` on an unmoved base.
- **FR-004**: On a stale verdict, the train MUST abandon the trial (close its pull request and delete its branch), re-pin the base to the current base branch head, re-assemble the same member set on it with conflicts resolved as at ordinary assembly, and re-validate.
- **FR-005**: Each stale rebuild MUST count against the existing rebuild limit (`max_train_rebase_cycles`, default 3). The count MUST be per run, persisted in the run record so it survives a restart, and shared with the landing-time main-moved rebuild cycles so one run cannot spend the budget twice.
- **FR-006**: A stale verdict MUST NOT eject, pause, reroute or auto-repair any member; MUST NOT start or advance bisection; MUST NOT increment the ejection count or the runaway-guard trial counter; and MUST NOT consume the red-singleton auto-repair budget. Members MUST stay Queued throughout.
- **FR-007**: A stale verdict on a bisection half-trial or a one-at-a-time trial MUST end that episode without isolating or ejecting anyone, and the run MUST re-form the current member set on the new base under the FR-005 budget.
- **FR-008**: When the rebuild budget is spent, the batch MUST be dissolved back to Queued, uncharged, as the landing path does today. The dissolve comment MUST say the base kept moving under the trial and name the old and new base SHAs.
- **FR-009**: A stale trial MUST produce its own log line and train-episode outcome (including the status-line phase) giving the trial name, PR number, pinned and live base SHAs, and rebuild cycle n/max. No member-facing comment MUST be posted for a rebuild still within budget; no message on this path may say a member's "own combined Validate is failing" or use the auto-repair "failing against the moved base" wording; and the channel `merge-train-failed` event MUST NOT be emitted for a stale trial.
- **FR-010**: FR-001 to FR-009 MUST hold under both the synchronous driver and the persisted, per-poll run state machine, and a restart between the stale verdict and the rebuild MUST neither lose the cycle count nor rebuild twice.
- **FR-011**: The as-built documentation MUST be updated: the main-moved recovery section (now also covering mid-trial-CI), the persisted run-state section (new verdict and persisted cycle count), the bisection text stating main-moved is out of scope, and the auto-repair section (not reached for a stale trial). The bundled docs file MUST be regenerated in the same commit.

### Key Entities *(if applicable)*

- **Trial**: the combined branch and draft CI pull request assembled off a pinned base commit to validate a batch (or a bisection half, or a one-at-a-time member).
- **Run record**: the persisted state of one merge-train episode for a (repo, base) partition; gains a persisted rebuild-cycle count.
- **Stale verdict**: a new trial outcome meaning "the trial's pinned-base premise no longer holds; rebuild, don't blame the members".

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In the singleton scenario (conflicting base advance while trial CI is pending), the member is never rerouted, paused or auto-repaired, no comment claims its own Validate is failing, and the member lands from a replacement trial built on the new base.
- **SC-002**: In the two-member scenario, zero bisection steps run and zero members are ejected.
- **SC-003**: With repeated base moves exceeding `max_train_rebase_cycles`, the batch ends in Queued with no member paused and the dissolve comment names both base SHAs.
- **SC-004**: Under the asynchronous driver with an engine restart between the stale verdict and the rebuild, the persisted cycle count is unchanged and exactly one replacement trial is built.
- **SC-005**: A `dirty` trial on an unmoved base still yields a red verdict with the unmoved-base diagnostic, and a stale verdict is not charged by trial accounting.
- **SC-006**: Each new test fails when the fix is neutralised (`dirty` mapped back to red unconditionally); this is recorded in the PR.
- **SC-007**: The sim bed derives `dirty` for a trial PR when the base advances conflictingly, or is extended to do so with the fidelity note recorded.

## Assumptions

- "The same member set" on a rebuild means the members currently in the run; for a bisection or one-at-a-time episode this is the current member set the run is re-forming, not the narrowed half.
- The existing rebuild limit and its default (3) are reused unchanged; no new configuration is introduced.
- The choice of how to detect "base has moved" (the existing behind-check or a head comparison) is left to Plan, provided it is a live read and meets FR-002.
- A stale trial within budget is silent to members (log and episode outcome only), to avoid comment noise during routine base churn.

## Out of Scope *(optional)*

- A deterministic resolver for numbered-record/ADR clashes (Part 2 of the report); such clashes are still resolved by the existing assembly-time agent conflict resolution.
- `dirty` on the landing PR (it already leaves members Queued uncharged).
- The landing mark-ready race (#2297) and cancelled-check classification (#2296), which are separate work issues.
- The post-landing invalidation scan (#2047), which still runs only after Fabrik's own landings.

## Source References *(optional)*

- Mention #2213 in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2213. The PR's own issue, #2298, gets `Closes`.
- Related: ADR-059 (D-b pinned base, D5 main-moved recovery), ADR 2051 (persisted run state), #2045 (auto-repair), #2050 (train episode), #2296.
