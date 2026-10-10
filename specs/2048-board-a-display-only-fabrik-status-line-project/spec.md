# Feature Specification: Display-only "Fabrik" status-line project field, with the merge train as its first writer

**Feature Branch**: `fabrik/issue-2048`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "board: a display-only 'Fabrik' status-line project field, with the merge train as its first writer

## Problem

On the project board, a Queued card looks the same in every phase of the train: waiting its turn, waiting for a slot, resolving conflicts, waiting on trial CI, bisecting or landing. A maintainer can't tell whether the train is working or stuck without reading the daemon log or the TUI. The same goes for most other engine states, such as a paused or blocked item, or one waiting on CI or review.

Report: #2042 (on the public triage board).

## Requirements

**R1. A general status-line field.**
- Fabrik writes one line, saying what the item is doing or waiting on right now, into a ProjectV2 **text** field. The name is configurable and defaults to `Fabrik`.
- This is Fabrik's first display-only board field, so build it as a general mechanism (one writer, called from engine state transitions), not something train-specific.

**R2. Merge-train values (required in this issue).** For example:
- `queued · batch of 3`
- `trial #1692 · resolving conflicts`
- `trial #1692 · CI running`
- `bisecting · step 2 of 3`
- `landing`
- `deferred: overlaps #1549`

When the singleton catch-up path is in play, the line names whichever PR's CI it is waiting on.

**R3. Other states (included where cheap; the rest in a follow-up).** All of these come from transitions the engine already logs:
- `Implement · running`, `Validate · comment review`
- `blocked by #1568`
- `paused: <one-line reason>`
- `claude-limit until HH:MM`
- `awaiting CI on PR #1615`

**R4. Rules.**
- **Display only.** No handler reads the field back. Engine state, meaning labels and the store, stays the source of truth.
- **Write only on a transition**, and skip the write when the value hasn't changed. Never write per turn.
- **Clear the field when the item reaches Done.**
- **Keep it short:** truncate to about 60 characters.
- **If the field doesn't exist on the board, skip silently** and log once at startup.

**R5. Webhook echo.**
- Every field write comes back as a `projects_v2_item` edit. The board cache (`boardcache/delta.go`) and the reconcile drift check must treat a change to the display field alone as a no-op.
- It must not be counted as drift, trigger a deep fetch, invalidate the item, or wake the poll.
- Without this, each write would cost a reconcile cycle.

**R6. No `fabrik:merge-train` label.** The report also proposes a label. Leave it out: labels are engine state in Fabrik, and adding and removing one on every member of every train adds issue events and cache churn for information the field already shows. The board's filter on the text field covers "in the train now".

**R7. Budget note for docs.** ProjectV2 mutations don't count against the GraphQL rate-limit budget, which is why a per-transition write is acceptable. Write-on-change still applies.

## Scope

- In scope: the field writer, the train's transition hooks, the cheap non-train states from R3, the cache echo suppression, config (`project_fields.status_line: <name>|off`) and docs.
- Out of scope: the numeric/date fields (next issue), any gating on the field.
- Mention #2042 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2042. This issue's own number gets `Closes`.

## Acceptance

- Unit tests:
  - each train phase transition writes the expected line once;
  - an unchanged value writes nothing;
  - Done clears it;
  - a missing field is skipped silently;
  - an incoming `projects_v2_item` edit that changes only the display field causes no drift, deep fetch or wake.
- Wire-contract fixture for the text-field update mutation (`github/testdata/`).
- `docs/USER_GUIDE.md` covers the field and its config. `docs/state-machine.md` notes the field is display-only. Regenerate `docs/llms-full.txt`.
- Neutralisation of the echo-suppression test."

## Background

On the project board, a card in the merge train's `Queued` column looks identical in every phase of the train: waiting its turn, waiting for a worker slot, resolving conflicts, waiting on trial CI, bisecting, or landing. A maintainer cannot tell whether the train is working or stuck without reading the daemon log or the TUI. The same blindness applies to most other engine states — a paused or blocked item, or one waiting on CI or review. The report is #2042 on the public triage board; it also proposed a `fabrik:merge-train` label, which this spec deliberately rejects (see Out of Scope).

This is Fabrik's first **display-only** board field: a single human-readable line, written by the engine into a ProjectV2 text field, that states what an item is doing or waiting on right now. Engine state (labels, the in-memory store) remains the only source of truth; the field is a one-way projection for people looking at the board. Because every field write is echoed back by GitHub as a `projects_v2_item` edit webhook, the board cache and reconcile drift check must not mistake those echoes for real changes, or each write would cost a reconcile cycle. ProjectV2 mutations do not count against the GraphQL rate-limit budget, which is why a per-transition write is acceptable; write-on-change still applies.

The merge-train phase values build on the overlap-aware deferral work in #2047 (now closed; the `deferred: overlaps #N` line reflects its outcome) and the singleton catch-up path (#2044).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - See what the merge train is doing from the board (Priority: P1)

As a maintainer watching the board, I want each Queued card to show a one-line train phase, so I can tell working from stuck without opening logs.

**Why this priority**: This is the reported pain point and the explicit required deliverable of this issue.

**Independent Test**: With a test board that has a text field named `Fabrik`, drive a train through queue → trial → conflict resolution → CI → bisect → landing and observe the field value after each transition.

**Acceptance Scenarios**:

1. **Given** a member joins a batch of three, **When** the batch is formed, **Then** its field reads `queued · batch of 3`.
2. **Given** a trial is assembling and a conflict is being resolved, **When** the phase begins, **Then** the field reads `trial #<N> · resolving conflicts`, and when trial CI starts it reads `trial #<N> · CI running`.
3. **Given** a red batch is bisected, **When** each bisection step starts, **Then** the field reads `bisecting · step <i> of <n>`.
4. **Given** the batch is landing, **When** landing starts, **Then** the field reads `landing`.
5. **Given** a member was held back from a batch for overlapping another member, **When** the deferral occurs, **Then** the field reads `deferred: overlaps #<M>`.
6. **Given** a singleton catch-up is waiting on a PR's CI, **When** it waits, **Then** the line names the PR whose CI it is waiting on.

---

### User Story 2 - Other engine states show the same way (Priority: P2)

As a maintainer, I want paused, blocked, CI-waiting, running and Claude-limit states shown on the card too, so the whole board is legible, not just the train.

**Why this priority**: Same mechanism, lower urgency; the issue asks for the cheap ones now and the rest in a follow-up.

**Independent Test**: Trigger each transition on a test item and read the field.

**Acceptance Scenarios**:

1. **Given** a stage starts, **When** the worker is dispatched, **Then** the field reads like `Implement · running`; during comment processing it reads like `Validate · comment review`.
2. **Given** an item becomes blocked on a dependency, **When** the block is applied, **Then** the field reads `blocked by #<N>`.
3. **Given** an item is paused, **When** the pause is applied, **Then** the field reads `paused: <one-line reason>`.
4. **Given** the account hits a Claude usage limit, **When** the suspension is recorded, **Then** an affected item reads `claude-limit until HH:MM`.
5. **Given** a stage is waiting on CI, **When** the wait begins, **Then** the field reads `awaiting CI on PR #<N>`.

---

### User Story 3 - The field is safe, quiet and optional (Priority: P1)

As an operator, I want the field to cost nothing when absent, never churn the cache, and never influence engine behavior.

**Why this priority**: Without the quiet-echo and no-write-when-unchanged guarantees the feature would make the engine slower and noisier.

**Independent Test**: Unit tests per rule below, plus the neutralisation check on the echo-suppression test.

**Acceptance Scenarios**:

1. **Given** the same line would be written twice in a row, **When** the second transition fires, **Then** no write is made.
2. **Given** an item reaches Done, **When** the Done transition completes, **Then** the field is cleared.
3. **Given** the board has no field with the configured name (or the config is `off`), **When** any transition fires, **Then** nothing is written, no error is logged per transition, and a single startup log line says the field is unavailable.
4. **Given** a `projects_v2_item` edit that changes only the display field, **When** the webhook is applied or a reconcile runs, **Then** it causes no drift count, no deep fetch, no item invalidation and no poll wake.

---

### Edge Cases

- The configured field exists but is not a text field: treated as missing (skipped, logged once at startup).
- The field write fails (API error, rate limit): non-fatal; logged, not retried in a way that blocks the engine, and the engine behaves exactly as if the write had succeeded.
- Line longer than the limit: truncated to about 60 characters, still readable (ends with an ellipsis marker).
- Rapid successive transitions: only the final distinct value matters; each distinct change may write, an unchanged value never does.
- Engine restart: the in-memory "last written value" is lost, so the first transition after a restart may rewrite an identical value once; this is acceptable and must not be read back from the board.
- A webhook echo arriving for a write whose field value matches nothing the engine tracks: still a no-op.
- An edit to the display field made by a human on the board: also treated as a no-op by the cache; the engine overwrites it on its next transition and never reads it.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Fabrik writes one line describing what an item is currently doing or waiting on into a ProjectV2 **text** field on the board, through a single general writer invoked from engine state transitions (not train-specific code).
- **FR-002**: The field name is configurable via `project_fields.status_line: <name>|off`, defaulting to `Fabrik`; `off` disables the feature entirely.
- **FR-003**: The merge train writes the phase lines `queued · batch of N`, `trial #<N> · resolving conflicts`, `trial #<N> · CI running`, `bisecting · step i of n`, `landing` and `deferred: overlaps #<M>` at the corresponding transitions.
- **FR-004**: When the singleton catch-up path is waiting on CI, the line names the PR whose CI is being waited on (the member's PR, as appropriate to the phase).
- **FR-005**: Where the transition is already a distinct engine event, the following are also written: `<Stage> · running`, `<Stage> · comment review`, `blocked by #<N>`, `paused: <one-line reason>`, `claude-limit until HH:MM`, `awaiting CI on PR #<N>`. States without a cheap hook are left for a follow-up.
- **FR-006**: The field is display-only: no engine decision reads the field's value back from the board or from any cache of it.
- **FR-007**: A write is made only on a state transition and only when the new line differs from the last line the engine wrote for that item; no write happens per Claude turn or per poll.
- **FR-008**: When an item reaches Done, the field is cleared.
- **FR-009**: Lines are truncated to about 60 characters.
- **FR-010**: If the configured field does not exist on the board (or is not a text field), all writes are skipped silently, with exactly one log line at startup stating this.
- **FR-011**: A failed field write never fails, retries-in-a-blocking-way, pauses or otherwise alters the transition that triggered it.
- **FR-012**: A `projects_v2_item` edit that changes only the display field is a no-op in the board cache and the reconcile drift check: it is not counted as drift, triggers no deep fetch, does not invalidate the item and does not wake the poll.
- **FR-013**: No `fabrik:merge-train` label (or any new label) is introduced for this feature.
- **FR-014**: A wire-contract fixture in `github/testdata/` covers the text-field update (and clear) mutation, validated by the existing wire-contract test layer.
- **FR-015**: `docs/USER_GUIDE.md` documents the field, its states and the `project_fields.status_line` config, including the note that the writes do not consume the GraphQL rate-limit budget while write-on-change still applies; `docs/state-machine.md` states that the field is display-only; `docs/llms-full.txt` is regenerated.
- **FR-016**: Tests cover: each train phase transition writes the expected line once; an unchanged value writes nothing; Done clears it; a missing field is skipped silently; and an incoming display-field-only edit causes no drift, deep fetch or wake — with the echo-suppression test shown to fail when the suppression is neutralised.

### Key Entities *(if applicable)*

- **Status line**: The single short text value shown per board item, derived from engine state at each transition.
- **Display field**: The ProjectV2 text field (default name `Fabrik`) that holds the status line; display-only, never read back.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A maintainer can tell the current merge-train phase of any Queued card from the board alone, for every phase listed in FR-003.
- **SC-002**: Across a full train run, the number of field writes per item equals the number of distinct line changes — zero writes for unchanged values and none per turn or per poll.
- **SC-003**: A display-field-only webhook echo produces zero drift counts, zero deep fetches, zero item invalidations and zero poll wakes.
- **SC-004**: On a board lacking the field, behavior is identical to today apart from one startup log line.
- **SC-005**: Every line written is at most about 60 characters.
- **SC-006**: The new unit tests, wire-contract fixture and the neutralisation check pass in the normal `go test -race ./...` run.

## Assumptions

- The field is a plain text field the operator creates on the board; Fabrik does not create it (board-structure administration is out of scope here).
- The `·` separator and the line wording in the examples are the intended wording; exact strings for non-train states follow the same style.
- Truncation uses an ellipsis marker and counts characters, not bytes.
- `claude-limit until HH:MM` uses the daemon's local time.
- The "last written value" cache is in memory only and is not persisted across restarts; one redundant identical write after a restart is acceptable.
- Because #2047 is closed, the `deferred: overlaps #N` state is reachable and in scope.
- Config absent means the default `Fabrik` field name is used (feature on); `off` is the only way to disable it.

## Out of Scope *(optional)*

- Numeric and date project fields (the next issue).
- Any gating, filtering or decision logic that reads the field.
- A `fabrik:merge-train` label (rejected: labels are engine state and would add issue events and cache churn).
- Automatic creation of the field on the board.
- Status lines for engine states without a cheap, already-logged transition (follow-up).

## Source References *(optional)*

- Report: #2042 (public triage board). Mention only; do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2042 in the PR body.
- Related: #2047 (overlap-aware batching), #2044 (singleton catch-up).
