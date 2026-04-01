# Fabrik User Guide

Fabrik is an automated Claude Code SDLC driver powered by GitHub Issues and Projects.
This guide covers everything you need to set up, configure, and use Fabrik effectively.

For a quick overview see the [README](../README.md).

---

## Table of Contents

1. [Getting Started](#1-getting-started)
2. [Configuration Reference](#2-configuration-reference)
3. [Workflow Patterns](#3-workflow-patterns)
4. [Stage Reference](#4-stage-reference)
5. [Labels Reference](#5-labels-reference)
6. [Troubleshooting](#6-troubleshooting)

---

## 1. Getting Started

### Prerequisites

- Go 1.21+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub personal access token with `repo` and `project` scopes
- A GitHub Project (v2) with board columns matching your stage names

### Initial Setup

```bash
# Build
go build -o fabrik .

# Set your GitHub token
export GITHUB_TOKEN=ghp_...

# Copy the example stages and customize
cp -r stages/examples stages/mystages
```

### Create a Project Board

Create a GitHub Project (v2) for your repository. Add board columns that correspond to
your stage names — the column name must match the `name` field in each stage YAML file
exactly (case-sensitive). The default pipeline uses:

`Backlog` → `Research` → `Plan` → `Implement` → `Review` → `Validate` → `Done`

### First Run

```bash
./fabrik \
  --owner your-org \
  --repo your-repo \
  --project 1 \
  --user your-github-username \
  --stages ./stages/mystages
```

Fabrik polls the project board by default every 30 seconds (configurable with `--poll`). Move an issue to `Research` on the
board to start processing it.

### Development Mode (self-upgrade)

The `--auto-upgrade` flag enables Fabrik to upgrade itself when idle. After 2
consecutive idle polls, Fabrik checks `origin/main` for new commits. If found, it
runs `git pull --ff-only`, rebuilds the binary with `go build`, and re-execs itself.
This is intended for the self-evolving workflow where Fabrik develops Fabrik.

```bash
./fabrik --auto-upgrade --owner your-org --repo your-repo --project 1 --user you
```

Upgrade lifecycle events are logged; failures are non-fatal and logged as warnings.

---

## 2. Configuration Reference

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--owner` | GitHub repo owner (org or user) | required |
| `--repo` | GitHub repo name | required |
| `--project` | GitHub Project (v2) number | required |
| `--user` | Your GitHub username — only processes board changes made by this user | required |
| `--token` | GitHub API token | `$GITHUB_TOKEN` |
| `--stages` | Directory containing stage YAML configs | `./stages` |
| `--yolo` | Auto-advance issues through stages without human approval | `false` |
| `--auto-upgrade` | When idle for 2 polls, check origin/main for new commits and self-upgrade | `false` |
| `--poll` | Poll interval in seconds | `30` |

### `.env` File Support

Fabrik loads configuration from a `.env` file in the working directory if present.
All flags can be set via `.env` — flags take precedence over `.env` values.

```
FABRIK_TOKEN=ghp_...         # Preferred token env var
GITHUB_TOKEN=ghp_...         # Fallback token env var (backward-compatible)
FABRIK_OWNER=your-org
FABRIK_REPO=your-repo
FABRIK_PROJECT_NUMBER=1
FABRIK_USER=your-username
FABRIK_STAGES=./stages/mystages
FABRIK_YOLO=true
FABRIK_POLL=30
FABRIK_MAX_CONCURRENT=5
```

**Safety:** If a `.env` file exists but is not listed in `.gitignore`, Fabrik will
refuse to start with a fatal error to prevent accidental token leaks.

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `FABRIK_TOKEN` | GitHub personal access token (preferred) | required (or use `--token`) |
| `GITHUB_TOKEN` | GitHub personal access token (fallback) | required (or use `FABRIK_TOKEN`) |
| `FABRIK_OWNER` | GitHub repo owner | — |
| `FABRIK_REPO` | GitHub repo name | — |
| `FABRIK_PROJECT_NUMBER` | GitHub Project (v2) number | — |
| `FABRIK_USER` | Your GitHub username | — |
| `FABRIK_STAGES` | Stage configs directory | `./stages` |
| `FABRIK_YOLO` | Auto-advance (`true`/`1`/`yes`) | `false` |
| `FABRIK_POLL` | Poll interval in seconds | `30` |
| `FABRIK_MAX_CONCURRENT` | Maximum number of issues processed simultaneously | `5` |

Token precedence: `--token` flag > `FABRIK_TOKEN` > `GITHUB_TOKEN`

`FABRIK_MAX_CONCURRENT` controls how many Claude Code sessions can run in parallel.
Increase it if you have many issues in active stages; lower it to reduce API costs or
system load. Invalid values fall back to the default of `5`.

### Stage YAML Reference

Each stage is a YAML file in your stages directory. The filename is arbitrary; the
`name` field determines which board column it matches.

```yaml
name: Research            # Required. Must match a Project board column exactly.
order: 1                  # Required. Lower values are processed earlier in the pipeline.
prompt: |                 # Required. System prompt given to Claude Code for stage work.
  You are a research agent...
comment_prompt: |         # Optional. Prompt used when processing user comments on this stage.
  You are reviewing user comments...
model: sonnet             # Optional. Claude model to use: "opus", "sonnet", etc.
max_turns: 10             # Optional. Maximum conversation turns Claude Code may take.
post_to_pr: true          # Optional. Post detailed output on the linked PR; brief summary on issue.
allowed_tools:            # Optional. Restrict which Claude Code tools are available.
  - Read
  - Grep
  - Glob
completion:
  type: claude            # "claude" (default and only supported type)
  value: ""               # Reserved for future completion types
auto_advance: false       # Optional. Override global --yolo setting for this specific stage.
```

#### Completion Types

| Type | Behavior |
|------|----------|
| `claude` | Claude signals completion by outputting the exact line `FABRIK_STAGE_COMPLETE` (default) |

Stages that do not complete apply a cooldown before retrying (see [Cooldown on Incomplete Stages](#cooldown-on-incomplete-stages)).

#### Model Override via Stage Config

Set `model: opus` or `model: sonnet` in a stage YAML to use a specific model for that
stage. This is useful for balancing cost vs. capability — e.g., using `sonnet` for
lightweight Research and `opus` for complex Implement work.

#### `allowed_tools`

Restrict the Claude Code tools available during a stage. An empty list (the default)
allows all tools. Example — read-only Research stage:

```yaml
allowed_tools:
  - Read
  - Grep
  - Glob
```

Available tools include the full Claude Code tool set: `Read`, `Write`, `Edit`, `Bash`,
`Grep`, `Glob`, `LSP`, and others.

#### `post_to_pr`

When `true`, the stage's detailed Claude output is posted as a comment on the linked PR,
and a brief summary is posted on the issue. Requires a PR to be linked to the issue.
If no linked PR is found, output falls back to the issue.

This is used by the default Review stage to keep detailed code-review feedback on the
PR where it belongs, while keeping the issue comment thread clean.

When using `post_to_pr: true`, include instructions in your prompt for Claude to emit
a `FABRIK_SUMMARY_BEGIN` / `FABRIK_SUMMARY_END` block with a 2–4 sentence summary of
what was done. Fabrik extracts this summary and posts it on the issue.

#### `auto_advance`

Override the global `--yolo` flag for a specific stage:

```yaml
auto_advance: true   # Always auto-advance from this stage, even without --yolo
auto_advance: false  # Always wait for human approval from this stage, even with --yolo
```

Omitting `auto_advance` inherits the global `--yolo` setting.

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

- *"Please link the PR to this issue"* — Claude creates the PR link
- *"Please process PR feedback on PR #18"* — Claude reads PR reviews and addresses them
- *"Let's use approach B instead"* — Claude updates the plan and continues
- *"The answer to your question about X is Y"* — Claude incorporates your answer
- *"Please push and link the PR"* — Claude pushes the branch and creates a draft PR

When you post a comment:
1. Fabrik reacts with 👀 to acknowledge the comment.
2. Claude is invoked with the stage's `comment_prompt` (or a default prompt).
3. Claude performs any requested actions.
4. If the issue body should be updated, Claude outputs the new body between
   `FABRIK_ISSUE_UPDATE_BEGIN` and `FABRIK_ISSUE_UPDATE_END` markers.
5. Fabrik updates the issue body (or posts a comment if no markers are found).
6. Fabrik reacts with 🚀 to mark the comment as processed.

The 🚀 reaction is durable — on restart, Fabrik skips comments that already have it,
preventing duplicate processing.

### Reaction Flow Summary

| Reaction | Meaning |
|----------|---------|
| 👀 | Comment received and queued for processing |
| 🚀 | Comment has been fully processed |

### When to Intervene

You do not need to babysit the pipeline. The intended human role is:

- **File issues** with clear specs in the body.
- **Answer questions** when Research surfaces a checklist of unknowns.
- **Move cards** (or use `--yolo` to automate this).
- **Comment** to steer when the plan goes sideways or you want to redirect.
- **Review PRs** before merging — Fabrik gets them review-ready, not merge-ready.

### Draft PR Workflow

The Implement stage is intended to push commits and create a **draft PR** linked to
the issue. This gives you a place to review incrementally. The Review stage then
rebases, reviews, fixes, and pushes — turning the draft into a review-ready PR.

If Claude does not automatically create the PR, comment:
*"Please push and link the PR"*

### Review and Fix Workflow

The default Review stage (`post_to_pr: true`) does the following:
1. Checks for uncommitted changes from previous sessions and commits/incorporates them.
2. Rebases the branch onto latest main and resolves merge conflicts.
3. Reviews the code for correctness, security issues, test coverage, and style.
4. Fixes any issues found, committing after each fix.
5. Pushes to the PR branch.
6. Posts detailed findings on the PR and a brief summary on the issue.

If the Review stage finds an issue it cannot fix autonomously (e.g., a design decision),
it lists the blocker clearly and does **not** signal completion. The stage will retry
after a cooldown, or you can comment to provide guidance.

### Cooldown on Incomplete Stages

If a stage doesn't signal completion (no `FABRIK_STAGE_COMPLETE` output), Fabrik
applies a cooldown before retrying. The cooldown is `poll_interval × 10` seconds
(default: 30s × 10 = 5 minutes).

After the cooldown expires, Fabrik retries the stage automatically. This prevents
hot-looping on stages awaiting external input (like a human answering a question or
a CI check completing).

---

## 4. Stage Reference

### Default Pipeline

| Stage | Order | Purpose |
|-------|-------|---------|
| **Backlog** | — | Parking lot. No stage config needed; issues here are not processed. |
| **Research** | 1 | Explore codebase, surface questions, summarize findings. |
| **Plan** | 2 | Design approach, break into tasks, document decisions. |
| **Implement** | 3 | Write code and tests, commit frequently, push to branch. |
| **Review** | 4 | Rebase, review, fix issues, push. Posts output on PR. |
| **Validate** | 5 | Run tests, verify requirements met, confirm PR is ready. |
| **Done** | — | Terminal state. No stage config needed; issues here are not processed. |

### Research Stage

- **Model:** `sonnet`
- **Max turns:** 10
- **Completion:** `claude` (outputs `FABRIK_STAGE_COMPLETE`)
- **Comment prompt:** Incorporates user answers into the issue body, removes resolved questions.

Claude explores the codebase, lists relevant files, surfaces ambiguities as a checklist,
and summarizes findings in the issue body. When all questions are answered (by user
comments), Claude signals completion.

### Plan Stage

Claude reads the research output and designs a solution. Output is written into the
issue body as a structured plan with a task checklist that later stages can follow.

### Implement Stage

- **Model:** `sonnet`
- **Max turns:** 50
- **Completion:** `claude`

Claude checks for prior uncommitted work (from interrupted sessions), implements the
plan task-by-task, commits after each logical unit, writes tests, and pushes commits
to the `fabrik/issue-N` branch.

### Review Stage

- **Model:** `sonnet`
- **Max turns:** 30
- **`post_to_pr: true`**
- **Completion:** `claude`

Claude rebases, reviews, fixes, and pushes. Detailed findings go on the PR; a brief
summary goes on the issue. If issues are unfixable, Claude blocks completion and
describes the blocker.

### Validate Stage

Claude runs the test suite, checks that requirements in the issue body are met, and
confirms the PR is ready for human merge review.

### Customizing Stages

Copy the example stages and modify them:

```bash
cp -r stages/examples stages/mystages
# Edit stages/mystages/*.yaml
./fabrik --stages ./stages/mystages ...
```

You can add, remove, or reorder stages. Stages must include the required fields
described in the YAML reference (for example, `name`, `order`, and `prompt`). In addition,
`name` should match a board column and `order` values should be unique and define the stage sequence.

---

## 5. Labels Reference

### Fabrik-Managed Labels

Fabrik creates and manages these labels automatically:

| Label | Purpose |
|-------|---------|
| `fabrik:locked:<user>` | Issue is being actively processed by this user's Fabrik instance. Other instances skip the issue. |
| `fabrik:editing` | Issue body is being updated. Prevents concurrent edits. |
| `stage:<name>:complete` | The named stage has been completed. Prevents re-running after restart. |

### Model Override Labels

Add a `model:<name>` label to an issue to override the model for that specific issue,
regardless of what the stage YAML specifies:

| Label | Effect |
|-------|--------|
| `model:opus` | Use Claude Opus for all stage invocations on this issue |
| `model:sonnet` | Use Claude Sonnet for all stage invocations on this issue |

The label override takes precedence over the stage YAML `model` field. If multiple
`model:*` labels are present, the first one is used and the others are ignored with
a warning in the logs.

Use model labels when an issue is unusually complex and needs extra capability, or when
a particular issue can be handled with a lighter model to reduce cost.

### Label Precedence

```
model:<name> label on issue  >  model field in stage YAML  >  no model specified
```

---

## 6. Troubleshooting

### Stale Worktrees

Worktrees are created at `.fabrik/worktrees/issue-N/` on branch `fabrik/issue-N`.
Fabrik keeps them up to date with main automatically: on each poll, if the worktree
has no uncommitted changes, Fabrik fetches and merges the latest main.

If a worktree has uncommitted changes, the auto-update is skipped and Claude will
handle any necessary rebasing during the stage (as the Review stage does explicitly).

To manually clean up a worktree:
```bash
git worktree remove --force .fabrik/worktrees/issue-N
git branch -D fabrik/issue-N
```

### Killed or Interrupted Sessions

Claude Code sessions are stored at `~/.fabrik/sessions/issue-N/stage-name.session`.
If a session is interrupted mid-stage, Fabrik resumes from the session file on the
next poll. The Implement stage's instruction to commit frequently minimizes lost work.

If a session is in a bad state and you want a clean restart:
1. Remove the session file: `rm ~/.fabrik/sessions/issue-N/stage-name.session`
2. Remove the `stage:<name>:complete` label from the issue (if incorrectly applied)
3. Fabrik will start a fresh session on the next poll

### Comment Reprocessed After Restart

The 🚀 reaction on a comment is the durable "processed" marker. If you restart Fabrik
and a comment gets processed again, it means the 🚀 reaction was not applied before
shutdown (e.g., Fabrik was killed mid-flight).

This is expected behavior for the in-flight comment. Subsequent polls will not
reprocess it once 🚀 is applied.

### Stage Keeps Retrying

A stage that never outputs `FABRIK_STAGE_COMPLETE` will retry indefinitely after each
cooldown period. Common causes:

- **Claude found an unfixable issue** — comment on the issue to provide guidance or
  make a decision. Claude will incorporate your input and may then be able to complete.
- **Missing context in the issue body** — the issue spec may be incomplete. Add detail
  or answer Claude's questions via comments.
- **Bug in the stage prompt** — the prompt may not instruct Claude to signal completion.
  Check your stage YAML `prompt` field ends with instructions to output `FABRIK_STAGE_COMPLETE`.

### Issue Not Being Picked Up

- Confirm the board column name exactly matches the `name` field in a stage YAML (case-sensitive).
- Confirm the issue is on the board (issues not added to the project are not visible to Fabrik).
- Check that no `fabrik:locked:<other-user>` label is present (another instance has it locked).
- Check that no `fabrik:editing` label is stuck on the issue (remove it manually if stale).

### Multi-User Conflicts

Multiple users can run Fabrik against the same project board. Each instance only
processes changes made by its own `--user`. If you see an issue being processed by
another user's instance, the `fabrik:locked:<user>` label will be present — your
instance will skip it until the lock is released.

### Post-to-PR Output Missing

If a stage with `post_to_pr: true` is not posting to the PR:
- Confirm the issue has a linked PR. Fabrik searches for a PR with the issue number
  in its title or body, or linked via the GitHub "Development" section.
- If no PR is found, output falls back to the issue comment thread.

### FABRIK_SUMMARY Not Appearing

For `post_to_pr: true` stages, Claude needs to emit the summary markers in its output:

```
FABRIK_SUMMARY_BEGIN
Brief summary of what was done (2–4 sentences).
FABRIK_SUMMARY_END
```

If your stage prompt does not instruct Claude to produce this block, no summary will
appear on the issue. Add explicit instructions to your prompt:

```yaml
prompt: |
  ...your existing prompt...

  After completing your work, output a brief summary (2-4 sentences) wrapped in:
  FABRIK_SUMMARY_BEGIN
  <your summary here>
  FABRIK_SUMMARY_END

  Then signal completion with FABRIK_STAGE_COMPLETE
```
