# ADR 031: Watch View Parses NDJSON Log Files for Real-Time Turn Data

**Status:** Accepted  
**Date:** 2026-04-26  
**Issue:** #431 (Show live turn count in TUI for in-progress stages)

## Context

The `fabrik watch <N>` view displays live Claude output for a single issue. Issue #431 requires adding a real-time turn counter to this view, updating as each Claude assistant turn completes.

The overview TUI (the main `fabrik` process) solves this by having the engine emit `TurnProgressEvent` messages through its in-process TUI channel. The watch view runs as a **separate process** (`fabrik watch` is a separate CLI invocation) and cannot receive events through the engine's channel.

Two approaches were considered for the watch view:

### Option A: Engine writes a state file

The engine writes a small JSON file (e.g., `.fabrik/state/issue-<N>-turns.json`) after each assistant turn. The watch view polls this file.

**Pros:** Clean separation; watch view doesn't need to understand NDJSON format.  
**Cons:** Introduces new persistent state that must be cleaned up; adds file I/O on every turn on the critical Claude output path; creates a new data contract that every future real-time metric would need to replicate.

### Option B: Watch view parses the live NDJSON log file

The watch view's `followFile` goroutine already reads individual NDJSON lines from the live log file (written by the engine in real time). It can detect `{"type":"user"}` lines and count them locally, with no engine-side changes beyond what the log already records.

**Pros:** Zero new I/O on the engine's critical path; no new files to clean up; self-contained in the watch package; any existing log file can be replayed to reconstruct the count.  
**Cons:** Watch view must understand enough NDJSON to detect logical turn boundaries; count matches Claude's internal `num_turns` exactly (each logical turn emits exactly one `{"type":"user"}` event). *(Note: the original implementation used `{"type":"assistant"}` events, which over-counted when turns had multiple tool calls — see Correction below.)*

## Decision

**Option B**: The watch view parses `{"type":"user"}` NDJSON lines from the live log file directly, counting one logical turn per user event. *(The original implementation used `{"type":"assistant"}` lines — see Correction below for why user events are the correct boundary.)*

The `followFile` goroutine in `watch/logfollow.go` already reads individual NDJSON lines using `bufio.Reader.ReadBytes('\n')`. Adding a minimal JSON type-check (`json.Unmarshal` into `struct{ Type string }`) on each line is fast and self-contained. The turn count is incremented in a local variable that resets when `followFile` switches to a new log file (stage transition).

## Implementation

- `watch/logfollow.go`: `isUserTurn(line []byte) bool` helper; `followFile` counts turns and sends `TurnCountMsg{TurnsUsed int}` on each match *(originally `isAssistantTurn` — renamed in issue #447)*
- `watch/events.go`: new `TurnCountMsg` message type
- `watch/model.go`: `turnsUsed` field updated on each `TurnCountMsg`; reset to 0 on `NewLogFileMsg` (stage transition); `effectiveMaxTurns()` derives the budget heuristically from stage config + `fabrik:extend-turns` label + log file count

## Correction (Issue #447, 2026-04-26)

The original implementation counted `{"type":"assistant"}` NDJSON events as the turn boundary. This was incorrect: a single logical Claude turn (one user→assistant cycle) produces **one** `{"type":"user"}` event followed by **one or more** `{"type":"assistant"}` events — one text response plus one per `tool_use` block. Counting `"assistant"` events therefore over-counted by the number of tool calls per turn, causing the live badge to routinely display values like "76/50" while Claude internally counted 50 turns.

The fix (applied to both `engine/claude.go` and `watch/logfollow.go`) switches the detected event type from `"assistant"` to `"user"`. Each logical turn begins with exactly one `"user"` event (either the initial prompt or tool-result responses), so counting `"user"` events aligns precisely with Claude's own `num_turns` accounting.

The function `isAssistantTurn(line []byte) bool` (watch path) and `isAssistantTurnLine(line []byte) bool` (engine path) were renamed to `isUserTurn` and `isUserTurnLine` respectively, with the detection string updated accordingly.

## Implications for Future Real-Time Data in the Watch View

The core constraint driving this decision — **the watch view is a separate process and cannot receive the engine's in-process TUI channel events** — applies to any future real-time data addition to the watch view.

Future contributors who want to surface additional real-time engine state in `fabrik watch` have two viable paths:

1. **Parse existing log output**: If the data is already present in the NDJSON log stream (e.g., token usage from `{"type":"result"}`), add detection in `followFile` and send a new message type.

2. **Write an intermediate state file**: For data that is NOT in the log stream (e.g., cost accumulation mid-invocation, extension-loop budget upgrades), the engine can write a small JSON state file to `.fabrik/state/` that the watch view polls alongside the log file. This file should be cleaned up when the issue transitions to Done.

Option A (state file) was rejected here because the turn count is already available in the log stream. If a future feature requires data that is genuinely not in the log (e.g., the extension-loop `totalMultiple` value), a state file is the appropriate choice.
