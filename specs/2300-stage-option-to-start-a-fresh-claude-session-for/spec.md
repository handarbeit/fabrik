# Feature Specification: Stage option to start a fresh Claude session for comment review and stage re-entry when the saved session is large

**Feature Branch**: `fabrik/issue-2300`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "Report #1909: every comment-review run and most stage re-entries resume the stage's saved Claude session (`.fabrik/sessions/<repo>/issue-N/<Stage>.session`). An operator cannot ask for a fresh one except by deleting the session file. On long stages, the resumed context is most of the cost. In one downstream repo, Implement comment-review runs started at a median of about 450k tokens of context (up to 858k), against about 72k for a fresh Implement run. Comment-triggered runs were 40% of a week's spend. Deleting the session file first produced a comparable result for about a tenth of the cost." (Full original issue text — Problem, Requirements R1–R7, Scope, Acceptance — is restated below in structured form.)

## Background

Report #1909: comment-review runs and stage re-entries resume the stage's saved Claude session. The only way for an operator to get a fresh session is to delete the session file by hand. On long stages the resumed context dominates cost: in one downstream repo, Implement comment-review runs started at a median of about 450k tokens of context (up to 858k), against about 72k for a fresh Implement run. Comment-triggered runs were 40% of a week's spend, and deleting the session file first gave a comparable result for about a tenth of the cost.

Observed behaviour in v0.0.85:

- **Comment review always resumes.** The comment-review invocation resumes the saved session unless the caller sets `NoResume`. `NoResume` is set only at the two merge-train conflict-resolution sites. Every other caller resumes whenever the session file exists: ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke` and `rebase-reinvoke`.
- **The resume check is file existence only.** Nothing measures or bounds the session's size.
- **Stage re-entry resumes by accident of in-memory state.** The stage dispatch resumes whenever the item's in-memory `LastAttemptAt` for that stage is non-zero. That value is never cleared when an item leaves a column and comes back, so a card moved back into Implement resumes the old Implement session (the report observed this). The same re-entry after a daemon restart starts fresh, because the in-memory store is empty. A few paths clear `LastAttemptAt` and so make the next run fresh as a side effect: `clearFailedStage`, `reenterValidate`, `handleValidateSHAInvalidation` and `unblockAwaitingInput`. Whether a re-entry resumes is therefore not a decision anyone made, and it varies with uptime.
- **The session's size is not recorded.** The engine decodes cumulative model usage and the top-level input/output counts only. Nothing keeps the context size of the session's last API call, which is what a resume starts from. The stream-json the engine already tees to the item's log directory does carry per-call `usage` on each assistant message.
- **A fresh re-entry would get less context than a fresh first run needs.** Prior stages' outputs are written for a stage invocation (stages strictly earlier in order); all stages including the current one are written only for comment processing. A fresh re-entry of Implement would therefore not see Implement's own previous output.

**Related regression, fixed separately.** In v0.0.85 a stage continued after a time slice (turn cap or `max_wall_time`) started fresh by mistake, because the backoff-exempt path zeroes `lastAttempt` and the resume decision reuses that variable. #2292 fixed that and restored resume for slice continuations. This issue builds on #2292 and must not undo it.

Report #1909 is mentioned for context only; the PR for this issue must not use a closing keyword for it.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Cap the cost of comment review on a large session (Priority: P1)

An operator running long Implement stages sets the new option to `auto` on that stage. When a reviewer comment arrives and the saved Implement session has grown past the threshold, comment review starts a fresh session instead of resuming a context of hundreds of thousands of tokens.

**Why this priority**: This is the cost problem reported in #1909 (comment-triggered runs were 40% of a week's spend).

**Independent Test**: Configure a stage with `auto` and a small threshold, complete the stage with a large recorded session size, post a comment, and observe the comment-review invocation runs without `--resume`.

**Acceptance Scenarios**:

1. **Given** a stage with the option set to `auto`, a threshold of 250k and a saved session recorded at 400k tokens, **When** a comment triggers comment review, **Then** the invocation starts a fresh session and logs `fresh session (saved session 400k > threshold 250k)`.
2. **Given** the same stage and a saved session recorded at 100k tokens, **When** a comment triggers comment review, **Then** the invocation resumes the saved session and logs `resuming session <id> (100k tokens, threshold 250k)`.
3. **Given** the same stage and a saved session with no recorded size, **When** a comment triggers comment review, **Then** the invocation resumes and logs `resuming session <id> (size unknown)`.

---

### User Story 2 - Operator can force fresh or force resume per stage (Priority: P1)

An operator who never wants a stage's old conversation carried into comment review or re-entry sets the option to `fresh`. An operator who wants today's behaviour leaves it unset (or `resume`).

**Why this priority**: The report's core complaint is that there is no operator control at all other than deleting a file.

**Independent Test**: Run the same comment-review scenario under each of `resume`, `fresh` and `auto` (unset ≡ `resume`) and assert the presence or absence of `--resume`.

**Acceptance Scenarios**:

1. **Given** a stage with the option set to `fresh`, **When** a comment-review-family invocation runs, **Then** it starts a fresh session and logs `fresh session (resume_session: fresh)` (using the option's final name).
2. **Given** a stage with no option set, **When** a comment-review-family invocation runs against an existing session, **Then** it resumes exactly as it does today.
3. **Given** a stage YAML with an unknown option value, **When** the engine loads stages, **Then** startup fails like other invalid stage keys.

---

### User Story 3 - Stage re-entry behaves the same regardless of daemon uptime (Priority: P2)

When a card is moved back into a stage that has already run (or the stage re-enters after completing), whether it resumes the old session is decided by the option, not by whether the daemon happens to have restarted since.

**Why this priority**: Today the same action gives different results depending on in-memory state, so cost and output are unpredictable.

**Independent Test**: Re-enter a completed stage under each option value, with and without a simulated restart (empty in-memory store), and assert the same resume decision.

**Acceptance Scenarios**:

1. **Given** a stage with the option `fresh` and an item moved back into that column, **When** the stage dispatches, **Then** it starts fresh both before and after a daemon restart.
2. **Given** the default (`resume`) and an item moved back into that column, **When** the stage dispatches, **Then** it resumes both before and after a daemon restart.

---

### User Story 4 - Continuations always resume (Priority: P1)

After a turn-capped or wall-time-capped run, or after an incomplete or errored run of the same stage, the next run continues the same work. It must resume the session whatever the option says, so a time-sliced stage does not lose its progress.

**Why this priority**: Starting fresh mid-task loses work and would reintroduce the regression #2292 fixed.

**Independent Test**: Under `fresh`, run a stage that stops at the turn cap, then let the engine continue it, and assert the continuation resumes and logs `resuming session <id> (continuation)`.

**Acceptance Scenarios**:

1. **Given** a stage with the option `fresh` whose run ended at the turn cap, **When** the engine runs the continuation, **Then** it resumes the saved session and logs `resuming session <id> (continuation)`.
2. **Given** the same situation with a daemon restart between the slice and the continuation, **Then** the decision is unchanged.

---

### User Story 5 - A fresh re-entry still sees the stage's own earlier output (Priority: P2)

When a stage re-entry starts fresh, it is given what a fresh first run gets, plus the stage's own most recent output, so it does not redo or contradict its earlier work.

**Why this priority**: Without this, `fresh` re-entry of Implement would not know what the previous Implement run produced.

**Independent Test**: Start a fresh re-entry of a stage and assert that `stage-<Current>.md` is among the context files; start a fresh comment-review and assert the new comments are in the prompt.

**Acceptance Scenarios**:

1. **Given** a fresh stage re-entry, **When** context files are written, **Then** `stage-<Current>.md` (the stage's own most recent output) is present in addition to prior stages' outputs.
2. **Given** a fresh comment-review run, **When** the prompt is built, **Then** it carries the new comments and the context files include the current stage's output.

---

### Edge Cases

- Size record missing, unreadable or from before this change: `auto` resumes and logs that the size was unknown.
- Session file replaced (new session ID): the size record is replaced with it; a stale size never applies to a new session.
- Existing resume-failure handling (dead-session self-heal, `.session.resumefails` abandonment) still applies when a resume is chosen; it is unchanged.
- A session that is abandoned or deleted by that handling carries no usable size record.
- Merge-train conflict-resolution invocations keep `NoResume: true` and are not governed by the option.
- The four paths that clear `LastAttemptAt` today (`clearFailedStage`, `reenterValidate`, `handleValidateSHAInvalidation`, `unblockAwaitingInput`) must have a defined, documented outcome under the option (see FR-013).
- Daemon restart between a time slice and its continuation, or between completion and a re-entry: the decision is the same as without the restart.
- The last assistant message of a run is not the largest: the recorded size is the last call's, not the maximum or the sum.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A stage YAML option (final name settled in Plan; `resume_session` and `comment_session` are candidates) accepts exactly `resume`, `fresh` and `auto`. When absent it behaves as `resume`. An unknown value fails stage loading at startup like other invalid stage keys.
- **FR-002**: The option governs every comment-review-family invocation: ordinary comment review, `review-reinvoke`, `ci-fix-reinvoke` and `rebase-reinvoke`.
- **FR-003**: The option governs every stage dispatch that is a **re-entry**: the stage starting again after it completed, or after the item left its column and came back.
- **FR-004**: The option does not govern the merge-train conflict-resolution sites, which keep `NoResume: true`.
- **FR-005**: **Continuations always resume, whatever the option says.** A continuation is the next run after a time slice (turn cap or `max_wall_time`), or the retry after an incomplete or errored run of the same stage. This preserves the #2292 behaviour. The existing resume-failure handling is unchanged.
- **FR-006**: The signal that separates a continuation from a re-entry is stated explicitly in the as-built docs, and does not depend on the in-memory `LastAttemptAt` alone: the same item gets the same decision before and after a daemon restart. How the signal behaves after a restart is documented.
- **FR-007**: `auto` resumes only when the saved session's measured context size is at or below a threshold, and starts fresh above it. The threshold is configurable per stage and has a documented default (250k tokens is the reporter's suggestion; the Plan stage may choose another value and must state the reason).
- **FR-008**: The measured size is the context of the session's **last** API call — input tokens plus cache-read and cache-creation tokens, as reported on the stream-json assistant message. It is not the cumulative total of the run, and not the sum or maximum across calls.
- **FR-009**: The size is recorded per session file, survives a daemon restart (the `.session.resumefails` sidecar is the precedent for persistence next to the session file), and is replaced every time the session file is replaced.
- **FR-010**: When the size is unknown (no record, unreadable record, or a session that predates this change), `auto` resumes and the log line says the size was unknown.
- **FR-011**: A fresh session started under this option receives what a fresh run of that invocation type receives, plus the stage's own most recent output (`stage-<Current>.md`) for a stage re-entry. The comment-review prompt keeps carrying the new comments. No new file for the PR diff is added unless Plan finds a gap (the worktree already holds the PR's commits).
- **FR-012**: Every governed invocation logs its decision once, with the reason, in one of these forms: `resuming session <id> (<N>k tokens, threshold <T>k)`, `fresh session (saved session <N>k > threshold <T>k)`, `fresh session (resume_session: fresh)` (with the option's final name), `resuming session <id> (size unknown)`, and, for a continuation, `resuming session <id> (continuation)`.
- **FR-013**: The re-entry inconsistency is resolved under the default: a re-entry resumes (or not) per the option regardless of uptime. For each of `clearFailedStage`, `reenterValidate`, `handleValidateSHAInvalidation` and `unblockAwaitingInput`, the Plan stage decides whether its current fresh-session effect is kept on purpose (documented as such) or is brought under the option, and records why.
- **FR-014**: **Default is `resume`.** Behaviour is unchanged for every existing stage config. The embedded example stage YAMLs and `docs/USER_GUIDE.md` describe `auto` and recommend it for long stages (Implement especially).
- **FR-015**: The option and threshold are documented in the as-built docs: `docs/stage-lifecycle.md` "Session Resume" (decision rules, the size record, the log lines); `docs/USER_GUIDE.md` stage configuration (the new key and the threshold key, with the `auto` recommendation); `docs/state-machine.md` §2.17 (resume-failure abandonment and how it interacts with the new decision); and the stage-option list in `CLAUDE.md`. `docs/llms-full.txt` is regenerated in the same commit (`bash scripts/generate-llms-full.sh`).

### Key Entities *(if applicable)*

- **Saved session**: the per-item, per-stage Claude session file (`.fabrik/sessions/<repo>/issue-N/<Stage>.session`) that a later invocation can resume.
- **Session size record**: the context size, in tokens, of the saved session's last API call, persisted next to the session file and replaced with it.
- **Resume policy**: the stage option (`resume` / `fresh` / `auto`) plus the `auto` threshold.
- **Invocation class**: continuation (always resumes), re-entry, or comment-review-family run — the class decides whether the policy applies.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `auto` and a threshold below the recorded size, a comment-review run starts with no resumed context; with the option unset, the same scenario resumes exactly as before this change.
- **SC-002**: For a stage whose saved session exceeds the threshold, the starting context of a comment-review run is that of a fresh run of the stage, not that of the saved session (the reporter's reference points: ~450k median resumed vs ~72k fresh).
- **SC-003**: The decision for the same item and the same persisted state is identical before and after a daemon restart, for both re-entries and continuations.
- **SC-004**: A time-sliced stage's continuation resumes under all three option values.
- **SC-005**: Every governed invocation produces exactly one decision log line from the documented set (FR-012), so an operator can measure how often each outcome occurs.
- **SC-006**: Stage YAMLs that do not mention the option load and behave byte-for-byte as before.
- **SC-007**: Test evidence of non-vacuity: forcing the decision to always resume makes the `auto`/`fresh` tests fail, and removing the continuation exemption makes the continuation test fail.

## Assumptions

- The option's final name and the threshold key's name are settled in Plan; the values `resume`/`fresh`/`auto` are fixed here.
- The threshold default is 250k tokens (reporter's suggestion) unless Plan chooses otherwise with a recorded reason.
- Default stays `resume`: the data come from one downstream repository, a changed default would alter cost and output for every user on upgrade, and comment review relies on the resumed conversation to answer questions about its own earlier work. Changing the default is a later decision informed by the decision log lines.
- The per-call context size is available from the stream-json that the engine already tees to the item's log directory (per-call `usage` on each assistant message).
- #2292 is merged and its continuation-resume behaviour is the baseline this issue builds on.
- Tests follow the repository's standing layers: unit tests, an `InvokeClaude`/`InvokeClaudeForComments` argument check for `--resume`, and a sim twin in `tests/sim`.

## Out of Scope *(optional)*

- Automatic compaction or summarisation of a session.
- Changing the time-slice continuation behaviour (owned by #2292).
- Reducing unnecessary re-dispatches (#1743).
- The merge-train `NoResume` sites.
- Any change of default (FR-014).

## Source References *(optional)*

- Report #1909 (mention by bare number only; no closing keyword in the PR).
- #2292 — time-sliced stage resumes its Claude session (prerequisite, merged).
- #1743 — reducing unnecessary re-dispatches (out of scope).
- `docs/stage-lifecycle.md` "Session Resume"; `docs/state-machine.md` §2.17.
