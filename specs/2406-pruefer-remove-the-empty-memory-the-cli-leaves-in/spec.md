# Feature Specification: Pruefer removes the empty `memory/` the CLI leaves in a review's profile folder

**Feature Branch**: `fabrik/issue-2406`
**Created**: 2026-10-19
**Status**: Specified
**Input**: User description: "## Problem

#2363 (PR #2379) removes a review's sessions from the Claude profile. It then removes the encoded-cwd folder the review leaves empty, under these guards:

- the folder sits directly under `<profile>/projects/`;
- its name carries `pruefer-pr-<N>-`;
- removal uses `os.Remove`, which fails on a non-empty directory.

Live on the dev daemon (2026-10-19), every review still leaves its `*-pruefer-pr-<N>-*` folder. The session's `<id>.jsonl` and `<id>/` are gone, but the CLI also creates an empty `memory/` subdirectory in the folder. `os.Remove` therefore fails, and Pruefer logs:

```
[pr#2405 review] leaving …/projects/-private-var-folders-…-T-pruefer-pr-2405-961453037: remove …: directory not empty
```

The leftover folders contain only `memory/`, and it is empty. The privacy problem #2363 addressed is solved: no session content remains. The folders still pile up, one per review.

## Requirements

- **R1.** In `removeEmptyReviewDir`, after the existing direct-child and PR-name checks pass, remove `<parent>/memory` with `os.Remove`, then remove `<parent>` as today. `os.Remove` never removes a non-empty directory, so a `memory/` holding any file is left alone, and so is its parent. Do not use `RemoveAll`.
- **R2.** Remove nothing else inside the folder. Any other entry keeps the folder and is logged as today.
- **R3.** Record in ADR 2363, as an amendment, that the CLI creates an empty `memory/` in the session's encoded-cwd folder. Name the CLI version this was observed with.

## Acceptance

- Unit tests with a fake profile:
  - a folder holding only an empty `memory/` is removed;
  - a folder whose `memory/` holds a file is kept, and so are the file and `memory/`;
  - a folder holding another empty directory is kept;
  - the existing PR-name and direct-child guards still apply before `memory/` is touched.
- Removing the `memory/` step makes the first test fail.
- After merge, a live review on the dev daemon leaves no `*-pruefer-pr-*` folder under the profile.

## Out of scope

Folders left by earlier versions. These are deleted by hand; Pruefer never sweeps a Claude profile."

## Background

Pruefer reviews run the Claude CLI in a per-review working directory, and the CLI records each session in the Claude profile under an encoded-cwd folder in `<profile>/projects/`. #2363 (PR #2379, ADR 2363) made Pruefer delete the review's session files and then the emptied encoded-cwd folder, guarded so only a direct child of `<profile>/projects/` whose name carries `pruefer-pr-<N>-` can be removed, and using `os.Remove` so a non-empty directory is never deleted.

Observed live on the dev daemon on 2026-10-19: the session content is gone (so the privacy goal of #2363 is met), but the CLI also creates an empty `memory/` subdirectory in that folder. The folder is therefore never empty, `os.Remove` fails with "directory not empty", Pruefer logs a "leaving …" line, and one empty folder accumulates per review. The fix is to also remove that one known-empty subdirectory, without weakening any guard.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A finished review leaves no folder in the Claude profile (Priority: P1)

An operator running Pruefer wants the Claude profile to stay clean: after a review completes, nothing named for that review remains under `<profile>/projects/`.

**Why this priority**: This is the whole change; it completes the cleanup #2363 intended.

**Independent Test**: With a fake profile, create a `pruefer-pr-<N>-*` folder directly under `projects/` containing only an empty `memory/`, run the removal, and check the folder is gone.

**Acceptance Scenarios**:

1. **Given** a review folder directly under `<profile>/projects/` whose name carries `pruefer-pr-<N>-` and which contains only an empty `memory/`, **When** the review's cleanup runs, **Then** both `memory/` and the folder are removed and no "leaving …" line is logged for it.
2. **Given** the same folder where `memory/` contains a file, **When** cleanup runs, **Then** the file, `memory/` and the folder all remain and the existing "leaving …" log line is written.
3. **Given** the same folder also containing another (empty) directory beside `memory/`, **When** cleanup runs, **Then** `memory/` may be removed but the folder and the other directory remain, and the existing "leaving …" log line is written.

---

### User Story 2 - The safety guards still come first (Priority: P1)

A maintainer wants assurance that the new step can never touch anything outside a review's own folder.

**Why this priority**: The cleanup deletes inside a user's Claude profile; the guards from #2363 are what make that safe.

**Independent Test**: With a fake profile, present a folder that is not a direct child of `projects/`, and one whose name lacks `pruefer-pr-<N>-`, each holding an empty `memory/`; check that nothing is removed in either case.

**Acceptance Scenarios**:

1. **Given** a folder that is not a direct child of `<profile>/projects/` containing an empty `memory/`, **When** cleanup runs, **Then** neither `memory/` nor the folder is touched.
2. **Given** a direct child of `projects/` whose name does not carry `pruefer-pr-<N>-` containing an empty `memory/`, **When** cleanup runs, **Then** neither is touched.

---

### Edge Cases

- The folder has no `memory/` (older CLI versions or a CLI that stops creating it): behaviour is as today, and the folder is removed if empty.
- `memory/` does not exist but the folder holds other entries: the folder is kept and logged as today.
- Removing `memory/` fails for a reason other than non-empty (permissions): the failure is logged as today and the folder is left; it does not fail the review.
- `memory/` is a symlink or a file rather than a directory: it is never followed or recursively deleted; the folder is kept if the removal fails.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: In the removal of a review's emptied encoded-cwd folder (`removeEmptyReviewDir`), after the existing direct-child-of-`<profile>/projects/` and `pruefer-pr-<N>-` name checks pass, Pruefer MUST remove `<folder>/memory` using a non-recursive removal that fails on a non-empty directory, and then remove `<folder>` the same way as today.
- **FR-002**: Pruefer MUST NOT use any recursive removal for this step. A `memory/` holding any entry, and its parent folder, MUST be left in place.
- **FR-003**: Pruefer MUST NOT remove any other entry inside the folder. If any entry other than an empty `memory/` is present, the folder is kept and the existing "leaving …" log line is written.
- **FR-004**: The `memory/` removal MUST NOT run unless both existing guards (direct child of `<profile>/projects/`, name carries `pruefer-pr-<N>-`) have passed.
- **FR-005**: A failure to remove `memory/` or the folder MUST remain non-fatal and logged, as today.
- **FR-006**: ADR 2363 MUST be amended to record that the CLI creates an empty `memory/` in the session's encoded-cwd folder, naming the CLI version it was observed with.
- **FR-007**: Unit tests using a fake profile MUST cover: only-empty-`memory/` removed; `memory/` holding a file keeps the file, `memory/` and the folder; a folder holding another empty directory is kept; the direct-child and PR-name guards apply before `memory/` is touched. Disabling the `memory/` removal step MUST make the first of these fail.

### Key Entities *(if applicable)*

- **Review folder**: the encoded-cwd directory the CLI creates under `<profile>/projects/` for a review, named with `pruefer-pr-<N>-`.
- **`memory/`**: an empty subdirectory the CLI creates inside the review folder.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After a review on the dev daemon following this change, zero `*-pruefer-pr-*` folders remain under `<profile>/projects/` for that review.
- **SC-002**: No "leaving …: directory not empty" line is logged for a review whose folder holds only an empty `memory/`.
- **SC-003**: In the unit tests, a `memory/` containing a file, and any extra entry in the folder, are never deleted (100% of those cases keep their contents).
- **SC-004**: The unit test for the empty-`memory/` case fails when the `memory/` removal step is taken out.

## Assumptions

- The CLI creates only an empty `memory/` there; the issue reports this from live observation on 2026-10-19. The CLI version for the ADR amendment is taken from the version the dev daemon was running when this was observed (the implementer reads it from the daemon's CLI).
- The exact name `memory` is stable enough to special-case; if the CLI adds other directories later, the folder is kept and logged, which is the safe failure.
- Earlier-version leftover folders are removed by hand; this change does not clean them up.

## Out of Scope *(optional)*

- Folders left by earlier versions. These are deleted by hand; Pruefer never sweeps a Claude profile.
- Removing any entry other than an empty `memory/`, or any recursive cleanup.

## Source References *(optional)*

- #2363 / PR #2379 and `adrs/2363-pruefer-review-sessions-leave-profile.md` (the cleanup this extends)
- Live log observation on the dev daemon, 2026-10-19: `[pr#2405 review] leaving …/projects/-private-var-folders-…-T-pruefer-pr-2405-961453037: remove …: directory not empty`
