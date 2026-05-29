# ADR 052: Warnings Panel Backed by mtime-Polled File

**Date**: 2026-05-28  
**Status**: Accepted  
**Issue**: #843 — Persistent pre-flight warnings panel in TUI

## Context

The TUI needs to surface pre-flight warnings (e.g. `allow_auto_merge` disabled, stage-config drift) persistently across auto-upgrade restarts. Two architectural options were considered for how the Warnings panel receives state updates:

**Option A — Engine event channel**: The engine emits events (via the existing typed-event pattern) every time a detector fires. The TUI subscribes and updates its in-memory state. State is lost when the process restarts (including auto-upgrade).

**Option B — File-backed state with mtime polling**: Warnings are written to `.fabrik/warnings.json` by detectors. The TUI polls the file's mtime every second (existing `TickEvent` cadence) and reloads when changed.

The headline requirement is that warnings survive auto-upgrade restarts — the operator may be away for days while Fabrik self-restarts multiple times. Option A cannot satisfy this without additional IPC or a separate persistence layer. The operator's explicit constraint is: "auto-upgrade is fine to skip [prompts] SO LONG AS the warnings appear at the bottom of the panel and are actionable by the user next time they look at the TUI."

## Decision

Use **Option B**: file-backed state with mtime polling.

`warnings/warnings.go` is a new top-level package (importable by both `engine/` and `stages/` without cycle risk) that provides:
- `Record(entry Entry) error` — upsert; preserves `first_seen` and `dismissed` on re-record
- `Clear(key string) error` — removes entry regardless of dismissed state
- `Dismiss(key string) error` / `Undismiss(key string) error` — toggle
- `Load() ([]Entry, error)` — read-only; missing file → nil, nil; corrupt → nil, err

Storage is `.fabrik/warnings.json` (CWD-relative, consistent with `.fabrik/history.json` per ADR 023). Writes are atomic (temp-file-then-rename) and protected by a package-level `sync.Mutex` for concurrent-goroutine safety.

The `WarningsPaneComponent` in `tui/warnings.go` calls `os.Stat(warnings.Path())` on each `TickEvent` and reloads if `ModTime()` changed. Initial load happens at component creation, so entries visible at startup are immediate.

## Consequences

**Positive**:
- Warnings survive auto-upgrade restarts with `first_seen` preserved and `dismissed` sticky — the P1 headline requirement is fully satisfied.
- No IPC, no shared memory, no new Bubbletea event types required. The file is the message.
- Consistent with the existing `history.json` precedent (ADR 023).
- External processes (operator, tests, future tools) can read/write the file without engine coordination.
- `--no-tui` mode gets warnings persistence for free (detectors write the file unconditionally).

**Negative / trade-offs**:
- The TUI lags up to 1 second behind file changes (bounded by `TickEvent` cadence). For warnings that change at startup frequency, this is imperceptible.
- A new contributor would expect a Bubbletea model to receive engine events for state updates, not poll a file. This ADR documents why the file-based approach is correct here.
- The file write on every `Record`/`Clear` adds latency to detector paths. For startup-time detectors (the only current callers), this is acceptable — the file is small and the write is a single atomic rename.

## Alternative Considered

**Shared in-memory state via channel**: The engine could send `WarningEvent` messages to the TUI's program via `tui.Program.Send()`. This eliminates polling but:
1. Does not persist across restarts.
2. Requires the TUI to be running when the event fires (auto-upgrade restarts happen headlessly).
3. Adds coupling between engine and TUI packages.
The file-backed approach is strictly superior for the auto-upgrade use case.
