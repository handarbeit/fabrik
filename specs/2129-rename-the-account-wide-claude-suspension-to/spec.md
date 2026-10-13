# Feature Specification: Rename the account-wide Claude "suspension" to "usage limited"

**Feature Branch**: `fabrik/issue-2129`
**Created**: 2026-10-11
**Status**: Specified
**Input**: User description: "Rename the account-wide Claude 'suspension' to 'usage limited' before the MCP surfaces ship

## Problem

\"Suspended\" means two different things across Fabrik's user-visible surfaces, and #2120 needs the word for something else.

The account-wide Claude state that Fabrik calls a \"suspension\" is, in every case, a **usage limit**. Its only trigger is a worker exit that the CLI's structured result classifies as a usage-limit hit:
- `terminal_reason == \"blocking_limit\"`; or
- `terminal_reason == \"api_error\"` with `api_error_status == 429` (ADR-1811).

`activateClaudeSuspension` is called only from those three classified paths (`engine/item.go`, `engine/comments.go`, `engine/merge_train.go`).

Calling it \"suspended\" is vaguer than the truth, and it collides with #2120's proposed **Suspended** item state (a stage is complete and the item is waiting for a human decision to proceed). The GraphQL/REST budget banner also says \"polling suspended\", which is a third meaning.

The MCP surfaces that carry the word ship for the first time in 0.0.84 (v0.0.83 is the latest release). Renaming them now costs nothing. Renaming them after the release breaks subscribers.

## Requirements

- **R1 — Rename the account-wide Claude state to \"usage limited\" on every user-visible surface:**
  - **Health:** `fabrik_health.claude_usage_limit.suspended` / `suspended_until` → `usage_limited` / `usage_limited_until` (or the closest equivalent; keep the parent key).
  - **Events:**
    - `claude-limit-suspended` → `claude-usage-limited`;
    - `claude-limit-lifted` → `claude-usage-limit-lifted`;
    - update `internal/channelevents` and the shim's tool descriptions.
  - **Attention:** the deadline kind `claude-suspension-end` → `claude-usage-limit-end`, plus any summary or meaning text that says \"suspension\".
  - **TUI and logs:** the banner \"Claude usage limit hit — dispatch suspended\" and the `[claude-limit]` log lines say \"usage limited\" / \"dispatch held until <reset>\" rather than \"suspended\".
  - **GraphQL/REST budget banner:** reword \"polling suspended\" as well (e.g. \"polling paused for the rate limit\"), so that \"suspended\" is left only for #2120's item state.
- **R2 — Internal names.** Rename the engine identifiers (`claudeSuspendedUntil`, `activateClaudeSuspension`, `clearClaudeSuspension`, `claudeSuspendedUntilTime`, `channelSuspendedUntil`, `ClaudeSuspendedUntil` in `internal/attention`) to usage-limit names, so that a grep for \"suspend\" finds only the #2120 state once it lands.
- **R3 — Labels unchanged.** `fabrik:claude-limit` and `fabrik:clear-claude-limit` are already accurate and stay as they are. The `fabrik_clear_claude_limit` tool name also stays.
- **R4 — Docs.**
  - `docs/USER_GUIDE.md`: the Claude usage-limit section, the MCP tool and event tables, and the rate-limit banner text.
  - `docs/state-machine.md` (the #1968 channel-events note and the `fabrik:claude-limit` row).
  - Regenerate `docs/llms-full.txt`.
  - In-place edits to existing ADRs are not required. Add a short dated note to ADR-1966-b recording the rename.

## Scope

In scope: names and wording only. Behaviour is unchanged.

Out of scope:
- the #2120 state itself;
- changing how a usage limit is detected or cleared.

## Acceptance

- **Health and events:** tests assert the new JSON field names in `fabrik_health` and the new event type strings. The old strings (`claude-limit-suspended`, `\"suspended\"` under `claude_usage_limit`) appear nowhere in `internal/`, `engine/`, `cmd/` or `docs/` outside ADRs and historical notes. A grep assertion in the PR body is acceptable.
- **Behaviour:** the existing usage-limit tests pass unchanged apart from names. The sim scenarios that cover the limit and its lift still pass.
- `go test -race ./...` is clean.

Mentioned for context: #1966. Mention it by bare number only in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #1966."

## Background

Fabrik calls its account-wide Claude hold a "suspension". Every trigger for it is a usage-limit hit, classified from the CLI's structured result (`blocking_limit`, or an `api_error` with status 429; ADR-1811). "Suspended" is therefore vaguer than the truth, and the word is now contested in three ways:

1. The Claude usage-limit hold, the subject of this issue, appears in the health report, channel events, attention deadlines, the TUI banner, logs and docs.
2. The GraphQL/REST budget banner says "polling suspended", which names a different condition (a GitHub rate limit).
3. #2120 proposes a **Suspended** item state: a stage is complete and the item is waiting for a human decision to proceed. It needs the word.

The MCP surfaces that carry the word (the `fabrik_health` fields and the channel event types) first ship in 0.0.84; v0.0.83 is the latest release. Renaming them before that release is free, and renaming them afterwards would break subscribers that match on event type strings or read the health fields.

This is a naming and wording change only. How a usage limit is detected, how long it lasts and how it clears do not change.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - MCP consumers see "usage limited" vocabulary (Priority: P1)

An overseer session or MCP client reads `fabrik_health` and subscribes to channel events. The Claude usage-limit state reads as "usage limited" in health fields, event type names and attention deadline kinds, and "suspended" does not appear for this state.

**Why this priority**: These are the only surfaces whose names become a compatibility contract at 0.0.84. Getting them wrong after release breaks subscribers.

**Independent Test**: Put the engine into a usage-limit hold, then read `fabrik_health` and the published channel events. Check the field names and event type strings.

**Acceptance Scenarios**:

1. **Given** an account-wide usage-limit hold is active, **When** a client calls `fabrik_health`, **Then** the `claude_usage_limit` object reports `usage_limited: true` and `usage_limited_until: <reset time>`, with no `suspended` or `suspended_until` keys.
2. **Given** a subscriber is attached, **When** the hold begins, **Then** it receives an event of type `claude-usage-limited`, and when the hold ends it receives `claude-usage-limit-lifted`.
3. **Given** an item's attention classification includes the hold deadline, **When** the deadline is rendered, **Then** its kind is `claude-usage-limit-end` and no summary or meaning text says "suspension".
4. **Given** the shim's tool descriptions and the event catalogue, **When** they are listed, **Then** they describe the hold as a usage limit and list only the new event type names.

---

### User Story 2 - Operators see "usage limited" in the TUI and logs (Priority: P2)

An operator watching the TUI or the engine log during a usage-limit hit sees that Claude dispatch is held because of the usage limit, and until when.

**Why this priority**: These are user-visible but not a compatibility contract. They must agree with the MCP wording so one state has one name.

**Independent Test**: Trigger a usage-limit hit in a sim or unit scenario. Check the banner text and the `[claude-limit]` log lines.

**Acceptance Scenarios**:

1. **Given** a usage-limit hit, **When** the TUI banner is shown, **Then** it reads along the lines of "Claude usage limit hit — dispatch held until <reset>" and contains no form of "suspend".
2. **Given** the hold begins and later lifts, **When** the `[claude-limit]` log lines are written, **Then** they say "usage limited" / "dispatch held until <reset>" instead of "suspended".

---

### User Story 3 - The budget banner no longer claims "suspended" (Priority: P2)

When the GitHub GraphQL or REST budget is exhausted, the banner and the matching log line say polling is paused for the rate limit, not "suspended".

**Why this priority**: This frees the word "suspended" for #2120's item state.

**Independent Test**: Render the rate-limit banner for REST-only and for REST + GraphQL exhaustion, and check the text.

**Acceptance Scenarios**:

1. **Given** the REST and GraphQL budgets are exhausted, **When** the banner is rendered, **Then** it says polling is paused for the rate limit (for example "polling paused for the rate limit") and does not contain "suspended".
2. **Given** only the REST budget is exhausted, **When** the banner is rendered, **Then** the same rewording applies and the existing semantics are preserved (it still does not claim a stand-down that does not happen).

---

### User Story 4 - Maintainers can grep for "suspend" and find only the new item state (Priority: P3)

A maintainer implementing #2120 greps for "suspend" and finds no leftovers from the Claude usage-limit hold.

**Why this priority**: It avoids two meanings of one word in the codebase and docs, but has no runtime effect.

**Independent Test**: Grep `engine/`, `internal/`, `cmd/`, `tui/` and `docs/` for "suspend" and review each hit.

**Acceptance Scenarios**:

1. **Given** the rename is complete, **When** the engine identifiers are listed, **Then** none of `claudeSuspendedUntil`, `activateClaudeSuspension`, `clearClaudeSuspension`, `claudeSuspendedUntilTime`, `channelSuspendedUntil` or `ClaudeSuspendedUntil` remains; each has a usage-limit name.
2. **Given** the docs, **When** they are read, **Then** the Claude usage-limit section, the MCP tool and event tables, the budget banner text, the `docs/state-machine.md` #1968 channel-events note and the `fabrik:claude-limit` row all use the new names, and `docs/llms-full.txt` is regenerated to match.
3. **Given** ADR-1966-b, **When** it is read, **Then** it carries a short dated note recording the rename and the old and new names.

---

### Edge Cases

- **Unrelated meanings of "suspend".** Some hits are not this state and are not renamed: a GitHub App deleted or *suspended by GitHub* (`internal/githubauth`), the TUI suspending itself to launch an inline subprocess (`tui/commands.go`, `watch/model.go`), and the vendored GitHub GraphQL schema fixture. The "grep finds only #2120" goal applies to the Claude usage-limit state, not to these.
- **Historical text.** ADRs, release notes (`release-notes/v0.0.*.md`) and earlier specs describe past behaviour and are not rewritten. Only ADR-1966-b gets a dated note.
- **Persisted channel state.** The channel hub persists subscriptions, held-event queues and dedup counters on disk. A held entry or subscription written by a pre-rename build could name an old event type string. The implementer must make sure an old persisted entry cannot crash startup or leave a subscriber permanently unmatched.
- **Event type wording in tool schemas.** Anywhere the shim enumerates event types (subscribe tool descriptions, catalogue, glob examples) must use the new names, so a subscriber filtering by event type does not silently match nothing.
- **Labels and tool name.** `fabrik:claude-limit`, `fabrik:clear-claude-limit` and the `fabrik_clear_claude_limit` tool keep their names. Nothing should rename them as a side effect of a broad search-and-replace.
- **Test-only and TUI-event field names.** The TUI alert event's `Suspended` field and test helper names follow the rename so no stale name remains. These are internal and have no compatibility concern.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: `fabrik_health.claude_usage_limit` reports the hold with the keys `usage_limited` and `usage_limited_until`. The parent key `claude_usage_limit` is unchanged, and the old `suspended` and `suspended_until` keys are no longer emitted.
- **FR-002**: The channel event type `claude-limit-suspended` is renamed `claude-usage-limited`, and `claude-limit-lifted` is renamed `claude-usage-limit-lifted`, in the event catalogue (`internal/channelevents`), every engine publisher, and the shim's tool descriptions.
- **FR-003**: The attention deadline kind `claude-suspension-end` is renamed `claude-usage-limit-end`. Any attention summary or meaning text that says "suspension" or "suspended" for this state is reworded to "usage limit" / "usage limited".
- **FR-004**: The TUI banner shown while the hold is active reads "Claude usage limit hit — dispatch held until <reset>" (or equivalent wording) and contains no form of "suspend".
- **FR-005**: Engine log lines tagged `[claude-limit]` describe the state as "usage limited" and the dispatch as "held until <reset>", with no form of "suspend".
- **FR-006**: The GraphQL/REST budget banner and its matching log text no longer say "polling suspended". They say polling is paused for the rate limit, and their meaning is unchanged.
- **FR-007**: The internal identifiers `claudeSuspendedUntil`, `activateClaudeSuspension`, `clearClaudeSuspension`, `claudeSuspendedUntilTime`, `channelSuspendedUntil` and `ClaudeSuspendedUntil` (in `internal/attention`) are renamed to usage-limit names. Other identifiers, struct fields and comments that refer to this state as a suspension (for example the TUI alert event's `Suspended` field) are renamed likewise.
- **FR-008**: The labels `fabrik:claude-limit` and `fabrik:clear-claude-limit` and the tool name `fabrik_clear_claude_limit` are unchanged.
- **FR-009**: `docs/USER_GUIDE.md` (the Claude usage-limit section, the MCP tool and event tables, the rate-limit banner text) and `docs/state-machine.md` (the #1968 channel-events note and the `fabrik:claude-limit` row, plus any other mention of this state) use the new names. `docs/stage-lifecycle.md` and `plugin/fabrik-workflows/LABELS.md` are updated where they refer to this state. `docs/llms-full.txt` is regenerated in the same change.
- **FR-010**: ADR-1966-b gets a short dated note recording the rename, with the old and new names. No other existing ADR is edited in place.
- **FR-011**: Tests assert the new `fabrik_health` JSON field names and the new event type strings. Existing usage-limit tests and the sim scenarios covering the limit and its lift pass with only name changes, so behaviour is demonstrably unchanged.
- **FR-012**: The old strings `claude-limit-suspended`, `claude-limit-lifted`, `claude-suspension-end` and a `suspended` key under `claude_usage_limit` appear nowhere in `internal/`, `engine/`, `cmd/`, `tui/` or `docs/`, except in ADRs and historical notes (release notes, earlier specs). The PR body includes a grep that shows this.
- **FR-013**: No behaviour changes. The detection of a usage-limit hit, the computation of the reset time, the dispatch hold, the label handling and the lift and clear paths are untouched.

### Key Entities

- **Claude usage-limit hold**: the account-wide state, set when a worker exit is classified as a usage-limit hit, in which Claude dispatch is held until the CLI-reported reset time. It was previously named "suspension".
- **Usage-limit health fields**: the `claude_usage_limit` object in `fabrik_health` (`usage_limited`, `usage_limited_until`).
- **Usage-limit channel events**: `claude-usage-limited` (hold begins) and `claude-usage-limit-lifted` (hold ends).
- **Usage-limit attention deadline**: the deadline kind `claude-usage-limit-end`.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A grep for "suspend" (case-insensitive) over `engine/`, `internal/`, `cmd/`, `tui/` and `docs/` finds no hit that refers to the Claude usage-limit hold or the budget banner. Remaining hits are only the unrelated meanings listed under Edge Cases.
- **SC-002**: A client reading `fabrik_health` during a hold sees `usage_limited` and `usage_limited_until` and no `suspended` keys. A subscriber sees `claude-usage-limited` and `claude-usage-limit-lifted` and none of the old type strings.
- **SC-003**: Every pre-existing usage-limit test and sim scenario passes with only identifier and string updates, and `go test -race ./...` is clean.
- **SC-004**: `docs/llms-full.txt` regenerates with no diff after the change, so the docs-drift check passes on the first push.
- **SC-005**: When #2120 lands, it can introduce its "Suspended" item state with no existing user-visible surface giving that word a different meaning.

## Assumptions

- The health keys become exactly `usage_limited` / `usage_limited_until`. The issue allows "the closest equivalent", and this is the plainest one.
- The TUI banner uses "dispatch held until <reset>" wording, taken from the issue's own example.
- The budget banner uses "polling paused for the rate limit" wording, taken from the issue's own example. The implementer may adjust it as long as it contains no form of "suspend" and keeps the existing meaning.
- No compatibility alias is kept for the old names. The surfaces first ship in 0.0.84, so no released consumer exists, and keeping aliases would defeat the purpose of the rename.
- Hold state is in memory, so the rename needs no data migration. Only the channel hub's persisted queues and subscriptions can carry old strings (see Edge Cases), and handling them safely is part of the implementation.
- Release notes already published (v0.0.83 and earlier) and earlier specs are historical and not edited.
- No new label, tool or event is added, and no existing event's delivery, dedup or matching semantics change.

## Out of Scope *(optional)*

- The #2120 "Suspended" item state itself.
- Any change to how a usage limit is detected, how its reset time is computed, or how it is cleared.
- Renaming `fabrik:claude-limit`, `fabrik:clear-claude-limit` or the `fabrik_clear_claude_limit` tool.
- Rewriting existing ADRs, published release notes or earlier specs, other than the dated note in ADR-1966-b.
- Renaming unrelated uses of "suspend" (GitHub App suspended by GitHub, the TUI suspending itself for an inline subprocess, the vendored GraphQL schema).

## Source References *(optional)*

- #1966 (the MCP surfaces that carry the current names). Mentioned for context only. The PR body must mention it by bare number only and must not use a closing keyword (`Closes`/`Fixes`/`Resolves`) near it.
- #2120 (the proposed "Suspended" item state that needs the word).
- ADR-1811 (usage-limit classification), ADR-1966-b (channel events; receives the dated note).
