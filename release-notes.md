# Fabrik v0.0.38

## Features

- **Per-issue base branch override via `base:<branch>` label** — Fabrik forks from, rebases onto, and targets PRs at the specified branch instead of the repository default. Must be set before Research; falls back to the default branch (with a comment) if the branch does not exist on the remote.
- **`--review-wait-timeout` and `--max-review-cycles` CLI flags** — First-class flags for tuning the reviewer gate. Explicit `=0` values are honored and no longer silently overridden by environment variables.
- **Richer PR review thread context** — Review thread comments now include the file, line, and diff-hunk context so the review-comment skill can navigate straight to the affected location instead of searching for the code.
- **Closed issues auto-advance when their current stage is complete** — Keeps the board moving when an issue is closed externally after its stage finished.

## Fixes

- **Stop runaway review re-invocation loop** — Added an `inFlight` guard and moved it before the cycle-limit check so issues don't get prematurely paused; only unresolved inline thread comments now trigger re-invocation; `reviewCycleCount` resets on unpause.
- **Use real PR review thread comments for re-invocation** — Prevents phantom cycles driven by unrelated issue comments.
- **Thread `baseBranch` through prompt builder** — Prompts no longer hardcode `main`; the base-branch statement is omitted when empty and fallback prose is never backtick-wrapped as a branch name.
- **Robust review thread decoding** — Use `*int`/`*string` for nullable GraphQL fields so missing location data doesn't break the decode.
- **Worktree mutex held around `branchExists`** — Serializes base-branch existence checks and deduplicates the fallback comment.
- **`gh`/`git` execution safety** — Fixed a 4-backtick fence issue in the review-comment SKILL.md example so nested code blocks render correctly.

## Improvements

- **TUI In Progress panel is no longer height-capped** — The panel can grow to show all in-flight workers.

## Internal

- Docs refreshed for `base:<branch>` label, reviewer-gate CLI flags, and the three-phase Pending Reviewer Gate behavior across README, USER_GUIDE, index.md, CLAUDE.md, and tui/help.go.

## Upgrading

```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo handarbeit/fabrik --pattern '*darwin_arm64*' -O - | tar xz
```
