# Feature Specification: Update docs for v0.0.87

**Feature Branch**: `fabrik/issue-2338`
**Created**: 2026-10-15
**Status**: Specified
**Input**: User description: "Update USER_GUIDE.md, README.md, and the marketing site to reflect changes in v0.0.87.

See release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.87

## Scope

- USER_GUIDE.md — update sections affected by new features or changed behavior
- README.md — update feature list if new user-facing capabilities were added
- docs/index.md — update marketing page if warranted
- Ensure code examples and configuration references are current
- Regenerate `docs/llms-full.txt` after any canonical-doc edits (per CLAUDE.md)"

## Background

Each release is followed by a documentation pass so the User Guide, README and marketing page describe what the shipped binary does. v0.0.87 is a small release with one user-facing change (#2322): `fabrik mcp` resolves its subscriber name on its own, and the `fabrik` plugin stops setting one. The current docs still describe the old behavior in several places:

- The plugin's "Subscriber name" setting is documented as an optional plugin setting that is passed to `fabrik mcp` as `FABRIK_SUBSCRIBER`. The setting no longer exists; a value stored for it is ignored.
- The overseer actions are documented as refusing without a subscriber name. They now always have one, because the name falls back to the OS username.
- Pushed events are documented as depending on "a subscriber name set". Now only an explicitly chosen name starts the push stream. The default OS-username name is used for audit comments and as `fabrik_subscribe`'s default, but it never starts the stream.

The v0.0.87 release notes (the source of truth for this pass) state:

- **Name resolution.** `fabrik mcp` takes the first of `--subscriber`, `FABRIK_SUBSCRIBER`, `CLAUDE_TOPIC` (exported by the `topic` session launcher), then the OS username (`$USER`, else the system user database). A blank value or an unexpanded `${...}` placeholder counts as unset at every step.
- **Plugin.** `.mcp.json` no longer sets `FABRIK_SUBSCRIBER`, and `userConfig` no longer has `subscriber`. Several Claude Code sessions can each attach under their own name.
- **Default name receives no pushes.** Nothing Claude Code sends at server start says whether a session can receive pushes, and a stream under a name every session shares would let a `claude -p` script take over an interactive session's stream. Set `FABRIK_SUBSCRIBER` or pass `--subscriber` to receive pushes.
- **Hand-registered servers.** Anyone who registers `fabrik mcp` with `--subscriber` instead of using the plugin should disable the plugin's copy. The channel source then reads `server:fabrik` rather than `plugin:fabrik@fabrik`.
- **Messages.** A session whose default-named stream is taken over is told how to give each session its own name. `fabrik_subscribe` and a pushed `fabrik_situation` say plainly when the name is the default and events are held rather than delivered. Every action tool's description states where its name comes from.
- **Not user-facing.** The remaining items (release metadata, plugin pins, e2e gate coverage) need no documentation. Nothing in `engine/` changed, so there are no engine behavior or configuration changes.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator installs the plugin and uses overseer actions (Priority: P1)

An operator installs the `fabrik` PM plugin and, with no configuration, wants to know what works.

**Why this priority**: The no-configuration behavior is the headline of the release. Docs that still say "leave empty for read-only use" and "refuse without a name" would send people to configure something they no longer need to.

**Independent Test**: Read the plugin's Settings section and the overseer-actions section. Without prior knowledge, a reader can tell that the plugin has only the Fabrik directory setting, that actions work out of the box, and where the audit-comment name comes from.

**Acceptance Scenarios**:

1. **Given** the plugin Settings section, **When** a reader looks for a subscriber setting, **Then** the section lists only the Fabrik directory setting and says the name is resolved automatically (or points to the resolution rules).
2. **Given** the overseer-actions section, **When** a reader asks whether a name must be configured, **Then** it says every action carries a name resolved by the documented order, with no refusal for a missing name.

---

### User Story 2 - Operator wants pushed channel events (Priority: P1)

An operator wants events pushed into a Claude Code session and needs to know what to set.

**Why this priority**: This is the one place where the new behavior has a trap. The default name delivers nothing, so a reader who infers "it works with no config" will wait for pushes that never arrive.

**Independent Test**: Follow only the channel documentation to get pushes. The instructions say to set `FABRIK_SUBSCRIBER` or pass `--subscriber` explicitly, and state that the OS-username default and the plugin without a configured name start no stream.

**Acceptance Scenarios**:

1. **Given** the channel requirements, **When** a reader has set nothing, **Then** the docs say pushes are not delivered and name the two ways to opt in.
2. **Given** the instructions for launching with the development-channels flag for the plugin, **When** read, **Then** they no longer say to fill in the plugin's "Subscriber name" setting and instead say to export `FABRIK_SUBSCRIBER` in the session environment.
3. **Given** the `fabrik_subscribe` documentation, **When** read, **Then** it says that with a default name the subscription is held, not pushed.

---

### User Story 3 - Existing user upgrades from an earlier release (Priority: P2)

A user who set the plugin's "Subscriber name" or registered `fabrik mcp` by hand reads the docs to see what to change.

**Why this priority**: These readers lose pushed events silently if they miss the change.

**Independent Test**: A reader who had the old setting finds, in the docs, the replacement (`FABRIK_SUBSCRIBER` in the session environment), the fact that the old stored value is ignored, and the plugin-vs-manual duplicate-server note, including the changed channel source name.

**Acceptance Scenarios**:

1. **Given** a user who had set the plugin's subscriber setting, **When** they read the plugin section or highlights, **Then** they learn to export `FABRIK_SUBSCRIBER` instead.
2. **Given** a user who registered `fabrik mcp --subscriber` by hand, **When** they read the guidance, **Then** they learn to disable the plugin's copy and that the channel source then reads `server:fabrik` rather than `plugin:fabrik@fabrik`.

---

### User Story 4 - Evaluator reads the README or marketing page (Priority: P3)

A prospective user skims the README and `docs/index.md`.

**Why this priority**: These are summary surfaces. They need a v0.0.87 highlight in the pattern established for v0.0.86, but correctness of the guide matters more.

**Independent Test**: The README highlight and the marketing page's "New in" line both name v0.0.87, summarize the zero-configuration MCP change and the pushes caveat, and link to the User Guide. No older release's highlight is left stale in a way that contradicts them.

**Acceptance Scenarios**:

1. **Given** the README, **When** read, **Then** it carries a v0.0.87 highlights note and its "Pushed events" bullet no longer claims a plain "subscriber name set" suffices without saying how the name is set.
2. **Given** `docs/index.md`, **When** read, **Then** its "New in" line reflects v0.0.87.

---

### Edge Cases

- A full-text search of the docs for "Subscriber name", "subscriber name set", "--subscriber" and "FABRIK_SUBSCRIBER" finds no statement that contradicts the new behavior. This includes the `subscriber` subscription option ("override the launch name"), the `stream-superseded` event row, and the FAQ or troubleshooting pages if they mention it.
- `CLAUDE_TOPIC` is exported by the user's `topic` launcher. Documenting it as a name source must not imply it starts pushes any differently from the other explicit sources. Per the release notes it is an explicit source and does start the stream; only the OS-username default does not.
- The plugin's stored-setting note: a value saved for the removed setting is ignored, not an error.
- A reader might assume the OS-username default is a safe shared name. The docs must not suggest several sessions can share it for pushes.
- `docs/llms-full.txt` is a generated bundle. Hand edits to it are overwritten and would fail the drift check.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The User Guide's PM plugin Settings section no longer lists a "Subscriber name" setting. It lists the Fabrik directory setting and states that the subscriber name is resolved automatically.
- **FR-002**: The User Guide documents the subscriber-name resolution order exactly as shipped: `--subscriber`, then `FABRIK_SUBSCRIBER`, then `CLAUDE_TOPIC`, then the OS username (`$USER`, else the system user database), with blank values and unexpanded `${...}` placeholders counted as unset at every step.
- **FR-003**: The User Guide states that the default (OS-username) name signs overseer-action audit comments and is `fabrik_subscribe`'s default, but never starts the push stream, and gives the reason in one sentence (no way to tell an interactive session from a `claude -p` script; a shared name would let a script take over the interactive stream).
- **FR-004**: The overseer-actions text no longer says that actions refuse without a subscriber name or that they need the server launched with a name. It states where the name comes from.
- **FR-005**: The channel requirements state that pushes need an explicit `FABRIK_SUBSCRIBER` or `--subscriber` (`CLAUDE_TOPIC` also counts when exported), and that with nothing set events are held rather than delivered. The plugin's development-channels launch instructions no longer refer to a plugin "Subscriber name" setting.
- **FR-006**: The User Guide has an upgrade note covering: the removed plugin setting and its ignored stored value, exporting `FABRIK_SUBSCRIBER` as the replacement, and the duplicate-server guidance for hand-registered servers, including the `server:fabrik` versus `plugin:fabrik@fabrik` channel source difference.
- **FR-007**: The User Guide mentions that a session whose default-named stream is taken over is told how to give each session its own name, and that `fabrik_subscribe` and a pushed `fabrik_situation` state when the name is the default and events are held. The `stream-superseded` entry and the `subscriber` subscription option are consistent with this.
- **FR-008**: The README has a v0.0.87 highlights note in the format of the existing v0.0.86 one. Its feature-list bullet on pushed events is corrected to match the new name behavior.
- **FR-009**: `docs/index.md` updates its "New in" line to v0.0.87, in the style of the existing line.
- **FR-010**: Any other doc page that describes the plugin's subscriber setting or the "name required" behavior (for example the FAQ, troubleshooting, the PM plugin's own skill or command docs under `plugin/fabrik/` if they restate it, or `docs/llms.txt`) is corrected, or confirmed by search to contain no such statement.
- **FR-011**: Code examples and configuration references in the touched sections are current: the `claude mcp add` example, the development-channels flag examples and the environment variable names match the shipped behavior.
- **FR-012**: `docs/llms-full.txt` is regenerated with `bash scripts/generate-llms-full.sh` and committed in the same change as the canonical-doc edits.
- **FR-013**: No documentation claims engine, configuration-file or CLI-flag changes for v0.0.87 beyond the MCP/plugin change (the release notes record none).

### Key Entities *(if applicable)*

- **Subscriber name**: The identifier a `fabrik mcp` session uses to sign overseer-action audit comments and to attach to the daemon's channel stream. It comes from the resolution order in FR-002 and is either explicit (a pushed stream) or the default (no pushed stream).
- **Doc surfaces**: `docs/USER_GUIDE.md` (canonical), `README.md`, `docs/index.md` (marketing), and the generated `docs/llms-full.txt` bundle.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A search of `docs/USER_GUIDE.md`, `README.md`, `docs/index.md`, the FAQ and `docs/llms.txt` finds zero statements that the plugin has a "Subscriber name" setting or that overseer actions refuse for lack of a name.
- **SC-002**: Every place that explains how to receive pushed events states the explicit-name requirement, and every place describing the plugin's settings lists only the Fabrik directory.
- **SC-003**: The resolution order in the docs matches `fabrik mcp --help` and the v0.0.87 release notes, with no step added or omitted.
- **SC-004**: The README and marketing page both name v0.0.87 and link to the relevant User Guide section, and all new intra-doc anchors resolve.
- **SC-005**: The `docs-drift` check passes on first push, because `docs/llms-full.txt` was regenerated and committed with the doc edits.
- **SC-006**: A reader who only ever used the old plugin setting can complete the migration using the docs alone, with no need for the release notes.

## Assumptions

- The v0.0.87 release notes are the authoritative description of the change. Where they and the existing docs disagree, the docs are updated to match the release notes, with `fabrik mcp --help` and the shipped plugin manifest as a spot-check.
- `CLAUDE_TOPIC` counts as an explicit, stream-starting source, as the release notes list it above the OS username and only call the OS-username default push-less.
- The v0.0.86 highlight is replaced by a v0.0.87 one, or both are kept, whichever the README and index pages already do release to release. Following the existing pattern is preferred.
- No new section is needed beyond what the change touches. The pass edits the existing plugin, overseer-actions and channels sections, plus the upgrade note.
- No blog post or changelog page is required for this patch release.

## Out of Scope *(optional)*

- Any code, plugin or configuration change; this is a documentation-only issue.
- Re-auditing the docs for older releases or undocumented features (separate audit work).
- Documenting the release's internal items (marketplace pins, plugin version, e2e gate coverage).
- Rewriting ADRs; ADR 2322 already records the decision.
- Changes to `docs/state-machine.md` or `docs/stage-lifecycle.md`, since no engine behavior changed. If a statement there contradicts the new name behavior, correcting it is allowed but not expected.

## Source References *(optional)*

- Release notes: https://github.com/handarbeit/fabrik/releases/tag/v0.0.87
- Issue #2322 and ADR 2322, which amends ADRs 2141, 1966-b and 1966-c
- Prior documentation pass for the same pattern: #2333 / PR #2335 (v0.0.86)
