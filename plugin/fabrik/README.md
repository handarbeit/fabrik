# Fabrik — Claude Code Plugin

A Claude Code plugin that gives your interactive Claude session ambient awareness of [Fabrik](https://fabrik.shadoworg.dev), the GitHub-Project-driven SDLC pipeline orchestrator. With this plugin installed, Claude becomes a **product-management buddy** for projects that use Fabrik to automate issue processing — helping you author specs Fabrik can act on, read board state, decide when to intervene, and bootstrap Fabrik in new projects.

## What this plugin is *not*

This plugin (`fabrik`) is for the **human's interactive Claude Code session**. It is distinct from `fabrik-workflows` (the worker-side plugin embedded in the Fabrik binary, which Fabrik deploys to `.fabrik/plugin/` and loads into the Claude Code workers it spawns to execute pipeline stages). The two plugins have no overlap:

| Plugin | Audience | Role |
|---|---|---|
| `fabrik-workflows` | Worker Claude (spawned by `fabrik` engine) | Stage-execution skills (Specify, Research, Plan, Implement, Review, Validate) |
| `fabrik` *(this one)* | Human's interactive Claude Code | PM buddy — talks about Fabrik with the user |

## What it provides

### Skills (ambient — no slash command needed)

| Skill | When it activates |
|---|---|
| `fabrik:fabrik-setup` | User wants to install or bootstrap Fabrik in a project that doesn't yet have a `.fabrik/` directory |
| `fabrik:fabrik` | User is working in (or asking about) a project that uses Fabrik — `.fabrik/` exists, or the conversation references stages, the board, workers, or `fabrik:`-prefixed labels |

The supervisor skill (`fabrik:fabrik`) is **ambient**: it loads Fabrik's mental model into the conversation so Claude can advise as a PM buddy without the user having to invoke anything explicitly.

### Slash commands

| Command | Purpose |
|---|---|
| `/fabrik:status` | Summarise what Fabrik is currently doing — board state, in-flight workers, worktrees with uncommitted changes |

## Installation

Install via a Claude Code marketplace (recommended), or for local testing:

```bash
claude --plugin-dir /path/to/fabrik/plugin/fabrik
```

## More information

- [Fabrik docs site](https://fabrik.shadoworg.dev)
- [User Guide](https://fabrik.shadoworg.dev/USER_GUIDE) — installation, configuration, labels
- [State Machine](https://fabrik.shadoworg.dev/state-machine) — authoritative engine spec
- [Stage Lifecycle](https://fabrik.shadoworg.dev/stage-lifecycle) — per-invocation lifecycle
