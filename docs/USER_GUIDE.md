# Fabrik User Guide

Fabrik is an automated Claude Code SDLC driver powered by GitHub Issues and Projects.
This guide covers everything you need to set up, configure, and use Fabrik effectively.

For a quick overview see the [README](../README.md).
For details on the internal stage lifecycle, see [Stage Lifecycle](stage-lifecycle.md).

---

## Table of Contents

1. [Getting Started](#1-getting-started)
2. [Configuration Reference](#2-configuration-reference)
3. [Workflow Patterns](#3-workflow-patterns)
4. [Stage Reference](#4-stage-reference)
5. [Labels Reference](#5-labels-reference)
6. [TUI Dashboard](#6-tui-dashboard)
7. [Observability](#7-observability)
8. [Troubleshooting](#8-troubleshooting)

---

## 1. Getting Started

### Prerequisites

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub personal access token with `repo` and `project` scopes
- A GitHub Project (v2) with board columns matching your stage names
- tmux (optional, for observing Claude sessions in real-time)

### Initial Setup

```bash
# Build
go build -o fabrik .

# Set your GitHub token
export GITHUB_TOKEN=ghp_...

# Initialize default stage configs
./fabrik init
# This creates .fabrik/stages/ with default YAML files
```

### Create a Project Board

Create a GitHub Project (v2) for your repository. Add board columns that correspond to
your stage names — the column name must match the `name` field in each stage YAML file
exactly (case-sensitive). The default pipeline uses:

`Backlog` -> `Research` -> `Plan` -> `Implement` -> `Review` -> `Validate` -> `Done`

### First Run

```bash
./fabrik \
  --owner your-org \
  --repo your-repo \
  --project 1 \
  --user your-github-username
```

Fabrik polls the project board every 30 seconds by default (configurable with `--poll`).
Move an issue to `Research` on the board to start processing it.

### Development Mode (self-upgrade)

The `--auto-upgrade` flag enables Fabrik to upgrade itself when idle. After 2
consecutive idle polls, Fabrik checks `origin/main` for new commits. If found, it
runs `git pull --ff-only`, rebuilds the binary with `go build`, and re-execs itself.
This is intended for the self-evolving workflow where Fabrik develops Fabrik.

```bash
./fabrik --auto-upgrade --owner your-org --repo your-repo --project 1 --user you
```

---

## 2. Configuration Reference

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--owner` | GitHub repo owner (org or user) | required |
| `--repo` | GitHub repo name | required |
| `--project` | GitHub Project (v2) number | required |
| `--user` | Your GitHub username — only processes comments by this user | required |
| `--token` | GitHub API token | `$GITHUB_TOKEN` |
| `--stages` | Directory containing stage YAML configs | `./.fabrik/stages` |
| `--yolo` | Auto-advance issues through stages without human approval | `false` |
| `--auto-upgrade` | When idle, self-upgrade from origin/main | `false` |
| `--tui` | Enable the interactive TUI dashboard | `false` |
| `--no-tmux` | Disable tmux session wrapping; run Claude directly | `false` |
| `--poll` | Poll interval in seconds | `30` |
| `--max-retries` | Max failed stage attempts before pausing the issue (0 = unlimited) | `3` |
| `--debug-output` | Save Claude stage output to `.fabrik/debug/` | `false` |

### `.env` File Support

Fabrik loads configuration from a `.env` file in the working directory if present.
All flags can be set via `.env` — flags take precedence over `.env` values.

```
FABRIK_TOKEN=ghp_...         # Preferred token env var
GITHUB_TOKEN=ghp_...         # Fallback token env var
FABRIK_OWNER=your-org
FABRIK_REPO=your-repo
FABRIK_PROJECT_NUMBER=1
FABRIK_USER=your-username
FABRIK_STAGES=./.fabrik/stages
FABRIK_YOLO=true
FABRIK_POLL=30
FABRIK_MAX_CONCURRENT=5
FABRIK_MAX_RETRIES=3
FABRIK_NO_TMUX=false
FABRIK_DEBUG_OUTPUT=false
```

**Safety:** If a `.env` file exists but is not listed in `.gitignore`, Fabrik will
refuse to start with a fatal error to prevent accidental token leaks.

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `FABRIK_TOKEN` | GitHub personal access token (preferred) | required |
| `GITHUB_TOKEN` | GitHub personal access token (fallback) | required |
| `FABRIK_OWNER` | GitHub repo owner | -- |
| `FABRIK_REPO` | GitHub repo name | -- |
| `FABRIK_PROJECT_NUMBER` | GitHub Project (v2) number | -- |
| `FABRIK_USER` | Your GitHub username | -- |
| `FABRIK_STAGES` | Stage configs directory | `./.fabrik/stages` |
| `FABRIK_YOLO` | Auto-advance (`true`/`1`/`yes`) | `false` |
| `FABRIK_POLL` | Poll interval in seconds | `30` |
| `FABRIK_MAX_CONCURRENT` | Max parallel Claude sessions | `5` |
| `FABRIK_MAX_RETRIES` | Max retries before pausing (0 = unlimited) | `3` |
| `FABRIK_NO_TMUX` | Disable tmux wrapping | `false` |
| `FABRIK_DEBUG_OUTPUT` | Save raw Claude output for debugging | `false` |

Token precedence: `--token` flag > `FABRIK_TOKEN` > `GITHUB_TOKEN`

### Stage YAML Reference

Each stage is a YAML file in your stages directory. The filename is arbitrary; the
`name` field determines which board column it matches.

```yaml
name: Research            # Required. Must match a Project board column exactly.
order: 1                  # Required. Lower values processed earlier in the pipeline.
prompt: |                 # Required. System prompt for Claude Code.
  You are a research agent...
comment_prompt: |         # Optional. Prompt for processing user comments.
  You are reviewing comments...
model: sonnet             # Optional. Claude model: "opus", "sonnet", etc.
max_turns: 10             # Optional. Max conversation turns per invocation.
read_only: false          # Optional. Stash/restore worktree (for analysis stages).
post_to_pr: true          # Optional. Post output on linked PR; summary on issue.
create_draft_pr: true     # Optional. Create draft PR on completion.
mark_pr_ready_on_complete: true  # Optional. Mark PR ready when stage completes.
auto_advance: false       # Optional. Override global --yolo for this stage.
no_tmux: false            # Optional. Skip tmux wrapping for this stage.
cleanup_worktree: false   # Optional. Terminal stage — remove worktree, no Claude.
allowed_tools:            # Optional. Restrict Claude's available tools.
  - Read
  - Grep
  - Glob
completion:
  type: claude            # Only supported type (default).
```

---

## 3. Workflow Patterns

### How Issues Move Through the Pipeline

1. Create an issue and add it to your GitHub Project board.
2. Move the issue to a stage column (e.g., `Research`).
3. Fabrik picks it up on the next poll, creates a worktree, and invokes Claude Code.
4. Claude works in the worktree and posts progress as issue comments.
5. When Claude completes the stage, the `stage:<name>:complete` label is applied.
6. In `--yolo` mode, the issue is automatically moved to the next stage column.
   Otherwise, a human reviews and drags the card.

### Steering with Comments

Fabrik responds to natural language comments you post on an issue. Claude sees the full
issue body and all prior comments, so context carries forward.

**Effective comment patterns:**

- *"Please link the PR to this issue"* -- Claude creates the PR link
- *"Please process PR feedback on PR #18"* -- Claude reads PR reviews and addresses them
- *"Let's use approach B instead"* -- Claude updates the plan and continues
- *"The answer to your question about X is Y"* -- Claude incorporates your answer
- *"Please push and link the PR"* -- Claude pushes the branch and creates a draft PR

When you post a comment:
1. Fabrik reacts with eyes to acknowledge the comment.
2. Claude is invoked with the stage's `comment_prompt` (or a default prompt).
3. Claude performs any requested actions.
4. If the issue body should be updated, Claude outputs the new body between
   `FABRIK_ISSUE_UPDATE_BEGIN` and `FABRIK_ISSUE_UPDATE_END` markers.
5. Fabrik updates the issue body (or posts a comment if no markers are found).
6. Fabrik reacts with rocket to mark the comment as processed.

The rocket reaction is durable -- on restart, Fabrik skips comments that already have it.

### Reaction Flow

| Reaction | Meaning |
|----------|---------|
| Eyes | Comment received and queued for processing |
| Rocket | Comment has been fully processed |

### When to Intervene

You do not need to babysit the pipeline. The intended human role is:

- **File issues** with clear specs in the body.
- **Answer questions** when Research surfaces a checklist of unknowns.
- **Move cards** (or use `--yolo` to automate this).
- **Comment** to steer when the plan goes sideways or you want to redirect.
- **Review PRs** before merging -- Fabrik gets them review-ready, not merge-ready.

### Draft PR Workflow

The Implement stage creates a **draft PR** linked to the issue. This gives you a place
to review incrementally. The Review stage then rebases, reviews, fixes, and pushes --
turning the draft into a review-ready PR.

### Retry and Escalation

When a stage doesn't complete (Claude doesn't output `FABRIK_STAGE_COMPLETE`):

1. **Cooldown**: Fabrik waits `poll_interval x 10` seconds (default 5 minutes) before retrying.
2. **Resume**: On retry, Claude resumes the existing conversation session with full context.
3. **WIP commit**: Partial work is committed and pushed to preserve progress.
4. **Max retries**: After `--max-retries` failures (default 3):
   - `fabrik:paused` and `stage:<name>:failed` labels are added
   - An explanatory comment is posted on the issue
   - The issue stops being processed until a human investigates

To resume after escalation: remove the `fabrik:paused` label. Fabrik will clear the
failed label, reset the retry count, and try again immediately.

---

## 4. Stage Reference

### Default Pipeline

| Stage | Order | Purpose |
|-------|-------|---------|
| **Backlog** | -- | Parking lot. No stage config needed. |
| **Research** | 1 | Explore codebase, surface questions, summarize findings. |
| **Plan** | 2 | Design approach, break into tasks, document decisions. |
| **Implement** | 3 | Write code and tests, commit frequently, push to branch. |
| **Review** | 4 | Rebase, review, fix issues, push. Posts output on PR. |
| **Validate** | 5 | Run tests, verify requirements, confirm PR is ready. |
| **Done** | -- | Terminal state. Cleanup stage removes worktree. |

### Customizing Stages

```bash
# Initialize with defaults
./fabrik init

# Edit stage configs
vim .fabrik/stages/research.yaml

# Or point to a custom directory
./fabrik --stages ./my-custom-stages ...
```

You can add, remove, or reorder stages. Stages must have `name`, `order`, and `prompt`
fields. The `name` must match a board column and `order` values define the sequence.

---

## 5. Labels Reference

### Fabrik-Managed Labels

| Label | Purpose |
|-------|---------|
| `fabrik:locked:<user>` | Issue being processed by this user's instance |
| `fabrik:editing` | Issue body being updated (comment processing) |
| `fabrik:paused` | Processing paused (max retries exceeded or manual) |
| `stage:<name>:in_progress` | Stage actively running |
| `stage:<name>:complete` | Stage completed successfully |
| `stage:<name>:failed` | Stage hit max retries |

### User-Set Labels

| Label | Effect |
|-------|--------|
| `model:opus` | Override Claude model to Opus for this issue |
| `model:sonnet` | Override Claude model to Sonnet for this issue |
| `fabrik:paused` | Manually pause processing (add to pause, remove to resume) |

Model label precedence: `model:<name>` label > stage YAML `model` field > default.

---

## 6. TUI Dashboard

Enable the interactive terminal dashboard with `--tui`:

```bash
./fabrik --tui --owner your-org --repo your-repo --project 1 --user you
```

### Layout

The TUI shows three panes:
- **Header**: Poll timer countdown, latest status message
- **In Progress**: Active Claude sessions with issue title, stage, elapsed time, and last status
- **History**: Completed jobs with title, stage, duration, turns, cost, and timestamp

### Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Tab` | Switch focus between In Progress and History panes |
| `Up/Down` or `j/k` | Navigate items within the focused pane |
| `a` | Attach to tmux session for selected in-progress job |
| `l` | Open latest log file for selected history job |
| `C` (shift-c) | Clear history |
| `q` | Quit |

### History Persistence

Job history is saved to `~/.fabrik/history.json` and restored on restart.

---

## 7. Observability

### Tmux Sessions

By default, each Claude invocation runs inside a named tmux session. You can observe
Claude working in real-time:

```bash
# List active sessions
tmux list-sessions | grep fabrik

# Attach to a session
tmux attach -t fabrik-42-implement
```

The tmux pane shows a human-readable stream of Claude's activity:
- Thinking blocks (with reasoning)
- Text responses
- Tool calls with compact argument summaries
- Completion summary with turns and cost

Detach with `Ctrl-b d` without interrupting Claude's work.

To disable tmux wrapping: `--no-tmux` flag, `FABRIK_NO_TMUX=true`, or `no_tmux: true`
in the stage YAML.

### Log Files

Session logs are saved to `~/.fabrik/logs/issue-<N>/`:
```bash
ls -lt ~/.fabrik/logs/issue-42/
# Research-20260402-150405-1775174307809137000.log
# Implement-20260402-160000-1775174400000000000.log
```

### Debug Output

Enable `--debug-output` to save Claude's raw output to `.fabrik/debug/` in the
working directory. Useful for diagnosing prompt issues or unexpected behavior.

### Rate Limit Monitoring

Fabrik reports GitHub API rate limit stats in each poll cycle:
- REST API: requests remaining / limit
- GraphQL API: points remaining / limit

The GraphQL query uses a two-phase fetch (shallow board scan + targeted detail fetch)
to minimize rate limit consumption. Typical cost is ~5-30 points per poll depending on
active items, well within the 5,000 points/hour limit.

---

## 8. Troubleshooting

### Issue Not Being Picked Up

- Confirm the board column name exactly matches a stage YAML `name` (case-sensitive).
- Confirm the issue is on the project board.
- Check for `fabrik:locked:<other-user>` label (another instance has it).
- Check for stuck `fabrik:editing` label (remove manually if stale).
- Check the poll log -- the issue may be filtered by `updatedAt` caching. Restart Fabrik
  to force a fresh scan.

### Stage Keeps Retrying

A stage that never outputs `FABRIK_STAGE_COMPLETE` retries after each cooldown. Causes:

- **Claude found an unfixable issue** -- comment to provide guidance.
- **Missing context** -- add detail to the issue body.
- **Bug in the stage prompt** -- check that the prompt instructs Claude to output
  `FABRIK_STAGE_COMPLETE` when done.

After `--max-retries` failures, the issue is paused with `fabrik:paused`. Remove the
label to resume.

### Stale Worktrees

Worktrees are at `.fabrik/worktrees/issue-N/` on branch `fabrik/issue-N`. On each stage
invocation, Fabrik rebases onto latest main (unless it's a retry). If the rebase
conflicts, it's silently aborted and Claude works from the current base.

To manually clean up:
```bash
git worktree remove --force .fabrik/worktrees/issue-N
git branch -D fabrik/issue-N
```

### Killed or Interrupted Sessions

Claude sessions are stored at `~/.fabrik/sessions/issue-N/<stage>.session`. On retry,
Fabrik resumes from the session file. The Implement stage commits frequently to minimize
lost work.

To force a fresh session:
1. Remove: `rm ~/.fabrik/sessions/issue-N/<stage>.session`
2. Remove `stage:<name>:complete` label if incorrectly applied
3. Fabrik starts a fresh session on next poll

### Comment Reprocessed After Restart

The rocket reaction is the durable "processed" marker. If a comment gets reprocessed
after restart, it means the rocket was not applied before shutdown (killed mid-flight).
This is expected for in-flight comments.

### Post-to-PR Output Missing

For stages with `post_to_pr: true`:
- Confirm the issue has a linked PR (via "Development" section or `Closes #N` in PR body)
- If no PR is found, output falls back to the issue

### Raw JSON in Comments

If you see raw JSON dumped in issue/PR comments, Claude Code's output format has
changed. Update Fabrik to the latest version -- `parseClaudeJSON` handles multiple
output formats (single JSON, JSON array, and stream-json NDJSON).

### Multi-User Conflicts

Multiple users can run Fabrik against the same board. Each instance only processes
comments by its `--user`. The `fabrik:locked:<user>` label prevents conflicts.
