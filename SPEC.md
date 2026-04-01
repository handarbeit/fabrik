# Fabrik — Automated Claude Code SDLC Driver

## Overview

Fabrik is a local CLI tool (written in Go) that orchestrates Claude Code through a software development lifecycle defined as **stages** on a GitHub Project board. GitHub Issues are the unit of work. The issue body is the "spec," comments are user input, and the Project board's columns represent workflow stages. Fabrik watches the board, drives Claude Code through each stage, and advances issues — either automatically ("yolo mode") or waiting for human approval.

## Core Concepts

### Issue as Work Unit

- Each GitHub Issue represents a piece of work (feature, bug, task).
- The issue **body** is the spec/requirements — a living document that evolves as stages process it.
- **Comments** on the issue are user input/feedback that Fabrik processes and incorporates into the issue body.
- Issues live in a GitHub Project (v2) with a Kanban board.

### Stages

- Each column/status on the Project board is a **stage** (e.g., `Research`, `Plan`, `Implement`, `Review`, `Validate`).
- Each stage maps to a **Claude Code agent configuration** — a specific prompt, model, tool set, and completion criteria.
- Stages are user-defined and configurable via YAML files.
- Each stage can define both a main processing prompt and a comment review prompt.
- Stage configs live in the repo (e.g., `stages/examples/research.yaml`).

### Processing Loop

1. Fabrik polls the GitHub Project board (via GraphQL API — single query to pull the whole board).
2. For each issue in an active stage:
   - **New comments**: Process user comments first — react with :eyes:, invoke Claude with the comment review prompt (Claude can perform actions and/or update the issue body), react with :rocket:.
   - **Stage processing**: If the stage hasn't been run yet, invoke Claude with the stage prompt in the issue's isolated worktree. The prompt includes all prior comments (previous stage outputs and user feedback) as context.
   - Loop until the stage's **completion criteria** are met.
   - On completion, label the issue `stage:<name>:complete`.
3. **Advancement:**
   - **Yolo mode:** Auto-advance the issue to the next stage.
   - **Non-yolo (default):** Wait for human to review and drag to the next column.

### Comment Processing

When a user comments on an issue:
1. :eyes: reaction added (marks as "in review")
2. `fabrik:editing` label applied (locks issue during update)
3. Claude invoked with stage-specific comment review prompt
4. Claude performs any requested actions using available tools
5. If issue body needs updating, updated body extracted from Claude output (between `FABRIK_ISSUE_UPDATE_BEGIN`/`END` markers)
6. Issue body updated on GitHub (or output posted as comment if no markers)
7. `fabrik:editing` label removed
8. :rocket: reaction added (marks as "processed"; also used to skip already-processed comments on restart)

### Git Worktrees

- Each issue gets an isolated worktree at `.fabrik/worktrees/issue-N/` on branch `fabrik/issue-N`.
- Claude Code runs inside the worktree, so multiple issues can be in flight without conflicting.
- Worktrees persist across polls for session continuity.
- Branches fork from the default branch, ready to become PRs.

### Multi-User / Concurrency

- The driver only processes changes **made by the configured user** (you).
- Multiple people can run their own Fabrik instances watching the same board.
- Labels on issues serve as lightweight locks:
  - `fabrik:locked:<user>` — processing lock per user
  - `fabrik:editing` — body update lock
  - `fabrik:paused` — issue is skipped entirely; no stage processing or comment processing occurs
- The "who made the change" rule is the primary guard.

### Architecture

- **Written in Go.**
- Runs locally (your laptop), alongside Claude Code.
- GitHub GraphQL API for efficient board state retrieval (single query).
- Local state is ephemeral (in-memory processed set, session files, worktrees).
- All authoritative state lives in GitHub.

### Observability & Skill Tuning (Future)

- Track how well each stage's skill configuration performs.
- Audit/oversight of agent behavior per stage.
- Detect drift from Claude Code releases or changing behavior.
- Enable iterative tuning of stage configurations based on observed outcomes.

## Key Design Decisions

- **All authoritative state lives in GitHub** — cloud-based, multi-user, shared.
- **No custom database** — GitHub Issues + Project board are the data store.
- **Polling over webhooks** — runs on a laptop, no public endpoint needed.
- **Stages are composable** — each is an independent Claude Code configuration.
- **Human-in-the-loop by default** — yolo mode is opt-in.
- **Git worktrees for isolation** — each issue gets its own branch and working directory.
- **Shell out to Claude CLI** — leverages full Claude Code capabilities without reimplementation.

See [adrs/](adrs/) for detailed rationale behind each decision.
