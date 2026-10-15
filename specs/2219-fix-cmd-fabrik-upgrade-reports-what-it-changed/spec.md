# Feature Specification: `fabrik upgrade` reports what it changed, not what it wrote

**Feature Branch**: `fabrik/issue-2219`
**Created**: 2026-10-13
**Status**: Specified
**Input**: Seen while testing the 0.0.84 workflow migration on an existing project.

## Background

Running `fabrik upgrade` on an already-current deployment prints `fabrik upgrade: refreshed 15 plugin file(s)` (`cmd/upgrade.go`) although no file changed: a second run after the migration left `git status` clean. `RefreshPlugin`/`RefreshPluginTo` (`plugin/refresh.go`) count files written, not files whose content changed. The startup auto-refresh line (`fabrik: auto-refreshed %d plugin file(s)`) shares the count.

The effect is that an operator cannot tell a real refresh from a no-op. A message claiming 15 files were refreshed on an up-to-date install invites needless investigation, and rewriting identical files also bumps their mtimes for no reason, which can disturb file watchers and build caches.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Upgrade on a current install says nothing changed (Priority: P1)

An operator runs `fabrik upgrade` on a project whose `.fabrik/plugin` already matches the binary's embedded plugin. The command reports that the plugin is already up to date instead of claiming files were refreshed.

**Why this priority**: This is the reported defect, and the message is what the operator reads.

**Independent Test**: Run `fabrik upgrade` twice in a temp project; the second run prints `fabrik upgrade: plugin already up to date`.

**Acceptance Scenarios**:

1. **Given** a plugin directory that exactly matches the embedded plugin, **When** `fabrik upgrade` runs, **Then** it prints `fabrik upgrade: plugin already up to date` and no file's content or mtime changes.
2. **Given** a plugin directory that exactly matches the embedded plugin but a missing or stale `.installed-version`, **When** `fabrik upgrade` runs, **Then** `.installed-version` is written and the plugin files are untouched.

---

### User Story 2 - A real refresh reports the true count (Priority: P2)

When some plugin files differ from the embed, are missing, or are obsolete, the reported count equals the number of files created, changed or pruned, not the total embed size. Only the affected files are written.

**Why this priority**: The count is only trustworthy if it is exact in the non-trivial case as well.

**Independent Test**: Delete one file, edit another and add an obsolete shipped file; the refresh reports 3 and rewrites only those.

**Acceptance Scenarios**:

1. **Given** an empty target directory, **When** the refresh runs, **Then** it returns the number of files in the embed.
2. **Given** one file with altered content, **When** the refresh runs, **Then** it returns 1 and only that file is rewritten.

---

### User Story 3 - Other callers report the new count (Priority: P3)

The startup auto-refresh message and the workflow migration's "restored … from the binary (N file(s))" note use the same corrected count.

**Why this priority**: Consistency across every place that surfaces the number.

**Independent Test**: Trigger each caller on a current install and on a drifted install and compare the printed counts with the files that actually changed.

**Acceptance Scenarios**:

1. **Given** a migration that restores a clean base over an identical plugin, **When** it completes, **Then** the note reports the number of files actually changed, which may be 0.

---

### Edge Cases

- The target file exists with identical content but different permissions or mtime: treated as unchanged and not rewritten.
- The target file is missing: counted as created.
- A shipped-identical file that the embed no longer carries is pruned: counted as pruned (existing pruning behaviour is kept).
- `.installed-version` is written even when the plugin files are unchanged, and does not count toward the file count.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `RefreshPluginTo` returns the number of files created, changed or pruned, and does not rewrite a file whose content already matches.
- **FR-002**: With nothing to change, `fabrik upgrade` prints `fabrik upgrade: plugin already up to date` (and still writes `.installed-version` if it is missing or stale).
- **FR-003**: Callers that relied on the old count (startup auto-refresh, the migration's "restored … from the binary (N file(s))") report the new count.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test runs the refresh twice on a temp directory: the first run returns the embed's file count, the second returns 0 and leaves every file's mtime unchanged. Shown non-vacuous by restoring the unconditional write.

## Assumptions

- "Content already matches" means byte-identical file content; mode and mtime are not compared.
- The `fabrik upgrade` messages for the customization-refusal and migration paths are unchanged apart from the corrected count.
- The existing prune behaviour for shipped-identical files the embed no longer carries is retained and now counts toward the total.

## Out of Scope

- Changing which files are refreshed, pruned or migrated, or the customization detection logic.
- Changing the wording of other `fabrik upgrade` messages.
- Per-file listing of what changed.
- Documentation updates beyond what the changed message requires.
