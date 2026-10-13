# Feature Specification: A local API that fails to start is a warnings-panel entry

**Feature Branch**: `fabrik/issue-2223`
**Created**: 2026-10-13
**Status**: Specified
**Input**: Review of how the `fabrik@fabrik` PM plugin behaves when its tools cannot reach the daemon.

## Background

`startLocalAPI` (`engine/localapi_server.go`) opens the daemon's local socket API at startup. If `srv.Start()` fails (an unusable socket path, permissions, a regular file in the way), it logs `local API unavailable (continuing without it): <err>` through `logf` and the daemon carries on without it.

That line reaches only the log. The operator sees nothing in the TUI. Meanwhile every `fabrik mcp` session (the PM plugin's tools) reports "Fabrik daemon not running at <socket>", although the daemon is running and polling. This condition blocks the overseer tools and needs a person to act, so under ADR 2135 it belongs in the warnings panel.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator learns the local API is down (Priority: P1)

An operator starts the daemon in an environment where the local socket cannot be opened. The daemon keeps polling, but the PM plugin's tools say the daemon is not running. The operator opens the warnings panel and finds one entry that names the socket path and the error, and explains that `fabrik mcp` and the PM plugin tools will report the daemon as not running.

**Why this priority**: Without it the operator has no in-product signal that a running daemon is unreachable by its own tooling, and the plugin's messages contradict what the TUI shows.

**Independent Test**: Make the local API start fail (for example a regular file at the socket path), start the engine, and check the warnings store for one entry with the stable key whose detail contains the socket path and the error.

**Acceptance Scenarios**:

1. **Given** a regular file exists at the socket path, **When** the engine starts, **Then** exactly one warnings-panel entry with the stable key is recorded, its detail names the socket path and the error and says `fabrik mcp` / PM plugin tools will report the daemon as not running, and the existing log line is still written once.
2. **Given** that entry is present, **When** a later start of the local API succeeds (including after a SIGHUP restart or a self-upgrade re-exec), **Then** the entry is resolved and no longer shown.

---

### User Story 2 - The entry says what to check (Priority: P2)

The operator reads the entry and can act on it without reading source. Where no single command fixes the problem, the detail says what to check instead of offering a misleading fix action.

**Why this priority**: An entry that cannot be acted on is only marginally better than the log line.

**Independent Test**: Inspect the recorded entry for the regular-file-in-the-way case and confirm it has no fix action and its detail covers the directory, permissions and the never-replaced non-socket file.

**Acceptance Scenarios**:

1. **Given** the start failed, **When** the entry is read, **Then** its detail tells the operator to check the socket path's directory and permissions, and states that a non-socket file at the path is never replaced.
2. **Given** no concrete command applies, **When** the entry is recorded, **Then** its fix action is empty.

---

### Edge Cases

- The local API fails on several successive starts: one entry exists, not one per failure (the log line still appears once per failure).
- The failure changes between starts (different error text): the single keyed entry reflects the latest failure.
- A start succeeds when no entry exists: nothing is recorded and nothing errors.
- A stale entry from an earlier process: it clears when the next start succeeds, including across a re-exec.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When the local API fails to start, record one warnings-panel entry through `warnings.Report` with a stable key (for example `local_api:unavailable`). Its detail names the socket path and the error, and says that `fabrik mcp` / the PM plugin tools will report the daemon as not running.
- **FR-002**: The entry is resolved (`warnings.Resolve`) when a later start succeeds, including after a SIGHUP restart or self-upgrade re-exec.
- **FR-003**: `FixAction` is set only where a concrete command applies. Otherwise the detail says what to check: the socket path's directory and permissions, and that a non-socket file at the path is never replaced.
- **FR-004**: The existing log line stays (one line per failure).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An engine test makes `srv.Start()` fail (for example a regular file at the socket path) and asserts one panel entry with the key and the path in its detail. A second start that succeeds resolves it. Shown non-vacuous by removing the `Report` call.
- **SC-002**: `TestOperatorWarningsReachThePanel` stays green.

## Notes

- Target release: 0.0.85.
- The PM plugin's troubleshooting text (PR #2222) points users at the log line until this lands.

## Assumptions

- The stable key is `local_api:unavailable` (the issue's own example); any key that is stable and unique to this condition satisfies FR-001.
- The condition is operator-level (it blocks the overseer tools and needs a person to act), so it belongs in the panel per ADR 2135 rather than being a per-item state.
- A failed start leaves the daemon otherwise healthy, so the daemon keeps running without the API; this issue changes visibility only, not startup behavior.
- Resolution is tied to the next successful start of the local API; a daemon that never restarts keeps the entry until it does.

## Out of Scope

- Retrying the local API start while the daemon runs, or replacing a non-socket file at the socket path.
- Changing how `fabrik mcp` words its "daemon not running" message.
- Any change to the PM plugin's troubleshooting text (PR #2222 owns it).
- Other operator-level conditions not already in the panel.
