# Feature Specification: Live merge-train TUI row and one History entry per train outcome

**Feature Branch**: `fabrik/issue-2050`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "tui: live merge-train row (membership, phase, PR, time in phase) and one History entry per train outcome. #1661 gave the merge train a row in the TUI's active-jobs panel, but it's hard to read: stale membership (the row shows the batch as it started — after two members were ejected at assembly it still said '5 member(s)' while the trial held 3 (shadoworg/liminis-models, 2026-10-05)); no phase (waiting for a slot, waiting on CI, bisecting and a wedged trial all look like 'in progress'; a trial whose CI never started sat like that for two hours); no History (`runMergeTrainWorker` always emits `JobCompletedEvent` with `Skipped: true`, so landed, red, bisected, ejected and dissolved trains leave the same trace, or none). Report: #2036 (on the public triage board). R1 Live membership: update the row's title as members are admitted, ejected or deferred, e.g. `3 of 5: #1555 #1562 #1576 (ejected #1549 #1560)`. R2 Phase, PR and time in phase: show the current phase (`waiting for slot`, `assembling`, `resolving conflicts on #N`, `trial CI #NNNN`, `bisecting (step n)`, `landing`, `one-at-a-time`, `catching up #N` for the singleton catch-up), the trial or member PR whose CI it's waiting on, and how long it has been in the current phase, so a wedged trial stands out. R3 One History entry per train episode, with its real outcome: `landed` (members, integration PR), `red → bisected` (the ejected poisoner), `ejected at assembly` (member and reason), `one-at-a-time`, `abandoned` (e.g. CI never started), `dissolved` (nothing to land); `Success` reflects the outcome instead of a blanket `Skipped: true`. R4 One source of truth: the row and History are driven by the same train phase transitions that write the board status line (the status-line issue earlier in this chain); use one internal 'train phase changed' hook that feeds both, so the TUI and the board always tell the same story. R5 Presentation only: no change to train decisions, matching #1661's R8. Scope: in scope — TUI event types and fields for the train row and History, the shared phase hook, and the emit points in `engine/merge_train.go`; out of scope — the board field writer itself (already built earlier in the chain), and any change to train behaviour. Mention #2036 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2036. This issue's own number gets `Closes`. Acceptance — unit tests: ejecting a member at assembly updates the row title; each phase transition updates the phase and time-in-phase; each outcome produces exactly one History entry with the right `Success` value; the board status line and the TUI row receive the same phase sequence for one scripted train. TUI rendering test (golden or model-level) for the row and a History entry. Neutralisation of the History-outcome test."

## Background

#1661 gave the merge train a row in the TUI's active-jobs panel (keyed by repo, issue number 0), emitted once when the train worker starts and removed when it ends. The row's title is built once from the batch as dispatched and never changes, the row carries no notion of what the train is currently doing, and the completion event is always marked skipped, so the row disappears without a trace in History.

A real incident (shadoworg/liminis-models, 2026-10-05, reported as #2036 on the public triage board) shows the cost: two members were ejected at assembly, yet the row kept saying "5 member(s)" while the trial held 3. Waiting for a worker slot, waiting on CI, bisecting and a wedged trial all looked identical, and a trial whose CI never started sat looking "in progress" for two hours. Afterwards, landed, red, bisected, ejected and dissolved trains all left the same trace in History, or none.

The preceding issue in this chain (#2048) added a display-only status-line field on the project board, written from the merge train's phase transitions. This issue makes the TUI tell the same story, from the same transitions, so the board and the TUI cannot diverge. Report: #2036 (mentioned for context only; it stays open until a release containing the fix ships).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See who is in the train right now (Priority: P1)

As an operator watching the TUI, I want the merge-train row's title to reflect the batch's current membership, so I am never told the train holds members it has already ejected or deferred.

**Why this priority**: The stale "5 member(s)" row was the concrete misreport in the incident, and membership is the cheapest piece of the row to get right.

**Independent Test**: Script a train of five members in which two are ejected at assembly; observe the row title after each membership change.

**Acceptance Scenarios**:

1. **Given** a train dispatched with members #1549 #1555 #1560 #1562 #1576, **When** #1549 and #1560 are ejected at assembly, **Then** the row title reads like `3 of 5: #1555 #1562 #1576 (ejected #1549 #1560)`.
2. **Given** a member is deferred (for example by overlap), **When** the membership changes, **Then** the title drops it from the active list and names it as deferred rather than leaving it listed as a member.
3. **Given** a bisection narrows the active set, **When** the set changes, **Then** the title shows the current active members out of the original batch size.

---

### User Story 2 - See what phase the train is in, on which PR, and for how long (Priority: P1)

As an operator, I want the row to show the current phase, the PR whose CI is being waited on, and the time spent in the current phase, so a wedged trial stands out from a healthy one.

**Why this priority**: A trial whose CI never started was invisible for two hours; phase plus time-in-phase is what makes "stuck" visible at a glance.

**Independent Test**: Drive a scripted train through every phase and assert, after each transition, the displayed phase, the PR (where relevant), and that the time-in-phase restarts at the transition and grows while the phase is unchanged.

**Acceptance Scenarios**:

1. **Given** the train is waiting for a worker slot for conflict resolution, **When** the row is rendered, **Then** it shows `waiting for slot` with an elapsed time.
2. **Given** the train moves from assembling a trial to waiting on trial CI, **When** the phase changes, **Then** the row shows `trial CI #NNNN` naming the trial PR and the elapsed time restarts from zero.
3. **Given** the train is bisecting, **When** it starts step n, **Then** the row shows `bisecting (step n)`.
4. **Given** the singleton catch-up is waiting on a member's PR CI, **When** the row is rendered, **Then** it shows `catching up #N` and names that PR.
5. **Given** a phase has lasted unusually long, **When** the row is rendered, **Then** its elapsed time is visibly larger than that of a fresh phase.

---

### User Story 3 - Find out afterwards how each train ended (Priority: P1)

As an operator, I want exactly one History entry per train episode that states its real outcome, so I can tell a landed batch from an abandoned or dissolved one without reading the daemon log.

**Why this priority**: Today the outcome is erased; History is the only durable operator-facing record in the TUI.

**Independent Test**: Run one scripted train per outcome and assert exactly one History entry appears for each, with the expected outcome text and `Success` value.

**Acceptance Scenarios**:

1. **Given** a batch lands, **When** the episode ends, **Then** one History entry states `landed` with the members and the integration PR, marked successful.
2. **Given** a red trial is bisected and a poisoner is ejected, **When** the episode ends, **Then** one History entry states `red → bisected` and names the ejected poisoner.
3. **Given** members are ejected at assembly, **When** the episode ends, **Then** the History entry names each ejected member and its reason.
4. **Given** the train falls back to one-at-a-time landing, **When** the episode ends, **Then** the entry states `one-at-a-time`.
5. **Given** trial CI never starts and the train gives up, **When** the episode ends, **Then** the entry states `abandoned` with the cause and is marked unsuccessful.
6. **Given** every member leaves the batch before there is anything to land, **When** the episode ends, **Then** the entry states `dissolved` and the row is removed.
7. **Given** any episode, **When** it ends, **Then** exactly one History entry is written (never zero, never two), and the row is removed.

---

### User Story 4 - The TUI and the board tell the same story (Priority: P2)

As an operator, I want the TUI row and the board status line to be driven by the same train phase transitions, so they never disagree.

**Why this priority**: Two independently maintained descriptions of the same state will drift; a single source removes the class of bug.

**Independent Test**: Script one train with a recording board writer and a recording TUI sink; assert both receive the same ordered sequence of phase transitions.

**Acceptance Scenarios**:

1. **Given** one scripted train covering queue, assembly, conflict resolution, trial CI, bisection and landing, **When** it completes, **Then** the sequence of phases received by the board status line and by the TUI row is identical.

---

### Edge Cases

- Membership shrinks to one member (singleton fast path or catch-up): the title and phase still render correctly (`3 of 5` becomes `1 of 5`).
- A train that fails during setup, before any membership or phase exists: the row still disappears and exactly one History entry (an abandoned-style outcome with the cause) is written rather than none.
- A train whose members are all ejected or deferred: History records the dissolved/ejected outcome once.
- An episode that combines events (members ejected at assembly, then the survivors land): one entry is written; the primary outcome is stated and ejections are listed in its detail.
- A restart mid-train: the interrupted episode produces no entry; the next worker begins a fresh episode.
- Sibling partitions of one repo (different `base:`) share a row today (#1648/#1661); each partition's episode still yields its own History entry.
- A phase repeats (for example a second trial CI wait): the elapsed time restarts at each transition.
- Rapid transitions: the row reflects the latest phase and never a stale one.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The merge-train row's title is updated whenever the train's membership changes (members admitted, ejected, deferred, or narrowed by bisection), and reads like `3 of 5: #1555 #1562 #1576 (ejected #1549 #1560)`.
- **FR-002**: The row displays the current phase, drawn from: `waiting for slot`, `assembling`, `resolving conflicts on #N`, `trial CI #NNNN`, `bisecting (step n)`, `landing`, `one-at-a-time`, `catching up #N`.
- **FR-003**: When the phase is waiting on CI (trial CI or singleton catch-up), the row names the PR whose CI is being waited on.
- **FR-004**: The row displays the time elapsed in the current phase, restarting at each phase transition.
- **FR-005**: Each train episode produces exactly one History entry when it ends, whatever the exit path (landed, red → bisected, ejected at assembly, one-at-a-time, abandoned, dissolved, or a setup failure).
- **FR-006**: A `landed` entry names the landed members and the integration PR; a `red → bisected` entry names the ejected poisoner; an `ejected at assembly` entry names each ejected member and its reason; an `abandoned` entry names its cause (for example CI never started).
- **FR-007**: The History entry's `Success` value reflects the outcome and is no longer a blanket `Skipped: true`.
- **FR-008**: The row and the History entry are driven by the same internal "train phase changed" hook that also drives the board status line; there is no separate TUI-only phase tracking.
- **FR-009**: For one scripted train, the board status line and the TUI row receive the same ordered sequence of phase transitions.
- **FR-010**: No merge-train decision, condition or control flow changes; the work only affects what is emitted to the TUI and to the shared hook (matching #1661's R8).
- **FR-011**: Tests cover: ejecting a member at assembly updates the row title; each phase transition updates the phase and time-in-phase; each outcome yields exactly one History entry with the correct `Success`; the board and TUI see the same phase sequence for one scripted train.
- **FR-012**: A TUI rendering test (golden or model-level) covers the live row and a History entry.
- **FR-013**: The History-outcome test is shown to fail when the outcome emission is neutralised (restored to the blanket skipped completion).
- **FR-014**: The change adds no new label and no new GitHub write, and the board status line's behaviour (lines written, when, how often) is unchanged.

### Key Entities *(if applicable)*

- **Train phase**: the named step the train is in (the list in FR-002), with the PR it concerns, if any, and the time it was entered.
- **Train membership**: the original batch size, the currently active members, and the ejected/deferred members with their reasons.
- **Train episode outcome**: the terminal result of one worker run (landed, red → bisected, ejected at assembly, one-at-a-time, abandoned, dissolved), with its detail and success flag, rendered as one History entry.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: At any moment during a train, the row's member count and member list match the members the train actually holds.
- **SC-002**: An operator can tell from the row alone which phase the train is in, which PR it is waiting on, and how long it has been there; a trial stuck for an hour shows roughly an hour in its phase.
- **SC-003**: Across scripted trains covering every listed outcome, each produces exactly one History entry (100%) with the expected outcome text and `Success` value.
- **SC-004**: For a scripted train, the phase sequences seen by the board status line and the TUI row are identical.
- **SC-005**: With the outcome emission neutralised, the History-outcome test fails.

## Assumptions

- The History entry's `Success` is true for outcomes where the train did its job (landed; red → bisected where the poisoner was isolated and survivors proceeded; one-at-a-time that landed; dissolved with nothing to land) and false for outcomes where it did not (abandoned, setup failure, nothing landed because of a fault). This is a low-impact, reversible choice and can be adjusted in Research/Plan.
- An "episode" is one train worker run for one (repo, base) partition; an episode that combines events yields a single entry stating its primary outcome with the other events as detail.
- The phase wording is as given in the request; exact formatting (separators, truncation of long member lists on small screens) follows the existing TUI conventions.
- The existing shared row keyed by repo (#1661) is kept; the known display caveat for sibling partitions sharing one row is unchanged by this issue.
- The board status-line work from #2048 is already merged and provides the transition points this hook reuses.

## Out of Scope *(optional)*

- The board field writer itself (built in #2048) and any change to what it writes.
- Any change to merge-train behaviour, decisions or timing.
- Giving each sibling partition its own TUI row (separate display concern from #1661/#1648).
- Alerting, thresholds or highlighting that flags a phase as "wedged" beyond showing elapsed time.
- Closing or updating the community report #2036 (done by hand after a release ships).

## Source References *(optional)*

- #2036 (community report, public triage board), #1661 (original train job row), #2048 (board status line), #1648 (per-base partitions)
- `adrs/1661-merge-train-job-row-emission-timing.md`
