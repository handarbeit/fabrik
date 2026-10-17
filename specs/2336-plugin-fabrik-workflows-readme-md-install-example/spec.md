# Feature Specification: Correct stale paths and plugin names in the workflow plugin README

**Feature Branch**: `fabrik/issue-2336`
**Created**: 2026-10-17
**Status**: Specified
**Input**: User description: "The workflow plugin's README (`plugin/fabrik-workflows/README.md:23`) shows:

```
claude --plugin-dir /path/to/fabrik/plugin/fabrik
```

`plugin/fabrik` is the human-facing PM plugin (ADR 2141). The workflow plugin, named `fabrik-stages` since ADR 2140, lives in `plugin/fabrik-workflows`. The README ships into every project's `.fabrik/plugin/README.md`. Pruefer flagged it on shadoworg/liminis-models#2267.

## Acceptance
- The example names `plugin/fabrik-workflows`.
- Any other stale path or plugin name in that README is corrected in the same change.
- `plugin/shipped_files.json` gets the new content's entry through the normal release step (no hand edit).

Small docs fix; yolo-sized."

## Background

Fabrik ships two Claude Code plugins that are easy to confuse:

- `plugin/fabrik` is the human-facing PM plugin (ADR 2141).
- `plugin/fabrik-workflows` is the stage-workflow plugin. It has been named `fabrik-stages` since ADR 2140, and its skills are referenced as `fabrik-stages:<skill>`.

The workflow plugin's README is embedded in the binary and copied into every project's `.fabrik/plugin/README.md`. Its local-development example points `--plugin-dir` at `plugin/fabrik`. A reader who follows it loads the PM plugin instead of the stage skills. Pruefer flagged this while reviewing shadoworg/liminis-models#2267.

A read of the README against the current tree found other statements that are stale or inaccurate in the same way:

- The title is "Fabrik Plugin for Claude Code", which does not say which plugin this is.
- It says the plugin provides "Six skills, one per pipeline stage". The plugin has twelve: six stage skills and six `*-comment` skills.
- It says Claude "auto-loads these skills from `~/.claude/skills/`". The engine instead passes the plugin to each worker with `--plugin-dir`.
- Its "For regular use" section says to "add to a Fabrik plugin marketplace". The repository's marketplace lists only the PM plugin, not `fabrik-stages`.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Local-testing example loads the right plugin (Priority: P1)

A contributor or operator reads the README and runs the `--plugin-dir` example to try the stage skills locally.

**Why this priority**: This is the reported defect, and the README's own acceptance criterion. Following the example today loads the wrong plugin.

**Independent Test**: Copy the example command from the README and run it from a checkout. Confirm the session lists the `fabrik-stages:*` skills and not the PM plugin's skills.

**Acceptance Scenarios**:

1. **Given** the README's local-development example, **When** its path is compared with the repository layout, **Then** it names `plugin/fabrik-workflows`.
2. **Given** a session started with that command, **When** the available skills are listed, **Then** `fabrik-stages:specify` and the other stage skills are present.

---

### User Story 2 - The rest of the README matches the shipped plugin (Priority: P2)

A reader of the README, including the copy in a project's `.fabrik/plugin/README.md`, should find nothing in it that contradicts the current plugin.

**Why this priority**: The issue requires correcting any other stale path or plugin name in the same change, so the file is not left half-fixed.

**Independent Test**: Check each path, plugin name, skill name and skill count in the README against `plugin/fabrik-workflows/.claude-plugin/plugin.json`, its `skills/` directory and the engine's plugin loading.

**Acceptance Scenarios**:

1. **Given** the skills table and the stated skill count, **When** compared with the `skills/` directory, **Then** the count is correct and every listed skill reference resolves.
2. **Given** the description of how the engine makes the skills available, **When** compared with the engine's plugin loading, **Then** it describes `--plugin-dir` loading and not `~/.claude/skills/`.
3. **Given** the installation guidance, **When** compared with the repository's marketplace, **Then** it does not claim an install route that does not exist.
4. **Given** the README's name for the plugin, **When** compared with the plugin manifest, **Then** it uses `fabrik-stages` and does not blur it with the PM plugin.

---

### User Story 3 - The shipped-files table picks up the new README (Priority: P3)

The release maintainer cuts a release and the changed README is recorded in the shipped-files table.

**Why this priority**: The table is what lets `fabrik upgrade` recognise an unedited README. Updating it is a release step, not part of this change.

**Independent Test**: This change leaves `plugin/shipped_files.json` untouched. The next release run produces the new entry through the existing `-add-current` step.

**Acceptance Scenarios**:

1. **Given** the change set, **When** its files are listed, **Then** `plugin/shipped_files.json` is not among them.
2. **Given** the repository's tests run, **Then** none of them fail because the README differs from the table's recorded hashes.

---

### Edge Cases

- Other files may carry the same wrong path. Only the workflow plugin README is in scope; any found elsewhere become follow-ups.
- `LABELS.md`, which the README links to, is unchanged.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The README's local-development example names `plugin/fabrik-workflows` as the `--plugin-dir` target.
- **FR-002**: The README's title and introduction identify the plugin as the workflow plugin, `fabrik-stages`, so it is not mistaken for the PM plugin `fabrik`.
- **FR-003**: The README states the number of skills the plugin provides correctly, and its skills table does not contradict the contents of the plugin's `skills/` directory.
- **FR-004**: The README describes how the engine makes the skills available (`--plugin-dir`) and no longer says they are auto-loaded from `~/.claude/skills/`.
- **FR-005**: The README's installation guidance does not tell readers to install `fabrik-stages` from a marketplace that does not list it. It states only routes that exist today.
- **FR-006**: Every other file path, plugin name or skill reference in the README is checked against the current repository and corrected if stale.
- **FR-007**: `plugin/shipped_files.json` is not hand-edited in this change.
- **FR-008**: The README's existing links keep working: `LABELS.md` and the two external documentation links.

### Key Entities

- **Workflow plugin (`fabrik-stages`)**: lives in `plugin/fabrik-workflows`, carries the stage and comment skills, and is loaded into workers by the engine.
- **PM plugin (`fabrik`)**: lives in `plugin/fabrik` and is meant for a human's interactive session. It is not the subject of this README.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Searching the README for `plugin/fabrik` finds only `plugin/fabrik-workflows`, with no reference to the PM plugin directory.
- **SC-002**: Running the README's local-development command loads the `fabrik-stages` skills in 100% of attempts.
- **SC-003**: Every plugin name, skill name, count and path stated in the README matches the repository, as checked in one review pass.
- **SC-004**: The change set touches no file other than the README (and the regenerated docs bundle, if one is required), and the full test suite passes.

## Assumptions

- "Other stale path or plugin name" includes factual inaccuracies about how the plugin is loaded and how many skills it has, since these are the same kind of stale-name problem in the same file.
- Where the "regular use" wording points to a marketplace route that does not exist, the minimal fix is to remove or reword it, not to add a marketplace entry.
- The README's per-stage skill table need not list the six `*-comment` skills individually. It must not call the plugin's total six, and a short statement that each stage skill has a `-comment` counterpart is enough.
- This README is not one of the canonical doc pages, so `docs/llms-full.txt` needs no regeneration. This should be confirmed when the change is made.

## Out of Scope

- Editing `plugin/shipped_files.json` by hand. The release step produces its entry.
- Changes to the PM plugin's README, to `LABELS.md` or to any skill.
- Adding `fabrik-stages` to the marketplace or defining a new install route.
- Auditing other docs for the same wrong path. Any found should be filed as separate issues.
- Bumping the plugin version in `plugin.json`.
