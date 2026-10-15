# Feature Specification: Pruefer keeps the review transcript when coverage is partial

**Feature Branch**: `fabrik/issue-2303`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

When the coverage check from #2190 reports a review as partial, the only evidence behind that verdict is the stream-json transcript, and Pruefer throws it away. `deriveOpened` reads the transcript from `stdout` in `pruefer/claude.go`, and the buffer is then dropped along with the review's temp directory. So a coverage result that disagrees with the reviewer's own account can't be investigated afterwards.

**Observed 2026-10-15 on PR #2191** (head `56d9361e`):

- **Reviewer's text:** \"I read all 10 staged entries and checked the surrounding code.\"
- **Coverage field:** `PRUEFER_COVERAGE: partial — 10 of 10 diff entries not opened`. The log line was `transcript authoritative=true`.
- **Run time:** 18 seconds, 00:53:59 to 00:54:17.
- **Same daemon, same moment, PR #2189:** the review reported `coverage: complete (5 entries)`.

The resolved paths are not the cause. `invokeReview` passes the same symlink-resolved staged dir to the prompt, the tool rules and `deriveOpened`. That leaves two explanations, and the transcript is the only thing that tells them apart:

1. **The check undercounts.** The reviewer opened the entries in a way `deriveOpened` doesn't credit: Grep, `sed`/`head`, a Read with offset/limit or a truncated result, or a Read of the clone's file instead of the staged entry.
2. **The reviewer's text is false.** It didn't read the entries, and the coverage line is correctly the only honest signal.

## Requirements

- **R1:** When a review's coverage is partial, Pruefer keeps that invocation's raw stream-json transcript. It writes it under `.pruefer/transcripts/` with a name containing the owner, repo, PR number, head SHA and a timestamp. The file is written before the temp directory is removed.
- **R2:** The `coverage: partial` warn line names the saved file's path.
- **R3:** Retention is bounded by a fixed count, defaulting to the newest 50 files. Older files are deleted when a new one is written. A failure to write or delete a file is logged and never fails the review.
- **R4:** Complete reviews keep no transcript by default. An optional config key (e.g. `keep_transcripts: partial|all|off`, default `partial`) may widen or disable this.
- **R5:** The saved transcript contains only what the review invocation itself saw. Nothing from the daemon's environment or config is added.

## Out of scope

- Changing how `deriveOpened` credits reads. If the saved transcripts show a check gap, that gets its own issue with the transcript as evidence.
- Posting transcripts anywhere outside the daemon's own `.pruefer/` directory.

## Acceptance

- A test review whose coverage comes back partial leaves exactly one transcript file under `.pruefer/transcripts/`, and the warn line names it.
- A complete review leaves no transcript under the default setting.
- With the cap at N, writing transcript N+1 deletes the oldest.
- A write failure, simulated with an unwritable directory, still submits the review."

## Background

Pruefer's coverage check (#2190) compares the staged diff entries a review was supposed to read against what the invocation's stream-json transcript proves it opened. When the two disagree it marks the review `partial` and says so in the review body and the log. The transcript is the sole evidence for that verdict, but it exists only as an in-memory buffer inside the Claude invocation. It is parsed by `deriveOpened` and then discarded with the review's temp directory.

On PR #2191 (head `56d9361e`) the reviewer's prose claimed it had read all 10 staged entries, while the coverage field reported `partial — 10 of 10 diff entries not opened` with an authoritative transcript. In the same 18-second window the daemon reviewed PR #2189 and reported `complete (5 entries)`. The path handling is shared between the prompt, tool rules and check, so it is not the cause. Only two explanations remain: the check undercounts some way of reading an entry, or the reviewer's text is false. Without the transcript neither can be confirmed, so each partial verdict is unreproducible. This feature preserves the evidence when it matters, with bounded disk use, so a disagreement can be settled afterwards.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Investigate a partial-coverage verdict (Priority: P1)

An operator sees `coverage: partial` on a review, or in the Pruefer log, and wants to know whether the check undercounted or the reviewer skipped the entries. The warn line gives the path of the saved transcript, and they open the raw stream-json to see exactly which tool calls the reviewer made.

**Why this priority**: This is the whole point of the issue. Without it, partial verdicts cannot be diagnosed and any decision about the check's accuracy is guesswork.

**Independent Test**: Run a review through a fake invoker whose transcript yields partial coverage. Verify exactly one file appears under `.pruefer/transcripts/`, that its name carries owner, repo, PR number, head SHA and a timestamp, that its content equals the invocation's raw transcript, and that the warn line names its path.

**Acceptance Scenarios**:

1. **Given** a review whose coverage is computed as partial, **When** the review finishes, **Then** exactly one transcript file exists under `.pruefer/transcripts/` and the `coverage: partial` warn line contains that file's path.
2. **Given** the saved file, **When** an operator reads it, **Then** it is the invocation's raw stream-json output, byte for byte, with nothing added from the daemon's environment or configuration.
3. **Given** a review that ends partial and whose temp directory is removed immediately afterwards, **When** the operator looks for the transcript, **Then** it is still present, because it was written before the removal.

---

### User Story 2 - Disk use stays bounded (Priority: P1)

An operator runs the daemon unattended for weeks. Transcripts must not accumulate without limit: the newest 50 are kept and older ones are deleted as new ones are written.

**Why this priority**: Unbounded retention on a long-running daemon would turn a diagnostic aid into a disk-fill hazard.

**Independent Test**: With the cap set to N, write N+1 transcripts for distinct reviews and verify that the oldest is gone and the newest N remain.

**Acceptance Scenarios**:

1. **Given** the transcripts directory already holds N files (the cap), **When** another partial review writes a new transcript, **Then** the oldest file is deleted and N files remain.
2. **Given** the default configuration, **When** no cap is set, **Then** the cap is 50.
3. **Given** deletion of an old file fails, **When** a new transcript is written, **Then** the failure is logged and the review still completes.

---

### User Story 3 - Retention never harms the review (Priority: P1)

Saving a transcript is a diagnostic side effect. A full disk, an unwritable directory or a failed cleanup must not stop a review from being submitted.

**Why this priority**: A diagnostic feature that can suppress or fail real reviews would be a regression on the daemon's core job.

**Independent Test**: Point the transcripts location at an unwritable directory, run a partial review and verify the review is submitted and a failure is logged.

**Acceptance Scenarios**:

1. **Given** the transcripts directory cannot be created or written, **When** a partial review finishes, **Then** the review is still submitted and a warning is logged.
2. **Given** a write failure, **When** the warn line is emitted, **Then** it does not claim a saved path that does not exist.

---

### User Story 4 - Complete reviews cost nothing by default; operators can widen or disable (Priority: P2)

By default only partial reviews keep a transcript. An operator investigating a suspected undercount in the other direction can opt in to keeping every transcript, or turn retention off entirely.

**Why this priority**: The default covers the diagnostic need. The knob is a convenience and the issue marks it optional.

**Independent Test**: Run a complete review under the default and verify no file is written. Then set the option to keep all and verify a file is written; set it to off and verify a partial review writes none.

**Acceptance Scenarios**:

1. **Given** the default setting, **When** a review's coverage is complete, **Then** no transcript is kept.
2. **Given** the setting is `all`, **When** a complete review finishes, **Then** its transcript is kept under the same naming and retention rules.
3. **Given** the setting is `off`, **When** a partial review finishes, **Then** no transcript is kept and the warn line names no path.
4. **Given** an unrecognised value, **When** the config is loaded, **Then** it is handled like other invalid Pruefer config values (rejected or defaulted with a logged warning).

---

### Edge Cases

- Two reviews of the same PR and head SHA (a forced re-review) must not overwrite each other; the timestamp in the name keeps files distinct.
- The invocation fails before producing a transcript (error, timeout, empty output): there is nothing to keep, so none is written and the existing failure handling is unchanged.
- The transcript is large: it is saved as-is, without truncation, since a truncated transcript would defeat the diagnosis. Count-based retention is the only bound.
- Owner, repo or head SHA values containing characters unsafe in filenames must not allow a path outside `.pruefer/transcripts/`.
- Transcripts reflect the diff under review, which may be from a private repository. They stay on the daemon's host and are restricted to its user.
- A coverage check with no transcript available (an invoker that cannot observe one) has nothing to save.
- The transcripts directory contains unrelated files: cleanup must delete only files Pruefer created there.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When a review's computed coverage is partial, Pruefer MUST save that invocation's raw stream-json transcript to a file under `.pruefer/transcripts/` before the review's temporary working directory is removed.
- **FR-002**: The file name MUST contain the repository owner, repository name, PR number, head SHA and a timestamp, and MUST be safe to use as a single path component whatever those values contain.
- **FR-003**: The `coverage: partial` warn line MUST include the path of the saved file when a file was saved.
- **FR-004**: The saved content MUST be exactly what the review invocation produced on its output stream; nothing from the daemon's environment or configuration may be added.
- **FR-005**: Retention MUST be bounded by a fixed count, defaulting to the newest 50 files. Writing a transcript beyond the cap MUST delete the oldest files so that the cap is respected.
- **FR-006**: Any failure to create the directory, write a transcript or delete an old one MUST be logged and MUST NOT fail, skip or alter the review's submission.
- **FR-007**: A complete review MUST NOT keep a transcript under the default setting.
- **FR-008**: An optional config key (`keep_transcripts`, with values `partial`, `all` and `off`, default `partial`) MUST control which reviews keep a transcript, following Pruefer's usual flag / environment / config-file resolution and invalid-value handling.
- **FR-009**: Retention cleanup MUST affect only transcript files that Pruefer wrote to `.pruefer/transcripts/`.
- **FR-010**: Saved transcript files MUST be readable only by the daemon's own user.
- **FR-011**: The existing coverage computation (`deriveOpened` and the credit rules) MUST remain unchanged by this feature.

### Key Entities

- **Review transcript**: The raw stream-json output of one review invocation, stored as a file whose name identifies the owner, repo, PR, head SHA and capture time.
- **Transcript retention policy**: The mode (`partial` / `all` / `off`) and count cap that decide which transcripts are kept and how many are retained.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every review that finishes with partial coverage under default settings, exactly one transcript file exists under `.pruefer/transcripts/` and the warn line names its path.
- **SC-002**: A complete review leaves no transcript file under default settings.
- **SC-003**: After any number of partial reviews, the transcripts directory never holds more than the configured cap (50 by default).
- **SC-004**: With the transcripts directory unwritable, 100% of otherwise-successful reviews are still submitted.
- **SC-005**: An operator can decide between "the check undercounts" and "the reviewer's text is false" for a given partial verdict from the saved file alone, without re-running the review.

## Assumptions

- "Partial" means the same verdict the coverage check already produces and the existing warn line already reports; this feature adds no new notion of partial.
- The raw transcript is the invocation's complete stdout stream-json and already contains only what the invocation saw, so saving it verbatim satisfies the "nothing added from the daemon's environment" requirement.
- `.pruefer/` is the daemon's own working directory, relative to the same base as its existing state files (config, log, lock). It is not committed to any repository.
- Retention is by count only, ordered by the creation time recorded in the file name or file modification time; no age-based expiry is needed.
- The cap is a fixed default of 50 and is not operator-configurable unless a later issue asks for it; the optional config key controls which reviews keep a transcript, not the count.
- Transcripts may include fragments of private code. Restricting file permissions to the daemon's user and keeping them local is considered adequate; no redaction is attempted.
- The existing docs for Pruefer's configuration and logging will be updated to mention the new directory and key.

## Out of Scope *(optional)*

- Changing how `deriveOpened` credits reads. If saved transcripts reveal a check gap, that gets its own issue with the transcript as evidence.
- Posting or uploading transcripts anywhere outside the daemon's own `.pruefer/` directory.
- Redacting, compressing or truncating saved transcripts.
- Age-based expiry or an operator-configurable count cap.

## Source References *(optional)*

- #2190 — Pruefer diff coverage contract (the check that produces the partial verdict).
- #2191 — the pull request on which the discrepancy was observed (head `56d9361e`); #2189 — the same-daemon comparison with `coverage: complete (5 entries)`.
