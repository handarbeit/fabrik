# Feature Specification: First-run polish — empty board, multi-repo banner, default stages' own notices

**Feature Branch**: `fabrik/issue-1894`
**Created**: 2026-11-07
**Status**: Specified
**Input**: User description: "Seen on the first engine start against a board freshly created by `fabrik init --github-app --create-board` (shadoworg #3, 2026-09-26). Each one makes a clean install look broken.

1. **An empty board is treated as an indexer hiccup, every poll.** A new board legitimately has 0 items. Startup and every poll print `project board fetch returned 0 items, totalCount=0 (attempt 1/3) — retrying in case of indexer hiccup` twice, then `initial board probe returned 0 items — deferring bootstrap to next poll`, and the next poll repeats all of it. When `totalCount=0` is returned consistently, Fabrik should conclude the board is empty, bootstrap the cache as empty, and log one line: \"board is empty — add issues to the board to start\". It should not retry and warn every poll. The retry must still protect against a genuine transient zero on a populated board, so Research must establish how to tell the two apart (e.g. the cached board previously had items, or the zero is consistent across N polls).
2. **`repo: shadoworg/` in the startup banner** when `repo:` is unset (multi-repo mode). It should read something like `repo: (all repos on the board)`.
3. **Shipped default stages trip their own startup notices.** `fabrik init` extracts Review and Validate with `wait_for_reviews: true` and no `expected_reviewers`, so every fresh install prints two \"no expected_reviewers declaration\" notices. Either ship defaults that declare `expected_reviewers` (for example `[]`, or a commented choice), or suppress the notice for unmodified defaults. The one-time `CIWaitTimeout` semantics-change notice should also not fire on a fresh install that never had the old setting.

Out of scope: the callback page styling (#1887) and App-mode operator identity (#1893)."

## Background

The first engine start against a board freshly created by `fabrik init --github-app --create-board` (2026-09-26) showed three kinds of noise. Each makes a correct, clean install look broken, which is the worst moment for a new operator to doubt the tool.

1. **Empty board.** A new board legitimately has no items. The board fetch treats a zero-item result as a possible indexer hiccup and retries with a warning on every fetch. The startup probe then defers bootstrap, and the cache stays unpopulated. The warnings and the deferred bootstrap therefore repeat on every poll, indefinitely, until the operator adds an item.
2. **Banner.** In multi-repo mode (`repo:` unset) the plain-text startup banner prints the owner followed by an empty repo, e.g. `repo: shadoworg/`, which reads as a truncated value.
3. **Startup notices.** The shipped Review and Validate stages set `wait_for_reviews: true` without declaring `expected_reviewers`, so an unmodified install triggers the "no expected_reviewers declaration" notice twice, in the log and as two warnings-panel entries. A separate one-time notice about the changed `CIWaitTimeout` semantics fires on every start.

Maintainer re-check on `main` at 52014e4b (2026-11-07) found all three symptoms still reproduce, with these refinements:

- The "deferring bootstrap" line is now throttled, so it no longer prints every poll. The "retrying in case of indexer hiccup" warnings still fire on every fetch, and the cache still stays unpopulated, so the probe bootstrap re-runs each poll.
- The banner line is in the plain-text banner. The TUI header must be checked for the same `owner/` rendering.
- For the `expected_reviewers` notice, fixing the shipped defaults is preferred over suppressing the notice for unmodified files. The `fabrik init` output should then agree with the defaults.
- The `CIWaitTimeout` semantics notice is unconditional by design, and a fresh install cannot be told apart from an upgrade without new state. Retiring the notice outright is acceptable, since it has shipped for about three months of releases. This is recorded in the ADR rather than by adding a "had the old setting" detector.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A new, empty board starts quietly (Priority: P1)

An operator creates a board with `fabrik init --create-board`, starts the engine and sees one clear line saying the board is empty and how to proceed. No retry warnings follow on later polls, and the first item they add is picked up normally.

**Why this priority**: It is the most visible symptom, repeating on every poll, and it is the first thing every new installation sees.

**Independent Test**: Start the engine against a board that has zero items. Observe the log across several polls. Then add an item and confirm it is processed.

**Acceptance Scenarios**:

1. **Given** a board that consistently reports zero items and has never held any, **When** the engine starts and polls repeatedly, **Then** it concludes the board is empty, treats the cache as bootstrapped-empty, and logs a single "board is empty — add issues to the board to start" line rather than retry warnings on every poll.
2. **Given** an engine that has concluded the board is empty, **When** an item is later added to the board, **Then** it is discovered and processed on the normal schedule with no restart.
3. **Given** a board that previously held items (cache populated), **When** a fetch suddenly returns zero items, **Then** the engine still treats it as a possible transient indexer fault, retries, and does not wipe or replace the cache with an empty one.
4. **Given** a populated board at engine startup (no cache yet) whose first fetch returns a transient zero, **When** later fetches return the real items, **Then** the board is bootstrapped from the real items and is never left marked empty.

---

### User Story 2 - The banner reads correctly in multi-repo mode (Priority: P2)

An operator running without `repo:` sees a banner that states plainly that all repositories on the board are in scope, instead of a dangling `owner/`.

**Why this priority**: Cosmetic, but a truncated-looking value undermines confidence in the first start.

**Independent Test**: Start with `repo:` unset and with `repo:` set. Compare the banner and the TUI header in both modes.

**Acceptance Scenarios**:

1. **Given** `repo:` is unset, **When** the plain-text startup banner prints, **Then** the repo line reads like `repo: (all repos on the board)` and never shows a bare `owner/`.
2. **Given** `repo:` is set, **When** the banner prints, **Then** it shows `owner/repo` exactly as before.
3. **Given** `repo:` is unset, **When** the TUI header renders the repo scope, **Then** it uses equivalent wording and never shows a bare `owner/`.

---

### User Story 3 - An unmodified install raises no notices about its own defaults (Priority: P1)

An operator runs `fabrik init` and starts the engine with the shipped stage files untouched. The log and the warnings panel contain no notice about missing `expected_reviewers` and no `CIWaitTimeout` semantics notice.

**Why this priority**: A fresh install that immediately raises its own warnings looks broken and trains operators to ignore real notices.

**Independent Test**: Run `fabrik init` in an empty directory and start the engine with unmodified defaults. Confirm the log and the warnings panel are free of both notices. Separately, edit a stage to set `wait_for_reviews: true` with no declaration and confirm the notice still appears.

**Acceptance Scenarios**:

1. **Given** freshly extracted default stages, **When** the engine starts, **Then** no "no expected_reviewers declaration" notice appears in the log or the warnings panel.
2. **Given** a stage with `wait_for_reviews: true` and no `expected_reviewers` that the operator wrote or edited, **When** the engine starts, **Then** the notice still fires.
3. **Given** any start, fresh or upgraded, **When** the engine starts, **Then** no one-time `CIWaitTimeout` semantics-change notice is printed.
4. **Given** the `fabrik init` closing output (#2496), **When** it describes the stages, **Then** it agrees with the shipped defaults.

---

### Edge Cases

- A board that was empty and is then populated and then emptied again by a human while the engine runs. A drop to zero after items were seen must keep the transient-fault protection.
- A zero-item result accompanied by a non-zero `totalCount`, which is a real indexer inconsistency and must keep today's retry and warn behaviour.
- An engine restarted against an empty board repeatedly. Each start logs the empty-board line once, not once per poll.
- An existing install with customised Review/Validate stages. Its stage files are not rewritten by this change; only the shipped defaults and the notice's treatment of them change.
- Users who already ran `fabrik init` with the old defaults still see the notice until they refresh their stage files (for example via `fabrik refresh-stages`).
- Multi-repo mode where the owner is also unset or unknown. The banner must still avoid rendering a dangling separator.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the project board consistently reports zero items with a `totalCount` of zero, and the cache has never held items, the engine MUST conclude the board is empty, bootstrap the cache as empty, and stop treating the result as a transient indexer fault on subsequent polls.
- **FR-002**: On reaching the empty-board conclusion the engine MUST log exactly one line stating that the board is empty and that issues must be added to the board to start, and MUST NOT repeat the "retrying in case of indexer hiccup" warnings or the "deferring bootstrap" message on every following poll while the board remains empty.
- **FR-003**: The existing protection against a transient zero MUST be preserved: a zero-item result on a board the engine has previously seen items on, or a zero-item result that conflicts with a non-zero `totalCount`, MUST still be retried and MUST NOT replace or clear a populated cache.
- **FR-004**: The rule for telling a genuinely empty board from a transient zero MUST be determined by Research and recorded in an ADR, with the signals considered (prior cached items, consistency across N polls, `totalCount`) and the reason for the chosen rule.
- **FR-005**: An item added to a board previously concluded empty MUST be discovered and processed without an engine restart.
- **FR-006**: When `repo:` is unset (multi-repo mode), the plain-text startup banner MUST NOT print a bare `owner/`; it MUST present the repo scope as all repositories on the board (e.g. `repo: (all repos on the board)`). When `repo:` is set, the output is unchanged.
- **FR-007**: Wherever the TUI header (and any other operator-facing surface that renders the owner/repo scope) formats the repo scope, it MUST apply the same multi-repo wording and MUST NOT render a dangling `owner/`.
- **FR-008**: The shipped default Review and Validate stages MUST be such that an unmodified `fabrik init` install produces no "no expected_reviewers declaration" notice, either in the log or as a warnings-panel entry. The preferred means is that the shipped defaults themselves declare their reviewer expectation, not suppression of the notice for unmodified files.
- **FR-009**: The "no expected_reviewers declaration" notice MUST continue to fire for a stage that sets `wait_for_reviews: true` without declaring `expected_reviewers` when that stage is operator-authored or operator-modified.
- **FR-010**: The reviewer expectation the shipped defaults declare MUST be a deliberate, documented choice, with the reasoning for it stated in the stage file's comments and the ADR, and it MUST be consistent with the `fabrik init` closing output (#2496) and with the documentation of `expected_reviewers`.
- **FR-011**: The one-time `CIWaitTimeout` semantics-change notice MUST no longer be printed on any start, fresh or upgraded. Its retirement and the rationale (shipped for about three months of releases; fresh vs upgrade is not distinguishable without new state) MUST be recorded in the ADR, and no "had the old setting" detector may be added.
- **FR-012**: User-facing documentation that describes the retired notice, the empty-board behaviour, the banner or the shipped stage defaults MUST be updated to match, including any regeneration of the documentation bundle required by the repository's conventions.

### Key Entities *(if applicable)*

- **Board emptiness conclusion**: The engine's determination that a board has no items (as opposed to a transient fetch fault), deciding whether the cache is bootstrapped empty or the fetch is retried.
- **Shipped default stage**: A stage file extracted by `fabrik init` from the embedded defaults; its content determines which startup notices an unmodified install raises.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A fresh install against an empty board produces exactly one empty-board log line over any number of polls and zero "indexer hiccup" warnings.
- **SC-002**: A fresh install with unmodified default stages produces zero startup notices about `expected_reviewers` and zero about `CIWaitTimeout`, in both the log and the warnings panel.
- **SC-003**: In multi-repo mode the startup banner and the TUI header contain no bare `owner/` text.
- **SC-004**: A previously populated board that transiently returns zero items never results in an emptied cache; the transient-fault behaviour is unchanged for it.
- **SC-005**: After an item is added to a board concluded empty, it is picked up within the normal poll cadence with no restart.

## Assumptions

- The empty-board rule is a Research decision (FR-004); the spec fixes the required behaviours at both ends (consistent zero on a never-populated board is empty; zero after items were seen is transient) and leaves the exact criterion open.
- The maintainer's preference is adopted: fix the shipped stage defaults rather than suppress the notice for unmodified files. The specific reviewer expectation to declare is for Plan to select, subject to FR-010.
- Retiring the `CIWaitTimeout` notice outright is acceptable (maintainer comment, 2026-11-07), and requires no new persisted state.
- Existing installs keep their on-disk stage files; they are not migrated by this change.
- The operator-facing log wording for the empty board and the banner may differ slightly from the examples in the request as long as the meaning is the same.

## Out of Scope *(optional)*

- Styling of the authentication callback page (#1887).
- App-mode operator identity (#1893).
- Rewriting or migrating stage files already on disk in existing installs.
- Changing the retry count or timing used for genuine transient zeros.
- Adding a new persisted marker to distinguish fresh installs from upgrades.

## Source References *(optional)*

- Maintainer re-check comment on this issue (2026-11-07), against `main` at 52014e4b.
- #1887 and #1893 (adjacent first-run issues, excluded here); #2496 (`fabrik init` closing output, which must stay consistent with the shipped defaults); ADR 1410 (origin of the `CIWaitTimeout` notice).
