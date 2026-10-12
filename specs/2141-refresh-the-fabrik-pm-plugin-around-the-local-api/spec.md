# Feature Specification: Refresh the `fabrik` PM plugin around the local API and ship it with each release

**Feature Branch**: `fabrik/issue-2141`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Refresh the fabrik (pm) plugin and ship it via a Claude Code marketplace advertised from our open source fabrik repo."

## Background

The `fabrik` plugin is for the human's own interactive Claude Code session: a product-management buddy and supervisor for a Fabrik board. It is published as `fabrik@fabrik` from this repository's marketplace. It has fallen behind the engine:

- It reads board and worker state by scraping — raw GraphQL, `ps` greps for worker processes, shell scripts — although the daemon now serves a local API and `fabrik mcp` exposes it to Claude Code (status of one issue, the board's attention view, daemon health, event push, and a small set of audited overseer actions).
- Its ambient skill describes a subset of today's labels and none of the attention states the engine now computes (needs-human, escalated, waiting, working, stalled, idle).
- The marketplace serves it from `main`, so users get unreleased changes, while its fixed version string means installed copies never see an update until someone bumps it by hand. Nothing tells a user that their plugin and their binary disagree.
- Installing it is a manual two-command step nobody is prompted for.

The earlier issues in this chain free the `fabrik` plugin name for this plugin and keep it out of workers.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - "What's the status of #1067?" answers from the daemon (Priority: P1)

In a Fabrik project, the user asks about an issue or the board; Claude answers from the running daemon — column, time in column, worker, PR/CI, blockers, attention state and what the engine will do next — without scraping GitHub.

**Why this priority**: The core of a PM buddy, and the reason the plugin needs refreshing.

**Independent Test**: With a running daemon, ask for one issue and for the board's attention view; answers match the daemon.

**Acceptance Scenarios**:

1. **Given** the daemon is running, **When** the user asks about an issue, **Then** the answer comes from the daemon's status tool.
2. **Given** the daemon is not running, **When** the user asks, **Then** Claude says so plainly and how to start it.

---

### User Story 2 - The plugin a user has matches the binary they run (Priority: P1)

A user installs `fabrik@fabrik`; the version they get is the one shipped with a Fabrik release, and when their plugin and binary versions differ, the session tells them.

**Why this priority**: Merged is not shipped; users must never receive unreleased plugin changes, and skew must not be silent.

**Independent Test**: Cut a release; the marketplace entry points at that release; a session with a mismatched binary shows a notice.

**Acceptance Scenarios**:

1. **Given** a release is cut, **When** users update the plugin, **Then** they receive exactly that release's plugin.
2. **Given** the installed plugin and the `fabrik` binary versions differ, **When** a session starts, **Then** a one-line notice says so; it never blocks the session.

---

### User Story 3 - Steering with the overseer actions (Priority: P2)

The user asks Claude to promote an item out of Backlog, change its autonomy, re-run Validate, or clear a Claude usage-limit suspension; Claude uses the audited overseer actions rather than editing labels by hand.

**Why this priority**: These exist already; the plugin should teach their use and their limits (no action lifts a pause).

**Independent Test**: Ask for each action; the audit comment appears on the item.

**Acceptance Scenarios**:

1. **Given** a managed item in Backlog, **When** the user asks to start it, **Then** Claude uses the promote action and reports the result.
2. **Given** a paused item, **When** the user asks Claude to resume it, **Then** Claude explains that no action lifts a pause and that a human comment on the item does.

---

### User Story 4 - Getting the plugin is discoverable (Priority: P3)

After `fabrik init`, the operator is shown how to install the plugin in one line.

**Why this priority**: The project-settings auto-install route is not usable (project-scoped plugins do not download, and their MCP servers are skipped), so a printed hint is the practical path.

**Independent Test**: Run `fabrik init`; the hint is printed.

**Acceptance Scenarios**:

1. **Given** `fabrik init` completes, **When** output is shown, **Then** it includes the plugin install commands.

### Edge Cases

- A user-scope install starts the MCP server in every project, including non-Fabrik ones; it must answer "no Fabrik daemon here" quickly and quietly.
- Event push (channels) from a community-marketplace plugin needs a per-session opt-in and claude.ai/Console auth; document it as opt-in, never assume it.
- Several Fabrik instances on one machine: the session talks to the daemon for its own directory unless the user configures another Fabrik directory (see Assumptions).
- The session-start version check runs where `fabrik` is not on `PATH`, or its version cannot be read: it stays silent and does not fail the session.
- A pre-release or development build of the binary reports a version that never equals a released plugin version: the notice should say so rather than imply a downgrade is needed.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The plugin bundles the configuration that starts `fabrik mcp` for the session, with the subscriber name as a user setting.
- **FR-002**: Board and issue state come from the daemon's tools; the shell-script scraping and `ps`-based worker detection are removed.
- **FR-003**: The ambient skill covers the steering labels a human applies, the attention states, the overseer actions and their limits; label knowledge is not duplicated where the daemon already explains state.
- **FR-004**: `/fabrik:status` presents the board's attention view and daemon health from the daemon.
- **FR-005**: The marketplace entry pins the plugin to the release tag, and the plugin's version equals the binary's; both are set by the release process.
- **FR-006**: A session-start check compares plugin and binary versions and shows a notice on mismatch; it never blocks and is silent when `fabrik` is not installed.
- **FR-007**: The README and User Guide document installation, enabling auto-update, the MCP tools and the channel opt-in; `fabrik init` prints the install commands.
- **FR-008**: When no daemon is reachable, the plugin's guidance has Claude say so plainly and name how to start the daemon, rather than falling back to scraping GitHub.
- **FR-009**: By default the session addresses the daemon of the session's own working directory; the plugin offers an optional user setting naming a different Fabrik directory for users who run their Claude Code session elsewhere.
- **FR-010**: The ambient skill states that no overseer action lifts a pause or posts a free-form comment, and directs the user to the human-comment route for resuming a paused item.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: "Status of #N" is answered from the daemon in one tool call when it is running.
- **SC-002**: After a release, the marketplace serves that release's plugin and no later commit.
- **SC-003**: A plugin/binary mismatch is reported in 100% of sessions where both are present.
- **SC-004**: In a non-Fabrik project with the plugin installed at user scope, a session shows no error output and no noticeable start-up delay from the plugin.
- **SC-005**: No part of the plugin reads board or worker state by any route other than the daemon's tools.

## Assumptions

- A bare `fabrik` command on `PATH` is found by Claude Code when starting a plugin's MCP server (the docs' own examples rely on this; confirm live).
- The MCP tools and overseer actions are used as they exist; extending them is separate work.
- Daemon discovery (previously an open question) uses what exists today: the daemon of the session's working directory by default, with an optional configured Fabrik directory as override. A machine running a fleet of instances is served by pointing the setting at the wanted one; addressing several daemons within one session is not supported by this work. Low-impact and reversible, so recorded here for challenge rather than blocking.
- Users who want auto-update turn it on for the marketplace themselves; the plugin documents how but does not change their settings.

## Out of Scope *(optional)*

- The stage plugin (earlier issues).
- New local-API or MCP capabilities, including talking to several daemons from one session or any "resume a paused item" action.

## Source References *(optional)*

- `plugin/fabrik/` (skills `fabrik`, `fabrik-setup`; `commands/status.md`; `skills/fabrik/scripts/`), `.claude-plugin/marketplace.json` (today pins `ref: main`).
- `internal/mcpstdio/`, `cmd/mcp.go` (`--dir` defaults to the current directory; `--subscriber`/`FABRIK_SUBSCRIBER`), `internal/attention/`, `docs/state-machine.md` §7.13–7.15.
- Claude Code docs: plugin `.mcp.json`, `userConfig`, SessionStart hooks, marketplace `ref`/`sha` pinning, project-scope plugin limits (https://code.claude.com/docs/en/plugins/loading).
- `scripts/cut-release.sh` (plugin version bump step).
- Supersedes #1343 and #825.
