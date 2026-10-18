# Feature Specification: Pruefer review sessions leave the Claude profile; transcripts kept 7 days in `.pruefer/transcripts`

**Feature Branch**: `fabrik/issue-2363`
**Created**: 2026-10-17
**Status**: Specified
**Input**: User description: the original issue body, reproduced verbatim below.

<details>
<summary>Original request (verbatim)</summary>

## Problem

Every Pruefer review leaves a session folder under the resolved Claude profile: `<CLAUDE_CONFIG_DIR>/projects/-private-var-folders-…-pruefer-pr-<N>-<rand>/`. Each folder holds the session JSONL plus `tool-results/*.txt`, the full outputs the CLI hides behind a 2 KB `<persisted-output>` preview.

On the maintainer's machine (2026-10-16) there were 1,927 such folders, 788 MB in total, going back to 2026-09-16. Nothing removes them. They contain full reads of reviewed code, private repositories included, and they sit in a profile that is shared with the operator's interactive sessions.

They are useful for debugging; #2362 was diagnosed from transcripts. ADR 2303 already gives Pruefer a place of its own for this evidence, `.pruefer/transcripts/`. Today it keeps transcripts only for partial reviews, capped at 50 by count.

**Constraint from #2375:** the coverage loop resumes the review's own Claude session to read the entries left unread. The session therefore has to persist for as long as the review runs, so `--no-session-persistence` is not an option. Pruefer instead removes the review's own sessions once the review has ended. #2375 does this already when its cap is above 0; this issue completes it.

## Decision

1. Let the CLI persist the session while the review runs.
2. When the review ends, move the evidence worth keeping into `.pruefer/transcripts/` and delete that review's own session folder from the profile.
3. Save a transcript for every review by default.
4. Retire transcripts by age instead of by count.

## Requirements

- **R1. Build on #2375's session removal; do not duplicate it.** #2375 (PR #2376) added `removeReviewSessions`. When a review ends with `max_coverage_passes` above 0, it deletes `<profile>/projects/*/<id>.jsonl` and `<id>/` for every session id the review's CLI reported. Each id is checked against a UUID regex before use. Extend that function rather than adding a second one:
  - **R1a. Run it with every cap.** Also run it when `max_coverage_passes` is 0, so no setting leaves sessions in the profile.
  - **R1b. Remove the folder the review left empty.** After the files are gone, remove the now-empty encoded-cwd directory only if all of these hold:
    - it is empty;
    - it sits directly under `<profile>/projects/`;
    - its name contains `pruefer-pr-<N>-` for this review's PR.

    Today the empty `-private-var-folders-…-pruefer-pr-<N>-…` directories still pile up.
  - **R1c. Keep the CLI's session folder search narrow.** It may keep searching for a session id across `projects/*`, since the CLI's encoding is not Pruefer's. It must never delete anything other than an exact `<id>.jsonl` or `<id>/` for an id the review itself reported.
  - **R1d. Keep sessions persistent.** Pruefer still does not pass `--no-session-persistence`: ADR 2375's live probe showed it blocks resuming.
  - **Failures stay quiet.** Any failure is logged and never fails or changes the review.
- **R2. Keep the hidden outputs before removal.**
  - Before `removeReviewSessions` deletes `<id>/`, move its `tool-results/` files into the transcript directory as `<transcript-name>.tool-results/<file>`, beside the review's JSONL.
  - Do this only when a transcript is kept for this review (R3).
  - Today #2375 deletes them, and those files are the full outputs the CLI hid behind its 2 KB previews.
  - Record in the ADR what the CLI writes under the session folder, and whether Pruefer's own stream-json transcript already holds everything in the profile JSONL.
  - A failure to move the files is logged and never fails the review.
- **R3. Keep every transcript by default.** Change the `keep_transcripts` default from `partial` to `all`. `partial` and `off` keep their meaning. With `off`, the folder is still deleted (R1) and nothing is moved.
- **R4. Retire transcripts by age.** Replace the fixed count (`transcriptRetainCount = 50`) with an age bound, `transcript_max_age`.
  - It is set by the flag `--transcript-max-age`, the env var `PRUEFER_TRANSCRIPT_MAX_AGE`, or YAML in the operator config only. The default is `168h`.
  - Validate it fail-loud like `keep_transcripts`. Refuse zero and negative values.
  - Keep a hard size ceiling as a backstop, `transcript_max_bytes` (default 1 GiB). Above it, the oldest files go first.
  - Both are operator-only. Neither is ever read from the repo-resident config (ADR 1642).
- **R5. Prune after each save and once at startup.**
  - Prune by the timestamp in the file name, not by mtime (ADR 2303).
  - Delete only names that match Pruefer's own transcript and `.tool-results` shapes. Anything else in the directory is left alone.
  - Keep the existing package mutex.
- **R6. Record the decision.** Amend ADR 2303, or add ADR 2363, covering:
  - The privacy consequence changes: every review's file reads are kept for up to 7 days, not just 50 partial reviews. They now live outside the shared Claude profile.
  - Disk use is bounded by age and by `transcript_max_bytes`.
  - Why the session is persisted and then deleted rather than never persisted (#2375).
- **R7. Document it in `cmd/pruefer/README.md`.**
  - Document the new defaults.
  - Tell operators that older versions left `*-pruefer-pr-*` folders under `<profile>/projects/`, and that these can be deleted by hand.

## Relation to #2375

#2375 has landed or is landing first. This issue extends its `removeReviewSessions` and its end-of-review hook; it does not move that hook. A test runs a two-invocation review and shows the session survives between invocations.

## Acceptance

- Unit tests with a fake profile directory:
  - Sessions are removed with `max_coverage_passes: 0` as well.
  - The empty encoded-cwd folder is removed only under the R1b checks.
  - A sibling `*-pruefer-pr-*` folder belonging to another review, and an unrelated folder, are untouched.
  - A non-empty folder, or one not named for this PR, is left alone and logged.
  - `tool-results` files are moved beside the transcript under `all`, and are not moved under `off`.
  - `--no-session-persistence` is absent at every cap.
  - The default is `all`.
  - Pruning by age, with an injected clock and named files.
  - The byte ceiling.
  - Foreign files in the transcript directory survive a prune.
  - Invalid `transcript_max_age` and `transcript_max_bytes` values are refused.
- After merge, a live review on the dev daemon:
  - leaves no new `*-pruefer-pr-*` folder under the profile;
  - writes its transcript, and any `tool-results`, to `.pruefer/transcripts/`;
  - a transcript older than 7 days is purged at the next save.

## Out of scope

- Automatic deletion of folders left by earlier versions. They are removed by hand. Pruefer never sweeps a Claude profile.
- #2362 (the reviewer's reading behaviour) and #2375 (the coverage loop itself).

</details>

## Background

Every Pruefer review leaves a session folder under the resolved Claude profile (`<CLAUDE_CONFIG_DIR>/projects/<encoded-cwd>-pruefer-pr-<N>-<rand>/`). Each folder holds the session JSONL plus `tool-results/*.txt`, the full outputs the Claude CLI hides behind a 2 KB `<persisted-output>` preview. Nothing removes them. On the maintainer's machine (2026-10-16) there were 1,927 such folders, 788 MB in total, going back a month. They hold full reads of reviewed code, private repositories included, and they sit in a profile shared with the operator's interactive sessions.

The evidence is useful for debugging (#2362 was diagnosed from transcripts), and ADR 2303 already gives Pruefer a private place for it, `.pruefer/transcripts/`. Today that directory keeps transcripts only for partial reviews, capped at 50 by count.

The coverage resume loop (#2375, PR #2376) resumes a review's own Claude session, so the session must persist while the review runs. Passing `--no-session-persistence` is therefore not an option, because ADR 2375's live probe showed it blocks resuming. #2375 already deletes the review's sessions once the review ends, but only when `max_coverage_passes` is above 0, and it deletes the hidden `tool-results` files and leaves the empty encoded-cwd directory behind. This issue completes that work.

Decision: the CLI persists the session while the review runs; when the review ends, the evidence worth keeping is moved into `.pruefer/transcripts/` and the review's own session folder is deleted from the profile; a transcript is saved for every review by default; transcripts are retired by age instead of by count.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - No review leaves anything in the shared Claude profile (Priority: P1)

An operator running Pruefer, with any `max_coverage_passes` setting, finds that no review session data remains under the Claude profile once a review has ended.

**Why this priority**: This is the privacy and disk-growth problem the issue exists to fix, and it is independent of the transcript changes.

**Independent Test**: Run a review against a fake profile directory with `max_coverage_passes` set to 0, to a positive value, and to the default. Afterwards the profile holds no `<id>.jsonl`, no `<id>/` and no empty `*-pruefer-pr-<N>-*` directory for that review.

**Acceptance Scenarios**:

1. **Given** a finished review with `max_coverage_passes: 0`, **When** the review ends, **Then** the session files and folders for every session id the review's CLI reported are deleted from the profile.
2. **Given** a review whose session left an encoded-cwd directory that is now empty, directly under `<profile>/projects/`, with `pruefer-pr-<N>-` for this PR in its name, **When** cleanup runs, **Then** that directory is removed.
3. **Given** a sibling `*-pruefer-pr-*` directory belonging to another PR or another running review, or an unrelated directory, **When** cleanup runs, **Then** it is untouched.
4. **Given** a candidate directory that is non-empty, or whose name does not carry this PR's `pruefer-pr-<N>-`, **When** cleanup runs, **Then** it is left alone and the skip is logged.
5. **Given** a two-invocation review (coverage loop), **When** the first invocation finishes, **Then** the session is still present for the resume, and it is removed only when the review as a whole has ended.

---

### User Story 2 - Every review's evidence is kept privately for a week (Priority: P1)

A maintainer debugging a review can open its transcript, together with the full tool outputs the CLI hid, from `.pruefer/transcripts/`, without anything depending on the Claude profile.

**Why this priority**: Without it, deleting the sessions (Story 1) would throw away the debugging evidence the issue wants to keep.

**Independent Test**: With `keep_transcripts` unset, run a review whose session has `tool-results` files. The transcript and a `<transcript-name>.tool-results/` directory containing those files appear in `.pruefer/transcripts/`, and the profile copy is gone.

**Acceptance Scenarios**:

1. **Given** the default configuration, **When** a review ends, **Then** a transcript is saved whether or not coverage was partial.
2. **Given** `keep_transcripts: all` (or the default) and a session folder with `tool-results/*` files, **When** the review ends, **Then** those files are moved to `<transcript-name>.tool-results/<file>` beside the transcript before the session folder is deleted.
3. **Given** `keep_transcripts: off`, **When** the review ends, **Then** the session folder is still deleted and nothing is moved into `.pruefer/transcripts/`.
4. **Given** `keep_transcripts: partial`, **When** a complete review ends, **Then** no transcript is kept and no `tool-results` are moved; when a partial review ends, the transcript and `tool-results` are kept.
5. **Given** moving the `tool-results` files fails, **When** the review ends, **Then** the failure is logged and the review's posted result and outcome are unchanged.

---

### User Story 3 - Transcripts are retired by age, with a size backstop (Priority: P2)

An operator controls how long transcripts live and how much disk they may use, with safe defaults and loud refusal of nonsense values.

**Why this priority**: Keeping every transcript is only acceptable if disk use is bounded; but the cleanup of the profile (Stories 1–2) delivers value without it.

**Independent Test**: With an injected clock and named transcript files of different ages and sizes in a temp directory, run a prune and check which files survive.

**Acceptance Scenarios**:

1. **Given** transcripts and `.tool-results` directories whose file-name timestamps are older than `transcript_max_age` (default 168h), **When** a prune runs after a save, **Then** they are deleted, judged by the timestamp in the name and not by mtime.
2. **Given** the total size exceeds `transcript_max_bytes` (default 1 GiB), **When** a prune runs, **Then** the oldest files are deleted first until the total is within the ceiling.
3. **Given** files in the transcript directory that do not match Pruefer's own transcript and `.tool-results` name shapes, **When** a prune runs, **Then** they are left alone.
4. **Given** the daemon starts, **When** startup completes, **Then** one prune has run.
5. **Given** `transcript_max_age` or `transcript_max_bytes` is zero, negative or unparseable, **When** configuration loads, **Then** startup is refused with an error naming the setting, as `keep_transcripts` already is.
6. **Given** a reviewed repository's own `.pruefer/config.yaml` sets either value, **When** configuration loads, **Then** it has no effect.

---

### User Story 4 - Operators learn the new behaviour and how to clean up old folders (Priority: P3)

An operator reading the docs sees the new defaults and knows that folders left by earlier versions can be removed by hand.

**Why this priority**: Required for the change to be adoptable, but it carries no behaviour.

**Independent Test**: Read `cmd/pruefer/README.md` and the ADR.

**Acceptance Scenarios**:

1. **Given** the README, **When** an operator looks up transcripts, **Then** it states the defaults (`keep_transcripts: all`, 168h, 1 GiB) and the three ways to set the age.
2. **Given** an operator upgrading from an older version, **When** they read the README, **Then** it tells them old `*-pruefer-pr-*` folders remain under `<profile>/projects/` and may be deleted by hand.

---

### Edge Cases

- A session id the review reported is not a valid UUID: it is not used for any path, as today.
- The CLI's encoded-cwd directory differs from Pruefer's own encoding: the session id may still be searched across `projects/*`, but only an exact `<id>.jsonl` or `<id>/` is ever deleted.
- The session folder has no `tool-results/` directory: nothing is moved, nothing fails.
- The transcript is not kept (`off`, or `partial` on a complete review): `tool-results` are deleted with the folder, not moved.
- Two reviews run concurrently: pruning and the empty-directory removal never touch another review's files or directory; the existing package mutex keeps saves and prunes serialised.
- Transcript save or session cleanup fails (permissions, missing directory): logged only; the review still posts and its result is unchanged.
- A foreign file or directory sits in `.pruefer/transcripts/`: never deleted, never counted as one of Pruefer's own for age, and (assumption below) not counted towards the byte ceiling.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The session cleanup introduced by #2375 (`removeReviewSessions`) is extended, not duplicated; it runs at the end of every review regardless of the `max_coverage_passes` value, including 0.
- **FR-002**: After a review's session files are deleted, the encoded-cwd directory is removed only if it is empty, sits directly under `<profile>/projects/`, and its name contains `pruefer-pr-<N>-` for this review's PR number; otherwise it is left alone and the reason is logged.
- **FR-003**: The cleanup may search for a session id across all `projects/*` directories, but deletes nothing other than an exact `<id>.jsonl` file or `<id>/` directory for a (UUID-validated) session id that this review's CLI reported.
- **FR-004**: Pruefer never passes `--no-session-persistence`, at any `max_coverage_passes` value, and the session survives between the invocations of a multi-invocation review.
- **FR-005**: Any failure in session cleanup, empty-directory removal or `tool-results` relocation is logged and never fails or alters the review or its posted result.
- **FR-006**: When a transcript is kept for the review, the files in the session folder's `tool-results/` are moved, before the folder is deleted, into the transcript directory as `<transcript-name>.tool-results/<file>`, beside the review's transcript. When no transcript is kept they are deleted with the folder.
- **FR-007**: The `keep_transcripts` default changes from `partial` to `all`; `partial` and `off` keep their current meaning. With `off`, the session folder is still deleted and nothing is moved.
- **FR-008**: The fixed retention count of 50 transcripts is replaced by an age bound `transcript_max_age`, default `168h`, settable by `--transcript-max-age`, `PRUEFER_TRANSCRIPT_MAX_AGE` or the operator YAML config.
- **FR-009**: A size ceiling `transcript_max_bytes`, default 1 GiB, bounds the transcript directory; above it the oldest files are deleted first. It is settable through the operator-level configuration channels.
- **FR-010**: `transcript_max_age` and `transcript_max_bytes` are validated at configuration load in the same fail-loud way as `keep_transcripts`; zero, negative and unparseable values are refused with an error naming the setting.
- **FR-011**: Both new settings are operator-only; a reviewed repository's resident config (`.pruefer/config.yaml` at the base ref) can never set or change them (ADR 1642).
- **FR-012**: Pruning runs after each transcript save and once at startup. Age is determined from the timestamp in the file name, never from mtime (ADR 2303).
- **FR-013**: Pruning deletes only names matching Pruefer's own transcript and `.tool-results` shapes; any other entry in the directory is left alone.
- **FR-014**: The existing package-level mutex continues to serialise transcript saves and prunes.
- **FR-015**: The decision is recorded in an amendment of ADR 2303 or a new ADR 2363. It covers: the changed privacy consequence (every review's file reads kept up to 7 days, outside the shared Claude profile); disk use bounded by age and `transcript_max_bytes`; why the session is persisted and then deleted rather than never persisted (#2375); what the CLI writes under the session folder; and whether Pruefer's own stream-json transcript already contains everything the profile JSONL does.
- **FR-016**: `cmd/pruefer/README.md` documents the new defaults and the three ways to set the age, and tells operators that older versions left `*-pruefer-pr-*` folders under `<profile>/projects/` which can be deleted by hand.
- **FR-017**: Existing documentation that states the old behaviour is updated to match: the Pruefer retention paragraph in `docs/USER_GUIDE.md` (`keep_transcripts` default `partial`, "newest 50 files") and the Pruefer entries in `CLAUDE.md`. Because `docs/USER_GUIDE.md` is a canonical page, `docs/llms-full.txt` is regenerated in the same change.
- **FR-018**: Unit tests with a fake profile directory cover: removal with `max_coverage_passes: 0`; the empty-directory checks including sibling and unrelated directories and non-empty or wrongly named ones; `tool-results` moved under `all` and not under `off`; absence of `--no-session-persistence` at every cap; the `all` default; age pruning with an injected clock and named files; the byte ceiling; survival of foreign files; refusal of invalid `transcript_max_age` and `transcript_max_bytes`; and a two-invocation review in which the session survives between invocations.

### Key Entities

- **Review session**: the Claude CLI's persisted conversation for one Pruefer review invocation: `<id>.jsonl` plus an optional `<id>/` folder (containing `tool-results/`) under an encoded-cwd directory in the profile's `projects/`.
- **Encoded-cwd directory**: the per-working-directory folder the CLI creates under `<profile>/projects/`; for a review its name contains `pruefer-pr-<N>-<rand>`.
- **Transcript**: Pruefer's own saved record of a review in `.pruefer/transcripts/`, with a timestamped name, and its sibling `<transcript-name>.tool-results/` directory of relocated hidden outputs.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After any review ends, with any `max_coverage_passes` value, the number of session files, session folders and review-named empty directories it leaves under the Claude profile is zero.
- **SC-002**: With default settings, every completed review has a transcript in `.pruefer/transcripts/`, with its hidden tool outputs beside it when the session had any.
- **SC-003**: No transcript or `.tool-results` entry older than `transcript_max_age` (default 7 days) remains in the directory after the next save or startup prune, and the directory never stays above `transcript_max_bytes` (default 1 GiB) after a prune.
- **SC-004**: Entries in `.pruefer/transcripts/` that Pruefer did not write, and profile folders belonging to other reviews or to the operator's own sessions, are never deleted (zero in all test scenarios).
- **SC-005**: After merge, a live review on the dev daemon leaves no new `*-pruefer-pr-*` folder under the profile, writes its transcript and any `tool-results` to `.pruefer/transcripts/`, and a transcript older than 7 days is purged at the next save.

## Assumptions

- "Every review" means every review that runs the reviewer and reaches the end-of-review hook; a review that fails before any session exists has nothing to keep or remove.
- The transcript saved for a review is the one Pruefer already writes (its own stream-json transcript, ADR 2303); this issue changes when it is saved, how long it lives and what is stored beside it, not its content.
- The byte ceiling counts only Pruefer's own transcript and `.tool-results` entries; foreign files are not counted or deleted.
- The `.tool-results` directory is subject to the same age rule as its transcript, using the timestamp in the transcript name it carries.
- Sizing is by total bytes of Pruefer's own entries, and "oldest first" is determined by the timestamp in the name.
- The startup prune uses the same age and byte settings as the post-save prune, and runs once, not periodically.
- The exact name for the YAML keys matches the flag and environment names (`transcript_max_age`, `transcript_max_bytes`); the `transcript_max_bytes` flag and env names follow the same pattern as the age setting.

## Out of Scope *(optional)*

- Automatic deletion of folders left by earlier versions; operators remove them by hand, and Pruefer never sweeps a Claude profile.
- The reviewer's reading behaviour (#2362) and the coverage loop itself (#2375).
- Passing `--no-session-persistence`, or any other change to how the CLI persists sessions during a review.

## Source References *(optional)*

- ADR 2303 (partial-coverage transcript retention), ADR 2375 (coverage resume loop and its live probe), ADR 1642 (repo-resident config is base-ref and narrowing-only)
- #2375 / PR #2376 (`removeReviewSessions`), #2362 (diagnosed from transcripts)
