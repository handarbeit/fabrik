# Feature Specification: Move the skill catalogue, resolver and load-failure detector out of engine/ into internal/skillref

**Feature Branch**: `fabrik/issue-2305`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "First of a chain that replaces #1859: rich, customizable review tooling for Pruefer, brought up to the level of Fabrik's custom-workflow support (#2137, #2138, #2139, #2140).

## Problem

Fabrik resolves `plugin:skill` references against a catalogue of the plugins it actually loads, and it detects a skill that fails to load. Pruefer needs both. All of that code lives in `engine/`, which Pruefer must not import (ADR-1113):

- **Catalogue:** `engine/skill_catalogue.go`, i.e. `buildSkillCatalogue`, `discoverPlugin` and `scanSkillsDir`.
- **Resolver:** `engine/skill_resolve.go`, i.e. `skillCatalogue.resolve`/`resolveAsIs`, `skillResolver` with its rebuild on a miss, and `skillUnresolvedError`/`describe`.
- **Load-failure detection:** `engine/skill_load_detect.go`, i.e. `detectSkillLoadFailure`, `skillNamesMatch` and `sanitizeSkillLoadDetail`.

## Requirements

- **R1:** Move the plugin-independent parts into a new leaf package, `internal/skillref`. That covers plugin discovery (manifest name, else directory basename; `skills/<d>/SKILL.md`; the manifest `skills` key; a root single-skill plugin), the catalogue, `plugin:skill` resolution with the rebuild on a miss, the unresolved error with its description of the plugins searched, and the stream-json load-failure detector. The package may import `plugin` (for `NamespaceSep`) and the standard library, nothing from `engine`.
- **R2:** The base-plugin preference for bare names and the `plugin.LegacyRef` deprecation lookup stay parameters of the resolver, not constants. Fabrik passes `fabrik-stages` and the legacy table; Pruefer will pass its own base plugin and no legacy table.
- **R3:** The engine keeps exactly its current behaviour. The engine-specific wrappers (`resolveStageSkills`, `stageSkillRefs`, `handleSkillUnresolved`, `handleSkillLoadFailed`, warnings, labels) stay in `engine/` and call the new package.
- **R4:** The moved code's tests move with it. `engine/skill_dispatch_test.go`, `skill_prompt_test.go`, `skill_load_failed_test.go` and the sim twins pass unmodified.

## Out of scope

- Any Pruefer change. Pruefer starts using the package in the next issue of this chain.

## Acceptance

- `go list -deps ./internal/skillref` contains no `engine` package.
- The engine's existing skill tests and sim scenarios pass with no assertion changes.
- `go test -race ./...` and `go vet ./...` pass."

## Background

Fabrik resolves `plugin:skill` references against a catalogue of the plugins it actually loads, and it detects a skill that fails to load at runtime (#2137, #2138). Pruefer, the review bot, needs the same two capabilities so its review tooling can be customized to the level Fabrik's own custom-workflow support already offers (#2137–#2140). This is the first issue of a chain replacing #1859.

Today the code that provides those capabilities sits in the `engine` package, which Pruefer must not import (ADR-1113). It cannot be reused until it lives in a leaf package both can depend on. The repository already does this for other shared concerns (`internal/claudeerr`, `internal/claudeprofile`, `internal/pathglob`, `internal/sessionreap`), so this is a pure extraction, not a behaviour change.

The code to move is in three engine files:

- Catalogue: plugin discovery and the catalogue built from the plugins actually loaded.
- Resolver: `plugin:skill` resolution, a rebuild on a miss, and the unresolved-reference error with its description of the plugins searched.
- Load-failure detection: the structural stream-json detector, the skill-name comparison and the detail sanitiser.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Shared skill-reference package exists and the engine uses it (Priority: P1)

A maintainer of Fabrik or Pruefer can import one leaf package to discover loaded plugins, resolve `plugin:skill` references and detect a skill that failed to load, without importing `engine`. The engine continues to do exactly what it did before, now by calling that package.

**Why this priority**: This is the entire deliverable; the rest of the chain depends on it.

**Independent Test**: Build the new package on its own and list its dependencies; run the engine's existing skill tests and sim scenarios unchanged.

**Acceptance Scenarios**:

1. **Given** the new package, **When** its dependency list is inspected, **Then** it contains no `engine` package and only the standard library and `plugin`.
2. **Given** the engine's existing skill dispatch, prompt and load-failure tests and the sim twins, **When** they run after the move, **Then** they pass with no assertion changes.
3. **Given** a stage referencing a skill, **When** it is dispatched, **Then** resolution, the unresolved did-not-run outcome and the load-failure pause behave exactly as before.

---

### User Story 2 - Base plugin and legacy handling are caller-supplied (Priority: P1)

A consumer other than Fabrik can use the resolver with its own base plugin and with no legacy-name table, while Fabrik reproduces its current bare-name and deprecated-name behaviour by passing `fabrik-stages` and the legacy table.

**Why this priority**: Pruefer's next issue depends on this; hardcoding Fabrik's values would defeat the extraction.

**Independent Test**: Resolve the same references through the package with Fabrik's parameters and with a different base plugin and no legacy table, and compare outcomes.

**Acceptance Scenarios**:

1. **Given** a resolver configured with base plugin `fabrik-stages` and the legacy table, **When** a bare or pre-rename name is resolved, **Then** the result and its deprecation flag match today's engine behaviour.
2. **Given** a resolver configured with a different base plugin and no legacy table, **When** a bare name is resolved, **Then** it prefers that base plugin and no legacy lookup is attempted.

---

### User Story 3 - Moved tests travel with the moved code (Priority: P2)

The unit tests that exercise the moved logic live in the new package, so the package is verified independently of the engine.

**Why this priority**: Keeps coverage with the code and keeps the engine's tests focused on its own wrappers.

**Independent Test**: Run the new package's tests alone.

**Acceptance Scenarios**:

1. **Given** the moved catalogue, resolver and detector tests, **When** run in the new package, **Then** they pass and assert the same behaviour as before.

---

### Edge Cases

- A plugin with no manifest name falls back to its directory basename.
- A plugin exposing skills via `skills/<d>/SKILL.md`, via the manifest `skills` key, or as a root single-skill plugin is discovered in each form.
- A reference that misses is retried once after rebuilding the catalogue before being reported unresolved.
- An empty catalogue passes names through unchanged.
- The load-failure detector stays structural only (never text-based), ignores sub-agent lines and is inert for skill-less stages.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A new leaf package `internal/skillref` provides plugin discovery (manifest name, else directory basename; `skills/<d>/SKILL.md`; the manifest `skills` key; a root single-skill plugin) and the catalogue built from it.
- **FR-002**: The package provides `plugin:skill` resolution, including the rebuild of the catalogue on a miss.
- **FR-003**: The package provides the unresolved-reference error and its description of the plugins searched.
- **FR-004**: The package provides the stream-json skill load-failure detector, including skill-name matching and detail sanitisation.
- **FR-005**: The package imports only the standard library and `plugin` (for the namespace separator); nothing from `engine`.
- **FR-006**: The base-plugin preference for bare names and the legacy-reference lookup are parameters of the resolver, not constants; Fabrik supplies `fabrik-stages` and the legacy table, and a consumer may supply a different base plugin and no legacy table.
- **FR-007**: The engine's observable behaviour is unchanged: engine-specific wrappers (stage skill-reference collection, qualification of stage copies, the unresolved and load-failed handlers, warnings and labels) remain in `engine/` and call the new package.
- **FR-008**: Tests of the moved code move into the new package; the engine's skill dispatch, prompt and load-failed tests and the sim twins pass without any assertion changes.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: `go list -deps ./internal/skillref` lists no `engine` package.
- **SC-002**: The engine's existing skill tests and sim scenarios pass with no assertion changes.
- **SC-003**: `go test -race ./...` and `go vet ./...` pass for the whole module.
- **SC-004**: The new package's own tests pass when run in isolation.

## Assumptions

- This is a behaviour-preserving extraction; no new capability is added beyond making the base plugin and legacy table caller-supplied.
- Test files for the moved code (catalogue, resolve, load-detect) move with it; tests of engine wrappers stay in `engine/`. Where a moved test depends on an engine-only helper, it is adapted without changing what it asserts.
- Exported names and exact API shape of the new package are a design decision for Research/Plan, constrained by the requirements above.
- Documentation that names the old file locations (CLAUDE.md architecture bullets, `docs/state-machine.md` §7.18) is updated in the same change, with the `docs/llms-full.txt` bundle regenerated if a canonical doc page changes.

## Out of Scope

- Any Pruefer change; Pruefer adopts the package in the next issue of this chain.
- Any change to engine skill-resolution, unresolved, or load-failure behaviour.
- Moving engine-specific wrappers, warnings or labels.

## Source References

- #2137, #2138, #2139, #2140 (custom-workflow support), #1859 (replaced by this chain), ADR-1113 (Pruefer must not import `engine`), ADR-2137, ADR-2138
- Precedents for leaf-package extractions: `internal/claudeerr` (ADR-1449), `internal/pathglob`, `internal/claudeprofile`
