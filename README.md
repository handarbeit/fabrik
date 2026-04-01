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

# Copy example stage configs
cp -r stages/examples stages/mystages

# Run
./fabrik \
  --owner your-org \
  --repo your-repo \
  --project 1 \
  --user your-github-username \
  --stages ./stages/mystages
```

## How It Works

1. **Poll** — Fabrik fetches the entire project board via GitHub's GraphQL API.
2. **Match** — Each issue's board status is matched to a stage config (YAML file).
3. **Invoke** — Claude Code runs with the stage's prompt, model, and tool configuration.
4. **Complete** — When a stage's completion criteria are met, the issue is labeled `stage:<name>:complete`.
5. **Advance** — In yolo mode, the issue auto-advances to the next stage. Otherwise, a human moves it.

## Stage Configuration

Each stage is a YAML file in your stages directory:

```yaml
name: Design              # Must match a Project board column name
order: 2                  # Processing order (lower = earlier)
prompt: |                 # System prompt for Claude Code
  You are a design agent...
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

## Multi-User

Multiple people can run Fabrik against the same project board. Each instance
only processes changes made by its `--user`. Labels (`fabrik:locked:<user>`)
provide lightweight locking to prevent conflicts.

## Requirements

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub personal access token with `repo` and `project` scopes
