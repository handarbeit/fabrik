# Feature Specification: Merge train: act on a trial's CI verdict within one poll of completion

**Feature Branch**: `fabrik/issue-2542`
**Created**: 2026-11-05
**Status**: Specified
**Input**: User description: "## Problem

A merge-train trial's CI verdict is read minutes after CI finishes. On the 0.0.91 gate (engine `000885c6`, 2026-10-31), trial CI took 8–20 s, but each verdict waited 2–6.5 min for a poll. Every trial in a bisection pays this delay: the bisect and conflict tests took 20–60 min, and the runaway guard's second firing came 303 s after the first.

There are two causes:

1. **Idle backoff while a trial is open.** While a run waits on trial CI, `poll()` finds nothing else to do, logs `nothing to do` and backs off (2× → 4× → about 5m24s). An open trial counts neither as activity nor as a cooldown cap in `PollWithBackoff`. `settleTrainRuns` evaluates the trial only when a poll runs.
2. **Trial PR deliveries never wake the poll.** The #2483 guard (`skipTrainPR`, `boardcache.TrainBranchPrefix`) drops every `pull_request`, review and `check_run` delivery for a `fabrik/merge-train/` head, logging `is a merge-train PR — ignoring`. That is right for the linkage heal, but it also means the trial's CI completion wakes nothing.

The same gate also showed two smaller oddities. They may share a cause and need diagnosing:

3. **A run kept holding the partition after its test failed.** After `TestMergeTrainConflictBisectPrefixRerere` failed on bed A `app/on/webhook`, its run held `alpha/main` until about 18:25. At 18:14 the pause guard had already refused to land the paused members. `TestMergeTrainHappyPathLanding` then failed because of it.
4. **Misleading engine logs, 18:03–18:14 on bed A:**
   - `train CI red … needs attention` appeared right after `trial … green` for the same run.
   - `fatal: Unable to create …MERGE_RR.lock: File exists` appeared during the rerere forget.

## Requirements

- **R1:** while any partition has a durable run with an open trial, the idle interval is capped at one configured poll interval. This is the same shape as the #2528 long-cooldown cap. Activity accounting (`pollResult.Active`) is unchanged.
- **R2:** a `check_run` or `check_suite` completion, or a `pull_request` event, for an open trial's PR (matched against the persisted run record's trial PR number/head, never by branch prefix alone) requests an early poll. It still never attaches the PR to a member: the #2483 heal guard stays exactly as it is, and the R2 wake must not reach `resolvePRLinkage`.
- **R3:** diagnose (3). If a failed episode can keep holding a partition after every member is paused, release it. Otherwise record why it is correct.
- **R4:** diagnose (4). Fix the red/green log contradiction, and make the rerere forget tolerate or serialise around an existing `MERGE_RR.lock`. Neither may change a train decision.

## Scope

- In: `engine/poll.go` (`PollWithBackoff`), `engine/train_run_poll.go`, the boardcache train-PR handlers (wake only), `engine/merge_train*.go` logging and rerere, `docs/state-machine.md` (Merge-Train Run State and the idle-backoff section), a new ADR, sim twins.
- Out: the #2483 heal guard's behaviour, and live test timeouts (#2540 and #2541 already widened them).

## Acceptance

- A sim test shows a trial verdict is acted on within one poll interval of CI completing on a poll-only engine (R1). A second sim test, with the trial's `check_run` delivered through the intake, shows the early poll (R2). Each has a non-vacuity seam that restores today's backoff and no wake.
- A test pins that an R2 wake never links the trial PR to a member.
- R3 and R4 each have a regression test or a recorded rationale.
- `go test -race ./...` passes."

## Background

A merge-train trial is a temporary PR that the engine opens to validate a batch of Queued members together. Its CI usually finishes in seconds, yet the engine only reads the verdict on its next poll. On the 0.0.91 release gate (engine `000885c6`, 2026-10-31) trial CI took 8–20 s and each verdict then waited 2–6.5 minutes. A bisection runs several trials in sequence and pays that wait on every one. The bisect and conflict live tests took 20–60 minutes, and the runaway guard's second firing came 303 s after the first.

Two things cause the delay:

1. **The idle poll backs off while a trial is open.** A run waiting on trial CI leaves the poll nothing else to do, so it backs off (2×, then 4×, up to about 5m24s). An open trial is neither activity nor a cap on the idle interval, and the trial is evaluated only when a poll runs.
2. **Trial PR deliveries wake nothing.** The #2483 guard drops every `pull_request`, review and `check_run` delivery for a `fabrik/merge-train/` head, so the linkage heal cannot attach a trial PR to a member. That is right for linkage, but it also discards the one signal that says the trial's CI finished.

Findings from the archived bed A engine log (`app-on-webhook/20261031T133135Z-4d0026/fabrik.log.1`) refine this and bear on the two smaller oddities:

- **The delay can exceed 6.5 minutes when an admission cooldown sets the cap.** Trial PR #10594 opened at 18:03:17 and its check runs arrived by 18:03:35, all ignored by the #2483 guard. The next poll was then capped at 10m51s by an admission cooldown, and the verdict was read at 18:14:13, about 10.6 minutes after CI finished. The fix must therefore beat the #2528 long-cooldown cap as well as the idle backoff.
- **The red-after-green line is a stale status message.** `train CI red … (PR #10594) — needs attention` comes from the dispatch guard in `dispatchMergeTrainWorker`. It reports the CI result held on the run's in-flight claim, which still carries the earlier bisection step's red result because the asynchronous per-poll evaluation never updates it. It is a log line only; no decision reads it.
- **The `MERGE_RR.lock` error is stderr from the trial-so-far reconstruction ahead of a poisoner.** That reconstruction already logs `could not faithfully reconstruct … skipping forget`, so the rerere forget was skipped, not half-run. A lock was left behind in the worktree. The cost is that the poisoner's recorded resolution may stay in rerere; no decision went wrong.
- **The partition hold followed the pause guard.** The survivors carried `fabrik:paused` when trial t6 went green (18:14:13). The pause guard (ADR 2341) refused the landing and left them Queued, as specified, and the partition re-formed at 18:25:22. This looks like a consequence of the failed test's state rather than an engine fault. What remains to check is who paused the survivors.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A trial verdict is acted on promptly on a poll-only engine (Priority: P1)

An operator running the engine with polling only has a merge-train run with an open trial. When the trial's CI completes, the engine acts on the verdict (lands, bisects, ejects or pauses) within one configured poll interval, not after an exponentially backed-off idle wait or a long admission-cooldown wait.

**Why this priority**: This is the dominant cost: every trial in every bisection pays it, and poll-only is the base configuration that every deployment has.

**Independent Test**: In the sim with no webhook transport, open a trial, complete its CI, and advance time. The verdict is acted on within one poll interval, however long the engine had already been idle and whatever admission cooldowns are pending.

**Acceptance Scenarios**:

1. **Given** a partition with a durable run whose trial is open and the engine has been idle long enough to have backed off, **When** the trial's CI completes, **Then** the verdict is acted on within one configured poll interval.
2. **Given** an open trial and an admission cooldown that would otherwise cap the next poll at about ten minutes, **When** the next poll is scheduled, **Then** the idle interval is still no longer than one configured poll interval.
3. **Given** no partition has an open trial, **When** the engine is idle, **Then** the idle backoff behaves exactly as it does today.

---

### User Story 2 - A trial's CI completion wakes the poll when events are delivered (Priority: P2)

An operator running with webhook delivery gets the verdict as soon as CI finishes. A `check_run` or `check_suite` completion, or a `pull_request` event, for an open trial's PR requests an early poll, without attaching that PR to any member.

**Why this priority**: It shortens the wait below one poll interval where events are available. It is secondary to User Story 1 because the capped interval already bounds the delay.

**Independent Test**: In the sim, deliver the trial's `check_run` completion through the intake and observe an early poll. Deliver the same event for a train-prefixed PR that is not an open trial's and observe no wake.

**Acceptance Scenarios**:

1. **Given** a run with an open trial PR, **When** a `check_run` or `check_suite` completion for that PR (matched by the run's trial PR number and head) is delivered, **Then** an early poll is requested.
2. **Given** a run with an open trial PR, **When** a `pull_request` event for that PR is delivered, **Then** an early poll is requested.
3. **Given** a delivery for a `fabrik/merge-train/` PR that is not an open trial's PR number and head, **When** it is delivered, **Then** no wake is requested.
4. **Given** any such wake, **When** it is processed, **Then** the trial PR is never linked to a member and the linkage heal is never reached.

---

### User Story 3 - A failed episode does not strand a partition (Priority: P3)

When a test or episode fails and every member of a run ends up paused, the partition is not held by a run that can no longer make progress. If the observed hold is correct behaviour, the reason is recorded.

**Why this priority**: It was seen once, may be correct, and its cost is a cascading failure in a later test rather than a regression in production behaviour.

**Independent Test**: Reproduce the shape in the sim: a run whose members are all paused when its trial goes green. Assert whether the partition is released or held, and that the outcome matches the recorded decision.

**Acceptance Scenarios**:

1. **Given** a run whose members are all paused, **When** its trial verdict arrives, **Then** either the run is released and the partition can re-form without waiting for an unrelated timeout, or a recorded rationale explains why the hold is correct.

---

### User Story 4 - Train logs and rerere cleanup are trustworthy (Priority: P3)

An operator reading the engine log does not see a `train CI red … needs attention` line contradict a green trial, and a leftover `MERGE_RR.lock` does not produce a fatal-looking error or leave stale rerere state.

**Why this priority**: These are log and cleanup defects. They mislead but change no decision.

**Independent Test**: Open a new trial after a red one in the same run and check the status line. Leave a `MERGE_RR.lock` in the trial worktree and run the rerere forget.

**Acceptance Scenarios**:

1. **Given** a run whose earlier step was red and whose current trial is green, **When** the dispatch guard logs the run's CI status, **Then** the line reflects the current trial or is not stated from stale data.
2. **Given** an existing `MERGE_RR.lock` in the worktree, **When** the rerere forget runs, **Then** it either serialises around the lock or tolerates it, with no `fatal:` line, and no train decision changes.

---

### Edge Cases

- Several partitions have runs: one open trial is enough to apply the cap, and the cap must not depend on which partition it is.
- A run whose trial has just closed or whose record was dropped between events: the wake finds no matching open trial and is ignored.
- A duplicate or replayed delivery for the trial PR: it may request an early poll, but no extra work is done beyond the normal coalesced poll.
- A trial PR number or head SHA that has been reused or replaced by a re-formation: matching by the persisted record's current number and head avoids waking for the old one.
- A `fabrik/merge-train/` PR for a stale or abandoned trial: it matches no open trial, so it is still ignored exactly as today.
- The configured poll interval is shorter than the existing idle cap: the cap must never lengthen an interval that is already shorter.
- Webhook delivery is unhealthy or absent: User Story 1 alone must still bound the latency.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: While any partition has a durable merge-train run with an open trial, the engine's idle poll interval is capped at one configured poll interval. The cap holds regardless of the idle backoff level and of any other cap, including the admission-cooldown cap (#2528), which may otherwise delay the next poll to about ten minutes.
- **FR-002**: The cap in FR-001 does not change activity accounting: an open trial does not make a poll count as active.
- **FR-003**: A `check_run` or `check_suite` completion, or a `pull_request` event, for an open trial's PR requests an early poll. The PR is identified by matching the persisted run record's trial PR number and head, never by branch prefix alone.
- **FR-004**: An event handled under FR-003 never attaches the trial PR to a member. The #2483 guard that stops merge-train PRs reaching the linkage heal is unchanged, and the wake path must not reach `resolvePRLinkage`.
- **FR-005**: An event for a merge-train-prefixed PR that does not match an open trial's number and head requests no wake.
- **FR-006**: Determine whether a failed episode can keep holding a partition after every member is paused. If it can, release the hold. If not, record why the observed hold is correct, including who paused the survivors.
- **FR-007**: The dispatch guard's CI status line for a run does not report a stale result from an earlier bisection step; it is refreshed when a new trial opens or worded from the run record.
- **FR-008**: The rerere forget tolerates or serialises around an existing `MERGE_RR.lock`, and a leftover lock does not surface a `fatal:` error.
- **FR-009**: Neither the log change in FR-007 nor the rerere change in FR-008 alters any train decision.
- **FR-010**: Each of FR-001 and FR-003 has a non-vacuity seam that restores today's behaviour (backoff without the cap; no wake) so its test can be shown to fail without the change.
- **FR-011**: The behaviour is documented in the as-built state-machine documentation (Merge-Train Run State and the idle-backoff section) and in a new ADR, and the bundled docs file is regenerated.

### Key Entities *(if applicable)*

- **Merge-train run**: the persisted episode for one (repo, base) partition, holding the pinned base, members and at most one open trial with its PR number and head.
- **Open trial**: the single trial PR a run is waiting on for CI; the only PR whose events FR-003 acts on.
- **Early poll request**: the coalesced request that makes the poll loop run before its next scheduled time.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: On a poll-only engine, a trial verdict is acted on within one configured poll interval of CI completion, in a sim test, including when an admission cooldown would otherwise defer the poll by about ten minutes.
- **SC-002**: With the trial's `check_run` delivered through the intake, an early poll is observed in a sim test, earlier than the next scheduled poll.
- **SC-003**: A test shows that waking for a trial PR event never links the PR to a member.
- **SC-004**: The tests for FR-001 and FR-003 each fail when their non-vacuity seam is enabled.
- **SC-005**: A regression test or a recorded rationale exists for each of the partition-hold diagnosis (FR-006), the stale CI status line (FR-007) and the rerere lock (FR-008).
- **SC-006**: The full test suite passes with the race detector enabled.
- **SC-007**: In the bisection live tests, the added latency per trial falls from minutes to at most one poll interval.

## Assumptions

- "One configured poll interval" is the engine's configured poll interval as used by the #2528 cap, not a new setting.
- The cap applies to the idle interval only. It does not shorten an interval that is already shorter.
- The #2483 guard is kept as it is; the wake is a separate, narrower path that reads the persisted run record and never touches linkage.
- Webhook events are an optimisation on top of the cap, not a replacement. The cap alone meets the latency bound without them.
- The partition-hold observation (FR-006) is most likely correct behaviour following the pause guard (ADR 2341); the work is to confirm who paused the survivors and either record that or release the hold.
- The `MERGE_RR.lock` failure is non-fatal to the decision flow, because the forget was already skipped.

## Out of Scope *(optional)*

- Changing the #2483 heal guard's behaviour.
- Live test timeouts (#2540 and #2541 already widened them).
- Changing how activity is counted for idle-backoff purposes.
- Changing any train decision (landing, bisection, ejection, pause) as a result of the log and rerere fixes.

## Source References *(optional)*

- Archived bed A engine log, `app-on-webhook/20261031T133135Z-4d0026/fabrik.log.1` (0.0.91 gate, engine `000885c6`).
- Issues and ADRs: #2528 (long-cooldown cap), #2483 (merge-train PR heal guard), ADR 2341 (pause guard), ADR 2051 (persisted train run), #2540, #2541.
