# ADR 014: Switch Claude output format to stream-json

## Status

Accepted

## Context

The Fabrik engine invokes Claude Code via `claude --output-format json`, which
produces a single JSON object on stdout only after the entire session completes.
This means the `.log` file (which captures stdout) is empty until Claude exits,
making real-time monitoring impossible.

Issue #96 introduces `fabrik watch`, a standalone TUI that follows the live
output from a running Claude session by tailing the `.log` file. To make this
work, Claude's stdout must be written incrementally — one JSON event per line
— as the session progresses.

## Decision

Switch `buildClaudeArgs` from `--output-format json` to `--output-format stream-json`.

In `runClaude`, open the `.log` file **before** `cmd.Run()` and set:

```go
cmd.Stdout = io.MultiWriter(&stdout, logFile)
```

This tees every NDJSON line to disk as Claude writes it. The in-memory
`bytes.Buffer` still accumulates the full output for downstream parsing.

The existing `-output-*.json` file (written after `cmd.Run()` returns from
`rawOutput`) now contains NDJSON instead of a single JSON object. All
consumers already handle NDJSON:

- `parseClaudeJSON` handles NDJSON via its line-scanning path.
- `streamfilter.RunFilter` handles NDJSON.
- `tui/model.go` pipes `.json` files through `stream-filter`, which handles NDJSON.

## Format comparison

| Format | Produced by | Structure |
|--------|------------|-----------|
| `json` (old) | `--output-format json` | Single JSON object: `{"result": "...", "session_id": "...", ...}` |
| `stream-json` (new) | `--output-format stream-json` | NDJSON: one JSON event per line; last line is the result object |

`parseClaudeJSON` handles all three legacy formats (single object, JSON array,
NDJSON) so no parsing changes are required.

## Consequences

**Positive:**
- `fabrik watch` can follow live Claude output in real time.
- The existing TUI `l` key (which opens `.log` files with `less +F`) now shows
  live streaming output instead of a stale empty file for in-progress jobs.
- NDJSON lines are richer — each assistant turn, thinking block, and tool use
  is a discrete event that can be rendered incrementally.

**Negative:**
- `.log` files now contain NDJSON (stream-json events), not stderr diagnostic
  text. Users who open a `.log` file directly (e.g., `cat ~/.fabrik/logs/.../*.log`)
  will see raw JSON lines rather than human-readable text. Piping through
  `fabrik stream-filter` renders them readably.
- `.log` file size increases because stdout (which can be many MB for long sessions)
  is now written to disk; previously only stderr diagnostics were written.
- The `-output-*.json` file now contains NDJSON rather than a single JSON object.
  External tools that consume these files and assume the single-object format will
  need to handle NDJSON. All Fabrik-internal consumers already do.

## Alternatives considered

**Keep `--output-format json`, write stdout to `.log` after `cmd.Run()`.**
Rejected — Claude buffers all output until the session ends with `--output-format json`,
so there is nothing to follow in real time even with `io.MultiWriter`.

**Use `--output-format stream-json` but write the `.log` file only at the end.**
Rejected — the point is live streaming. Writing at the end defeats the purpose.

**Add a separate streaming mechanism (e.g., a named pipe or Unix socket).**
Rejected — `io.MultiWriter` achieves the same result with zero additional complexity.
