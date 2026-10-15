# Feature Specification: Merge-train landing waits out a transient `blocked` after mark-ready on the same PR

**Feature Branch**: `fabrik/issue-2297`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description:

> ## Problem
>
> When a trial goes green, the landing paths promote the trial's draft CI PR to ready and merge it straight away:
>
> - `landMergeTrainBatch` (`engine/merge_train.go:4512`) calls `MarkPRReady` (`:4578-4585`), then `pollForMergeable` (`:4601`), then `MergePR` (`:4606`).
> - `landSingleton` (`engine/merge_train.go:1816`) does the same: `MarkPRReady` at `:1860-1866`, `pollForMergeable` at `:1882`, `MergePR` at `:1885`.
>
> Marking a PR ready fires `pull_request: ready_for_review`, which starts a new workflow run on the same head. For a short window that run's required check is queued or pending, and on some workflows it is not yet visible at all. GitHub then reports `mergeable_state = "blocked"`.
>
> Two things in v0.0.85 let that window turn into a failed landing:
>
> 1. **`pollForMergeable` (`engine/merge_train.go:4352`) does not look at `mergeable_state` once check runs exist.** It returns ready as soon as `classifyLandingCI` (`engine/merge_train.go:4274-4300`) says `TrainCIGreen`. With check runs present, that verdict comes from the check runs and `ciSuiteHold` only. The trial's draft-run checks are all green, and the new run's check runs or suite are not visible yet one second after mark-ready, so the poll says "ready to land" at once.
> 2. **`MergePR` refuses on `blocked` and nothing retries.** `mergePR` (`github/prs.go:1529-1531`) returns `ErrNotMergeableCI` for any `mergeable_state` outside `{clean, unstable}`. Both landing paths treat any `MergePR` error as a failed landing:
>    - `landMergeTrainBatch` posts "🏭 **Fabrik merge-train — merge failure** … Manual intervention may be required" on the first member (`:4607-4617`) and returns.
>    - `landSingleton` logs and returns (`:1886-1887`).
>
>    In both cases the deferred cleanup deletes the trial branch, which closes the trial PR: `CleanupTrainWorktree(trialName, true)` in `landMergeTrainBatch`'s defer (`:4523-4531`), and `cleanupTrialArtifacts` in `landSingleton` (`:1818`). The members stay Queued, and the next pass assembles a brand-new trial on the same base and runs the full CI again.
>
> The stage path already treats this refusal as transient. `reenableAutoMergeAfterRebase` (`engine/merge_gate.go:199-207`) logs and swallows `ErrNotMergeableCI` because "it is a CI-readiness refusal, not a conflict… the next poll's settle/convergence pass retries". The train has no equivalent.
>
> Reported impact (v0.0.83, code unchanged in v0.0.85): one single-member train built three trials of about 17 minutes each on an unchanged base, two seconds apart from a green verdict each time. Closing the trial PR also cancelled the pending `ready_for_review` run through the repository's cancel-on-close workflow. That left the green commit's latest `test` check `cancelled`, which then blocked a manual fast-forward.
>
> ## Requirements
>
> **R1. Mark-ready is followed by a settle wait, not an immediate merge.** After a landing path marks a trial or integration PR ready, the landing re-reads the PR's `mergeable_state` and its check runs and suites at the same head before merging. A merge is attempted only once `mergeable_state` is accepted (`gh.MergeableStateAccepted`) **and** `classifyLandingCI` is green for that head.
>
> **R2. `blocked` with nothing failed is pending.** On that head, a `blocked` (or `null`/`unknown`/empty) `mergeable_state` with no failed check run and no failed required context is pending, never a landing failure:
> - Pending check runs or an outstanding suite (for example the `ready_for_review` run) are waited out under the existing landing bound, `CIBackstopTimeout`, as `pollForMergeable` waits today.
> - When every visible check is green but `mergeable_state` stays `blocked`, the wait has its own short bound. This is GitHub not yet counting a just-created check, or a check that is about to appear. The bound is a named constant or setting, on the order of a few minutes, distinct from `CIBackstopTimeout`. Plan picks the value and whether it is configurable. Re-polls use the existing train CI poll interval.
>
> **R3. A merge refusal is retried on the same PR.** If `MergePR` itself returns `ErrNotMergeableCI` (the state flipped between the read and the merge), the landing goes back to the R2 wait and retries the merge on the **same** PR within the same bound. It does not fail the landing.
>
> **R4. What still fails a landing.** Only these end a landing attempt as failed:
> - a confirmed failed check run or required context on the head (as today);
> - `mergeable_state = "dirty"` (as today);
> - the PR's head moving away from the green trial head;
> - R2's bound running out;
> - a `MergePR` error other than `ErrNotMergeableCI`.
>
> **R5. Never rebuild a green trial on an unchanged base.** When a landing attempt ends without a merge, and the trial PR's head is still the green trial head, and the base has not moved (`trialBehind` false):
> - the trial PR and trial branch are **kept**, not closed or deleted;
> - the next landing attempt reuses that PR: no new assembly, no new trial CI run.
>
> Research confirms whether `reconstructTrainState` Route 2 (`resumeTrain`, `engine/merge_train.go:5118`) already resumes such a kept PR for both the batch integration PR and the singleton landing PR (its member lookup is `parseTrainMembers` on the PR body), and closes any gap. Once the base has moved, today's `landGreenBatch` main-moved rebase path (`effectiveMaxTrainRebaseCycles`) applies unchanged.
>
> **R6. Messaging.** A landing that is merely waiting posts no comment. The "merge failure — Manual intervention may be required" comment is posted only for an R4 failure. It names the PR, the `mergeable_state` observed and the reason, and it is not reposted for the same PR on later attempts. Logs name the wait: "integration PR #N blocked after mark-ready, N check(s) pending / all green, waiting up to X".
>
> **R7. Both paths and both drivers.** R1–R6 apply to `landMergeTrainBatch` and `landSingleton` (and so to `landOneAtATime`'s singleton landings), and under both the synchronous driver and the persisted run state machine (ADR 2051, `engine/train_step.go`). The run's restart behaviour during `stepLanding` (`engine/train_run_poll.go:145-148`) stays correct: a restart mid-wait must not lose the kept PR or double-merge.
>
> ## Scope
>
> In scope: the merge-train landing paths above, the settle wait and retry, keeping and reusing a green trial PR, messaging, tests and docs.
>
> Out of scope:
> - the singleton fast path's `MergePRAtHeadSHA` on the member's own PR (no mark-ready precedes it);
> - the stage-path auto-merge/direct-merge behaviour;
> - `mergeable=null` bounds on the stage CI gate (#1809, separate work issue);
> - cancelled check classification (#2296);
> - a trial going `dirty` mid-CI (#2298).
>
> Mention #2237 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2237. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - **Unit (`engine`).**
>   - A landing whose PR reads `blocked` with green checks on the first one or two reads, then `clean`, merges the same PR. There is exactly one `MergePR` success, no merge-failure comment and no trial cleanup before the merge.
>   - A `MergePR` that returns `ErrNotMergeableCI` once and then succeeds lands the same PR.
>   - `blocked` with a failed check is a landing failure, as today.
>   - The R2 bound running out on a green, unchanged-base trial keeps the PR and branch and posts at most one comment.
> - **Sim twin (`tests/sim`).**
>   - Extend `simgh` so `MarkPRReady` can schedule a `ready_for_review` effect: a new pending (or not-yet-visible) check run on the head and a transient `mergeable_state = "blocked"` that settles after a scripted delay to a skipped or successful check. The model can follow `ReopenStep`/`SeedRerunStep` in `tests/sim/simgh/workflowruns.go`; record it in `FIDELITY.md`.
>   - Scenario in `mergetrain_landing_test.go` or a sibling: a single-member train goes green and lands on the **first** trial PR, and only one trial PR is ever opened for the member.
>   - A second scenario runs a two-member batch through `landMergeTrainBatch`.
>   - A third scenario: when the bound runs out, the next pass reuses the kept trial PR without opening a new one.
> - **Non-vacuity.** Each new test fails with the fix neutralised: an immediate `MergePR` after mark-ready, and cleanup on failure. Record this in the PR.
> - **Docs.** Update `docs/state-machine.md`: "Merge-Train Landing Lifecycle (ADR-059 D3)", "Merge-Train Serialization + Main-Moved Recovery (ADR-059 D5)" and the run-state section (#2051) for the settle wait, the retry and keeping a green trial. Regenerate `docs/llms-full.txt` in the same commit.

## Background

When a merge-train trial goes green, the landing step marks the trial's draft CI PR ready and merges it immediately. Marking a PR ready starts a new workflow run (`ready_for_review`) on the same head, and for a short window GitHub reports the PR as `blocked` because that run's required check is queued, pending or not yet visible. Today the landing's readiness poll consults only the check runs and suites it can see, so it reports "ready" at once, and the merge is then refused as not mergeable. Both landing paths (batch integration PR and single-member landing) treat that refusal as a failed landing: they post a "merge failure — manual intervention may be required" comment, delete the trial branch (which closes the trial PR) and leave the members Queued. The next pass then builds a brand-new trial on the same base and repeats the full CI run.

Observed impact (v0.0.83, code unchanged in v0.0.85): a single-member train built three trials of about 17 minutes each on an unchanged base, each failing within two seconds of a green verdict. Closing the trial PR also cancelled the pending `ready_for_review` run via the repository's cancel-on-close workflow, leaving the green commit's latest `test` check `cancelled`, which then blocked a manual fast-forward.

The stage (non-train) auto-merge path already treats this same refusal as transient and retries on the next pass; the train has no equivalent. This spec gives the train that behaviour: wait the transient window out on the same PR, and never discard a green trial on an unchanged base.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A transient `blocked` after mark-ready lands on the first trial PR (Priority: P1)

An operator runs a merge train with a single queued member. The trial goes green, the landing marks the trial PR ready, and for a few seconds GitHub reports `blocked` while the `ready_for_review` run starts. The landing waits and merges the same PR once it settles.

**Why this priority**: This is the reported failure; it wastes about 17 minutes of CI per occurrence and can leave a cancelled check on the green commit.

**Independent Test**: Run a single-member train in the sim bed with a scripted `ready_for_review` effect (pending check, transient `blocked`). Assert one trial PR is ever opened, it is merged, no merge-failure comment is posted and no trial cleanup occurs before the merge.

**Acceptance Scenarios**:

1. **Given** a green single-member trial whose PR reads `blocked` with all visible checks green on the first one or two reads after mark-ready, **When** the landing runs, **Then** it waits and merges the same PR once `mergeable_state` is accepted, with exactly one successful merge, no comment and no cleanup before the merge.
2. **Given** the same trial where the `ready_for_review` run adds a pending check, **When** the landing runs, **Then** it waits for that check under the existing landing bound and merges once it completes green.

---

### User Story 2 - A multi-member batch lands its integration PR the same way (Priority: P1)

The same transient window occurs when a batch of two or more members lands through its integration PR; it is waited out identically.

**Why this priority**: The batch path has the same defect and is the train's main mode.

**Independent Test**: Run a two-member batch in the sim bed with the same scripted effect and assert the integration PR lands on the first trial with no new trial opened.

**Acceptance Scenarios**:

1. **Given** a green two-member batch whose integration PR reads transiently `blocked` after mark-ready, **When** the landing runs, **Then** the integration PR is merged once settled, with no merge-failure comment and no second trial.

---

### User Story 3 - A merge refusal at the moment of merging is retried on the same PR (Priority: P1)

The PR looks mergeable at the readiness read, but the merge call is refused as not mergeable because the state flipped in between.

**Why this priority**: The read-then-merge race cannot be closed by reading alone; the landing must tolerate it.

**Independent Test**: Unit test with a merge call that returns the not-mergeable refusal once and then succeeds; assert the same PR lands.

**Acceptance Scenarios**:

1. **Given** a landing whose merge is refused as not mergeable once, **When** it retries within the wait bound, **Then** the same PR is merged and no failure comment or cleanup occurs.
2. **Given** a merge that returns any other error, **When** the landing runs, **Then** it fails as a landing failure.

---

### User Story 4 - A genuine failure still fails the landing, and a green trial is never rebuilt on an unchanged base (Priority: P2)

If the wait cannot succeed (a check really failed, the PR is conflicting, the head moved, or the short bound ran out), the landing fails. If it fails while the trial is still green at its head and the base has not moved, the trial PR and branch are kept and the next attempt reuses them rather than assembling and running a new trial.

**Why this priority**: Bounds the wait and removes the expensive rebuild even in the residual failure cases, but is secondary to not failing in the first place.

**Independent Test**: Sim scenario where the settle bound runs out on a green, unchanged-base trial; assert the PR and branch are kept, at most one comment is posted, and the next pass reuses the kept PR without opening a new one. Unit tests for failed check and `dirty` states.

**Acceptance Scenarios**:

1. **Given** `blocked` with a failed check run or failed required context, **When** the landing evaluates it, **Then** it ends as a failed landing, as today.
2. **Given** `mergeable_state = "dirty"`, **When** the landing evaluates it, **Then** it ends as a failed landing, as today.
3. **Given** all visible checks green but `mergeable_state` stays `blocked` past the short bound, **When** the bound runs out on an unchanged base with the head still the green trial head, **Then** the trial PR and branch are kept, at most one explanatory comment is posted, and the next landing attempt reuses that PR.
4. **Given** the base has moved since the trial, **When** a landing attempt ends without a merge, **Then** the existing main-moved rebase path applies unchanged.
5. **Given** the PR's head has moved away from the green trial head, **When** the landing evaluates it, **Then** it ends as a failed landing.

---

### User Story 5 - Restart or the persisted run state machine does not lose or double-land the PR (Priority: P2)

The same behaviour holds under the synchronous driver and under the persisted run state machine, including when the daemon restarts in the middle of the wait.

**Why this priority**: The asynchronous driver is the production mode; a restart mid-wait must not strand the kept PR or merge it twice.

**Independent Test**: Sim scenario with the persisted run driver, restarting the engine during the wait; assert the kept PR is adopted and merged exactly once.

**Acceptance Scenarios**:

1. **Given** a landing waiting on a transient `blocked` under the persisted run driver, **When** the daemon restarts mid-wait, **Then** the kept PR is neither lost nor merged twice, and the landing completes on the same PR.

---

### Edge Cases

- `mergeable_state` reads `null`, `unknown` or empty with no failure: treated as pending, like `blocked`.
- Pending checks or an outstanding suite on the head: waited out under the existing landing bound; only a green-but-`blocked` state uses the shorter bound.
- The merge refusal repeats for the whole bound: ends as an R2 bound failure, not an endless retry.
- A failure comment was already posted for a PR on an earlier attempt: it is not reposted for the same PR.
- The head moves during the wait (for example a push to the trial branch): the landing fails rather than merging a different commit than the one validated.
- The wait is interrupted by shutdown or drain: no comment is posted and nothing is cleaned up that would destroy the green trial.
- Singleton landings inside one-at-a-time landing behave as ordinary singleton landings.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: After a merge-train landing marks a trial or integration PR ready, it MUST re-read the PR's merge state and its check runs and suites at the same head before attempting the merge.
- **FR-002**: The landing MUST attempt the merge only when the PR's merge state is an accepted one AND the landing CI classification is green for that head.
- **FR-003**: A `blocked`, `null`, `unknown` or empty merge state with no failed check run and no failed required context on that head MUST be treated as pending, never as a landing failure.
- **FR-004**: While check runs are pending or a check suite is outstanding on the head, the landing MUST wait under the existing landing bound (`CIBackstopTimeout`).
- **FR-005**: When all visible checks are green but the merge state stays `blocked`, the landing MUST wait only up to a separate, shorter bound, defined as a named constant or setting on the order of a few minutes and distinct from `CIBackstopTimeout`. Re-polls MUST use the existing train CI poll interval.
- **FR-006**: If the merge call is refused as not mergeable (a CI-readiness refusal), the landing MUST return to the wait and retry the merge on the same PR within the same bound, and MUST NOT treat it as a failed landing.
- **FR-007**: A landing attempt MUST end as failed only for: a confirmed failed check run or required context on the head; a `dirty` merge state; the PR head moving away from the green trial head; the shorter bound (FR-005) running out; or a merge error other than the not-mergeable refusal.
- **FR-008**: When a landing attempt ends without a merge, the trial PR's head is still the green trial head and the base has not moved, the trial PR and trial branch MUST be kept (not closed or deleted).
- **FR-009**: The next landing attempt after FR-008 MUST reuse the kept PR, with no new trial assembly and no new trial CI run, for both the batch integration PR and the singleton landing PR. Any gap in how a kept PR is resumed MUST be closed.
- **FR-010**: Once the base has moved, the existing main-moved rebase recovery MUST apply unchanged.
- **FR-011**: A landing that is merely waiting MUST post no comment.
- **FR-012**: The "merge failure — manual intervention may be required" comment MUST be posted only for an FR-007 failure; it MUST name the PR, the merge state observed and the reason, and MUST NOT be reposted for the same PR on later attempts.
- **FR-013**: Logs MUST name the wait, including the PR, whether checks are pending or all green, and the bound being waited on.
- **FR-014**: FR-001 to FR-013 MUST apply to both the batch landing and the singleton landing (including singleton landings within one-at-a-time landing), under both the synchronous driver and the persisted run state machine.
- **FR-015**: A daemon restart during the landing wait MUST NOT lose the kept PR and MUST NOT cause the PR to be merged twice.
- **FR-016**: The sim GitHub model MUST support scheduling a `ready_for_review` effect on mark-ready (a new pending or not-yet-visible check run on the head and a transient `blocked` merge state that settles after a scripted delay), documented in the sim's fidelity notes.
- **FR-017**: Tests MUST cover: transient `blocked` then `clean` merging the same PR with one successful merge, no comment and no pre-merge cleanup; a single merge refusal then success; `blocked` with a failed check failing as today; the bound running out keeping the PR and branch with at most one comment; and sim scenarios for a single-member train, a two-member batch and reuse of a kept PR after the bound runs out. Each new test MUST fail when the fix is neutralised (immediate merge after mark-ready; cleanup on failure), and this MUST be recorded in the PR.
- **FR-018**: The as-built docs for merge-train landing lifecycle, serialization and main-moved recovery, and the persisted run state MUST describe the settle wait, the retry and keeping a green trial, and the documentation bundle MUST be regenerated in the same commit.

### Key Entities *(if applicable)*

- **Trial / integration PR**: The draft CI PR that a green trial promotes to ready and merges to land its members.
- **Green trial head**: The commit at which the trial's CI was validated green; the landing only ever merges this head.
- **Landing attempt**: One pass of marking ready, waiting for settle and merging; may end merged, failed, or ended-without-merge with the PR kept.
- **Settle bounds**: The existing landing bound for pending checks and the new shorter bound for green-but-`blocked`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A single-member train whose PR is transiently `blocked` after mark-ready lands on its first trial PR, and exactly one trial PR is opened for the member.
- **SC-002**: A two-member batch under the same condition lands through its first integration PR with no second trial.
- **SC-003**: A transient `blocked` or a single merge refusal produces zero merge-failure comments and zero trial-branch deletions before the merge.
- **SC-004**: When the green-but-`blocked` bound runs out on an unchanged base, the PR and branch are still present afterwards, at most one comment has been posted, and the next pass opens zero new trial PRs.
- **SC-005**: A confirmed failed check or a `dirty` PR still ends the landing as failed.
- **SC-006**: Every new test fails when the corresponding fix is neutralised, evidenced in the PR.
- **SC-007**: After a restart mid-wait, the kept PR is merged exactly once.

## Assumptions

- The value of the shorter bound (a few minutes) and whether it is configurable are chosen at Plan stage.
- The existing train CI poll interval is suitable for the settle re-polls.
- The existing landing bound (`CIBackstopTimeout`) is adequate for waiting out pending `ready_for_review` runs, as it is today.
- Research will determine whether the existing restart resume already adopts a kept PR for both the batch and singleton cases; any gap is in scope to close.
- The "head moved" and "base moved" checks use the signals the landing already has available.

## Out of Scope *(optional)*

- The singleton fast path's merge of a member's own PR (no mark-ready precedes it).
- The stage path's auto-merge and direct-merge behaviour.
- `mergeable=null` bounds on the stage CI gate (#1809, separate work issue).
- Cancelled check classification (#2296).
- A trial going `dirty` mid-CI (#2298).

## Source References *(optional)*

- Related report #2237: mention it in the PR body by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it. This issue's own number gets `Closes`.
- Existing precedent for treating the not-mergeable refusal as transient: the stage path's post-rebase auto-merge re-enable.
- `docs/state-machine.md`: "Merge-Train Landing Lifecycle (ADR-059 D3)", "Merge-Train Serialization + Main-Moved Recovery (ADR-059 D5)", and the run-state section (#2051).
