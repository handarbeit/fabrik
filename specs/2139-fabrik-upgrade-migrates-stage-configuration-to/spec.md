# Feature Specification: `fabrik upgrade` migrates stage configuration to qualified skill names and moves customisations into a `local-workflows` plugin

**Feature Branch**: `fabrik/issue-2139`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Start the rollout by detecting customised workflows and automatically converting them into local workflow plugins — moving them aside in the namespace — and rewrite the config files to adapt to the namespace qualification. fabrik upgrade is the right place. A skipped upgrade must not break this migration pathway. local-workflows is acceptable, let's not get too fancy."

## Background

Most deployments have never edited the stage plugin. Those that did edited Fabrik's deployed copy (`.fabrik/plugin/`) in place, which has always been the way to customise a stage. Fabrik protects those edits — `fabrik upgrade` refuses to overwrite them, and startup warns — but that also means a customised deployment never receives plugin updates again, and its edits share Fabrik's own plugin name.

With fully-qualified skill names and several plugins per worker (earlier in this chain), customisations can live in their own plugin. This issue moves them there and rewrites stage configuration to fully-qualified references, so that the next issue can rename Fabrik's base plugin and the `fabrik` name can later go to the human-facing PM plugin.

The binary and the configuration change at different times: auto-upgrade replaces the binary and restarts it without running `fabrik upgrade`, and an operator may skip several releases. The migration therefore works from what it finds on disk, never from version numbers, and stays in the binary for later releases.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An uncustomised deployment migrates with no decisions (Priority: P1)

The operator runs `fabrik upgrade`. The stage files' bare skill names become fully-qualified references to Fabrik's base plugin; the command prints what changed. Behaviour is identical afterwards.

**Why this priority**: The common case; it must be effortless.

**Independent Test**: Run upgrade on a default deployment; diff the stage files; run every stage.

**Acceptance Scenarios**:

1. **Given** default stage files with bare names, **When** `fabrik upgrade` runs, **Then** every skill reference is qualified, the changes are listed, and a second run changes nothing.

---

### User Story 2 - A customised deployment keeps its customisations and gets upgrades again (Priority: P1)

The operator edited two stage skills in `.fabrik/plugin/`. `fabrik upgrade` moves the customised plugin into `.fabrik/plugins/local-workflows/` under the plugin name `local-workflows`, points the two edited stages at `local-workflows:<skill>`, points the untouched stages at Fabrik's base plugin, and restores a clean base plugin. Edited stages behave exactly as before; untouched stages receive future updates.

**Why this priority**: The customising operators are the ones the old behaviour stranded.

**Independent Test**: Fixture deployment with known edits; after upgrade, edited skills are byte-identical in `local-workflows`, YAML points at them, the base plugin matches the embedded one, every stage runs.

**Acceptance Scenarios**:

1. **Given** a customised `.fabrik/plugin/`, **When** `fabrik upgrade` runs, **Then** the edits are preserved in `local-workflows`, only stages whose skill differs from a shipped version reference it, and the base plugin is refreshed.
2. **Given** the conversion, **When** it completes, **Then** the operator sees exactly what moved and what was rewritten, and is told to review and commit it.

---

### User Story 3 - Skipped releases still land correctly (Priority: P1)

An operator jumps from a release several versions old straight to the latest and runs `fabrik upgrade`; the result is the same as if they had upgraded through every release.

**Why this priority**: Idle auto-upgrade and infrequent operators make skipped releases normal.

**Independent Test**: Fixtures captured from several earlier releases (default and customised); each migrates to the same end state.

**Acceptance Scenarios**:

1. **Given** a deployment from any earlier release, **When** `fabrik upgrade` runs, **Then** it reaches the current layout in one run, and running it again changes nothing.

### Edge Cases

- A plugin copy that matches *some* earlier shipped version, not the current one: not a customisation; refreshed as today.
- An edited shared file (label reference, skill references) in the customised copy: kept in `local-workflows` alongside the edited skills that use it.
- Stage YAML the operator already edited by hand (custom prompts, already-qualified references): only skill references are rewritten; nothing else in the file changes, and a reference that is already qualified is left alone.
- A vendored plugin set through the development override: reported with instructions, not moved.
- `local-workflows` already exists: never overwritten; the operator is told.
- Interrupted mid-run: re-running completes it; nothing is half-moved without being reported.
- Until the operator runs it, the engine keeps working on the old layout (previous issues) and the warnings panel says a migration is pending.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `fabrik upgrade` rewrites every skill-valued reference in the stage files to its fully-qualified form, changing nothing else in those files.
- **FR-002**: It detects a customised deployed plugin by comparing it with every shipped version (not just the current one), and moves it into a separate plugin named `local-workflows` under the Fabrik directory.
- **FR-003**: Stages whose skill differs from every shipped version are pointed at `local-workflows`; all others at Fabrik's base plugin; the base plugin is restored from the binary.
- **FR-004**: The migration decides only from what is on disk, is idempotent, and reaches the same end state from any earlier release.
- **FR-005**: It lists every file moved or rewritten and tells the operator to review and commit; it never commits to the user's repository itself.
- **FR-006**: The daemon never performs this migration at startup; until it is run, startup records a "migration pending" entry in the warnings panel.
- **FR-007**: The engine loads `local-workflows` automatically when it exists, as an additional plugin.
- **FR-008**: The migration code is kept in future releases; the User Guide documents it, including how to undo it with git.
- **FR-009**: The moved plugin's manifest declares the name `local-workflows` (not Fabrik's base plugin name), so the two plugins can be loaded together without a name clash; the skill files themselves are moved byte-for-byte.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For fixtures from at least three earlier releases (default and customised), one run reaches the current layout and a second run is a no-op.
- **SC-002**: Every customised skill is byte-identical before and after; every stage runs after migration.
- **SC-003**: A customised deployment receives base-plugin updates after migration (its upgrade no longer refuses).

## Assumptions

- The record of every shipped plugin fingerprint already kept by the binary is sufficient to tell "old shipped version" from "customised".
- Moving the whole customised tree, and pointing only the edited stages at it, is preferred over moving individual skills, so shared files stay with the skills that use them. Unedited skills that travel with the tree are simply left unreferenced.
- The engine can already load several plugins per worker (an additional-plugin mechanism exists from earlier in this chain), so loading `local-workflows` needs no new loading capability, only the automatic discovery in FR-007.

## Out of Scope *(optional)*

- Renaming Fabrik's base plugin (next issue).
- Removing bare-name support.

## Source References *(optional)*

- `plugin/refresh.go` (`CheckPluginState`, three-way comparison), `plugin/known_embedded_versions.go` (`KnownEmbeddedVersions`), `cmd/upgrade.go` (`--force`, `--reconcile`).
- `stages/drift.go`, `cmd/refresh_stages.go` — existing stage-file rewriting precedent.
- Mentioned for context: #969 (project-owned stage skills; `local-workflows` committed to the repo may answer it).
