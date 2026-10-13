# Feature Specification: Rename Fabrik's base stage plugin to `fabrik-stages` and free the `fabrik` plugin name

**Feature Branch**: `fabrik/issue-2140`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Rename our base plugin, shorten the name, and upgrade any configs that are still default to use the proper qualified name. Only then do we claim the fabrik@fabrik namespace for the PM plugin."

## Background

Fabrik ships two plugins and both declare the plugin name `fabrik`: the stage plugin the engine deploys to workers, and the human-facing PM plugin published as `fabrik@fabrik` in the repository's marketplace. Claude Code namespaces skills by plugin name, so both claim `fabrik:`.

Claude Code documents what happens: a plugin loaded with `--plugin-dir` silently replaces an installed marketplace plugin of the same name. So in workers the stage plugin always wins and the PM plugin is hidden — which has also been keeping the PM plugin out of workers by accident. Once the names differ, workers would load the PM plugin from the user's own settings, including any MCP server it bundles, unless the engine excludes it.

The `fabrik` name is wanted for the human-facing plugin. With fully-qualified skill references and the `fabrik upgrade` migration in place (earlier in this chain), the stage plugin can be renamed safely.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The stage plugin has its own short name (Priority: P1)

After upgrading, stage skills are `fabrik-stages:specify`, `fabrik-stages:specify-comment`, …, `fabrik-stages:validate-comment`, and default stage files reference them.

**Why this priority**: Frees `fabrik:` and gives stages a clean, unambiguous address.

**Independent Test**: Fresh `fabrik init`; every stage runs with the new names.

**Acceptance Scenarios**:

1. **Given** a fresh install, **When** stage files are generated, **Then** they reference `fabrik-stages:<stage>` names and every stage runs.

---

### User Story 2 - Existing deployments move over through `fabrik upgrade` (Priority: P1)

An operator's stage files still reference `fabrik:fabrik-review` (or bare `fabrik-review`). `fabrik upgrade` rewrites those to `fabrik-stages:review`; references to `local-workflows` and other plugins are untouched.

**Why this priority**: Configuration must follow the rename without hand-editing.

**Independent Test**: Fixtures from earlier releases, default and customised, migrate to the new names in one run.

**Acceptance Scenarios**:

1. **Given** references to the old base plugin name, **When** `fabrik upgrade` runs, **Then** they become `fabrik-stages:` references and nothing else changes.
2. **Given** the binary was upgraded but `fabrik upgrade` not yet run, **When** stages dispatch, **Then** old-form references still resolve to the renamed skills, with the deprecation notice in the warnings panel.

---

### User Story 3 - The PM plugin never runs inside a worker (Priority: P1)

A user with `fabrik@fabrik` installed runs Fabrik. Workers do not load it — not its skills, not its MCP server.

**Why this priority**: Without it, the rename turns the hidden PM plugin into a live one in every worker.

**Independent Test**: With `fabrik@fabrik` installed for the user, inspect a worker's loaded plugins and skills.

**Acceptance Scenarios**:

1. **Given** `fabrik@fabrik` is installed and enabled for the user, **When** a worker runs, **Then** it is not loaded, and the stage plugins (`fabrik-stages`, `local-workflows`) are.

### Edge Cases

- Old customised copies converted by the previous issue keep the name `local-workflows`; they never take the old `fabrik` name.
- A deployment that skipped every release in this chain reaches the final layout with one `fabrik upgrade`.
- Operator documentation, examples and the stage-config reference that mention the old skill names are updated.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The stage plugin's name is `fabrik-stages` and its skills drop the `fabrik-` prefix (`specify` … `validate`, `*-comment`).
- **FR-002**: Default stage files reference `fabrik-stages:<skill>`.
- **FR-003**: `fabrik upgrade` rewrites old-name references (qualified `fabrik:fabrik-*` and bare `fabrik-*`) to the new names; other plugins' references are untouched; idempotent and skip-safe as in the previous issue.
- **FR-004**: Until migrated, old-name references resolve to the renamed skills (one mapping), reported as deprecated.
- **FR-005**: Every worker invocation explicitly disables the `fabrik@fabrik` marketplace plugin, without affecting the stage plugins.
- **FR-006**: A test fails if Fabrik's stage plugin and the PM plugin ever declare the same plugin name again.
- **FR-007**: Release notes state the rename, the `fabrik upgrade` step and what it changes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `fabrik@fabrik` installed for the user **and** the stage plugins loaded, every stage skill resolves and the PM plugin is absent from the worker — demonstrated in that combined environment, not with the stage plugins alone.
- **SC-002**: Fixtures from at least three earlier releases reach the new names in one `fabrik upgrade`.
- **SC-003**: Zero stages fail to resolve on a deployment that has upgraded the binary but not yet run `fabrik upgrade`.

## Assumptions

- Disabling a marketplace plugin for one invocation does not disable a `--plugin-dir` plugin, which Claude Code identifies separately (`<name>@inline`) — confirm live.
- The chain ships in one release; the old-name mapping stays until a later decision removes bare/old forms.

## Out of Scope *(optional)*

- Changes to the PM plugin itself (next issue).

## Source References *(optional)*

- `plugin/fabrik/.claude-plugin/plugin.json`, `plugin/fabrik-workflows/.claude-plugin/plugin.json` — both `"name": "fabrik"`.
- `stages/examples/*.yaml`, `plugin/embed.go`.
- Claude Code docs: https://code.claude.com/docs/en/plugins/loading#name-conflicts ; per-invocation `--settings` sits above user settings.
- Supersedes #1405.
