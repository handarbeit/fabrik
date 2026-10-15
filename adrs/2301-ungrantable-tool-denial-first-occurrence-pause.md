# ADR 2301: Pause on the first ungrantable file-tool denial

Status: Accepted. Amends ADR 1523 (and its #1556 causal amendment) and ADR 1775.

## Context

Report #1981: a worker needed to edit `.claude/skills/<skill>/SKILL.md`. Claude Code refused the `Edit` in
`--permission-mode dontAsk` although Fabrik passes `--allowedTools Edit(<worktree>/**)`. Claude Code's
"protected paths" (`.git`, `.claude`, `.vscode`, …) are denied in `dontAsk`, and allow rules do not
pre-approve them. #1704 bounded the retry loop at `MaxToolsDeniedRetries` (3), but a denial that no rule can
change still cost three runs, never named the file, and the advice (simpler commands, an `allowed_tools` rule)
could not help.

## Decision

1. **The file is decoded.** `decodeToolPath` reads `tool_input.file_path` (`Edit`, `Write`, `MultiEdit`) or
   `notebook_path` (`NotebookEdit`) into `claudeerr.ToolDenial.Path`; best-effort, never fabricated.
   Provenance: `engine/testdata/protected_path_edit_denied_stream.jsonl` was captured from CLI 2.1.299 with
   Fabrik's flags (`--output-format stream-json --verbose --permission-mode dontAsk --allowedTools
   Edit(<worktree>/**)`) on a throwaway worktree; the init line is trimmed and paths are anonymised. The
   denial entry is `{"tool_name":"Edit","tool_input":{"file_path":…,"old_string":…,"new_string":…,
   "replace_all":false}}`. The `notebook_path` and `MultiEdit` shapes are from the documented tool schemas and
   not captured.
2. **Ungrantable is structural.** A denial is ungrantable iff it is a file-tool call, its path is lexically
   inside the worktree, and the `--allowedTools` values of the args actually passed contain the bare tool name
   or exactly `Tool(<worktree>/**)`; `--dangerously-skip-permissions` is never ungrantable. It never reads
   prose and never uses a protected-path list (CLI-version-specific; a drifting list would silently change
   behaviour). The documented list is used only to add a link to the comment. Reading rules from the args slice
   rather than recomputing them means `applyWorktreeBoundary` and the read-only/custom-list logic cannot drift
   from the verdict.
3. **Where.** `runClaude` stamps the verdict after `interpretClaudeResult` (the `skillLoadOverride`
   precedent) because only it sees the final args; `interpretClaudeResult` keeps its signature. The verdict
   rides on `ToolDenial` so the sim, which scripts the invoker, can set it directly.
4. **Handling.** Both consumers pause on first occurrence via `pauseForToolsDenied`; all other bookkeeping of
   ADR 1523 is unchanged (no `failed` label, no `max_retries` charge, WIP commit and push, breakers bypassed,
   `EnginePaused`). The explanatory comment and `fabrik:tools-denied` label are skipped for such a run: the
   label reads "the engine retries" and the pause comment replaces the comment. A label already left by an
   earlier bounded-retry denial of the same episode is removed, so it cannot contradict the pause.
5. **`MultiEdit`** joins the causal gate's write-capable and recovery tool sets.

## Consequences and known gaps

- **Causal gate.** The verdict only refines denials `denialCausal` judged causal. A run that committed work and
  was then denied on `.claude/` is still an ordinary failure charged to `max_retries`. Widening the gate is a
  separate decision.
- **False positives.** A hook-denied in-worktree `Edit` is structurally identical to a protected-path denial.
  The cost is a pause instead of up to three retries, recoverable by removing `fabrik:paused`. The comment
  therefore claims only that Claude Code refused the write in non-interactive mode.
- Lexical path check: no symlink resolution, matching the CLI's comparison with the rule text.
- After resuming, the same denial can re-pause at once; the comment's options are the way out.
