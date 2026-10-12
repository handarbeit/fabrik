# Feature Specification: Prove the workflow migration is skip-safe without `.installed-version`, and document editing after migration

**Feature Branch**: `fabrik/issue-2198`
**Created**: 2026-10-12
**Status**: Specified
**Input**: Post-merge review of PR #2184 (#2139).

## Background

#2139 promises that a deployment that skipped releases migrates correctly with one `fabrik upgrade`. The migration moves the plugin tree only when `CheckPluginState` reports local changes **and** the per-file classification (`plugin.ClassifyDir` over `plugin/shipped_files.json`) finds files matching no shipped release.

A review that neutralised the per-file table (making `IsShippedFile` consult only the current embed) found `TestMigrate_UncustomisedFromEarlierReleases` (`cmd/upgrade_migrate_test.go`) still passing. Its fixtures seed `.installed-version` from the pristine disk, so `CheckPluginState` reports no customisation and the per-file table is never consulted. The guarantee for an old deployment whose `.installed-version` is missing or untrustworthy therefore rests only on the `plugin` classify tests, not on an end-to-end `fabrik upgrade` test.

Separately, after a migration a second round of edits to `.fabrik/plugin` makes the plan `Blocked` (`local-workflows` already exists and is never overwritten). That is the safe outcome, but the USER_GUIDE does not tell users how to proceed.

Follow-up to #2139 / PR #2184. Note: as of this spec, PR #2184 is still open (queued for the merge train) and the migration code and tests named above are not yet on `main`; see Assumptions.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Skip-safety of an uncustomised old deployment is proven end to end (Priority: P1)

A maintainer wants confidence that an old, uncustomised deployment whose `.installed-version` is missing migrates with no false "customised" verdict, through the real `fabrik upgrade` path rather than only the `plugin` classify unit tests.

**Why this priority**: This is the guarantee #2139 advertises and the one the review showed to be untested end to end.

**Independent Test**: Run the new `cmd`-level test for each earlier-release fixture; it must fail when the per-file historical table is neutralised and pass when intact.

**Acceptance Scenarios**:

1. **Given** a pristine earlier-release fixture (v0.0.64, v0.0.70 or v0.0.77) with `.installed-version` deleted, **When** the upgrade runs, **Then** nothing is moved to `local-workflows`, the base plugin is restored to the current embed, and stage references are qualified to the base plugin.
2. **Given** the historical table is neutralised (`IsShippedFile` consults only the current embed), **When** the same test runs, **Then** it fails.

---

### User Story 2 - A genuinely edited old deployment still migrates, scoped to the edited skill (Priority: P1)

**Why this priority**: Proves the classification does not over-trigger: a real customisation is preserved and only the affected stages are redirected.

**Independent Test**: The same test with one fixture file hand-edited.

**Acceptance Scenarios**:

1. **Given** an earlier-release fixture with `.installed-version` deleted and one skill file hand-edited, **When** the upgrade runs, **Then** the tree moves to `local-workflows` and only the edited skill's stages point at `local-workflows`.

---

### User Story 3 - Users know how to edit after a migration (Priority: P2)

A user who has migrated and wants to customise again needs to know where edits belong and how to recover if they edit the wrong place.

**Why this priority**: The current behaviour is safe but undocumented, leaving users stuck on a Blocked upgrade.

**Independent Test**: Read the USER_GUIDE workflow-migration section and confirm it covers the second-edit case; confirm `docs/llms-full.txt` is regenerated and the docs-drift check passes.

**Acceptance Scenarios**:

1. **Given** the USER_GUIDE migration section, **When** a user reads it, **Then** it states edits belong in `.fabrik/plugins/local-workflows/` after migration.
2. **Given** a user edited `.fabrik/plugin` after migration and the next upgrade is Blocked, **When** they read the section, **Then** it explains the two resolutions: move the edit into `local-workflows`, or `fabrik upgrade --force` to discard it.

---

### Edge Cases

- `.installed-version` absent versus present-but-untrustworthy: this issue covers the absent case; the fixtures must make `CheckPluginState` report local changes so the per-file table is actually consulted.
- Each of the three fixtures must be exercised independently (a failure in one must be attributable).
- The hand-edited case must edit exactly one fixture file so "only the edited skill's stages" is observable.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Add a `cmd`-level test that, for each earlier-release fixture (at least v0.0.64, v0.0.70 and v0.0.77), deletes `.installed-version` before running the upgrade and asserts: nothing is moved to `local-workflows`, the base plugin is restored to the current embed, and stage references are qualified to the base plugin.
- **FR-002**: The same test with one fixture file hand-edited asserts the tree moves and only the edited skill's stages point at `local-workflows`.
- **FR-003**: Both tests are shown non-vacuous in the PR: neutralise the historical table (current-embed-only `IsShippedFile`) and show FR-001's test fails.
- **FR-004**: The USER_GUIDE section on the workflow migration explains the second-edit case: edits belong in `.fabrik/plugins/local-workflows/` after migration; an edit made to `.fabrik/plugin` instead blocks the next upgrade, and how to resolve it (move the edit into `local-workflows`, or `fabrik upgrade --force` to discard it). Regenerate `docs/llms-full.txt`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With the historical table neutralised, FR-001's test fails; with it intact, it passes.
- **SC-002**: No production code change is required. If one turns out to be needed, the PR says why.

## Assumptions

- The migration code, `plugin/shipped_files.json`, the earlier-release fixtures and `cmd/upgrade_migrate_test.go` come from PR #2184 (#2139). That PR is still open (queued in the merge train) when this spec was written, so this work depends on it landing on `main` first; if it has not landed when Implement runs, the work waits for it rather than reimplementing any of it.
- The USER_GUIDE's workflow-migration section is introduced by the same PR; FR-004 extends that section rather than creating a parallel one.
- "Neutralise the historical table" is a temporary local change used to demonstrate non-vacuity; it is evidence in the PR, not committed code.

## Out of Scope

- Changing migration behaviour, classification logic or the shipped-files table.
- Handling a present-but-wrong `.installed-version` beyond what the fixtures already cover.
- Making a second round of edits to `.fabrik/plugin` auto-migrate (the Blocked outcome is intentionally kept).

## Source References

- Issue #2139 and PR #2184
- `cmd/upgrade_migrate_test.go`, `plugin/shipped_files.json` (introduced by PR #2184)
