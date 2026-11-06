# Feature Specification: `fabrik init` names the required board columns and stops claiming readiness when it did not create the board

**Feature Branch**: `fabrik/issue-2496`
**Created**: 2026-11-05
**Status**: Specified
**Input**: User description:

> ## Problem
>
> `fabrik init` without `--create-board` still ends with a readiness claim it has not checked (`cmd/init.go`, the closing lines of `runInit`):
>
> ```
> Fabrik is ready. Stage configs and plugin skills are in .fabrik/
> Edit .fabrik/config.yaml with your project settings, then run fabrik.
> ```
>
> This covers three cases:
> - plain `fabrik init`;
> - `fabrik init <project-url>`, which links an existing board;
> - any user-owned board, which `--create-board` refuses (#770).
>
> In all three, nothing has created or checked the board's Status columns. If they do not match the stage names, the first `fabrik` run fails in `checkStageColumnAlignment` (`engine/startup.go`) right after being told setup succeeded. `init` knew the required column names, and both fixes already exist: `fabrik repair-board` (dry-run, then `--apply`) and `--create-board`. `init` mentions neither.
>
> `--create-board` (ADR-1714) closed most of the original #1359. This issue is the part that remains. It replaces #1359, whose spec was written before board creation existed.
>
> ## Requirements
>
> - **R1:** On every path that did not create the board, `init` prints the Status columns the board needs, one per line and in order, after writing the stage configs. Derive them with `boardColumnNames` (`cmd/board_admin.go`), the list `--create-board` and `repair-board` already use, applied to the stage configs actually on disk. Stage files `init` skipped because they already existed count.
> - **R2:** The printed list must contain every column `checkStageColumnAlignment` would demand for the same configs. It may be a superset: `boardColumnNames` also lists the cleanup stage (Done) and holding stages unconditionally. The two rules are not identical, so do not claim they are; state the relationship in a comment next to the print.
> - **R3:** The closing message stops saying "Fabrik is ready" on these paths. It says what was written and what remains:
>   - create the listed columns;
>   - or run `fabrik repair-board` to see what is missing (`--apply` adds the missing columns on an org-owned board);
>   - or, for a fresh org board, `fabrik init --create-board --owner <org>`.
>   Mention `--create-board` only when no board is configured yet. The `--create-board` path's own output is unchanged.
> - **R4:** `init` remains fully offline on these paths: no token, no network, no board query. Checking the board is `repair-board`'s job.
> - **R5:** Update `docs/USER_GUIDE.md`'s onboarding text if it shows `init`'s closing output or relies on the old wording. Regenerate `docs/llms-full.txt` if a canonical page changes.
>
> ## Non-goals
>
> - Any change to `checkStageColumnAlignment`, `repair-board` or `--create-board` behaviour.
> - Making `init` query or create the board on these paths.
>
> ## Acceptance Criteria
>
> - **AC1:** For a fixture stage set that includes a cleanup stage, a holding stage and an unmanaged stage, every name the startup check would require appears in `init`'s printed list. Assert this by driving both from the same fixture, so a divergence fails the build. The unmanaged stage must not appear.
> - **AC2:** With a customised stage set (one stage renamed, one added, one removed), the printed list changes to match. The test must fail against a hardcoded list; verify that by hardcoding it temporarily and confirming red.
> - **AC3:** `init` succeeds and prints the same guidance with no token or network credentials in the environment.
> - **AC4:** The closing message on these paths does not say "Fabrik is ready" and names `repair-board`. The `--create-board` path's output is unchanged.
>
> ## Context
>
> - Replaces #1359, closed because its premises ("no code path can create columns", R7's doctor subcommand) are now shipped as `--create-board` and `repair-board` (ADR-1714).
> - `boardColumnNames`, `missingStageColumns`: `cmd/board_admin.go`.

## Background

`fabrik init` writes the stage configs, the plugin and a config template. When it does not also create the board, its closing output still says "Fabrik is ready". Nothing at that point has created or checked the board's Status columns. If they do not match the stage names, the first `fabrik` run fails in the startup column-alignment check, immediately after the user was told setup succeeded.

`init` already knows the column names the board needs. Two fixes already exist: `fabrik repair-board` (dry run, then `--apply`) and `fabrik init --create-board`. `init` mentions neither. `--create-board` (ADR-1714) closed most of the earlier report #1359; this issue covers what remains. The paths affected are:

- plain `fabrik init`;
- `fabrik init <project-url>`, which links an existing board;
- any user-owned board, which `--create-board` refuses (#770).

On those paths the operator should be told which columns the board needs and which command checks or fixes it. The operator should not be told setup is finished.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See the exact columns my board needs (Priority: P1)

An operator runs `fabrik init` (plain, or with a project URL) and wants to know which Status columns to create on the board before the first run.

**Why this priority**: This is the information `init` already has and omits. It is what turns a startup failure into a setup step.

**Independent Test**: Run `init` in an empty directory on a path that does not create a board. Check that the output lists the board's columns, one per line, in pipeline order, matching the stage configs on disk.

**Acceptance Scenarios**:

1. **Given** the default stage set, **When** `init` completes without creating a board, **Then** the output lists every required column once per line in stage order, including the cleanup column (Done) and holding columns (Queued).
2. **Given** `.fabrik/stages/` already contains stage files from an earlier `init`, **When** `init` runs again and skips them, **Then** the printed list is derived from those files as they are on disk.
3. **Given** a stage set where one stage is renamed, one added and one removed, **When** `init` completes, **Then** the printed list reflects exactly that set.
4. **Given** a stage set containing an unmanaged parking stage (for example Backlog), **When** `init` prints the list, **Then** the unmanaged stage does not appear.

---

### User Story 2 - Closing message says what remains, not "ready" (Priority: P1)

An operator reads the last lines of `init` output to decide what to do next.

**Why this priority**: The false readiness claim is the defect being reported.

**Independent Test**: Capture `init` output on each non-board-creating path. Check for the absence of "Fabrik is ready" and the presence of `repair-board` guidance.

**Acceptance Scenarios**:

1. **Given** `init` ran without creating a board, **When** it finishes, **Then** the closing message states what was written and that the board's columns still need to match, and does not contain "Fabrik is ready".
2. **Given** the same run, **When** the operator reads the next steps, **Then** they can create the listed columns, or run `fabrik repair-board` to see what is missing (noting that `--apply` adds the missing columns on an org-owned board).
3. **Given** no board is configured yet, **When** `init` finishes, **Then** the message also offers `fabrik init --create-board --owner <org>` for a fresh org board.
4. **Given** a board is already linked (for example via a project URL), **When** `init` finishes, **Then** the message does not suggest `--create-board`.

---

### User Story 3 - Offline and unchanged elsewhere (Priority: P2)

An operator runs `init` without credentials or network access, or uses `--create-board`.

**Why this priority**: It protects existing behaviour. `init` is the first command run, often before any token is set up.

**Independent Test**: Run `init` on a non-board-creating path with no token or credential variables in the environment and the network unavailable; separately run the `--create-board` path and compare its output with the pre-change output.

**Acceptance Scenarios**:

1. **Given** no token or network credentials in the environment, **When** `init` runs on a non-board-creating path, **Then** it succeeds and prints the same column list and guidance as with credentials present.
2. **Given** `--create-board` succeeds, **When** `init` finishes, **Then** its output is unchanged from today.

---

### Edge Cases

- A stage file that `init` skipped because it already existed still contributes to the printed list. The list reflects the configs on disk, not only those just written.
- The startup check and the printed list use different rules. The list may be a superset (it always includes the cleanup stage and holding stages). It must never omit a column the startup check would require.
- A user-owned board: `--create-board` refuses it, so the guidance on this path must not imply `--create-board` can fix it. `repair-board --apply` is also refused on a user-owned board, so the guidance for this case is to create the listed columns by hand.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: On every `init` path that did not create the board, after the stage configs are written, `init` prints the Status columns the board needs, one per line, in stage order.
- **FR-002**: The printed list is derived from the stage configs actually present on disk, including files `init` skipped because they already existed, using the same column-naming rule that `--create-board` and `repair-board` use. It is not a hardcoded list.
- **FR-003**: The printed list contains every column the startup column-alignment check would require for the same stage configs. It may include more (the cleanup stage and holding stages are always listed). It never includes unmanaged parking stages. The code next to the print states how this rule relates to the startup check and does not claim the two are identical.
- **FR-004**: On these paths, the closing message does not say "Fabrik is ready". It states what was written (stage configs and plugin skills in `.fabrik/`) and what remains to be done.
- **FR-005**: The closing message names these next steps: create the listed columns on the board, or run `fabrik repair-board` to see what is missing (`--apply` adds the missing columns on an org-owned board).
- **FR-006**: The closing message offers `fabrik init --create-board --owner <org>` for a fresh org board only when no board is configured yet.
- **FR-007**: On these paths `init` stays fully offline: it needs no token, makes no network call and does not query the board.
- **FR-008**: The `--create-board` path's output and behaviour are unchanged.
- **FR-009**: `docs/USER_GUIDE.md` is updated wherever it shows `init`'s closing output or relies on the old "ready" wording. If any canonical doc page changes, `docs/llms-full.txt` is regenerated in the same change.

### Key Entities *(if applicable)*

- **Required column list**: the ordered set of Status column names derived from the on-disk stage configs, excluding unmanaged stages and including the cleanup and holding stages.
- **Startup alignment check**: the check at engine startup that compares stage names with board columns. It is the reference the printed list must cover.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For a fixture stage set containing a cleanup stage, a holding stage and an unmanaged stage, every name the startup check requires appears in the printed list, and the unmanaged stage does not. The test drives both the printed list and the startup check from the same fixture, so a divergence fails the build.
- **SC-002**: For a customised stage set (one stage renamed, one added, one removed), the printed list matches the customised set. The test fails when the list is hardcoded; this is confirmed by temporarily hardcoding it and seeing the test go red.
- **SC-003**: `init` on a non-board-creating path succeeds and prints identical guidance with no token or network credentials in the environment.
- **SC-004**: On these paths the closing output does not contain "Fabrik is ready" and does contain `repair-board`. The `--create-board` path's output is byte-for-byte unchanged.
- **SC-005**: `--create-board` is mentioned in the closing guidance only when no board is configured.
- **SC-006**: `docs/USER_GUIDE.md` contains no remaining reference to the old closing wording, and `docs/llms-full.txt` matches a fresh regeneration.

## Assumptions

- "Every path that did not create the board" includes plain `init`, `init <project-url>`, `init --github-app` without `--create-board`, and the user-owned-board case where `--create-board` is refused.
- "No board is configured yet" means no project number was supplied to `init` (by URL or by prompt). The next-steps text may be organised however reads best, so long as it meets FR-005 and FR-006.
- The pre-existing plugin install hint printed at the end of `init` stays as it is.
- The printed list uses plain stage names, one per line, with no decoration that would make copying them into the board UI harder.

## Out of Scope *(optional)*

- Any change to the startup column-alignment check, `repair-board` or `--create-board` behaviour.
- Making `init` query, validate or create the board on these paths. Checking the board is `repair-board`'s job.
- Reconciling the two column-naming rules so the printed list equals the startup requirement exactly. A documented superset is accepted.

## Source References *(optional)*

- Closing message in `runInit`: `cmd/init.go`
- `boardColumnNames`, `missingStageColumns`: `cmd/board_admin.go`
- `checkStageColumnAlignment`: `engine/startup.go`
- ADR-1714 (`--create-board`, `repair-board`); replaces #1359; user-owned boards refused by `--create-board` (#770)
- `docs/USER_GUIDE.md`: the `fabrik init` walkthrough near the top and "Repairing an Existing Board"
