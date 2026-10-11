# Feature Specification: Stage skills are addressed by fully-qualified name

**Feature Branch**: `fabrik/issue-2137`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Formalise a fully-qualified plugin:skill name for stage skills, deprecate the bare name, and resolve references against the plugins actually loaded, so custom workflow plugins are supported and resolution no longer depends on the model guessing a namespace."

## Background

Stage YAML names skills bare (`skill: fabrik-review`) and the engine passes that bare name to the model ("Follow the instructions in the fabrik-review skill"), while Claude Code registers plugin skills as `<plugin>:<skill>`. It works only because Claude models map the bare name onto the namespaced entry by themselves — across 2,144 observed calls they always used `fabrik:fabrik-<stage>`. A non-Claude model behind LiteLLM did not make that leap and every stage skill failed with "Unknown skill" (#1373, confirmed by the reporter). A bare name also cannot say *which* plugin, so there is no way to point a stage at a project's own plugin.

Deployments in the field differ: a customised `.fabrik/plugin/` is never auto-refreshed, so the plugin actually on disk may not be the one embedded in the running binary. Auto-upgrade replaces the binary without touching stage YAML, so a new binary can run for weeks against old configuration. Resolution must therefore work from what is loaded, not from what the binary expects.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - The model is told the exact registered name (Priority: P1)

Every stage prompt names the skill exactly as Claude Code registered it, so no inference is needed.

**Why this priority**: Removes the failure #1373 observed and is the basis for everything else.

**Independent Test**: Build the prompt for each stage and compare the skill name in it to the registered `plugin:skill` form.

**Acceptance Scenarios**:

1. **Given** `skill: fabrik:fabrik-review`, **When** the prompt is built, **Then** it contains `fabrik:fabrik-review`.
2. **Given** a bare `skill: fabrik-review`, **When** the prompt is built, **Then** it contains the qualified name of the loaded plugin that provides it.

---

### User Story 2 - A stage uses a project's own plugin (Priority: P1)

An operator writes `skill: local-workflows:review` (or any third-party plugin) and the stage runs that skill.

**Why this priority**: The point of qualification; turns custom workflow plugins into a supported feature.

**Independent Test**: A genuinely separate plugin (not a renamed copy of Fabrik's) loaded as an additional plugin; a stage pointing at it runs its skill. Must fail against `main` first.

**Acceptance Scenarios**:

1. **Given** an additional plugin `myorg` with skill `review`, **When** a stage says `skill: myorg:review`, **Then** the worker is told `myorg:review` and it loads.

---

### User Story 3 - Old configuration keeps working, with a clear nudge (Priority: P2)

A deployment upgraded without running `fabrik upgrade` still has bare names and possibly an old customised plugin. Stages keep running; startup says which references are bare and that `fabrik upgrade` will migrate them.

**Why this priority**: Auto-upgrade makes this the normal state right after release.

**Independent Test**: Start the new binary against a configuration from the previous release (bare names, customised plugin) and run every stage.

**Acceptance Scenarios**:

1. **Given** bare skill names, **When** the engine starts, **Then** it logs one deprecation notice listing them and pointing at `fabrik upgrade`.
2. **Given** a bare name that no loaded plugin provides, **When** the stage would dispatch, **Then** it does not run and the operator is told the name, the plugins searched and their paths.

### Edge Cases

- A bare name provided by more than one loaded plugin: resolved to Fabrik's base plugin, with a warning naming the others.
- A qualified name whose plugin is not loaded, or whose skill the plugin lacks: the stage does not dispatch; reported with plugin, skill and paths searched.
- Every skill-valued key is covered (`skill`, `comment_skill`, `ci_fix_skill`, `rebase_skill` and any others found).
- The stage-drift check and `refresh-stages` must not report a bare-vs-qualified difference that resolves to the same skill as drift.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every skill-valued stage key accepts `plugin:skill`.
- **FR-002**: The engine resolves each reference against the plugins actually loaded for the worker (their on-disk manifests and skills), never against the binary's embedded plugin or a constant.
- **FR-003**: The prompt carries the resolved, fully-qualified name.
- **FR-004**: A bare name is accepted, resolved per FR-002 (preferring Fabrik's base plugin), and reported as deprecated once per startup, in the log and the operator warnings panel.
- **FR-005**: A reference that does not resolve is caught before the worker is spawned; the stage is not run, no retry is consumed, and the operator is told the reference, the plugins searched and their paths — on the item and in the operator warnings panel. The failure shape (pause vs failed label) is decided and recorded in an ADR numbered after this issue.
- **FR-006**: The plugin namespace separator and Fabrik's base plugin name are each defined in one place.
- **FR-007**: User Guide and the stage-config reference document the qualified form, the deprecation, and how to point a stage at a project plugin.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every default stage, the emitted prompt contains the exact registered skill name — asserted on prompt text, since a successful run passes today through inference.
- **SC-002**: A stage pointing at a separate plugin loads its skill; the same test fails on current `main`.
- **SC-003**: A configuration from the previous release runs every stage unchanged on the new binary.
- **SC-004**: No unresolved reference ever reaches a worker.

## Assumptions

- Depends on workers loading several plugins and on operator-facing failures having a home in the warnings panel (both earlier in the chain).
- Bare-name support is kept for at least this release; removal is a later decision.

## Out of Scope *(optional)*

- Rewriting configuration and converting customisations (`fabrik upgrade` migration, next issue).
- Detecting a skill that is present on disk but fails to register at runtime (separate issue).
- Resolving skills from the project's `.claude/skills/` (#969, mentioned for context).

## Source References *(optional)*

- `engine/claude.go` — "Follow the instructions in the %s skill exactly." for `stage.Skill` and `stage.CommentSkill`.
- `stages/stages.go` — `Skill`, `CommentSkill`, `CIFixSkill`, `RebaseSkill`.
- `plugin/refresh.go` `CheckPluginState` — customised plugins are never auto-refreshed.
- #1373 (community report, mentioned for context): reporter confirmed it occurred only via LiteLLM → OpenRouter → a non-Claude model.
- Supersedes #1406 and the pre-flight half of #1407.
