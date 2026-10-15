# Feature Specification: `fabrik mcp` finds the project's daemon from a subdirectory and stays quiet outside Fabrik projects

**Feature Branch**: `fabrik/issue-2207`
**Created**: 2026-10-12
**Status**: Specified
**Input**: Review of PR #2206 (#2141, PM plugin refresh).

## Background

The `fabrik` PM plugin starts `fabrik mcp` in every Claude Code session where it is installed. `resolveMCPDir` (`cmd/mcp.go`) takes `--dir`, then `FABRIK_MCP_DIR`, then `CLAUDE_PROJECT_DIR`, then the working directory, and uses that directory as-is.

1. **Subdirectory sessions.** A session started in `repo/sub/` of a Fabrik project resolves `repo/sub/`, finds no socket, and reports "no daemon" although the project's daemon is running.
2. **Non-Fabrik projects with a subscriber set.** With `subscriber` set at user scope, a session in a non-Fabrik project still starts the channel stream shim, which retries forever. If the channel is opted in, it emits a `daemon-unreachable` notice after the 10 s grace. That is one notice per outage, but it says something misleading: there is no daemon to reach.
3. **Unproven tests.** `settingValue` treats an unexpanded `${...}` placeholder as unset, which fixed a Pruefer finding. Neutralising it in review produced a compile error rather than a test failure, so the placeholder tests have not been shown to catch a regression. Removing the hook's `command -v fabrik` guard also left the hook tests passing.

This is a follow-up to #2141 / PR #2206 and depends on that work landing first.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Subdirectory sessions reach the project's daemon (Priority: P1)

An operator starts a Claude Code session in `repo/sub/` of a Fabrik project. The PM plugin's `fabrik mcp` finds the project's `.fabrik/` in an ancestor directory and talks to that project's running daemon, exactly as it would from the project root.

**Why this priority**: It is the user-visible bug: a running daemon is reported as absent.

**Independent Test**: Create a fixture project with `.fabrik/` at its root and a nested subdirectory. Start `fabrik mcp` with `CLAUDE_PROJECT_DIR` (and separately the working directory) set to the subdirectory, and check that the socket path used is the fixture project's.

**Acceptance Scenarios**:

1. **Given** `CLAUDE_PROJECT_DIR` is `repo/sub/` and `repo/.fabrik/` exists, **When** `fabrik mcp` resolves its directory, **Then** it uses `repo/`.
2. **Given** no `CLAUDE_PROJECT_DIR` and a working directory of `repo/sub/`, **When** it resolves, **Then** it uses `repo/`.
3. **Given** `--dir repo/sub/` or `FABRIK_MCP_DIR=repo/sub/`, **When** it resolves, **Then** it uses `repo/sub/` literally, with no walk.

---

### User Story 2 - Sessions outside Fabrik projects stay quiet (Priority: P1)

An operator has `subscriber` set at user scope and opens a session in a project that has no Fabrik. The shim starts no channel stream, makes no connection attempts and emits no `daemon-unreachable` notice. Tools still answer "no Fabrik daemon here" promptly.

**Why this priority**: A user-scope subscriber currently produces a misleading notice in every unrelated project.

**Independent Test**: Run `fabrik mcp` with a subscriber set in a directory tree without `.fabrik/` and assert that no stream connection is attempted and no `daemon-unreachable` event appears.

**Acceptance Scenarios**:

1. **Given** a subscriber is set and the resolved directory has no `.fabrik/`, **When** the session initialises, **Then** no stream is started and no connection is attempted.
2. **Given** the same state, **When** a tool is called, **Then** it returns "no Fabrik daemon here" quickly.
3. **Given** a subscriber is set and the resolved directory has `.fabrik/` (daemon running or not), **When** the session initialises, **Then** the stream starts as today.

---

### User Story 3 - The regression tests are proven to catch regressions (Priority: P2)

A maintainer can trust that the `settingValue` placeholder tests and the hook's missing-binary test fail when the behaviour they guard is removed.

**Why this priority**: The tests protect an earlier Pruefer finding, but today they pass with the guard removed.

**Independent Test**: Remove the placeholder check, and separately remove the guard and empty-version check in the hook; the named tests fail, which the PR shows.

**Acceptance Scenarios**:

1. **Given** the placeholder check in `settingValue` is neutralised in a way that still compiles, **When** the placeholder tests run, **Then** at least one fails.
2. **Given** both the hook's `command -v fabrik` guard and its empty-version check are removed, **When** the missing-binary test runs, **Then** it fails.

---

### Edge Cases

- No ancestor contains `.fabrik/`: today's "no Fabrik daemon here" behaviour, with the originally resolved directory.
- Nested Fabrik projects: the nearest ancestor containing `.fabrik/` wins.
- The start directory is the home directory or a descendant of it: the walk stops at home and never looks above it. Home itself is checked, so a `.fabrik/` directly in home is found.
- The start directory is outside home (for example `/tmp/x`): the walk goes up to the filesystem root.
- A `.fabrik` entry that is not a directory (a plain file): not treated as a Fabrik project.
- `--dir` or `FABRIK_MCP_DIR` pointing at a directory without `.fabrik/`: taken literally, and the stream is not started (FR-002 applies to whatever directory is resolved).
- Home directory cannot be determined: the walk is bounded by the filesystem root only.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the directory comes from `CLAUDE_PROJECT_DIR` or the working directory (not from `--dir` or `FABRIK_MCP_DIR`, which are taken literally), `fabrik mcp` walks up from it to the nearest ancestor containing `.fabrik/` and uses that. It stops at the filesystem root, and at the user's home directory without searching above it. If none is found it keeps today's "no Fabrik daemon here" behaviour.
- **FR-002**: The channel stream shim is started only when the resolved directory contains `.fabrik/`. Outside a Fabrik project, a configured subscriber produces no stream, no retries and no `daemon-unreachable` notice; tools still answer "no Fabrik daemon here" quickly.
- **FR-003**: The `settingValue` placeholder tests are made to fail when the placeholder check is removed (restructure so the neutralisation compiles), and the PR shows it.
- **FR-004**: The hook's missing-binary path has a test that fails if the `command -v fabrik` guard and the empty-version check are both removed, or the redundant guard is deleted, with the PR saying which.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A test starting `fabrik mcp` with `CLAUDE_PROJECT_DIR` set to a subdirectory of a fixture project reaches that project's socket path.
- **SC-002**: A test with a subscriber set and no `.fabrik/` asserts that no stream connection is attempted.
- **SC-003**: Each new test is shown non-vacuous in the PR.

## Assumptions

- "Contains `.fabrik/`" means a directory entry named `.fabrik`; a file of that name does not count.
- The home directory is searched itself (inclusive) but never above it. When the start directory is not under home, the walk runs to the filesystem root.
- Paths are compared after resolving to absolute form; symlink handling follows what the existing directory resolution already does, with no new symlink semantics.
- `--dir` and `FABRIK_MCP_DIR` stay literal, as today. FR-002 is evaluated on the final resolved directory whichever source supplied it.
- "Quickly" for tool answers outside a Fabrik project means no waiting on a daemon connection or retry, as with today's absent-socket path.
- The work builds on the code from #2141 / PR #2206 (`resolveMCPDir`, `settingValue`, the hook), which must be on the base branch first. The issue is blocked on #2141 for that reason.

## Out of Scope

- Discovering daemons across several instances or machines (fleet discovery).
- The live checks of the plugin's `.mcp.json` assumptions: `${CLAUDE_PROJECT_DIR}` expansion in MCP env, an unset optional `userConfig`, and a bare `fabrik` on `PATH`. These are a release-cut check, not code.

## Source References

- #2141 and PR #2206 (PM plugin refresh), whose review produced this follow-up.
