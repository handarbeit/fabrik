# Fabrik

Automated Claude Code SDLC driver powered by GitHub Issues and Projects.

Fabrik watches a GitHub Project board and drives Claude Code through configurable
workflow stages. Issues are the unit of work — the issue body is the spec, comments
are user input, and the board columns define the workflow.

## Quick Start

```bash
# Build
go build -o fabrik .

# Set up your GitHub token
export GITHUB_TOKEN=ghp_...

# Copy and customize stage configs
cp -r stages/examples stages/mystages

# Run
./fabrik \
  --owner your-org \
  --repo your-repo \
  --project 1 \
  --user your-github-username \
  --stages ./stages/mystages
```

### Development

```bash
# Install air for auto-reload
go install github.com/air-verse/air@latest

# Run with auto-rebuild on file changes
air
```

Configure your flags in `.air.toml` under `args_bin`.

## How It Works

```
GitHub Project Board (source of truth)
        | GraphQL poll
    Fabrik (Go CLI, runs locally)
        | stage config match
    Claude Code (invoked per stage, in isolated worktree)
        | output
    GitHub Issue comments + labels + body updates
```

1. **Poll** — Fabrik fetches the entire project board via a single GitHub GraphQL query.
2. **Match** — Each issue's board status is matched to a stage config (YAML file).
3. **Worktree** — An isolated git worktree is created for each issue (`fabrik/issue-N` branch).
4. **Invoke** — Claude Code runs in the worktree with the stage's prompt, model, and tool configuration.
5. **Complete** — When Claude signals completion, the issue is labeled `stage:<name>:complete`.
6. **Advance** — In yolo mode, the issue auto-advances to the next stage. Otherwise, a human moves it.

### Comment Processing

When a user comments on an issue in an active stage:

1. Fabrik reacts with :eyes: to each new comment (marks as "in review")
2. Adds `fabrik:editing` label to lock the issue
3. Invokes Claude with a stage-specific comment review prompt
4. Claude performs any requested actions (e.g., linking PRs, running commands)
5. If the issue body needs updating, parses the updated body from Claude's output
6. Updates the issue body on GitHub (or posts output as a comment)
7. Removes `fabrik:editing` label
8. Reacts with :rocket: to each processed comment (also used to skip already-processed comments on restart)

This allows iterative refinement — comment to answer questions, provide feedback,
or steer the work, and Fabrik incorporates your input into the issue.

## Default Pipeline

The example stages implement a full SDLC pipeline:

| Stage | Purpose |
|-------|---------|
| **Backlog** | Parking lot (no processing) |
| **Research** | Explore codebase, identify scope, surface questions |
| **Plan** | Design approach, break into tasks, document decisions |
| **Implement** | Write code, tests, commit to issue branch |
| **Review** | Check correctness, security, coverage |
| **Validate** | Run tests, verify requirements met |
| **Done** | Terminal state (no processing) |

## Stage Configuration

Each stage is a YAML file in your stages directory:

```yaml
name: Research            # Must match a Project board column name
order: 1                  # Processing order (lower = earlier)
prompt: |                 # Prompt for Claude Code during stage processing
  You are a research agent...
comment_prompt: |         # Prompt for processing user comments (optional)
  You are reviewing user comments...
model: sonnet             # Optional: claude model to use
max_turns: 10             # Optional: limit Claude's turns
allowed_tools:            # Optional: restrict available tools
  - Read
  - Grep
  - Glob
completion:
  type: claude            # "claude", "tasklist", "label", or "approval"
  value: ""               # Type-specific (label name, approval keyword)
auto_advance: false       # Override global yolo setting for this stage
```

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--owner` | GitHub repo owner | required |
| `--repo` | GitHub repo name | required |
| `--project` | GitHub project number | required |
| `--user` | Your GitHub username | required |
| `--token` | GitHub token | `$GITHUB_TOKEN` |
| `--stages` | Stage configs directory | `./stages` |
| `--yolo` | Auto-advance through stages | `false` |
| `--poll` | Poll interval in seconds | `30` |

## Labels

Fabrik uses labels to track state:

| Label | Purpose |
|-------|---------|
| `fabrik:locked:<user>` | Issue is being processed by this user's Fabrik instance |
| `fabrik:editing` | Issue body is being updated (prevents concurrent processing) |
| `stage:<name>:complete` | Stage has been completed |

## Multi-User

Multiple people can run Fabrik against the same project board. Each instance
only processes changes made by its `--user`. Labels provide lightweight locking
to prevent conflicts.

## Git Worktrees

Each issue gets an isolated git worktree at `.fabrik/worktrees/issue-N/` on
branch `fabrik/issue-N`. This means:

- Multiple issues can be worked on simultaneously without conflicts
- Each issue's changes are on their own branch, ready for PR
- Worktrees persist across polls for Claude session continuity

## Architecture Decision Records

See [adrs/](adrs/) for documented decisions and their rationale.

## Requirements

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub personal access token with `repo` and `project` scopes
- A GitHub Project (v2) with board columns matching your stage names
