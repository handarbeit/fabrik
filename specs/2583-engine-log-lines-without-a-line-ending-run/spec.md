# Feature Specification: Engine log lines always end with exactly one line ending

**Feature Branch**: `fabrik/issue-2583`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description (verbatim original issue body):

> ## Problem
>
> `Engine.logf` / `logfRepo` (`engine/engine.go` `logEvent`) add no line ending. Each caller has to end its format string with `\n`. Ten call sites don't, so in plain-text mode and in the persistent log file their line runs straight into the next one. We saw this on the forge-hold lines during the v0.0.92 watch-item review.
>
> Call sites whose literal format has no trailing `\n` (found with a `go/ast` scan of `engine/*.go`, tests excluded):
>
> - `engine/forge_hold.go`: all four (`onForgeHoldEngaged`, its warning-record failure, the lift line, and its warning-resolve failure)
> - `engine/start_failure.go:157`, `:195` (warning record/resolve failures)
> - `engine/app_git_credential.go:211`, `:233`
> - `engine/item.go:1257` (the usage-limit dispatch hold)
>
> `engine/logthrottle.go:65`/`:86` pass `"%s", msg` through, so whether they end the line depends on each caller's message. Check them as part of this fix.
>
> The TUI path is unaffected, because it gets a `LogEvent.Message`, not a byte stream.
>
> ## Requirements
>
> - R1: Every engine log line ends with exactly one line ending in both plain-text stdout and the log file.
> - R2: Pick one fix and apply it consistently. One option is to append `\n` at the ten sites. The other is to make `logEvent` add the line ending when the message lacks one, which removes this class of bug for good. Either way, a message that already ends in `\n` must not get a second one.
> - R3: A test fails if R1 is broken. That means a static check over `engine/` for a newline-less literal format if the sites are fixed individually, or a `logEvent` unit test if `logEvent` normalises.
>
> ## Out of scope
>
> - Log formats outside the engine (`cmd/`, `pruefer/`).
>
> ## Acceptance
>
> - The forge-hold engage and lift lines appear on their own lines in the log file.
> - The R3 test fails when the fix is neutralised.

## Background

The engine's log helpers (`logf`, `logfRepo`, both funnelling into `logEvent`) format a message and write it to plain-text stdout and to the persistent log file without adding a line ending. By convention every caller ends its format string with `\n`. Ten call sites do not (the four in `engine/forge_hold.go`, two in `engine/start_failure.go`, two in `engine/app_git_credential.go`, one in `engine/item.go`), so in plain-text mode and in the log file their output runs into the next line. This was seen on the forge-hold engage and lift lines during the v0.0.92 watch-item review, making the log hard to read and to grep. The throttled helpers in `engine/logthrottle.go` forward a pre-formatted message with `"%s"`, so whether they end the line depends on each caller's message. The TUI path is unaffected because it receives a structured `LogEvent.Message` rather than a byte stream.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Every log line stands on its own (Priority: P1)

An operator reading the daemon's plain-text output or the persistent log file sees each engine log event on its own line, whichever call site produced it, including the forge-hold lines.

**Why this priority**: This is the reported defect. Merged lines break reading, grepping and any tooling that splits the log on newlines.

**Independent Test**: Emit a forge-hold engage, then a lift, followed by another log line in plain-text mode with a log file configured. Each appears on its own line in both outputs.

**Acceptance Scenarios**:

1. **Given** the forge-hold hold engages and later lifts, **When** the log file is read, **Then** the engage line and the lift line each occupy their own line and neither shares a line with another event.
2. **Given** a log message whose format string has no trailing newline, **When** it is logged, **Then** stdout (plain-text mode) and the log file each show it terminated by exactly one line ending.
3. **Given** a log message that already ends in a single `\n`, **When** it is logged, **Then** no second line ending is added in either output.

---

### User Story 2 - The defect class cannot silently return (Priority: P1)

A maintainer adding a new engine log call cannot reintroduce run-together lines unnoticed, because a test fails if the line-ending guarantee is broken.

**Why this priority**: The bug exists because correctness depended on every caller remembering a trailing `\n`. The fix is not complete without a regression guard.

**Independent Test**: Neutralise the fix (revert the normalisation, or restore a newline-less literal at a call site). The new test must fail; with the fix in place it passes.

**Acceptance Scenarios**:

1. **Given** the fix is in place, **When** the test suite runs, **Then** the line-ending test passes.
2. **Given** the fix is neutralised, **When** the test suite runs, **Then** the line-ending test fails.

---

### Edge Cases

- A message already ending in `\n` is not given a second one (no blank lines introduced).
- A message containing newlines in the middle but not at the end gets exactly one line ending at the end, and its internal newlines are untouched.
- An empty message still produces a well-formed line (prefix plus exactly one line ending).
- The throttled helpers (`logfThrottled`, `logfThrottledByInterval`) forward their message through `logf`, so their output ends in exactly one line ending regardless of the caller's format string. Throttle suppression is unchanged: a suppressed call still writes nothing, and the throttle's identity of a message is not altered by the line-ending fix.
- The TUI path and the per-Engine log sink receive the same message content as today (no behavioural change for them).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every engine log line emitted through `logf`, `logfRepo`, `logfThrottled` and `logfThrottledByInterval` ends with exactly one line ending in plain-text stdout output.
- **FR-002**: Every such line ends with exactly one line ending in the persistent log file.
- **FR-003**: A message that already ends in a line ending does not receive a second one in either output.
- **FR-004**: The fix is applied consistently by a single mechanism, either a central one in the shared log emitter or the ten identified call sites, not a mix of both.
- **FR-005**: The ten identified call sites (the four in `engine/forge_hold.go`, `engine/start_failure.go` lines 157 and 195, `engine/app_git_credential.go` lines 211 and 233, `engine/item.go` line 1257) produce correctly terminated lines. The two throttle forwarders in `engine/logthrottle.go` are checked and confirmed correct under FR-001.
- **FR-006**: The content delivered to the TUI event stream and to the per-Engine log sink for a given message is unchanged by this fix.
- **FR-007**: A test fails if FR-001 to FR-003 are broken. If `logEvent` normalises, the test is a unit test of `logEvent`. If call sites are fixed individually, it is a static check over the engine sources for newline-less literal formats.
- **FR-008**: The test in FR-007 must demonstrably fail when the fix is neutralised (non-vacuity).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: In a log file containing forge-hold engage and lift events, 100% of those events appear on their own line, with no log line containing more than one timestamped prefix.
- **SC-002**: Across all engine log calls, zero lines run together in plain-text output or the log file.
- **SC-003**: Zero blank lines are introduced for messages that already ended in a line ending.
- **SC-004**: The regression test passes with the fix and fails with the fix neutralised.

## Assumptions

- Fixing the line ending centrally in the shared log emitter is the preferred approach, since it removes the whole defect class; the choice is left to Plan as long as FR-004 holds.
- If the emitter normalises, it does so only for the plain-text stdout and log file outputs. The TUI and sink paths receive the message as they do today (FR-006).
- The ten call sites and two throttle forwarders listed in the issue are the full set found by the AST scan; the fix must not rely on that list being complete (hence FR-001 to FR-003 and FR-007 apply to all log calls).
- Only the engine package is affected.

## Out of Scope

- Log formats outside the engine (`cmd/`, `pruefer/`).
- Changing log content, tags, timestamps, or the TUI/log-sink presentation.

## Source References

- `engine/engine.go` `logf`, `logfRepo`, `logEvent`
- `engine/logthrottle.go` `logfThrottled`, `logfThrottledByInterval`
- Call sites: `engine/forge_hold.go`, `engine/start_failure.go`, `engine/app_git_credential.go`, `engine/item.go`
