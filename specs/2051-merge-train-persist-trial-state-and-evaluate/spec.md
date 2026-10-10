# Feature Specification: Merge-train persisted trial state and per-poll trial evaluation

**Feature Branch**: `fabrik/issue-2051`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description (verbatim original issue body):

> ## Problem
>
> A merge-train worker is a goroutine that runs a whole train episode synchronously: assemble, open the trial PR, block in `pollTrainCI` for 10–25 min, then bisect (recursively, one trial after another) and land. Stages work differently. A `wait_for_ci` stage finishes its worker, the item carries `fabrik:awaiting-ci`, and later polls evaluate it.
>
> Because the train holds its whole episode in a goroutine:
> - **A restart can't resume a trial's CI wait.** `reconstructTrainState` finishes landing an already-merged integration PR and closes stale open trial PRs, but an in-flight trial and any bisection progress are thrown away and rebuilt.
> - **A wedged trial shows up only as a goroutine that never returns.**
> - **Train phase is visible only from inside that goroutine.**
>
> The slot-release issue earlier in this chain removes the worst cost (a slot held for the whole CI wait). This is the structural follow-up.
>
> Report: the follow-up comment on #2037 (public triage board).
>
> ## Requirements
>
> **R1. Persist trial state.**
> - Keep each open trial as engine state: train key, trial branch and head SHA, integration PR, members, phase (`trial-ci`, `bisect` with its stack/position, `landing`, `one-at-a-time` with its position) and phase start time.
> - It must survive a daemon restart. The shape (file under `.fabrik/state/`, or derived from trial PR bodies and branch names) is a Plan decision, recorded in an ADR.
>
> **R2. Evaluate on each poll.**
> - The worker exits once the trial PR is open.
> - Each poll evaluates every open trial's CI the way `checkCIGate` evaluates items. Green → land. Red → schedule the next step (bisect half, eject, re-form). Only steps that need Claude take a slot, briefly.
>
> **R3. Restart resumes.** A restart mid-trial or mid-bisection resumes the same trial and position instead of discarding it.
>
> **R4. Bisection as data.** Convert the recursive `bisect` into an explicit state machine whose state is part of R1. Results must match today's halving order (bors-ng order, ADR-059 D4) and cost cap.
>
> **R5. Phase source.** The train phase hook from the TUI/status-line issues reads its phase from this state.
>
> ## Scope
>
> - In scope: train worker lifecycle, persisted trial state, per-poll trial evaluation, bisection state machine, restart reconstruction, and an ADR that supersedes the relevant parts of ADR-059's worker model.
> - Out of scope: batch composition, landing paths, and CI classification (unchanged and reused).
> - Mention #2037 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2037. This issue's own number gets `Closes`.
>
> ## Acceptance
>
> - Sim scenarios:
>   - a restart mid-trial resumes the same trial PR and lands it;
>   - a restart mid-bisection resumes at the same step;
>   - a trial with no CI progress is surfaced by per-poll evaluation within one poll of its deadline.
> - The bisection order and cost cap equal today's on the existing bisection tests, unmodified.
> - ADR (`adrs/<this issue>-...md`). `docs/state-machine.md` (merge-train worker model) rewritten and `docs/llms-full.txt` regenerated.

## Background

Today a merge-train worker runs an entire train episode inside one goroutine: it assembles a trial branch, opens the trial PR, blocks while polling CI (typically 10–25 minutes), then bisects recursively (one trial after another) and lands. Ordinary `wait_for_ci` stages behave differently — their worker finishes, the item carries `fabrik:awaiting-ci`, and later polls evaluate CI and decide what happens next.

Holding the whole episode in a goroutine has three costs:

1. **A restart cannot resume a trial.** Startup reconstruction only finishes landing an already-merged integration PR and closes stale open trial PRs. An in-flight trial, and any bisection progress, is discarded and rebuilt from scratch, wasting the CI time already spent.
2. **A wedged trial is invisible.** It manifests only as a goroutine that never returns; no engine-level state says which trial is stuck, in which phase, or since when.
3. **Train phase is observable only from inside the goroutine.** Any operator-facing surface (TUI, status line) that wants to show "trial CI", "bisecting", "landing" has no durable source to read.

The preceding slot-release change (#2046) already removed the worst cost — a worker slot held for the entire CI wait. This issue is the structural follow-up: make the trial a piece of persisted, per-poll-evaluated engine state, the way `fabrik:awaiting-ci` makes a stage's CI wait one. It originates from the follow-up comment on #2037 (public triage board; mentioned for context only).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A restart mid-trial resumes the same trial (Priority: P1)

An operator restarts (or upgrades) the daemon while a merge-train trial PR is waiting on CI. After restart the engine finds the persisted trial, keeps evaluating the *same* trial PR, and lands the batch when its CI goes green — without assembling a new trial or re-spending the CI time already elapsed.

**Why this priority**: This is the core user-visible benefit and the minimal slice: persisted trial state plus per-poll evaluation of a trial's CI.

**Independent Test**: In the sim bed, open a trial, restart the engine mid-CI-wait, make CI go green, and assert the same trial PR is landed and no second trial was built.

**Acceptance Scenarios**:

1. **Given** a trial PR is open with CI pending and its state persisted, **When** the engine restarts and CI later turns green, **Then** the same trial PR is landed and no new trial branch/PR is created.
2. **Given** the same setup, **When** the engine restarts and CI turns red, **Then** the engine takes the same next step (bisect, eject or re-form) it would have taken without the restart.
3. **Given** a trial PR is open and the worker has exited, **When** a poll runs, **Then** the engine evaluates that trial's CI without any worker goroutine or worker slot being held for it.

---

### User Story 2 - A restart mid-bisection resumes at the same step (Priority: P1)

After a red trial the train bisects. If the daemon restarts partway through, bisection continues from the same position — same remaining halves, same already-decided results — rather than restarting the bisection or the whole episode.

**Why this priority**: Bisection is the longest-running part of an episode (many sequential trials), so losing its progress is the most expensive form of the problem.

**Independent Test**: Run a multi-member batch through a bisection in the sim, restart at a chosen bisection step, and assert the next trial built is the one the uninterrupted run would have built next, and the final outcome (landed/ejected members) is identical.

**Acceptance Scenarios**:

1. **Given** a batch is mid-bisection with some halves already resolved, **When** the engine restarts, **Then** the resumed bisection evaluates the next pending half in the same order as an uninterrupted run.
2. **Given** bisection has consumed part of the trial cost cap, **When** the engine restarts, **Then** the already-consumed trials still count against the cap.
3. **Given** the existing bisection tests, **When** the bisection is run through the new state machine, **Then** halving order and cost cap are identical to today's (bors-ng order, ADR-059 D4), with those tests unmodified.

---

### User Story 3 - A stuck trial is surfaced by per-poll evaluation (Priority: P2)

A trial whose CI makes no progress (never starts, hangs) is detected by the poll loop itself — not by a goroutine that never returns — within one poll of its deadline, and handled by the same path as other trial CI timeouts/infrastructure failures.

**Why this priority**: Removes the "wedged goroutine" blind spot; valuable but depends on the persisted state from Stories 1–2.

**Independent Test**: In the sim, open a trial whose CI never progresses, advance the clock past the trial deadline, and assert the next poll surfaces and handles the stuck trial.

**Acceptance Scenarios**:

1. **Given** a trial with no CI progress, **When** the clock passes its deadline and one poll runs, **Then** the engine surfaces the stuck trial (log line and the handling the train already applies to a timed-out trial).
2. **Given** a trial still within its deadline, **When** a poll runs, **Then** it is left alone.

---

### User Story 4 - Train phase is readable from engine state (Priority: P3)

Anything that wants to display the train's current phase (trial CI, bisect with position, landing, one-at-a-time with position) and how long it has been in it reads that from the persisted trial state, not from inside a worker goroutine.

**Why this priority**: Enables the TUI/status-line phase hook; no behaviour change by itself.

**Independent Test**: With a trial in each phase, read the phase source and assert it reports the phase, position and phase start time.

**Acceptance Scenarios**:

1. **Given** an open trial in any phase, **When** the phase source is read, **Then** it returns the phase, position (where applicable) and phase start time.
2. **Given** no open trial for a partition, **When** the phase source is read, **Then** it reports no active trial.

---

### Edge Cases

- Restart while the trial PR was already merged but landing bookkeeping (Done moves, closes) was incomplete — existing landing-resume behaviour must still apply and must not double-land (ADR-1871 live-status guard).
- Persisted state references a trial PR/branch that no longer exists or was closed externally — the entry must be discarded or reconciled safely, falling back to today's rebuild behaviour, never wedging the partition.
- Persisted state is corrupt, truncated or from an older/newer format — treated as absent (today's behaviour), never fatal.
- A member in the persisted trial was moved off the holding stage, closed, paused or ejected while the daemon was down — it must not be landed from stale state.
- The base branch moved while the daemon was down — the resumed trial's pinned base must be re-validated using the existing base-moved handling rather than landing on a stale base.
- Two partitions (repo, base) with trials open at once — each is evaluated and persisted independently.
- A poll arrives while a Claude-requiring step (conflict resolution) for the same trial is still running — the trial must not be evaluated or advanced twice concurrently.
- The runaway guard, ejection counts and auto-repair/catch-up in-memory state (which reset on restart today) keep their present semantics unless persistence of a given counter is explicitly required for the cost cap (see FR-007).
- A restart mid-`landing` or mid-`one-at-a-time` resumes at the recorded position rather than re-landing members already landed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine MUST keep, for each open merge-train trial, durable state containing at least: the train key (repo + base partition), the trial branch and its head SHA, the integration/trial PR, the member set, the current phase (`trial-ci`, `bisect` with its stack/position, `landing`, or `one-at-a-time` with its position), and the phase start time.
- **FR-002**: That state MUST survive a daemon restart, including a self-upgrade re-exec; the storage shape is a Plan decision recorded in an ADR.
- **FR-003**: A merge-train worker MUST exit once the trial PR is open and its state recorded, rather than blocking for the trial's CI wait.
- **FR-004**: Each poll MUST evaluate the CI of every open trial using the existing CI classification (unchanged and reused), analogously to how `checkCIGate` evaluates `fabrik:awaiting-ci` items.
- **FR-005**: On green CI the poll MUST proceed to land the trial using the existing landing paths; on red CI it MUST schedule the next step (bisect half, eject, or re-form) exactly as the train does today.
- **FR-006**: Only steps that invoke Claude (conflict resolution) MAY take a worker slot, and only around the Claude call, consistent with #2046; trial CI evaluation MUST take no slot.
- **FR-007**: Bisection MUST be an explicit state machine whose state is part of the persisted trial state. Its halving order (bors-ng order, ADR-059 D4) and trial cost cap MUST equal today's, including after a restart (trials already consumed in the episode still count).
- **FR-008**: After a restart, the engine MUST resume an interrupted trial or bisection at the same trial and position instead of discarding it, provided the persisted state is still valid (trial PR open, members still eligible, base unchanged).
- **FR-009**: If persisted state is missing, corrupt, stale or inconsistent with GitHub, the engine MUST fall back to today's reconstruction behaviour (finish an already-merged landing, close stale open trial PRs, rebuild) and MUST NOT wedge the partition or double-land any member.
- **FR-010**: A trial with no CI progress MUST be surfaced by per-poll evaluation no later than one poll after its deadline, and handled by the train's existing timeout/infrastructure-failure handling.
- **FR-011**: A given trial MUST never be advanced by two actors at once (a poll evaluation and an in-flight Claude step, or two polls); the existing ownership/in-flight guard semantics (ADR-1208) MUST be preserved.
- **FR-012**: The persisted trial state MUST be the single source from which the train phase (and position, and time in phase) is exposed to the TUI/status-line phase hook.
- **FR-013**: Batch composition (admission, overlap, live-landed filters), landing paths and CI classification MUST behave as today; the existing merge-train unit and sim tests MUST pass without modification, in particular the existing bisection tests.
- **FR-014**: Persisted state MUST be removed when its trial/episode ends (landed, dissolved, ejected, abandoned) so no stale entry outlives its trial.
- **FR-015**: An ADR (`adrs/2051-…md`) MUST record the storage-shape decision and supersede the relevant parts of ADR-059's worker model; `docs/state-machine.md` (merge-train worker model) MUST be rewritten to the as-built behaviour and `docs/llms-full.txt` regenerated.
- **FR-016**: Sim-bed scenarios MUST cover: restart mid-trial resumes and lands the same trial PR; restart mid-bisection resumes at the same step; a no-progress trial is surfaced within one poll of its deadline.

### Key Entities *(if applicable)*

- **Trial record**: One open trial's persisted state — train key, trial branch + head SHA, integration PR, members, phase, phase start time.
- **Phase**: `trial-ci`, `bisect` (stack/position), `landing`, `one-at-a-time` (position).
- **Bisection state**: The explicit stack of pending member subsets, results so far and trial count against the cost cap.
- **Train key**: The (repo, base branch) partition identity used throughout the train.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A daemon restart at any point during a trial's CI wait results in the same trial PR being evaluated and landed (or handled on red) with zero additional trial PRs created because of the restart.
- **SC-002**: A daemon restart at any bisection step results in the same sequence of subsequent trials and the same final landed/ejected member sets as an uninterrupted run.
- **SC-003**: A trial whose CI makes no progress is surfaced within one poll interval after its deadline.
- **SC-004**: All existing bisection tests pass unmodified, demonstrating identical halving order and cost cap.
- **SC-005**: While a trial waits on CI, no worker slot (`MaxConcurrent`) and no long-lived goroutine is held for it.
- **SC-006**: The train phase of every open trial can be read from engine state without access to a worker goroutine.
- **SC-007**: Full `go test -race ./...` passes, including the three new sim scenarios; `docs/llms-full.txt` matches its regeneration.

## Assumptions

- The persistence mechanism (state file under `.fabrik/state/` vs derived from trial PR bodies/branch names) is deliberately left to the Plan stage, as the issue states; this spec only requires the properties above.
- Unavailable or invalid persisted state degrades to today's behaviour (rebuild), which is acceptable and safe; resumption is an optimisation of correctness-neutral work, not a new correctness requirement.
- Counters that are in-memory today (ejection counts, auto-repair attempts, catch-up caps) stay in-memory unless Plan finds that the bisection cost cap requires persisting a trial counter; FR-007 requires the cap to hold across a restart *within an episode*.
- "The way `checkCIGate` evaluates items" means the same polling cadence and the same CI classification primitives, not the same labels; whether per-trial markers are labels or only persisted records is a Plan decision.
- The existing trial deadline/timeout values are reused; no new configuration keys are required by this issue.
- The dependency #2050 is expected to land before implementation; this spec does not depend on its content beyond that ordering.

## Out of Scope *(optional)*

- Batch composition (admission, overlap filtering, caps) and post-landing invalidation.
- Changes to landing paths (batch, singleton, fast path, catch-up, one-at-a-time landing logic) beyond being invoked from poll-driven evaluation.
- Changes to CI classification, flake re-run, startup-failure handling.
- Building the TUI/status-line display itself (this issue only provides the phase source).
- Persisting unrelated in-memory train state (overlap cache, policy memos, auto-repair state).

## Source References *(optional)*

- ADR-059 (internal merge train; D3 trial/worker model, D4 bisection order)
- ADR-2046 (worker slot only around Claude calls) and `docs/state-machine.md` "Worker-slot scope"
- ADR-1208 (ownership of Queued members), ADR-1871 (live-status resume guard)
- Follow-up comment on #2037 (mentioned for context)
