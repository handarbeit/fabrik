# Feature Specification: Workers load several workflow plugins at once

**Feature Branch**: `fabrik/issue-2136`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Workers should be able to load Fabrik's own stage plugin and a project's own workflow plugin together, instead of a configured plugin replacing Fabrik's."

## Background

A stage worker receives exactly one plugin directory today. When a user configures their own (`--plugin-dir` / `FABRIK_PLUGIN_DIR`), it *replaces* Fabrik's deployed plugin rather than adding to it, so customising one stage means vendoring every stage skill and keeping them in step with the binary. Claude Code itself accepts any number of plugin directories, so the limitation is Fabrik's own.

This is the first link of a chain that formalises how stage skills are named and where they come from: fully-qualified skill references, automatic conversion of customised plugins into a separate `local-workflows` plugin, and renaming Fabrik's base stage plugin so the `fabrik` plugin name can belong to the human-facing PM plugin. Every later link needs workers to load more than one plugin.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A project plugin adds to Fabrik's stages instead of replacing them (Priority: P1)

An operator has a project-specific workflow plugin. They configure it, and every worker receives both it and Fabrik's own stage plugin; stages using Fabrik's skills keep working and stages pointing at the project plugin use it.

**Why this priority**: Without it no project can carry its own stage skill without forking all of Fabrik's.

**Independent Test**: Configure one extra plugin directory; inspect the worker's command line for both directories, and run a stage from each.

**Acceptance Scenarios**:

1. **Given** an extra plugin directory is configured, **When** a worker starts, **Then** it receives Fabrik's plugin directory and the extra one.
2. **Given** no extra plugin is configured, **When** a worker starts, **Then** it receives exactly Fabrik's plugin directory, as today.

---

### User Story 2 - The development override keeps its meaning (Priority: P2)

A Fabrik developer pointing the engine at a working copy of the stage plugin keeps doing so without the deployed copy also loading.

**Why this priority**: The existing flag is in daily development use; changing it silently would be a hard-to-spot behaviour change.

**Independent Test**: Use the existing override and confirm only the overridden base plugin plus any configured extra plugins load.

**Acceptance Scenarios**:

1. **Given** the existing override is set, **When** a worker starts, **Then** the override replaces the *base* plugin only, and configured extra plugins are still added.

### Edge Cases

- A configured extra plugin directory does not exist or has no manifest: startup says so by name; it is never silently dropped.
- Two loaded plugins declare the same manifest name: refused at startup with both paths named, rather than left to Claude Code's load order.
- An extra plugin is configured in a place the engine does not read: never accepted silently (the known trap of keys documented as `config.yaml` keys but read only from CLI/env).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The engine accepts an ordered list of additional workflow plugin directories and passes each to every worker, after Fabrik's base plugin.
- **FR-002**: Fabrik's base plugin is always loaded unless replaced by the existing development override; adding plugins never removes it.
- **FR-003**: With no additional plugins configured, the worker's arguments are unchanged from today.
- **FR-004**: The existing override keeps "replace the base plugin" semantics, documented as such.
- **FR-005**: The additional-plugin setting is honoured wherever it is documented (flag, environment, config file), and an unrecognised key is reported, not ignored.
- **FR-006**: Startup refuses two loaded plugins with the same manifest name, naming both.
- **FR-007**: A missing or malformed additional plugin directory is reported at startup by path.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test asserts the worker argument list contains one plugin directory per configured plugin, in the defined order — asserted on arguments, not on a successful run.
- **SC-002**: With a project plugin configured, all twelve of Fabrik's stage and comment skills still resolve.
- **SC-003**: Zero change to worker arguments for a configuration without extra plugins.

## Assumptions

- The project's own plugins live under the Fabrik directory (the later conversion step uses `.fabrik/plugins/local-workflows/`); the setting is not limited to that location.
- Order matters only for diagnostics, since same-name plugins are refused.

## Out of Scope *(optional)*

- How stage YAML names a skill in a particular plugin (the next issue in the chain).
- Converting existing customisations (a later issue).

## Source References *(optional)*

- `engine/claude.go` — `claudePluginDir` is a single string, emitted once as `--plugin-dir` in `buildClaudeArgs`.
- `engine/engine.go` (`PluginDir` resolution), `cmd/root.go` (`--plugin-dir` flag, `FABRIK_PLUGIN_DIR`).
- `docs/USER_GUIDE.md` — documents `--plugin-dir` and `FABRIK_PLUGIN_DIR` (the latter explicitly with no `config.yaml` key); the user guide will need the new setting and the "replaces the base plugin" wording.
- Claude Code docs: `--plugin-dir` is repeatable; name-conflict rules at https://code.claude.com/docs/en/plugins/loading#name-conflicts
- Supersedes #1417. Mentioned for context: #969, #1373.
