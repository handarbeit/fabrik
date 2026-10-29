# Feature Specification: Resume issues paused by a daemon clean stop on the next startup

**Feature Branch**: `fabrik/issue-2486`
**Created**: 2026-10-29
**Status**: Specified
**Input**: User description: "## Problem

A SIGINT/SIGTERM clean stop pauses every in-flight issue (`runShutdownPause`, ADR-1393) with `fabrik:paused` + `fabrik:awaiting-input` and the `<!-- fabrik:pause-cause=daemon-stop -->` marker. Nothing lifts that pause except a human. An operator who stops and restarts the daemon (a config change, a reboot) therefore leaves every interrupted issue parked until they notice. In the report, 17 issues lost 39 h this way; five restarts on one day paused three issues for 2.3–3.3 h each.

A SIGHUP restart already drains instead of pausing (#2259, v0.0.85), but a deliberate stop/start is common and legitimate.

## Requirements

1. On startup, an issue whose **current** pause was set by a daemon clean stop is resumed automatically: the engine removes `fabrik:paused`/`fabrik:awaiting-input` and the stage is redispatched normally (a continuation, so its session resumes per ADR 2300).
2. \"Current pause was set by the clean stop\" is decided from durable GitHub state only: the newest engine pause comment carries the daemon-stop marker **and** no later pause (any cause) or label change by a human supersedes it. Any doubt (unreadable timeline, a later human pause, a missing marker) → leave it paused.
3. A human who pauses an issue after the stop keeps it paused; a human comment is not required to resume a clean-stop pause.
4. One audit line per resumed issue in the log and one short `🏭 **Fabrik —` comment saying it was resumed after the restart.
5. An opt-out (flag + env + YAML, default **on**) for operators who rely on stop-means-stay-paused.

## Out of scope

Crash recovery (no marker → unchanged). SIGHUP behaviour (unchanged).

## Acceptance

- Sim twin: stop with an in-flight worker → restart → item resumes without a comment; with a human pause applied in between → stays paused.
- Neutralising the resume (seam) makes the first test fail.
- `docs/state-machine.md` updated (pause/resume section) and `docs/llms-full.txt` regenerated."

## Background

When the daemon receives a clean stop (SIGINT/SIGTERM), it interrupts every in-flight worker and pauses the affected issues so no work is silently lost. Each such issue is marked as paused, as awaiting input, and carries a hidden marker recording that the pause was caused by the daemon stopping (not by a question, a timeout or a failure). Today the only way to lift any pause is a human action, so a pause that the operator never intended to be a "decision point" behaves like one.

The consequence is that a deliberate stop/start of the daemon (a config change, an upgrade, a reboot) leaves every issue that was mid-flight parked until someone notices. The community report that motivated this measured the cost over October 1–26 on one project: 17 issues lost 39 hours this way, and five restarts on a single day each paused three issues for 2.3–3.3 hours.

A SIGHUP restart already avoids the problem by draining in-flight work rather than pausing it (#2259, v0.0.85). A deliberate stop followed by a start is nonetheless common and legitimate, and should be just as painless.

The cause of a pause is already recorded durably on GitHub (the hidden marker in the engine's pause comment, plus the pause-cause label), so the engine can recognise, after a restart, which pauses it set itself because it was stopping.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Interrupted work continues after a restart (Priority: P1)

An operator stops the daemon while issues have workers running, then starts it again. Each interrupted issue picks up where it left off, with no human action.

**Why this priority**: This is the whole point of the change; it removes the hours of idle time reported.

**Independent Test**: Stop the daemon (clean stop) while one issue has an in-flight worker, restart it, and observe that the issue is no longer paused and its stage is dispatched again without anyone commenting.

**Acceptance Scenarios**:

1. **Given** an issue paused by a daemon clean stop and no later pause or label change by a human, **When** the daemon starts, **Then** the issue's pause is lifted and its stage is redispatched as a continuation of the interrupted run.
2. **Given** that resumed issue, **When** it is redispatched, **Then** it resumes its prior session under the same rules that apply to any continuation (session resume per ADR 2300), not as a fresh start.
3. **Given** the same issue, **When** the resume happens, **Then** no human comment was required, and the issue carries exactly one short Fabrik comment saying it was resumed after the restart.

---

### User Story 2 - A human's pause is never overridden (Priority: P1)

A human who deliberately pauses an issue after the daemon stopped (or while it was down) keeps it paused across the restart.

**Why this priority**: Auto-resuming something a person chose to hold would be harmful and erode trust in the feature; it is as important as the resume itself.

**Independent Test**: Clean-stop the daemon with an in-flight worker, apply a human pause to the issue while the daemon is down, restart, and observe the issue stays paused and untouched.

**Acceptance Scenarios**:

1. **Given** an issue paused by a clean stop, **and** a human subsequently paused it (any cause) or changed its pause-related labels, **When** the daemon starts, **Then** the issue stays paused and the engine writes nothing to it.
2. **Given** an issue whose newest engine pause comment lacks the daemon-stop marker (any other pause cause), **When** the daemon starts, **Then** the issue stays paused.

---

### User Story 3 - Doubt means "stay paused" (Priority: P2)

When the engine cannot establish with certainty that the current pause came from a clean stop, it leaves the issue paused.

**Why this priority**: A pause nobody lifts is recoverable by a human; a wrongly lifted pause is not. The safe polarity must be explicit.

**Independent Test**: Make the issue's history unreadable (or ambiguous) at startup and confirm the issue remains paused with no writes.

**Acceptance Scenarios**:

1. **Given** the issue's timeline or comments cannot be read at startup, **When** the daemon starts, **Then** the issue stays paused and nothing is written.
2. **Given** a crash (no clean stop, so no daemon-stop marker was written), **When** the daemon starts, **Then** behaviour is unchanged from today.

---

### User Story 4 - Operators can opt out (Priority: P3)

An operator who relies on "stopped means stay paused" can turn the automatic resume off.

**Why this priority**: Some operators use a stop as a deliberate hold; the default favours the common case but must be reversible.

**Independent Test**: Start the daemon with the setting off against an issue paused by a clean stop and confirm it stays paused.

**Acceptance Scenarios**:

1. **Given** the opt-out is set (by flag, environment variable or YAML), **When** the daemon starts, **Then** clean-stop pauses are left in place exactly as today.
2. **Given** no setting is provided, **When** the daemon starts, **Then** automatic resume is on.

---

### Edge Cases

- A pause comment carrying the daemon-stop marker followed by a later human comment but no human pause or label change: the human comment alone does not supersede the clean-stop pause (a comment is not a pause), and the issue is still resumed.
- A human removes and re-applies the pause labels after the stop: this is a later human label change, so the issue stays paused.
- A clean stop paused an issue, then a different engine pause (any other cause) was recorded later: the newest engine pause is not a daemon-stop one, so the issue stays paused.
- The daemon is stopped cleanly again shortly after a resume: the issue is paused again with a new daemon-stop marker and resumed again on the next start; each pause/resume cycle yields its own audit line and comment, and a single pause is never resumed twice.
- An issue that was closed or left the board while the daemon was down: not resumed.
- Many issues paused by one stop: each is evaluated independently; a failure or doubt on one does not prevent resuming the others.
- The resume write itself fails (for example a GitHub error): the issue stays paused and the attempt can be retried; it must never end up half-resumed (for example with the pause removed but no continuation possible).
- A pause by a human that carries no engine comment at all (never touched by a clean stop): not affected.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On daemon startup, an issue whose current pause was set by a daemon clean stop MUST be resumed automatically: its pause labels are removed and its stage is redispatched normally as a continuation of the interrupted run (session resume governed by ADR 2300).
- **FR-002**: An issue's current pause counts as "set by a daemon clean stop" only if, from durable GitHub state alone, the newest engine pause comment carries the daemon-stop marker **and** no later pause (of any cause) or pause-related label change made by a human supersedes it.
- **FR-003**: Whenever that determination is in doubt (unreadable timeline or comments, a later human pause or label change, a missing marker, an ambiguous history), the issue MUST be left paused and untouched.
- **FR-004**: A human who pauses an issue after the clean stop MUST keep it paused; the engine MUST NOT lift a pause that a human set or superseded.
- **FR-005**: A human comment MUST NOT be required to resume a clean-stop pause, and a human comment alone MUST NOT prevent the resume.
- **FR-006**: For each issue resumed, the engine MUST write one audit line to the log and post one short comment beginning `🏭 **Fabrik —` stating that the issue was resumed after the restart.
- **FR-007**: A single pause MUST be resumed at most once; a later clean stop produces a new pause that is evaluated independently.
- **FR-008**: The automatic resume MUST be controllable by a setting available as a command-line flag, an environment variable and a YAML configuration key, with the default being **on**; when off, behaviour is identical to today.
- **FR-009**: The resume MUST NOT apply to pauses without the daemon-stop marker (crash recovery and every other pause cause are unchanged), and SIGHUP behaviour MUST be unchanged.
- **FR-010**: The resume MUST NOT apply to closed issues or issues no longer on the board.
- **FR-011**: A failure resuming one issue MUST NOT prevent the remaining eligible issues from being resumed, and MUST NOT leave an issue half-resumed.
- **FR-012**: The engine's own resume comment MUST NOT be treated as human steering input (it must not itself trigger comment processing or count as a human action that supersedes a later pause decision).

### Key Entities *(if applicable)*

- **Daemon-stop pause**: A pause applied by the clean-stop procedure, identified durably by the daemon-stop marker in the engine's pause comment (and mirrored by the daemon-stop pause-cause label, which is informational only).
- **Superseding human action**: A pause or pause-related label change made by a human after the engine's pause comment, which makes the pause the human's rather than the daemon's.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a clean stop and restart with no human intervention, 100% of issues that were interrupted by the stop and are eligible are running again within one normal polling cycle of startup, with no human comment needed.
- **SC-002**: After a clean stop, a human pause applied before restart is preserved in 100% of cases (zero issues resumed against a human's pause).
- **SC-003**: Every automatically resumed issue shows exactly one resume comment and one log line; no issue is resumed twice for the same pause.
- **SC-004**: With the opt-out set, zero issues are resumed automatically, and behaviour matches the pre-change daemon.
- **SC-005**: Time lost to clean-stop pauses (the 39 h across 17 issues in the motivating report) drops to roughly the length of the restart itself.

## Assumptions

- "On startup" means once the daemon has loaded the board after starting; the resume may be performed as part of the first pass(es) over the board rather than strictly before any polling.
- The stored daemon-stop marker in the engine's pause comment is the authoritative signal; the pause-cause label is informational and is not relied on for the decision.
- Only a human action after the engine's pause comment supersedes it; engine actions and bot activity do not.
- Resuming does not reset retry or cycle counters beyond what a normal continuation already does.
- The daemon only acts on items on the board it manages; it does not resume pauses on other boards or instances.
- The opt-out follows the existing configuration precedence (flag over environment over YAML).

## Out of Scope *(optional)*

- Crash recovery: pauses without the daemon-stop marker stay as today.
- SIGHUP restart behaviour (already drains instead of pausing).
- Changing how or when a clean stop pauses issues.
- Resuming pauses of any other cause (questions, timeouts, breakers, cycle limits, failures).
- Narrowing or changing the meaning of the awaiting-input label.

## Source References *(optional)*

- Community report #2453 and its flow analysis: https://github.com/handarbeit/fabrik/issues/2453#issuecomment-6425657874 (data: https://gist.github.com/totalslacker/d02ca162058fcde716ba9fd45beef4ec)
- ADR-1393 (clean-stop pause), ADR 2259 (two-phase SIGHUP restart), ADR 2300 (session resume decision), ADR 2487 (pause cause label)
- Delivery notes for the PR: mention #2453 by bare number only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it. Acceptance includes a sim twin (stop with an in-flight worker → restart → resumes without a comment; with a human pause in between → stays paused), a test seam whose neutralisation makes the first test fail, an update to `docs/state-machine.md` (pause/resume section), and a regenerated `docs/llms-full.txt`.
