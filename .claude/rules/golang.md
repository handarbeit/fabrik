# Go Conventions for Fabrik

## Error Handling
- Always check errors. Never use `_ = err` unless the function comment explicitly says errors are intentional (e.g., `ensureLabel`).
- Wrap errors with context: `fmt.Errorf("doing X: %w", err)`
- Non-fatal errors in the engine should `logf` a warning and continue, not return.

## Naming
- Use Go conventions: `MixedCaps`, not `snake_case`.
- Interface names should describe behavior: `GitHubClient`, `ClaudeInvoker`.
- Test helpers: `testEngine()`, `skipIfNoGit()`.

## Testing
- Use stdlib `testing` only — no testify or external test frameworks.
- Use `t.TempDir()` for temp files, `httptest.NewServer` for HTTP mocks.
- Real `git` binary is acceptable in tests (use `skipIfNoGit` guard).
- Mock Claude via the `ClaudeInvoker` interface, not a fake binary.
- Always run `go test -race ./...` to catch data races.

## Concurrency
- Protect shared state with `sync.Mutex`. Keep critical sections small.
- Use `sync/atomic` for simple counters shared across goroutines.
- Git operations that write `.git/config` must be serialized (worktree mutex).

## Logging
- Use `logf(issueNumber, tag, format, args...)` for all engine output — this prefixes with `[#N tag]` for concurrent log clarity.
- Use `fmt.Printf("[poll] ...")` only for poll-level messages (not per-issue).

## Dependencies
- Minimize external dependencies. Currently only `gopkg.in/yaml.v3`.
- Use stdlib for HTTP, JSON, testing, temp files.
