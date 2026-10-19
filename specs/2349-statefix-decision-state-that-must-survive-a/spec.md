# Feature Specification: Decision state that survives a restart (train caps, usage-limit hold, reinvoke cancel, feedbackGaps)

**Feature Branch**: `fabrik/issue-2349`
**Created**: 2026-10-18
**Status**: Specified
**Input**: User description:

> ## Problem
>
> Engine-private state that changes decisions does not survive a restart, or is never reset. Part of the state foundation from the engine state audit (`docs/design/engine-state-audit-2026-10.md`, 2026-10-15). It must land before a second webhook source (#2143). See §4.
>
> - **Merge-train caps:** `mergeTrainEjectionCounts` (engine/merge_train.go:3561) is neither consecutive (as its comment says) nor tied to the PR head, and resets on restart; the trial-rate window (`mergeTrainTrials`, :4035) and auto-repair attempts (merge_train_autorepair.go:67) reset too. A frequently restarted daemon can run past the caps that bound expensive loops.
> - **Usage-limit hold:** `claudeUsageLimitedUntil` (engine/engine.go:366) is memory only. After a restart `settleClaudeLimitLabelSweep` (claude_limit_settle.go:68) strips `fabrik:claude-limit` from every item, every slot dispatches into the limit, and each item gets its explanatory comment again.
> - **Reinvoke cancel handles:** `issueCtxs` is registered only for stage-run dispatch (engine/poll.go:2282). A stop on a comment-review, review, CI-fix or rebase reinvoke pauses the item while the worker keeps running (item.go:3400).
> - **`feedbackGaps`:** never cleared (engine/feedback_dispatch_view.go:104-112). Once a gap's repair budget is spent, the feedback gate holds until a restart.
>
> ## Requirements
>
> - **R1:** The ejection count, trial window and auto-repair attempts are keyed to the member's PR head (or its Queued entry) and persisted with the train run record; the "consecutive" wording matches the behaviour.
> - **R2:** The usage-limit deadline survives a restart (persist it, or seed it from the label's presence and its comment), so a restart during an outage dispatches nothing into the limit and posts no duplicate comment.
> - **R3:** Every worker kind registers a cancel handle on one map (or on the Store's worker handle); a stop request cancels any running worker for the item.
> - **R4:** A `feedbackGaps` budget resets when the item's feedback set or head changes.
> - **R5:** `recordTrial` and the claude-limit sweep use `e.now()` so the sim clock seam reaches them.
>
> ## Acceptance
>
> - A restart test per item above (sim `RestartEnv` where it fits).
> - A test that a stop request on a running reinvoke cancels it.

## Background

The 2026-10-15 engine state audit (§4) found four places where the engine keeps decision-changing state only in memory, or never resets it. Each is harmless on a long-lived, quiet daemon and harmful on one that restarts often (self-upgrade, SIGHUP, crashes, operator restarts):

- **Merge-train caps.** The caps exist to bound expensive loops: repeated ejections of a member, trial-rate runaway, and auto-repair attempts. The ejection count is described as "consecutive" but does not behave that way, is not tied to the PR head it was earned against, and starts again from zero on every restart. The trial-rate window and the auto-repair attempt count also reset on restart. A frequently restarted daemon can therefore run past the very limits that protect against runaway work.
- **Claude usage-limit hold.** The account-wide "usage-limited until" deadline lives only in memory. After a restart, the label sweep concludes the hold has lifted and strips `fabrik:claude-limit` from every item, every worker slot dispatches straight back into the limit, and each item gets its explanatory comment posted a second time.
- **Reinvoke workers cannot be stopped.** Only stage-run dispatch registers a cancel handle. A stop request on a comment-review, review-reinvoke, CI-fix or rebase worker pauses the item but the worker keeps running (and pushing).
- **`feedbackGaps`.** A per-gap repair budget is never reset. Once spent, the feedback gate holds the item until the daemon is restarted, even if the feedback or the PR head has since changed and the gap no longer exists in its old form.

This must land before a second webhook source (#2143) is added, because that work depends on the engine's decision state being sound across restarts.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Train caps hold across restarts (Priority: P1)

An operator whose daemon restarts frequently expects the merge-train ejection cap, trial-rate cap and auto-repair cap to keep counting toward the same limit, rather than being granted a fresh budget on every start.

**Why this priority**: These caps bound expensive, repeated work (CI runs, Claude invocations); a bypass costs real budget and can loop indefinitely.

**Independent Test**: Drive a member to one below each cap, restart the engine (sim `RestartEnv`), trigger one more event of each kind, and observe the cap fires as if there had been no restart.

**Acceptance Scenarios**:

1. **Given** a member ejected N-1 times against its current PR head, **When** the engine restarts and the member is ejected once more, **Then** the ejection cap fires on that ejection.
2. **Given** a member whose PR head changes (new push) after earlier ejections, **When** it is ejected again, **Then** the count reflects only ejections against the current head.
3. **Given** a trial-rate window with trials already recorded, **When** the engine restarts mid-window, **Then** the window still includes those trials.
4. **Given** auto-repair attempts already spent for a member, **When** the engine restarts, **Then** no further attempt beyond the cap is made.
5. **Given** a member ejected, then successfully landed or otherwise passing a trial, **When** later ejected again, **Then** the count behaves as documented by the "consecutive" wording (a success breaks the run).

---

### User Story 2 - Usage-limit hold survives a restart (Priority: P1)

During a Claude usage-limit outage, a restart of the daemon must not cause every slot to dispatch into the limit nor re-post the explanatory comment on every affected item.

**Why this priority**: A restart during an outage currently wastes every slot and spams every item; outages are exactly when operators restart or upgrade.

**Independent Test**: Put the engine into a usage-limit hold with items carrying `fabrik:claude-limit`, restart the engine, run polls before the hold's end, and observe no Claude dispatch, no label removal and no new comment.

**Acceptance Scenarios**:

1. **Given** an active usage-limit hold with a known end time, **When** the engine restarts before that time, **Then** it dispatches no work into the limit and the labelled items keep `fabrik:claude-limit`.
2. **Given** the same restart, **When** polls run, **Then** no item receives a second explanatory comment for the same episode.
3. **Given** the restart happened and the end time then passes, **When** the next sweep runs, **Then** the hold lifts and the label is removed as it is today.
4. **Given** the hold's recorded end time has already passed at startup, **When** the engine starts, **Then** it behaves as if no hold were active and the sweep clears the labels.

---

### User Story 3 - A stop request cancels any running worker (Priority: P2)

When an operator (or the TUI) requests a stop on an item, whichever kind of worker is running for it — stage run, comment review, review reinvoke, CI-fix reinvoke, rebase reinvoke — is cancelled, not just the stage run.

**Why this priority**: Today the item is paused while a worker keeps running and pushing, so the pause is not honoured and further changes land on a supposedly stopped item.

**Independent Test**: Start each reinvoke kind with a blocked fake invoker, issue a stop request for the item, and assert the worker's context is cancelled and the worker exits.

**Acceptance Scenarios**:

1. **Given** a running comment-review, review, CI-fix or rebase reinvoke worker, **When** a stop request arrives for the item, **Then** that worker is cancelled.
2. **Given** a running stage-run worker, **When** a stop request arrives, **Then** it is cancelled exactly as today.
3. **Given** a worker that has finished, **When** a stop request arrives, **Then** nothing is cancelled and no stale handle remains registered.

---

### User Story 4 - A spent feedback-gap budget resets when the feedback changes (Priority: P2)

When the feedback set or PR head for an item changes, a previously exhausted repair budget for its gaps starts fresh, so the feedback gate does not hold the item until a restart.

**Why this priority**: The item is otherwise stuck behind the gate for as long as the daemon stays up.

**Independent Test**: Exhaust a gap's repair budget, then change the item's feedback set (or push a new head); observe the gap is eligible for repair again without a restart.

**Acceptance Scenarios**:

1. **Given** a gap whose repair budget is spent, **When** the item's feedback set changes, **Then** the budget for the affected gaps resets.
2. **Given** a gap whose repair budget is spent, **When** the PR head changes, **Then** the budget resets.
3. **Given** a spent budget and neither feedback nor head changing, **When** polls continue, **Then** the budget stays spent (no repair loop).

---

### User Story 5 - Time-dependent logic follows the sim clock (Priority: P3)

Trial recording and the claude-limit sweep read time through the engine clock, so sim tests can control it.

**Why this priority**: Required to test Stories 1 and 2 deterministically; otherwise a small hygiene item.

**Independent Test**: Advance the sim clock and observe the trial window and the claude-limit sweep respond to it.

**Acceptance Scenarios**:

1. **Given** a sim clock set into the future, **When** the trial-rate window and the claude-limit sweep evaluate, **Then** they use the sim clock rather than wall time.

---

### Edge Cases

- A persisted state file is corrupt or has an unknown version: treated as absent (today's behaviour), never a startup failure.
- A persisted usage-limit end time is implausibly far in the future: it is bounded by the same clamp the live hold uses.
- A member leaves the Queued state or its issue closes: its persisted counters are discarded rather than accumulating forever.
- A PR head changes between ejections: counts tied to the old head no longer apply.
- A restart occurs between recording a decision and persisting it: the engine errs toward counting the event (never toward granting extra budget).
- A stop request arrives for an item with more than one worker kind briefly overlapping: all are cancelled.
- Items carrying `fabrik:claude-limit` with no persisted hold (state from a version predating this change): behaviour stays safe — the hold is seeded conservatively rather than every label stripped and every slot released.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The merge-train ejection count, trial-rate window and auto-repair attempt count MUST be keyed to the member's PR head (or its Queued entry) and persisted with the train run record, so they survive a restart.
- **FR-002**: The ejection count MUST behave consistently with its documented meaning ("consecutive"); the code comment and the behaviour must agree.
- **FR-003**: Persisted train counters for a member MUST be discarded when the member is no longer Queued or its issue is closed.
- **FR-004**: The Claude usage-limit deadline MUST survive a restart, whether by persisting it or by seeding it from durable evidence (the `fabrik:claude-limit` label and its comment), such that a restart during an outage dispatches no work into the limit.
- **FR-005**: After a restart during a usage-limit hold, the engine MUST NOT post a second explanatory comment for the same detection episode on any item.
- **FR-006**: After a restart during a usage-limit hold, the claude-limit label sweep MUST NOT strip `fabrik:claude-limit` before the hold has actually lifted.
- **FR-007**: Every worker kind (stage run, comment review, review reinvoke, CI-fix reinvoke, rebase reinvoke) MUST register a cancel handle in one shared registry (or on the Store's worker handle), and MUST deregister it when the worker ends.
- **FR-008**: A stop request for an item MUST cancel any running worker for that item, of any kind.
- **FR-009**: A `feedbackGaps` repair budget MUST reset when the item's feedback set or PR head changes, and MUST NOT reset otherwise.
- **FR-010**: `recordTrial` and the claude-limit sweep MUST read time via the engine's clock (`e.now()`), not wall time.
- **FR-011**: Persisted state introduced here MUST tolerate corrupt, missing or unknown-version files by treating them as absent.

### Key Entities *(if applicable)*

- **Train run record**: the existing persisted per-(repo, base) merge-train episode record; gains per-member counters tied to PR head.
- **Usage-limit hold**: the account-wide deadline until which Claude dispatch is suspended.
- **Worker cancel handle**: the means of cancelling one running worker for an item.
- **Feedback gap**: a detected mismatch in feedback processing with a bounded repair budget.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For each of the ejection cap, trial-rate cap and auto-repair cap, a scenario that restarts the engine just below the cap fires the cap on the next qualifying event, identically to a scenario with no restart.
- **SC-002**: A restart during a usage-limit hold results in zero Claude dispatches, zero label removals and zero additional comments before the hold's end time.
- **SC-003**: A stop request on a running worker of each kind (five kinds) cancels it; zero cancel handles remain registered after workers end.
- **SC-004**: An item whose gap budget was spent becomes repairable again after its feedback set or head changes, with no restart.
- **SC-005**: Sim tests can drive the trial window and claude-limit sweep purely via the sim clock.
- **SC-006**: A restart test exists for each of the four state items, and a test exists that a stop request on a running reinvoke cancels it.

## Assumptions

- "Consecutive" is the intended semantics of the ejection count (as its comment says); the fix aligns behaviour to the comment rather than changing the comment to match the old behaviour. A successful pass of a trial or landing breaks the run.
- Persisting the train counters alongside the existing train run record, and the usage-limit deadline in the engine's `.fabrik/state/` area, are acceptable storage locations; the choice of exact mechanism (persist vs. seed from label) for the usage-limit hold is left to Plan, provided FR-004–FR-006 hold.
- When no persisted usage-limit state exists but items carry `fabrik:claude-limit`, the engine treats the hold as still possibly active until it can establish otherwise.
- Behaviour on a clean first start (no persisted state) is unchanged from today.
- Persisting on every update is acceptable; these events are infrequent.

## Out of Scope *(optional)*

- Moving other engine-private state (dedup sets, caches, presentation memory, sessions) into the Store; the audit judged those legitimately private.
- Deriving anomaly retries/holds from the Store's `PendingRetry`/`GateHold` (noted separately in the audit).
- The Validate completion SHA rebuild (audit §1.3, #2343) and other §1–§3 items.
- The second webhook source itself (#2143) and its requirements (audit §5).
- Changing the cap values or the usage-limit reset-time derivation.

## Source References *(optional)*

- `docs/design/engine-state-audit-2026-10.md` §4 (items 4.1–4.4)
- #2143 (second webhook source, depends on this work)
