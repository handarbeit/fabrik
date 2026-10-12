# Feature Specification: Align the migration's bare-name rewrite with runtime resolution, and document which sessions load the PM plugin

**Feature Branch**: `fabrik/issue-2203`
**Created**: 2026-10-12
**Status**: Specified
**Input**: Review of PR #2202 (#2140, rename to `fabrik-stages`).

## Background

1. **Bare-name divergence.** For an unmigrated bare reference `fabrik-X`, the runtime resolver (#2137) tries the reference as written first, so a project plugin that provides a skill named `fabrik-X` wins; only on a miss does `LegacyRef` map it to `fabrik-stages:X`. The `fabrik upgrade` migration instead always rewrites a bare `fabrik-X` to `fabrik-stages:X`. So when a project plugin also provides `fabrik-X`, running `fabrik upgrade` silently switches that stage from the project's skill to the base skill. ADR 2140 records the rewrite as preserving the pre-rename resolver behaviour, but it no longer matches what the new binary does on the same unmigrated config.
2. **Interactive sessions.** Workers pass `--settings {"enabledPlugins":{"fabrik@fabrik":false}}`, so the user-installed PM plugin never loads into a stage worker. Interactive launches (`fabrik resume` in `cmd/resume.go`, the TUI's launch in `tui/commands.go`, `watch/model.go`) pass `--plugin-dir` without that setting, so after the rename the PM plugin loads beside `fabrik-stages` there. That is probably what an operator wants in an interactive session, but it is undocumented.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Upgrading never changes which skill a stage runs (Priority: P1)

An operator has a project plugin (a configured extra plugin, or `local-workflows`) that provides its own `fabrik-review` skill, and a stage file that still references the bare name `fabrik-review`. Today the new binary runs the project's skill for that stage, but `fabrik upgrade` rewrites the reference to `fabrik-stages:review` and the stage silently starts running the base skill. After this change the upgrade leaves the stage running the same skill it ran before.

**Why this priority**: It is a silent behaviour change on upgrade — the operator gets no error, just different stage behaviour — and it is the only functional defect in the issue.

**Independent Test**: Deploy a project plugin providing `fabrik-review` and a stage using bare `fabrik-review`; record the skill the resolver picks; run `fabrik upgrade`; record it again; they are the same skill.

**Acceptance Scenarios**:

1. **Given** a loaded non-base plugin providing `fabrik-review` and a stage with bare `fabrik-review`, **When** the operator runs `fabrik upgrade`, **Then** the stage reference is qualified to that plugin (`<plugin>:fabrik-review`) and the stage runs the project's skill before and after.
2. **Given** no loaded non-base plugin provides `fabrik-review`, **When** the operator runs `fabrik upgrade`, **Then** the reference becomes `fabrik-stages:review`, as today.
3. **Given** a reference was qualified to a non-base plugin, **When** the upgrade report is printed, **Then** it names that reference, the stage file and key, and the plugin it was qualified to.
4. **Given** the upgrade has already run, **When** it runs again, **Then** nothing further changes.

---

### User Story 2 - Operators know which sessions load the PM plugin (Priority: P2)

An operator with the `fabrik@fabrik` PM plugin installed reads the user guide to learn where it is and is not active.

**Why this priority**: Documentation only; no behaviour changes, but the current behaviour is surprising without it.

**Independent Test**: Read the user guide; it states that stage workers never load the PM plugin and that `fabrik resume`, TUI launches and watch sessions do when it is installed.

**Acceptance Scenarios**:

1. **Given** the user guide, **When** an operator looks for plugin loading behaviour, **Then** it says stage workers always disable `fabrik@fabrik` and interactive sessions (`fabrik resume`, TUI launches, watch) load it when the user has it installed.

---

### Edge Cases

- **Several non-base providers** of the same `fabrik-X`: the reference is qualified to the one the runtime resolver would choose (its load order), and the report names the others as the resolver does for ambiguous bare names.
- **Base-plugin copies are not project plugins.** The base plugin under its pre-rename name (`fabrik`), the renamed `fabrik-stages`, and the `local-workflows` copy the migration itself creates by moving an edited base plugin all carry `fabrik-X` skill names but originate from the base plugin. They stay governed by the existing rule (unedited skill → `fabrik-stages:X`; operator-edited skill → `local-workflows:…`), so unedited skills are not redirected to a moved copy.
- **An operator's own `local-workflows`** (a plugin not derived from the base plugin) that provides `fabrik-X` counts as a project plugin.
- **Already-qualified references** are untouched, as today.
- **Bare names that are not one of the twelve shipped skills** keep today's handling (left bare with a note), unless a loaded non-base plugin provides them, in which case the runtime would already resolve them there.
- **Plugin set unknown or unreadable at upgrade time** (an extra plugin directory missing): fall back to today's behaviour and say so in the report rather than failing the upgrade.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The migration resolves a bare `fabrik-X` reference the same way the runtime resolver does for the same deployment: if a loaded non-base plugin (e.g. `local-workflows` or a configured extra plugin) provides `fabrik-X`, the reference is qualified to that plugin; otherwise to `fabrik-stages:X`. The report names any reference qualified to a non-base plugin.
- **FR-002**: A test with a project plugin providing `fabrik-review` and a stage using bare `fabrik-review` asserts that the stage runs the same skill before and after `fabrik upgrade`. Shown non-vacuous by restoring the always-base rewrite.
- **FR-003**: ADR 2140 is amended (or a note added) to record FR-001's rule.
- **FR-004**: The USER_GUIDE states that stage workers never load the `fabrik@fabrik` PM plugin, while interactive sessions (`fabrik resume`, TUI launches, watch) do when the user has it installed.
- **FR-005**: For FR-001, "non-base plugin" excludes the base plugin under either of its names and the `local-workflows` copy produced by the migration from an edited base plugin; references to base skills continue to follow the existing edited/unedited rule, so no previously correct outcome changes.
- **FR-006**: When several loaded non-base plugins provide the same `fabrik-X`, the migration picks the same one the runtime resolver picks for that reference.
- **FR-007**: The rewrite is idempotent: a second `fabrik upgrade` makes no further change, and `docs/llms-full.txt` is regenerated if a canonical doc page is changed.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For any deployment, a stage's resolved skill is identical before and after `fabrik upgrade`.
- **SC-002**: Existing `TestMigrate_*` tests pass unchanged.
- **SC-003**: The new FR-002 test fails when the always-base rewrite is restored and passes with the fix.

## Assumptions

- "The same deployment" means the plugin set the engine would load: the base plugin, `local-workflows` if present, and plugins from `plugin_dirs` / `--plugin-dirs` / `FABRIK_PLUGIN_DIRS`. The migration is expected to be able to see that set; how is a Plan-stage decision.
- SC-001 compares the resolved skill's provider and name, not the qualified string: an unedited base skill resolving to the base plugin before and to `fabrik-stages:X` after counts as identical.
- Qualifying to the non-base plugin (rather than leaving the reference bare) is the desired outcome, because bare names are deprecated and the qualified form is unambiguous.
- Interactive sessions loading the PM plugin is intended behaviour; this issue documents it and does not change it.

## Out of Scope *(optional)*

- The live proof that `--settings enabledPlugins` keeps the PM plugin out of workers on a host where `fabrik@fabrik` is installed and enabled (SC-001 of #2140). That is a release-cut check, not code.
- Changing the runtime resolver, the legacy mapping table, or what interactive launches pass to Claude Code.
- Rewriting references to skills that are not bare `fabrik-X` names.

## Source References *(optional)*

- Follow-up to #2140 / PR #2202.
- ADR 2140 (decision 4 records the current always-base rewrite); ADR 2137 (resolver); ADR 2139 (migration).
