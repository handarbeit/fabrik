🏭 **Fabrik — stage: Specify**
*branch: fabrik/issue-107 | commit: 9d1c8386 | 2026-04-03 21:31 UTC*

I have enough context to write the spec. Let me now produce the structured issue body.

The codebase already has:
- Session file storage at `~/.fabrik/sessions/issue-<N>/<stageName>.session` (`engine/claude.go:113`)
- Terminal window launching via `openTerminalCmd` on macOS/Linux (`tui/model.go:570`)
- Model override logic in `buildClaudeArgs` (`engine/claude.go:165`)
- CLI subcommand dispatch pattern (`cmd/root.go:43`)

Key open questions I've identified:

1. **`fabrik resume` and stage auto-detection**: The "omit --stage → use current board status" path requires GitHub API access. Does the standalone CLI subcommand need owner/repo/project flags, or should it only look at local state (session files)?

2. **Concurrent access in TUI**: If `r` works on in-progress (active) items, a second Claude session would run concurrently in the same worktree. Is that acceptable, or should `r` be restricted to History entries only?

3. **Model source for CLI**: Without GitHub API access, the model label override (`model:<name>`) on the issue can't be read. Should `resume` just use the stage config model?


I've updated the spec with:
- Clear, testable requirements for both TUI and CLI
- Explicit scope boundaries (notably: `r` restricted to History pane only, `--stage` required for CLI)
- Three open questions that need answers before implementation can be designed

The most important question is **Q1** (whether `r` should also work on in-progress items) and **Q2** (whether the CLI should auto-detect stage). These affect the implementation scope significantly.

---
Used 12/20 turns, 0k input / 6k output tokens. Stage incomplete.