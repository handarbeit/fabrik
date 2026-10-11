# Feature Specification: Operator-facing failures always reach the TUI warnings panel

**Feature Branch**: `fabrik/issue-2135`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "The TUI doesn't really have a way to highlight loud failures. I see a warnings panel that is inconsistently used — these failures should be added to that."

## Background

The TUI has a persistent warnings panel backed by a dismissable, de-duplicated store. Only six conditions use it today — source staleness, repository access, `allow_auto_merge`, version skew, stage drift and undeclared reviewers — all pre-flight configuration checks. Other operator-facing problems of the same kind reach only the scrolling log (`[startup] warning: …` lines and similar), where they are easy to miss.

The skill-naming chain this issue belongs to adds several new failures that must be *loud*: a stage skill that does not resolve, two loaded plugins with the same name, a configured plugin directory that is missing, deprecated bare skill names, and a pending `fabrik upgrade` migration. "Loud" has to mean something an operator watching the TUI cannot miss. This issue makes the warnings panel that place, and defines which conditions belong there.

The panel is for **operator-level** conditions (configuration, environment, the engine itself). Per-item states that need a human — paused, awaiting input, escalated — already have their own surface (the board, the attention classifier, item comments) and are not moved here.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A configuration fault is visible without reading the log (Priority: P1)

An operator starts Fabrik with a stage pointing at a skill that does not exist. The TUI shows a warnings-panel entry naming the stage, the reference and where it looked; the entry clears by itself once the configuration is fixed.

**Why this priority**: A failure only in the log is effectively silent in a TUI session.

**Independent Test**: Introduce one of the conditions, start the engine, see the panel entry; fix it, see it clear.

**Acceptance Scenarios**:

1. **Given** a qualifying condition occurs, **When** the TUI is open, **Then** a panel entry appears with a title, detail and (where one exists) the fix.
2. **Given** the condition is resolved, **When** the engine next checks it, **Then** the entry clears without operator action.
3. **Given** the operator dismisses an entry, **When** the same condition recurs unchanged, **Then** it stays dismissed; if its detail changes, it reappears.

---

### User Story 2 - The panel is used consistently (Priority: P2)

Every existing operator-level warning that today reaches only the log is reviewed; those that meet the definition are moved to the panel.

**Why this priority**: An inconsistently used panel teaches operators to ignore it.

**Independent Test**: The audit list in the PR maps every existing operator-level log warning to "panel" or "log only, because …".

**Acceptance Scenarios**:

1. **Given** the audit, **When** an operator-level warning meets the definition, **Then** it is recorded in the panel as well as logged.

---

### Edge Cases

- Headless runs (no TUI): entries are still recorded and logged; nothing depends on the TUI being open.
- A condition that recurs every poll must not churn the panel (one entry, refreshed `last seen`).
- Many entries at once: the panel stays usable (existing windowing).
- A condition that is checked only at startup (not every poll) must still clear on the next check after it is fixed, including across a restart, so a stale entry never outlives its cause.
- A condition with no automatic fix still gets an entry; the panel shows the title and detail without a fix action.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Document what belongs in the warnings panel: operator-level conditions that block or degrade work and need a person to act; per-item attention states do not.
- **FR-002**: The skill-resolution and plugin-loading failures added by this chain, the bare-name deprecation notice and a pending `fabrik upgrade` migration are recorded in the panel with a clear fix.
- **FR-003**: Each entry clears itself when its condition no longer holds.
- **FR-004**: Every existing operator-level log-only warning is audited against FR-001, and those that qualify are recorded in the panel.
- **FR-005**: One shared way to record and clear a condition, so new code cannot reach only the log by accident; the as-built docs state the rule.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Every condition listed in FR-002 produces a panel entry and clears when fixed — one test per condition.
- **SC-002**: The audit covers 100% of existing operator-level log warnings, each with a recorded disposition.
- **SC-003**: No condition that recurs every poll creates more than one panel entry.

## Assumptions

- The existing warnings store (dismissal, de-duplication, persistence) is kept; this issue widens its use and states its rules rather than redesigning it.
- Per-item escalations stay on the board and in item comments.
- The skill-resolution, plugin-loading, bare-name deprecation and `fabrik upgrade` migration conditions are introduced by sibling issues in the skill-naming chain. This issue provides the shared record/clear path and the rule; where a sibling has not landed, its condition is wired to the panel as it lands, and the one-test-per-condition criterion (SC-001) applies to each condition that exists at that point.
- The audit's definition of "operator-level" is FR-001; borderline log lines are dispositioned individually with a stated reason rather than defaulting to the panel.

## Out of Scope *(optional)*

- A new TUI panel or layout redesign.
- Pushing these entries over the MCP channel (possible follow-up).

## Source References *(optional)*

- `warnings/warnings.go` (store), `tui/warnings.go` (panel).
- Current recorders: `engine/staleness.go`, `engine/startup.go` (repo access, `allow_auto_merge`), `engine/upgrade.go` (version skew), `stages/drift.go` (stage drift, undeclared reviewers).
- About nine `[startup] warning` log lines in `engine/`/`cmd/` that bypass the panel today.
