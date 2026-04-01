# Fabrik — Automated Claude Code SDLC Driver

## Overview

Fabrik is a local CLI tool (written in Go) that orchestrates Claude Code through a software development lifecycle defined as **stages** on a GitHub Project board. GitHub Issues are the unit of work. The issue body is the "spec," comments are user input, and the Project board's columns represent workflow stages. Fabrik watches the board, drives Claude Code through each stage, and advances issues — either automatically ("yolo mode") or waiting for human approval.

## Core Concepts

### Issue as Work Unit

- Each GitHub Issue represents a piece of work (feature, bug, task).
- The issue **body** is the spec/requirements.
- **Comments** on the issue are user input/feedback that Fabrik reprocesses.
- Issues live in a GitHub Project (v2) with a Kanban board.

### Stages

- Each column/status on the Project board is a **stage** (e.g., `triage`, `design`, `implement`, `review`, `done`).
- Each stage maps to a **Claude Code agent/skill configuration** — a specific prompt, set of tools, and behavioral definition.
- Stages are user-defined and configurable. Define as many as you want.
- Stage configs live in the repo (e.g., `stages/design.yaml`).

### Processing Loop

1. Fabrik polls the GitHub Project board (via GraphQL API — single query to pull the whole board).
2. For each issue assigned to the current user in an active stage:
   - Run the stage's Claude Code configuration against the issue.
   - Loop until the stage's **completion criteria** are met (all tasks checked off, all questions answered, defined end state).
   - On completion, label the issue `stage:<name>:complete`.
3. **Advancement:**
   - **Yolo mode:** Auto-advance the issue to the next stage.
   - **Non-yolo:** Wait for human interaction — either advance to another stage, or add a comment.
4. Comments trigger reprocessing in the context of the current stage. Claude sessions and worktrees are preserved as caches to maintain continuity.

### Multi-User / Concurrency

- The driver only processes changes **made by the configured user** (you).
- Multiple people can run their own Fabrik instances watching the same board.
- Labels on issues serve as lightweight locks to prevent two drivers from grabbing the same issue/comment.
- The "who made the change" rule is the primary guard.

### Architecture

- **Written in Go.**
- Runs locally (your laptop), alongside Claude Code.
- GitHub GraphQL API for efficient board state retrieval (single query).
- Local state treated as a **write-through cache** of the GitHub Project state.
- Claude Code sessions and git worktrees cached locally for continuity.

### Observability & Skill Tuning (Future)

- Track how well each stage's skill configuration performs.
- Audit/oversight of agent behavior per stage.
- Detect drift from Claude Code releases or changing behavior.
- Enable iterative tuning of stage configurations based on observed outcomes.

## Key Design Decisions

- **All authoritative state lives in GitHub** — cloud-based, multi-user, shared.
- **No custom database** — GitHub Issues + Project board are the data store.
- **Stages are composable** — each is an independent Claude Code configuration.
- **Human-in-the-loop by default** — yolo mode is opt-in.
