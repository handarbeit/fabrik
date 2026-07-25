# Fabrik Style Guide (for Gemini Code Assist)

Fabrik is a Go CLI that orchestrates Claude Code through an SDLC pipeline. This
guide summarizes the conventions reviewers should hold code to. Full detail
lives in `CLAUDE.md` and `.claude/rules/golang.md` — this is a condensed
version for automated review context.

## Error handling

- Always check errors. Don't discard an error with `_ = err` unless the
  function's own comment says the error is intentionally ignored (e.g. a
  best-effort `ensureLabel` call).
- Wrap errors with context: `fmt.Errorf("doing X: %w", err)`.
- Non-fatal errors in the engine should be logged via `logf` and the caller
  should continue, not return early — the poll loop must stay resilient to
  per-issue failures.

## Naming

- Standard Go conventions: `MixedCaps`, not `snake_case`.
- Interface names describe behavior, not implementation: `GitHubClient`,
  `ClaudeInvoker`.
- Test helpers follow established names: `testEngine()`, `skipIfNoGit()`.

## Testing

- Stdlib `testing` only — no testify or other external test frameworks.
- Use `t.TempDir()` for temp files, `httptest.NewServer` for HTTP mocks,
  `t.Setenv()` for environment variables (auto-restores, survives panics).
- Don't rely on external network access for failure-injection tests — use
  local tricks instead (e.g. `ENAMETOOLONG` via a very long path).
- The real `git` binary is acceptable in tests, guarded by `skipIfNoGit`.
- Mock Claude invocations via the `ClaudeInvoker` interface, not a fake
  binary on disk.
- `go test -race ./...` should pass — flag anything that looks like a data
  race on shared state.

## Concurrency

- Shared state is protected by `sync.Mutex`; critical sections should stay
  small.
- Simple shared counters use `sync/atomic`.
- Git operations that write `.git/config` (e.g. worktree creation) must be
  serialized — git config access is not concurrency-safe.

## Logging

- Per-issue engine output goes through `logf(issueNumber, tag, format, ...)`,
  which prefixes `[#N tag]` for readability under concurrent workers.
- `fmt.Printf("[poll] ...")` is reserved for poll-level (not per-issue)
  messages.

## Dependencies

- Minimize external dependencies — currently only `gopkg.in/yaml.v3` beyond
  stdlib.
- Prefer stdlib for HTTP, JSON, testing, and temp files.

## PR conventions worth flagging

- Every PR linked to a Fabrik issue must include a `Closes #N` line in the
  body (added by the engine, not authored by hand in Fabrik-generated PRs).
- Changes touching engine behavior described in `docs/state-machine.md` or
  `docs/stage-lifecycle.md` should update those docs in the same PR — they
  are as-built specs, not historical notes.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `refactor:`,
  `test:`, `docs:`, `chore:`).
