# Feature Specification: PM plugin version-check hook names the side that is behind

**Feature Branch**: `fabrik/issue-2584`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description: "## Problem

The PM plugin's `SessionStart` hook (`plugin/fabrik/hooks/version-check.sh`, #2141) reports any plugin/binary mismatch with the same advice:

> Fabrik plugin 0.0.91 and fabrik binary 0.0.92 differ. Update the plugin (/plugin update fabrik@fabrik) or upgrade the binary (fabrik upgrade) so they match.

It compares the versions as opaque strings, so it cannot say which side is behind. The common case is the one we hit after v0.0.92: a daemon with `--auto-upgrade` moves the binary forward and nothing moves the plugin. Here half of the advice (`fabrik upgrade`) is wrong. Following it does nothing for the plugin, and on a directory with a running daemon it is refused (#2458).

## Requirements

- R1: When both versions parse as release versions (`X.Y.Z`, optional leading `v`), compare them numerically and name only the side that is behind:
  - plugin behind: tell the user to update the plugin;
  - binary behind: tell the user to upgrade the binary.
- R2: Keep the opaque-string behaviour (today's two-remedy wording) for anything that does not parse, and keep the `dev*` branch unchanged.
- R3: Check whether `/plugin update fabrik@fabrik` alone picks up the new release. The marketplace entry's `ref` is pinned to the tag at each cut (`tools/pin-plugin-release`), so a cached marketplace may first need `/plugin marketplace update`. If so, the plugin-behind advice names both steps in order. Record what was observed.
- R4: The hook stays POSIX sh with no jq. It never blocks and prints one JSON object or nothing (`plugin/pm_hook_test.go`).

## Acceptance

- `pm_hook_test.go` covers plugin-behind, binary-behind, unparseable and `dev*`, and each case's wording names only the right remedy.
- After a binary auto-upgrade, following the hook's advice clears the notice at the next session start."

## Background

The human-facing PM plugin ships a `SessionStart` hook that warns when the plugin's version and the `fabrik` binary's version differ (#2141). Today it treats the two versions as opaque strings: any difference produces one message that offers both remedies ("update the plugin or upgrade the binary").

That is misleading in the most common situation. A daemon running with `--auto-upgrade` moves the binary forward on its own, while nothing moves the plugin, so after a release such as v0.0.92 the plugin is the side that is behind. In that case "upgrade the binary" is wrong advice: it does nothing for the plugin, and in a directory with a running daemon it is refused (#2458). The user is left to work out which half of the sentence applies.

When both versions are ordinary release versions the direction of the mismatch is knowable, so the hook should say which side is behind and offer only the remedy that fixes it. A second open concern is whether the plugin remedy as worded is sufficient: because the marketplace entry's `ref` is pinned to the release tag at each cut, a cached marketplace may need refreshing before a plugin update can see the new release.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Plugin behind the binary (Priority: P1)

A user's daemon auto-upgraded the `fabrik` binary to a newer release. At the next session start the plugin is older than the binary.

**Why this priority**: This is the case that actually occurs after each release with auto-upgrade, and it is the one where today's advice is half wrong.

**Independent Test**: Run the hook with a plugin manifest at an older release than the `fabrik` binary version and check that the message tells the user to update the plugin and does not mention upgrading the binary.

**Acceptance Scenarios**:

1. **Given** plugin version 0.0.91 and binary version 0.0.92, **When** the hook runs at session start, **Then** it prints one JSON object whose message tells the user to update the plugin and does not mention `fabrik upgrade`.
2. **Given** plugin version 0.0.9 and binary version v0.0.10 (leading `v`, multi-digit component), **When** the hook runs, **Then** it correctly treats the plugin as behind (numeric, not lexical, comparison) and gives the plugin-only advice.

---

### User Story 2 - Binary behind the plugin (Priority: P2)

A user installed a newer plugin release than the `fabrik` binary they have on PATH.

**Why this priority**: Less frequent than story 1, but equally misleading under the current wording.

**Independent Test**: Run the hook with a plugin version newer than the binary version and check that the message tells the user to upgrade the binary and does not mention updating the plugin.

**Acceptance Scenarios**:

1. **Given** plugin version 0.0.93 and binary version 0.0.92, **When** the hook runs, **Then** the message tells the user to upgrade the binary and does not tell them to update the plugin.

---

### User Story 3 - Versions that cannot be ordered (Priority: P2)

Either version is not a plain release version (for example a pre-release suffix, a different number of components, or a build string), or the binary is a development build.

**Why this priority**: The hook must not guess a direction it cannot establish, and today's behaviour for these inputs is relied upon.

**Independent Test**: Run the hook with an unparseable version and with a `dev*` binary and compare the output with today's.

**Acceptance Scenarios**:

1. **Given** versions that differ and at least one does not parse as `X.Y.Z` (optional leading `v`), **When** the hook runs, **Then** it prints today's two-remedy wording.
2. **Given** a `dev*` binary version, **When** the hook runs, **Then** it prints today's development-build wording unchanged.

---

### User Story 4 - Plugin update path actually works (Priority: P1)

A user following the plugin-behind advice should end up with the notice gone at the next session start.

**Why this priority**: Advice that does not resolve the notice is no better than the current wording.

**Independent Test**: With a plugin behind a newly released binary, follow the advice exactly as printed and start a new session; the notice must not appear.

**Acceptance Scenarios**:

1. **Given** a binary that was auto-upgraded to a new release and a plugin from the previous release, **When** the user follows the printed plugin advice and starts a new session, **Then** the hook prints nothing.

---

### Edge Cases

- Equal versions, including one with and one without a leading `v`: still silent, as today.
- Versions with leading zeros or components with several digits (e.g. `0.0.09` vs `0.0.10`): must compare numerically and must not be misread as octal or compared as strings.
- A plugin version with a leading `v` (the manifest normally has none): accepted the same way as the binary's.
- Versions with a pre-release or build suffix (`0.0.93-rc1`, `0.0.93+abc`), or with fewer or more than three numeric components: treated as unparseable, so today's two-remedy wording applies.
- Versions containing characters that could break the JSON string: still silent, as today.
- Missing manifest, unreadable plugin version, or `fabrik` not on PATH: still silent, as today.
- `FABRIK_PLUGIN_VERSION_CHECK=off`: still silent, as today.
- Very large numeric components: must not misbehave; the comparison either remains correct or falls back to the unparseable wording.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the plugin and binary versions both parse as release versions (`X.Y.Z`, each a run of digits, with an optional leading `v`) and differ, the hook compares them numerically, component by component.
- **FR-002**: When the plugin is behind the binary, the message names only the plugin as the thing to update and does not offer or mention upgrading the binary.
- **FR-003**: When the binary is behind the plugin, the message names only the binary as the thing to upgrade and does not offer or mention updating the plugin.
- **FR-004**: When either version does not parse as a release version, and the binary is not a `dev*` build, the hook prints today's two-remedy wording unchanged.
- **FR-005**: The `dev*` binary wording, the silent cases (versions equal after stripping a leading `v`, check switched off, unreadable manifest or binary version, characters unsafe for JSON) and the JSON output shape are unchanged.
- **FR-006**: The behaviour of the plugin update command used in the plugin-behind advice is checked, and the result recorded (what was run, what was observed, which release it was observed with). If `/plugin update fabrik@fabrik` alone does not pick up the new release because the marketplace needs refreshing first, the plugin-behind advice names both steps in order (marketplace refresh, then plugin update); if it does pick it up, the advice names only the plugin update.
- **FR-007**: The hook remains POSIX sh with no dependency on jq, never blocks (always exits 0), and prints exactly one JSON object or nothing.
- **FR-008**: The hook's test suite covers plugin-behind, binary-behind, unparseable and `dev*` cases, and for each asserts that the wording names the right remedy and omits the wrong one.

### Key Entities

- **Plugin version**: the `version` in the plugin manifest, set to the release version at each release cut.
- **Binary version**: the first line of `fabrik --version`; release builds omit the leading `v`, `go install` builds may keep it, development builds start with `dev`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For each of the four cases (plugin behind, binary behind, unparseable, `dev*`), the hook's message contains the correct remedy and none of the other side's, verified by automated tests.
- **SC-002**: After a binary auto-upgrade, a user who follows the printed advice exactly sees no version notice at the next session start.
- **SC-003**: Existing behaviour for equal versions, the off switch, unreadable inputs and unsafe characters is unchanged: all pre-existing hook tests still pass without modification of their expectations.
- **SC-004**: The observation behind the plugin-update advice (whether a marketplace refresh is needed first) is recorded where a reader of the change can find it.

## Assumptions

- "Release version" means exactly three dot-separated numeric components with an optional leading `v`. Anything else (pre-release, build metadata, other component counts) is treated as unparseable and gets today's wording, because the direction cannot be established reliably without a fuller version grammar.
- The binary is behind or ahead only in the sense of this numeric ordering; no attempt is made to account for the daemon's own version or for multiple installed binaries.
- The wording changes only in which remedy is named; message length, JSON shape and tone follow the existing message.
- The check in FR-006 is performed by a person or agent with access to a Claude Code session and the published marketplace; its outcome determines the plugin-behind wording, and a record of it is included with the change (for example in the change description).
- The exact final wording of each message is a Plan/Implement decision; this spec constrains only which remedies each message may name.

## Out of Scope

- Changing how the plugin is released, how the marketplace `ref` is pinned, or the auto-upgrade behaviour of the daemon.
- Comparing pre-release or build-metadata versions, or implementing full semver precedence.
- Making the hook update the plugin or binary itself.
- Changing the daemon/MCP-shim version-skew note.
- Changing the `dev*` branch's wording or the `FABRIK_PLUGIN_VERSION_CHECK` switch.

## Source References

- #2141 (PM plugin and the original version-check hook), #2458 (`fabrik upgrade` refusal while a daemon holds the directory), `plugin/fabrik/hooks/version-check.sh`, `plugin/pm_hook_test.go`, `tools/pin-plugin-release`
