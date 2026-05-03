---
layout: docs
title: Fabrik Positioning
---

# Fabrik Positioning Notes

> Working notes for future marketing use. Not finished copy. Landscape snapshot as of 2026-04.

## Landscape

### Proprietary / commercial kanban UIs for Claude Code

- **Vibe Kanban** (BloopAI, github.com/BloopAI/vibe-kanban) — most polished; kanban issues drive agent workspaces, each with a branch + terminal + dev server; auto-updates issue status on PR open/merge; supports 10+ agents.
- **Kanban Code** (langwatch, github.com/langwatch/kanban-code) — cards auto-move (In Progress → Waiting → In Review); GitHub issue backlog integration; session forking/checkpointing; remote SSH offload.
- **Claude Code Board** (github.com/cablate/Claude-Code-Board) — multi-session management UI with workflow stages and agent selection.

### Open source Claude Code + kanban

- **NikiforovAll/claude-code-kanban** — hook-based observer; reflects Claude's internal task state onto a board (Pending → In Progress → Completed); npx install.
- **cruzyjapan/Claude-Code-Kanban-Automator** — React/Node/SQLite; tasks on board trigger Claude Code execution.
- **alessiocol/claude-kanban** — `ACTIVE.md` as shared kanban state across multiple agent sessions; addresses context amnesia between sessions; WIP limits per agent.
- **The Claude Protocol** (github.com/AvivK5498/The-Claude-Protocol) — 13 Claude Code hooks enforce workflow; integrates with Beads (git-native tickets) as kanban; immutable closed issues; git worktree per task.

### Platform-level

- **Linear AI Agents** — first-class agent delegation from issues; Copilot integration opens draft PRs from Linear issues.
- **GitHub Agentic Workflows** (technical preview, Feb 2026) — Markdown-described workflows run Claude Code/Copilot/Codex in GitHub Actions; triggered by issue events, schedules, or comment commands; MIT licensed.

### Patterns observed

- Most tools build their own proprietary kanban rather than integrating with GitHub Projects.
- Split between *observer* tools (watch Claude, reflect state) vs *driver* tools (kanban is source of truth that triggers agent work).
- Multi-agent coordination (alessiocol, The Claude Protocol) is a distinct and less crowded sub-pattern.
- GitHub Agentic Workflows is the primary path toward using native GitHub Projects as the driver.

## Where Fabrik sits uniquely

1. **GitHub Projects *is* the kanban.** Unlike Vibe Kanban, Kanban Code, Claude Code Board, and the cruzyjapan/alessiocol/NikiforovAll cluster, Fabrik ships no UI. The board you already use in GitHub Projects is the driver. Same philosophical bucket as GitHub Agentic Workflows and Linear Agents, but without requiring GitHub Actions or a proprietary platform.

2. **Driver, not observer.** The poll loop reads board columns and dispatches Claude Code per issue. NikiforovAll's observer model is the opposite direction.

3. **YAML-configurable SDLC pipeline.** Research → Plan → Implement → Review → Validate are not hardcoded. Each stage has its own prompt, skill, model, tool allowlist, `max_turns`, and lifecycle flags (`post_to_pr`, `create_draft_pr`, `read_only`, `cleanup_worktree`, `wait_for_reviews`). Vibe Kanban and Kanban Code have fixed workflows; The Claude Protocol hardcodes 13 hooks. Closer to GitHub Agentic Workflows' markdown-described workflows, but typed and validated against the board on startup.

4. **Durable state via GitHub-native primitives.** Comment reactions (👀/🚀) survive restarts; labels (`stage:*:complete`, `fabrik:paused`, `fabrik:awaiting-review`, `model:*`, `effort:*`, `fabrik:yolo`, `fabrik:cruise`) encode control flow. No SQLite, no `ACTIVE.md`, no proprietary store. Everything inspectable by a human on github.com.

5. **Comment-driven revision loop.** Users comment on issues/PRs to steer a stage; each stage can define a separate `comment_prompt` / `comment_skill` / `comment_max_turns`. No project in the landscape above cleanly distinguishes initial-run prompts from comment-response prompts — most treat the whole agent session as one-shot per card.

6. **Worktree-per-issue with preservation semantics.** Bare repo + per-issue worktree on `fabrik/issue-N`, `.fabrik-context/` files injected before each stage (issue body, prior stage outputs, PR description, codebase diff since last stage ran). The Claude Protocol also uses worktrees but couples them to Beads tickets; Vibe Kanban uses branches + terminals without the context-file discipline.

## Where Fabrik is not differentiated

- Git worktree per task — shared with Vibe Kanban and The Claude Protocol.
- Draft PR → mark-ready lifecycle — shared with most driver tools.
- Multi-agent support — Fabrik is Claude Code only; Vibe Kanban supports 10+ agents. Conscious scope choice, but worth naming when comparing.

## Candidate positioning lines

- *GitHub Agentic Workflows without GitHub Actions — your existing Project board becomes an SDLC pipeline, with YAML-configurable stages and per-stage Claude Code skills, state encoded entirely in issues, labels, and reactions.*
- *Closest competitor by philosophy: GitHub Agentic Workflows (platform-native, Projects-compatible). Closest by feature set: The Claude Protocol (hooks + worktrees + tickets). Fabrik's niche is the intersection — platform-native and feature-rich, without requiring GHA or a separate ticket system like Beads.*
