## Summary

The test files in the `engine/` and `github/` packages have grown very large. `engine/engine_test.go` is 2049 lines covering 40+ test functions across multiple unrelated concerns. `engine/claude_test.go` is 1049 lines. `github/project_test.go` is 699 lines. These monolithic files cause frequent merge conflicts when multiple PRs touch the same file concurrently.

The goal is to split large test files into smaller, focused files — without changing any test logic — to reduce PR collision at the current development velocity.

## Requirements

**Split convention:** Follow Go convention: the primary test file for `foo.go` is `foo_test.go`. When that file grows too large, split into additional `foo_<feature>_test.go` files in the same package. All split files share package scope, so helpers remain accessible without duplication.

**`engine/engine_test.go` (2049 lines) → split into:**
- `engine/process_item_test.go` — all `TestProcessItem_*` tests
- `engine/poll_test.go` — all `TestPoll_*` tests
- `engine/advance_stage_test.go` — all `TestAdvanceToNextStage_*` tests
- `engine/format_test.go` — `TestFormat*`, `TestCaptureGitMeta_*`
- `engine/engine_test.go` — remaining tests (`TestNew`, `TestRun_*`, `TestMapKeys`, `TestFindNewComments`, `TestGitToplevel`, `TestNewWithDeps`)

**`engine/claude_test.go` (1049 lines) → split into:**
- `engine/build_prompt_test.go` — all `TestBuildPrompt_*` tests
- `engine/invoke_claude_test.go` — all `TestInvokeClaude_*` and `TestRealClaudeInvoker_*` tests
- `engine/parse_claude_test.go` — `TestParseClaudeJSON_*`, `TestCheckCompletion_*`, `TestTokenUsage*`
- `engine/session_test.go` — `TestSaveSessionID*`, `TestSessionDir`, `TestLogDir`, `TestFormatStatsFooter`, `TestSessionFile`

**`github/project_test.go` (699 lines) → split into:**
- `github/fetch_board_test.go` — all `TestFetchProjectBoard_*` tests
- `github/fetch_details_test.go` — all `TestFetchItemDetails_*` tests
- `github/project_test.go` — `TestParseTime` and any remaining shared helpers

**Mocks and helpers:**
- `engine/mocks_test.go` remains as-is (already focused, 181 lines)
- Shared engine test helpers (`testEngine`, `testStages`, `testStagesWithCleanup`) move to `engine/engine_helpers_test.go` so all split files can access them without duplication

**Quality gate:**
- No test logic is changed — pure mechanical relocation of functions between files
- `go test -race ./...` must pass after the split

## Scope

**In scope:**
- Splitting `engine/engine_test.go`, `engine/claude_test.go`, `github/project_test.go`
- Creating `engine/engine_helpers_test.go` for shared helpers currently embedded in `engine_test.go`

**Out of scope:**
- Changing any test logic, assertions, or coverage
- Refactoring production code
- Splitting test files already under ~400 lines
- Adding new tests

## Prior Art / Context

Go allows multiple `_test.go` files per package with shared scope — no build changes needed. The Go standard library and large Go projects routinely use `foo_bar_test.go` naming when a single test file grows unwieldy. All helper functions remain accessible to every test file in the same package.

## Risks / Dependencies

- Helper functions used across split files must live in `engine_helpers_test.go` (not duplicated)
- The split is purely mechanical — low regression risk if `go test -race ./...` is verified after each file split