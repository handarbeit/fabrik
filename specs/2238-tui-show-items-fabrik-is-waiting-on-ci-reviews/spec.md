# Feature Specification: TUI In Progress panel shows items Fabrik is waiting on

**Feature Branch**: `fabrik/issue-2238`
**Created**: 2026-10-14
**Status**: Specified
**Input**: User description: "## Problem

The TUI's In Progress panel lists running workers and items blocked on dependencies (`🔒#N <Stage> waiting for: #M`, `tui/active.go`). An item the engine is **waiting on for any other reason** doesn't appear at all.

Seen on 2026-10-13: #2232 sat at Validate-complete with `fabrik:awaiting-ci`, waiting for its PR's checks. The panel showed only three dependency-blocked items, and the header said `[poll] nothing to do`. To the operator that looks like Fabrik has forgotten the item, when in fact it is watching it on every poll. The same happens for every engine-held wait, which leads to unnecessary checking and manual nudging.

## Requirements

- **R1:** the In Progress panel shows a row for each open item the engine is holding on a known gate, in the same shape as blocked rows. Examples:
  - `⏳#2232 Validate waiting for: CI on PR #2236 (5/7 checks, since 13:52)`
  - `waiting for: review from handarbeit-pruefer (since …)`
  - `waiting for: retry at 14:03 (time slice)`
  - `waiting for: rebase (fabrik:rebase-needed)`
  - `waiting for: landing in merge train (Queued, position 2)`
  - `waiting for: human reply (paused)`
- **R2:** use the engine's existing classification. Don't add new detection. `internal/attention` already assigns each item `waiting`/`escalated`/`needs-human` with a code, a one-line summary and a next action with deadlines. The status-line field (#2048) already holds a ≤60-char \"what it's waiting on\". Render one of these, read the way the local API reads it (Store `Peek`/`Scan`, no GitHub calls).
- **R3:** separate \"Fabrik is waiting on something external\" from \"a human must act\" visually, e.g. `⏳` for waiting and `✋` for needs-human, so an operator scanning the panel can tell at once whether anything needs them.
- **R4:** the header's `nothing to do` should not read as idle while waits are open, e.g. `waiting on 2 item(s)`.
- **R5:** keep the panel bounded. Show at most N waiting rows, ordered by attention rank, with `+K more`.

## Acceptance

- A TUI test renders an item with `fabrik:awaiting-ci` and asserts a waiting row naming the PR.
- A test covers each R1 example, built from the attention classifier's output.
- No new GitHub calls from the TUI path. Reuse the local API's read-only guarantee.

## Notes

- Related: #2048 (status-line field), #1967 (attention classifier), #2124 (pipeline liveness)."

## Background

The TUI's In Progress panel shows running workers and items blocked on dependencies (`🔒#N <Stage> waiting for: #M`). An item the engine is holding for any other reason does not appear at all.

On 2026-10-13, #2232 sat at Validate-complete with `fabrik:awaiting-ci`, waiting on its PR's checks. The panel showed only three dependency-blocked items and the poll line said `nothing to do`. To an operator that looks like Fabrik has forgotten the item, when it is in fact re-checking it every poll. The same blind spot applies to every engine-held wait (CI, reviews, retry backoff, rebase, merge-train landing, a paused item awaiting a human reply). The result is unnecessary checking and manual nudging.

The engine already knows what each item is waiting on:

- The attention classifier (#1967) assigns every item a state (`waiting`, `escalated`, `needs-human`, and others) with a code, a one-line summary, a rank and a next action with deadlines.
- The display-only status-line field (#2048) holds a short "what it's waiting on" text.
- The local API already reads this information strictly from in-memory state, with no GitHub calls.

This feature is about presenting that existing knowledge in the TUI. It is not about detecting anything new.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See that an item is being waited on, and what for (Priority: P1)

An operator looks at the In Progress panel while an item is held on a gate such as CI (`fabrik:awaiting-ci`). They see a row for that item, in the same shape as a dependency-blocked row, naming the gate (for CI, the PR) and since when, and they do not mistake the item for forgotten.

**Why this priority**: This is the reported problem. Without it, the other stories add nothing.

**Independent Test**: Seed an open item carrying `fabrik:awaiting-ci` with a linked PR, render the panel, and assert a waiting row appears that names the item, its stage and the PR.

**Acceptance Scenarios**:

1. **Given** an open item at Validate with `fabrik:awaiting-ci` and a linked PR, **When** the panel renders, **Then** a waiting row for that item appears naming the stage and "CI on PR #N".
2. **Given** an item held on each of review, retry backoff, rebase, merge-train landing, and a human reply after a pause, **When** the panel renders, **Then** each shows a row whose text names that specific wait.
3. **Given** an item that is neither running, blocked nor waiting (idle), **When** the panel renders, **Then** it gets no row.

---

### User Story 2 - Tell "waiting on the system" from "a human must act" (Priority: P1)

An operator scanning the panel can tell at a glance whether anything needs them. Rows for external or engine-held waits carry one marker (`⏳`), and rows for items needing a human carry a different one (`✋`).

**Why this priority**: The point of the panel is to tell the operator whether they need to do anything. Without the distinction, the new rows add noise.

**Independent Test**: Render one item in a `waiting` attention state and one in `needs-human` (for example paused and awaiting input), and assert that the markers differ and match the state.

**Acceptance Scenarios**:

1. **Given** an item classified `waiting`, **When** the panel renders, **Then** its row starts with `⏳`.
2. **Given** an item classified `needs-human` or `escalated`, **When** the panel renders, **Then** its row starts with `✋`.

---

### User Story 3 - The header does not claim idleness while waits are open (Priority: P2)

When no worker is running and nothing is dispatchable, but items are held on waits, the header's `nothing to do` text is replaced by a count of the open waits (for example `waiting on 2 item(s)`).

**Why this priority**: The header is the first thing the operator reads. Showing "nothing to do" above open waits repeats the misleading signal even once the rows exist.

**Independent Test**: Render with two waiting items and no running worker, and assert the header text reports two open waits rather than `nothing to do`. Render with zero waiting items and assert `nothing to do` is unchanged.

**Acceptance Scenarios**:

1. **Given** the poll found nothing to dispatch and 2 items are open waits, **When** the header renders, **Then** it reads `waiting on 2 item(s)` (or equivalent) and not `nothing to do`.
2. **Given** the poll found nothing to dispatch and no item is waiting, **When** the header renders, **Then** it still reads `nothing to do`.

---

### User Story 4 - The panel stays bounded (Priority: P2)

With many waiting items, the panel shows only a fixed maximum number of waiting rows, most urgent first, followed by a `+K more` line, so it never crowds out running workers.

**Why this priority**: Without a bound, a large board of held items would push running work off screen.

**Independent Test**: Render N+3 waiting items of mixed rank and assert exactly N waiting rows, ordered by attention rank, plus `+3 more`.

**Acceptance Scenarios**:

1. **Given** more waiting items than the maximum, **When** the panel renders, **Then** only the maximum number of rows are shown, highest attention rank first, followed by `+K more` with K the exact remainder.
2. **Given** waiting items at or under the maximum, **When** the panel renders, **Then** no `+K more` line appears.

---

### Edge Cases

- An item already shown as running (worker) or dependency-blocked must not also get a waiting row; one row per item.
- An item whose attention state is unknown (state not yet loaded after a restart) gets no waiting row rather than a guessed one.
- Closed or Done items never show a waiting row.
- Where a detail is unavailable from in-memory state (for example the "5/7 checks" counts or the "since" time), the row omits that detail instead of showing a wrong or placeholder value.
- A waiting row's text longer than the panel width is truncated the same way existing rows are.
- A wait with a deadline (retry time, review timeout) shows it only when the classifier supplies a known deadline; an unknown deadline is never rendered as "now".
- Rows appear and disappear as an item's state changes, without waiting for a full poll cycle where running workers' rows already update that way.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The In Progress panel MUST render one row for each open, managed item whose attention classification is a wait (`waiting`), a human-needed state (`needs-human`, `escalated`), or a pending decision, in the same visual shape as the existing dependency-blocked rows (`<marker>#N <Stage> waiting for: <description>`).
- **FR-002**: The row's description MUST name the specific gate the item is held on. At minimum it covers: CI (naming the PR), review (naming the reviewer), retry/backoff (naming the retry time and reason when known), rebase needed, merge-train landing (Queued, with position when known), and a human reply after a pause.
- **FR-003**: The row content MUST come from the engine's existing classification: the attention classifier's summary/code/next action, or the status-line text. No new detection logic is added for any gate.
- **FR-004**: Data for the rows MUST be read the way the local API reads it (from the in-memory Store via non-fetching reads, plus already-guarded engine state), and the TUI path MUST make no GitHub or other network calls.
- **FR-005**: Rows for `waiting` items MUST carry the `⏳` marker, and rows for items needing a human (`needs-human`, `escalated`) MUST carry the `✋` marker, so the two classes are visually distinct.
- **FR-006**: An item shown as a running worker row or a dependency-blocked row MUST NOT also appear as a waiting row.
- **FR-007**: The number of waiting rows MUST be capped at a fixed maximum. Rows MUST be ordered by attention rank (most urgent first, with a stable tiebreak), and when items are omitted, a `+K more` line with the exact omitted count MUST follow.
- **FR-008**: When no worker is running and nothing is dispatchable but at least one item is a waiting row candidate, the header MUST show a count of open waits (for example `waiting on 2 item(s)`) instead of `nothing to do`. When there are no open waits the header text MUST be unchanged.
- **FR-009**: Row details that the classifier cannot supply (check counts, since-times, deadlines) MUST be omitted rather than fabricated, and an item with unknown classification MUST get no row.
- **FR-010**: Automated tests MUST cover: the `fabrik:awaiting-ci` row naming the PR; one row for each example gate in FR-002, built from the classifier's output; the marker distinction (FR-005); the cap and `+K more` (FR-007); the header change (FR-008); and a check that the TUI path performs no GitHub calls.

### Key Entities

- **Waiting row**: A single In Progress panel line for an open item the engine is holding on a gate or that needs a human. It has a marker (`⏳`/`✋`), item number, stage, and a "waiting for" description.
- **Attention classification**: The existing per-item result (state, code, summary, rank, next action with deadlines) from the attention classifier. It is the single source for the row's content and ordering.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For an item held on any gate named in FR-002, the operator can identify the item and what it is waiting on from the panel alone, without opening GitHub or the logs.
- **SC-002**: The 2026-10-13 situation (an item at Validate with `fabrik:awaiting-ci` while no worker runs) no longer shows a panel with no row for that item and a header saying `nothing to do`.
- **SC-003**: An operator can tell whether any item needs them (`✋` rows present or absent) from one glance at the panel.
- **SC-004**: With any number of waiting items, the panel adds no more than the maximum number of waiting rows plus one overflow line.
- **SC-005**: Rendering the waiting rows makes zero GitHub calls, verified by a test.

## Assumptions

- The rows reuse the attention classifier's summary and next action as the description. The status-line text (#2048) is an alternative or fallback source only where the classifier gives no usable text. Which one is shown is a presentation choice, not a new requirement on either.
- The example wording in the issue (for instance `5/7 checks, since 13:52`) is illustrative. Counts and times appear only where the classifier or the Store already holds them; otherwise they are omitted (FR-009).
- The row cap is a small fixed number (default 5), chosen so rows do not crowd out workers. It is not operator-configurable in this issue.
- Pending-decision items (an item with a completed stage waiting for a human to approve the next step) count as human-needed and use `✋`.
- Both the panel rows and the header count draw from the same set of items, so the header count matches the rows plus the `+K more` remainder.

## Out of Scope *(optional)*

- Any new detection of wait conditions, or changes to the attention classifier or the status-line field.
- Making the waiting-row cap operator-configurable.
- Interactive actions on waiting rows (for example resuming, nudging or navigating to an item).
- Changes to the local API, MCP tools or channel events.
- Changes to the poll log line text itself (`[poll] nothing to do`) outside the TUI header.

## Source References *(optional)*

- #2048: display-only status-line field
- #1967: attention classifier and local API
- #2124: pipeline liveness
- #2232: the item that exposed the gap (2026-10-13)
