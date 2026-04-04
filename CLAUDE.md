# Fabrik — Development Guide for Claude

## Project Overview

Fabrik is a Go CLI that orchestrates Claude Code through an SDLC pipeline defined on a GitHub Project board. Issues are the unit of work. The pipeline stages (Research → Plan → Implement → Review → Validate) are configured via YAML files.

## Build & Test

```bash
go build -o fabrik .     # Build
go test ./...            # Run all tests
go test -race ./...      # Run with race detector
go vet ./...             # Lint
```

## Architecture

- `cmd/root.go` — CLI entry point, flag parsing, .env loading
- `engine/engine.go` — Engine struct, Config, construction, Run() entry point
- `engine/poll.go` — Main poll loop, idle-upgrade, concurrent worker dispatch
- `engine/item.go` — Per-issue processing: stage runs, comment processing, blocking/pausing
- `engine/pr.go` — Output posting: issue comments, PR comments, summary extraction
- `engine/comments.go` — Comment detection and filtering logic
- `engine/context.go` — Context files (.fabrik-context/) and stage comment lookup
- `engine/claude.go` — Claude Code invocation, prompt building, marker extraction
- `engine/worktree.go` — Git worktree lifecycle (create, update, push, cleanup)
- `engine/interfaces.go` — GitHubClient and ClaudeInvoker interfaces (for testing)
- `github/project.go` — GraphQL board fetching (single query for all items + comments + linked PRs)
- `github/client.go` — HTTP client construction and shared request helpers
- `github/labels.go` — Label mutations (add, remove, ensure)
- `github/comments.go` — Comment mutations (add, update, reactions)
- `github/prs.go` — PR mutations (create draft, mark ready)
- `github/status.go` — Project board status updates
- `github/rest.go` — Low-level HTTP helpers
- `github/types.go` — Shared data types (ProjectItem, Comment, ReactionGroup)
- `stages/stages.go` — YAML stage config loading
- `stages/examples/` — Default stage YAML sources, embedded in binary via `//go:embed`
- `stages/embed.go` — Exposes embedded default stages as `stages.DefaultStages`
- `cmd/init.go` — `fabrik init` subcommand; extracts embedded YAMLs to `.fabrik/stages/`
- `.fabrik/stages/` — Live stage configs for this project (tracked in git)

## Key Patterns

### Reaction Flow
- 👀 (eyes) = comment acknowledged, processing started
- 🚀 (rocket) = comment processed successfully
- The rocket reaction is checked on restart to avoid reprocessing — it's durable state

### Markers in Claude Output
- `FABRIK_STAGE_COMPLETE` — Claude signals stage completion (must be on its own line)
- `FABRIK_BLOCKED_ON_INPUT` — Claude signals it needs user input before the stage can continue; mutually exclusive with `FABRIK_STAGE_COMPLETE`
- `FABRIK_ISSUE_UPDATE_BEGIN` / `FABRIK_ISSUE_UPDATE_END` — Updated issue body from comment processing
- `FABRIK_SUMMARY_BEGIN` / `FABRIK_SUMMARY_END` — Brief summary for issue when detailed output goes to PR

### Concurrency
- Workers dispatch via semaphore (`MaxConcurrent`, default 5)
- `processedSet` protected by `sync.Mutex`
- Worktree creation serialized by mutex (git config isn't concurrent-safe)
- In-flight issues tracked via `sync.Map` to prevent duplicate dispatch

### Worktrees
- Each issue gets `.fabrik/worktrees/issue-N/` on branch `fabrik/issue-N`
- NEVER destroy worktrees with existing content — they may have partial work
- `updateWorktreeFromMain` fetches and merges origin/main; leaves conflicts for Claude
- Dirty worktrees (uncommitted changes) skip the update

### PR Lifecycle
- Implement creates draft PR with `Closes #N` in body (links PR to issue)
- `closedByPullRequestsReferences` in GraphQL traverses issue → linked PRs → PR comments
- `post_to_pr` stages post detailed output on PR, summary on issue
- PR marked ready after Implement completes (triggers external review bots)

### Stage Config Options
```yaml
name: Research
order: 1
prompt: |
  ...
model: sonnet
max_turns: 50
comment_prompt: |          # Optional: prompt for processing user comments
  ...
allowed_tools:             # Optional: restrict Claude's tools
  - Read
  - Grep
update_issue_body: false   # Allow FABRIK_ISSUE_UPDATE markers to modify issue body (by convention, Specify only)
post_to_pr: true           # Post output to linked PR instead of issue
create_draft_pr: true      # Create draft PR before stage runs
mark_pr_ready_on_complete: true  # Mark PR ready when stage completes
auto_advance: false        # Override global yolo setting
```

## Important Conventions

- **Don't commit directly to main from worktrees** — always work on the issue branch
- **Every PR must include `Closes #N`** in the body so Fabrik can discover PR comments
- **Commit frequently** during implementation — preserves progress if session is interrupted
- **Rebase onto latest main** in Review and Validate stages before signaling completion
- **Check `git status` first** in any stage — there may be uncommitted work from a previous session
- **Labels are state**: `fabrik:locked:<user>`, `fabrik:editing`, `stage:<name>:complete`, `model:<name>`

## Common Issues

- **Max turns exceeded**: Increase `max_turns` in stage YAML or split the issue
- **Merge conflicts**: Left as conflict markers for Claude to resolve — check `git status`
- **Stale worktree**: `updateWorktreeFromMain` runs on each stage invocation; skip if dirty
- **SSH key expired**: `ssh-add ~/.ssh/<key>` — git operations fail silently with warning
- **processedSet is in-memory**: Rocket reactions provide durable "already processed" state across restarts
