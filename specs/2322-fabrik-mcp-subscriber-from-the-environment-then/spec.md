# Feature Specification: fabrik mcp subscriber from the environment, then CLAUDE_TOPIC, then a stable default

**Feature Branch**: `fabrik/issue-2322`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

`fabrik mcp` needs a subscriber name for channel pushes and the overseer actions. That name keys the daemon's held queue and subscriptions, so it must stay the same across restarts (#1968). Today it comes from `--subscriber` or `FABRIK_SUBSCRIBER`, with no default. The plugin fills `FABRIK_SUBSCRIBER` from its `subscriber` setting (`plugin/fabrik/.mcp.json`).

That breaks two cases:

1. **Out of the box.** A user who installs the plugin and never fills in the setting gets no pushes and no overseer actions. One Claude Code session talking to Fabrik should just work, with no configuration.
2. **Several sessions.** The plugin setting is one value for every session, so it cannot tell sessions apart. Worse, the `env` entry in `.mcp.json` overrides whatever the session inherited. A user or tool that sets `FABRIK_SUBSCRIBER` per session (verveguy/claude-topics#7, which starts long-running \"topic\" sessions under stable names) has that value silently replaced by the plugin setting.

## Proposal

### 1. Read the subscriber only from the environment

- Remove `FABRIK_SUBSCRIBER` from the `env` block in `plugin/fabrik/.mcp.json`.
- Remove the `subscriber` entry from `userConfig` in `plugin/fabrik/.claude-plugin/plugin.json`.

With that, `FABRIK_SUBSCRIBER` reaches `fabrik mcp` only when it is really in the session's environment, so it is always a deliberate choice. `FABRIK_MCP_DIR` / `fabrik_dir` are unchanged.

### 2. Resolve the name in this order

`subscriberName` (`cmd/mcp.go`) takes the first of these that is set (unexpanded `${...}` placeholders still count as unset):

1. `--subscriber`
2. `FABRIK_SUBSCRIBER`
3. `CLAUDE_TOPIC`: exported by [`topic`](https://github.com/verveguy/claude-topics) on every session it launches, set to the topic name. That name is stable across restarts and handoffs by design, so every topic session gets a distinct, stable subscriber with no configuration.
4. **A fixed default**: the OS username (`$USER`, falling back to `os/user`). It stays the same across restarts, so held events and subscriptions survive. It also names who acted, which the overseer actions write into their GitHub audit comments (`default` would read badly there).

This replaces the \"no generated default\" rule from #1968. That rule was meant to stop *unstable* defaults like PIDs or session ids. A fixed default is stable; its only cost is that two default-named sessions share one name, which is case 5 below.

### 3. A collision explains itself

When a second session attaches under the same name, the first already gets one `stream-superseded` notice. When the superseded name came from the **default** (step 4), the notice should also say how to fix it: set `FABRIK_SUBSCRIBER` per session, or run sessions under `topic`. That is how a user finds out they have outgrown the default.

### 4. Keep non-interactive sessions from taking the default (to investigate)

With a default, **any** `claude` process that loads the plugin, such as a `claude -p` script, attaches under the default and supersedes the human's interactive session. `CLAUDE_TOPIC` has the same issue: a child `claude -p` started from a topic session's shell inherits it. It matters most if Fabrik's own engine workers load user plugins.

The push stream should start only in a session that can receive pushes. Find out what `fabrik mcp` can observe to tell (e.g. what Claude Code sends in `initialize`: client capabilities, client info, whether the session enabled the channel). If nothing reliable exists, record that in the spec and make the default apply only to the action tools' audit name, not to starting the stream. Any choice made here must be stated in the spec.

### 5. Docs

- `docs/USER_GUIDE.md`: the \"Subscriber name\" setting row (~line 3020), the channel setup (~line 4104), and the overseer actions section, rewritten for the new order. Cover the two ways to set a name: export `FABRIK_SUBSCRIBER` (shell profile, direnv, inline), or register the server yourself with `--subscriber` and disable the plugin's copy, noting the channel source then changes from `plugin:fabrik@fabrik` to `server:fabrik`.
- `CLAUDE.md` (the `plugin/fabrik/` and `internal/mcpstdio/` entries) and ADR 2141 / #1968 notes where they describe the subscriber.
- Release notes: the plugin's `subscriber` setting is gone. Users who set it should export `FABRIK_SUBSCRIBER` instead.

## To verify live (CI cannot prove these; add to ADR 2141's list)

- Claude Code passes the session's own environment through to a **plugin's** stdio MCP server, so an exported `FABRIK_SUBSCRIBER` / `CLAUDE_TOPIC` reaches `fabrik mcp` once `.mcp.json` no longer sets it. The plugin passes `CLAUDE_PROJECT_DIR` explicitly today, which suggests this was not assumed.
- Removing a `userConfig` key from an installed plugin does not break the upgrade, and a stored old value does not still reach the server.

## Acceptance

- With the plugin installed and nothing configured, a session attaches under the OS username. `fabrik_subscribe` works, and after a restart the held events and subscriptions are still there.
- A session launched with `CLAUDE_TOPIC=X` attaches as `X`. One with `FABRIK_SUBSCRIBER=Y` attaches as `Y`, even if `CLAUDE_TOPIC` is also set. `--subscriber Z` beats both.
- An unexpanded placeholder in any of those counts as unset.
- A second session under the default name supersedes the first, and the first's notice says how to set a per-session name.
- The non-interactive case (part 4) behaves as the spec decides, with tests.
- `plugin/pm_plugin_test.go`, `cmd/mcp_test.go` and the `internal/mcpstdio` tests are updated for the removed setting and the new order."

## Background

`fabrik mcp` needs a subscriber name for two things: the daemon-side identity of the session's channel stream (held queue and subscriptions, which must survive restarts, #1968), and the audit name that the overseer actions write into their GitHub comments. Today the name comes only from `--subscriber` or `FABRIK_SUBSCRIBER`, and the plugin fills the latter from a single plugin-wide `subscriber` setting, with no default.

Two things go wrong:

1. **Out of the box.** Someone who installs the plugin and never fills in the setting gets no pushes and cannot use the overseer actions. One session talking to Fabrik should work without configuration.
2. **Several sessions.** One plugin setting is shared by every session, so it cannot tell sessions apart. Because the plugin's `env` entry overrides what a session inherited, a per-session `FABRIK_SUBSCRIBER` is silently replaced. This defeats tools such as `topic` (verveguy/claude-topics#7), which starts long-running sessions under stable names and exports `CLAUDE_TOPIC` on each.

The earlier "no generated default" rule (#1968) existed to prevent *unstable* names (PIDs, session ids). A fixed default such as the OS username is stable, so it can safely replace that rule. Its only cost is that two default-named sessions share one name; the spec handles that with a self-explaining collision notice.

A default also creates a new hazard: any `claude` process that loads the plugin (a `claude -p` script, a child of a topic session that inherits `CLAUDE_TOPIC`, possibly Fabrik's own workers if they load user plugins) would attach under the same name and supersede the human's interactive session. Part 4 of the request decides how to prevent that.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Works out of the box with no configuration (Priority: P1)

A user installs the Fabrik plugin and starts one Claude Code session without setting anything. The session can push channel events, subscribe, and use the overseer actions.

**Why this priority**: This is the primary failure today: a fresh install delivers neither pushes nor actions.

**Independent Test**: Start `fabrik mcp` with no `--subscriber`, no `FABRIK_SUBSCRIBER`, no `CLAUDE_TOPIC`; confirm the resolved name is the OS username, that an overseer action's audit comment is signed with it, and that after a restart the same name finds its held events and subscriptions.

**Acceptance Scenarios**:

1. **Given** none of the three explicit sources is set, **When** `fabrik mcp` resolves the subscriber, **Then** the name is the OS username (`$USER`, else the `os/user` lookup).
2. **Given** that default name, **When** the session subscribes, receives held events, and the shim restarts, **Then** the subscriptions and held events are still attached to the same name.
3. **Given** that default name, **When** an overseer action runs, **Then** its GitHub audit comment names the OS username as the actor.

---

### User Story 2 - Per-session names from the environment, flag first (Priority: P1)

A user or tool that runs several sessions gives each a stable name through the session environment, and the plugin no longer overwrites it.

**Why this priority**: Without this, `topic` and any per-session `FABRIK_SUBSCRIBER` cannot work with the plugin.

**Independent Test**: Resolve the name under each combination of `--subscriber`, `FABRIK_SUBSCRIBER`, `CLAUDE_TOPIC` and the default, with and without unexpanded `${...}` placeholders, and check precedence.

**Acceptance Scenarios**:

1. **Given** `CLAUDE_TOPIC=X` and nothing higher, **When** the name resolves, **Then** it is `X`.
2. **Given** `FABRIK_SUBSCRIBER=Y` and `CLAUDE_TOPIC=X`, **When** the name resolves, **Then** it is `Y`.
3. **Given** `--subscriber Z` plus both variables, **When** the name resolves, **Then** it is `Z`.
4. **Given** any of the three holds an unexpanded `${...}` placeholder, **When** the name resolves, **Then** that source counts as unset and the next one is used.
5. **Given** the plugin is installed, **When** its `.mcp.json` and `plugin.json` are inspected, **Then** neither sets `FABRIK_SUBSCRIBER` nor declares a `subscriber` setting, while `FABRIK_MCP_DIR` / `fabrik_dir` and `CLAUDE_PROJECT_DIR` are unchanged.

---

### User Story 3 - A name collision explains itself (Priority: P2)

When a second session attaches under the default name and supersedes the first, the first session's `stream-superseded` notice tells the user how to avoid it.

**Why this priority**: The default is only a good trade if outgrowing it is discoverable.

**Independent Test**: Attach two sessions under a default-sourced name; check the first receives the superseded notice with the remedy. Repeat with an explicitly named session and check the notice is unchanged.

**Acceptance Scenarios**:

1. **Given** session A attached under a default-sourced name, **When** session B attaches under the same name, **Then** A's one `stream-superseded` notice also says to set `FABRIK_SUBSCRIBER` per session or run sessions under `topic`.
2. **Given** session A's name came from `--subscriber`, `FABRIK_SUBSCRIBER` or `CLAUDE_TOPIC`, **When** it is superseded, **Then** the notice has no default-name advice.

---

### User Story 4 - Non-interactive sessions do not displace a human's session (Priority: P2)

A `claude -p` script, or any process that cannot receive pushes, loads the plugin and does not take over the human's interactive session's name.

**Why this priority**: The default makes this collision likely, and `CLAUDE_TOPIC` inheritance makes it possible even with explicit names.

**Independent Test**: Drive the server's `initialize` handshake with the client shapes the investigation identifies and check whether the stream starts; with a client that gives no usable signal, check the fallback behavior below.

**Acceptance Scenarios**:

1. **Given** the handshake gives a reliable signal that the session cannot receive pushes, **When** the server starts, **Then** it starts no stream and attaches under no name; action tools still work.
2. **Given** no reliable signal exists, **When** the name came only from the default (source 4), **Then** the default names the action tools' audit comments only and starts no stream.
3. **Given** a name from `--subscriber`, `FABRIK_SUBSCRIBER` or `CLAUDE_TOPIC`, **When** the server starts, **Then** the stream starts as before (those are deliberate or tool-assigned choices), subject to scenario 1.

---

### Edge Cases

- `$USER` is unset: fall back to `os/user`; if both fail or are empty, there is no default and the server behaves as it does today with no name (read-only; actions refuse).
- A source holds only whitespace or an empty string: treated as unset.
- `CLAUDE_TOPIC` or `FABRIK_SUBSCRIBER` is a `${...}` placeholder: treated as unset (same rule as `--subscriber`).
- A plugin installed earlier still has a stored `subscriber` value: it must not reach the server, and the upgrade must not fail on the removed key.
- Two sessions with the same default name: the later supersedes the earlier (existing last-attach-wins rule); only the default-sourced case adds advice.
- The OS username contains characters the audit-name sanitiser rejects: the audit name is sanitised as for any other source; resolution does not fail.
- The project directory has no `.fabrik/`: the existing rule keeps the stream off regardless of name source.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The plugin's `.mcp.json` MUST NOT set `FABRIK_SUBSCRIBER`; `FABRIK_MCP_DIR` and `CLAUDE_PROJECT_DIR` entries remain unchanged.
- **FR-002**: The plugin's `userConfig` MUST NOT declare a `subscriber` setting; `fabrik_dir` remains.
- **FR-003**: `fabrik mcp` MUST resolve the subscriber name as the first set value of: `--subscriber`, `FABRIK_SUBSCRIBER`, `CLAUDE_TOPIC`, then the OS username (`$USER`, else the `os/user` lookup).
- **FR-004**: A value containing an unexpanded `${...}` placeholder, or empty after trimming, MUST count as unset for every source in FR-003.
- **FR-005**: The resolved name's source MUST be known to the server so it can distinguish the default (source 4) from explicit sources.
- **FR-006**: When a session whose name came from the default is superseded, its single `stream-superseded` notice MUST additionally state how to fix it: set `FABRIK_SUBSCRIBER` per session, or run sessions under `topic`. Notices for explicitly named sessions MUST be unchanged.
- **FR-007**: The server MUST start the push stream only in a session that can receive pushes, using whatever signal the `initialize` handshake reliably provides (client capabilities, client info, or channel enablement). The investigation's findings, including the exact signals checked and the conclusion, MUST be recorded in the implementing PR and the docs.
- **FR-008**: If the investigation finds no reliable signal, the server MUST use the default name (source 4) only for the action tools' audit name and MUST NOT start the stream from it; names from `--subscriber`, `FABRIK_SUBSCRIBER` and `CLAUDE_TOPIC` continue to start the stream. If a reliable signal is found, the default starts the stream only in sessions that pass it.
- **FR-009**: The existing rule that the stream does not start when the resolved directory has no `.fabrik/` (#2207) MUST be unchanged.
- **FR-010**: The overseer actions MUST work with any resolved name, including the default, and sign their audit comment with it.
- **FR-011**: A held queue and subscriptions stored under a default-sourced name MUST be found again after a restart under the same name.
- **FR-012**: `docs/USER_GUIDE.md` MUST be rewritten for the new order in the "Subscriber name" setting row (now removed as a plugin setting), the channel setup, and the overseer actions section. It MUST describe both ways to set a name: export `FABRIK_SUBSCRIBER` (shell profile, direnv, inline), or register the server yourself with `--subscriber` and disable the plugin's copy, noting the channel source then changes from `plugin:fabrik@fabrik` to `server:fabrik`. It MUST state the FR-007/FR-008 outcome. `docs/llms-full.txt` is regenerated in the same change.
- **FR-013**: `CLAUDE.md` (the `plugin/fabrik/` and `internal/mcpstdio/` entries), ADR 2141 and the #1968 ADR notes MUST be updated where they describe the subscriber, including that the "no generated default" rule is superseded by a fixed default.
- **FR-014**: ADR 2141's list of live checks MUST gain: (a) a session's own environment reaches a plugin's stdio MCP server once `.mcp.json` no longer sets `FABRIK_SUBSCRIBER`, so an exported `FABRIK_SUBSCRIBER`/`CLAUDE_TOPIC` arrives; (b) removing a `userConfig` key from an installed plugin does not break the upgrade and a stored old value does not still reach the server.
- **FR-015**: Release notes MUST say the plugin's `subscriber` setting is gone and that users who set it should export `FABRIK_SUBSCRIBER` instead.
- **FR-016**: Tests MUST be updated or added in `plugin/pm_plugin_test.go`, `cmd/mcp_test.go` and the `internal/mcpstdio` tests, covering the removed setting, the resolution order and placeholders, the default-name collision notice, and the FR-007/FR-008 behavior.

### Key Entities *(if applicable)*

- **Subscriber name**: the stable identity under which a session's channel stream attaches and which keys the held queue and subscriptions; also the audit name on overseer actions.
- **Name source**: which of flag, `FABRIK_SUBSCRIBER`, `CLAUDE_TOPIC` or default produced the name; determines the collision advice and whether the name may start a stream when no push-capability signal exists.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A fresh plugin install with nothing configured yields a usable overseer action (audit comment signed with the OS username) with zero configuration steps.
- **SC-002**: For every combination of the four name sources and placeholder/unset values, the resolved name matches the stated precedence in 100% of test cases.
- **SC-003**: A per-session `FABRIK_SUBSCRIBER` or `CLAUDE_TOPIC` in the session environment is never overridden by the plugin.
- **SC-004**: After a restart, a default-named session finds all of its prior subscriptions and held events.
- **SC-005**: A user whose default-named session is superseded sees, in the notice itself, the remedy; no documentation lookup is needed to learn it.
- **SC-006**: A `claude -p` process loading the plugin never displaces an interactive session's stream under the default name (by a handshake signal, or, absent one, because the default never starts a stream).

## Assumptions

- The OS username is an acceptable fixed default; it is stable and reads sensibly in audit comments.
- `topic`'s `CLAUDE_TOPIC` export (claude-topics) is stable and holds the topic name.
- Part 4 is resolved by investigation during Research/Plan. The decision rule is already fixed by FR-007/FR-008: use a reliable handshake signal if one exists, otherwise restrict the default to the audit name. A `claude -p` child that inherits `CLAUDE_TOPIC` can still start a stream if no reliable signal exists; the documented remedy for that residual case is to unset `CLAUDE_TOPIC` or set `FABRIK_SUBSCRIBER` in the child.
- When no reliable signal exists and a user relies on the default, pushes are not delivered out of the box; that user sets `FABRIK_SUBSCRIBER` (or runs under `topic`) to receive pushes. This trade is the one the request itself prescribes.
- Removing the plugin `subscriber` setting is a breaking change for people who set it, acceptable with a release-note instruction.
- Live behavior in Claude Code (environment passthrough to a plugin's MCP server; removal of a `userConfig` key) cannot be proven by CI and is recorded as a live check rather than assumed.

## Out of Scope *(optional)*

- Generating names from PIDs, session ids or other unstable values.
- Any change to `FABRIK_MCP_DIR` / `fabrik_dir` resolution.
- Changing the last-attach-wins supersede behavior or letting several sessions share one name.
- Changing the channel event catalogue, daemon-side held-queue format or subscription semantics.
- Making Fabrik's own engine workers stop loading user plugins (a separate concern; this spec only ensures a worker cannot displace a human's stream).

## Source References *(optional)*

- #1968 / ADR 1966-b: channel events, "no generated default" rule being superseded
- ADR 2141, #2141: PM plugin and its live-verification list
- #2207: stream gated on a `.fabrik/` directory
- verveguy/claude-topics#7: `topic` sessions under stable names; `CLAUDE_TOPIC`
