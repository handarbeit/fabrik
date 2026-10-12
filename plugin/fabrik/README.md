# Fabrik — Claude Code Plugin

A Claude Code plugin that gives your interactive Claude session awareness of [Fabrik](https://fabrik.handarbeit.io), the GitHub-Project-driven SDLC pipeline orchestrator. With this plugin installed, Claude becomes a **product-management buddy and supervisor** for projects that use Fabrik: it answers "what's the status of #N?" and "what needs me?" from the running daemon, helps you author specs Fabrik can act on, steers items through audited overseer actions, and helps you bootstrap Fabrik in new projects.

## What this plugin is *not*

This plugin (`fabrik`) is for the **human's interactive Claude Code session**. It is distinct from `fabrik-stages` (the worker-side plugin embedded in the Fabrik binary, which Fabrik deploys to `.fabrik/plugin/` and loads into the Claude Code workers it spawns to execute pipeline stages). Fabrik also keeps this plugin out of every worker it spawns.

| Plugin | Audience | Role |
|---|---|---|
| `fabrik-stages` | Worker Claude (spawned by `fabrik` engine) | Stage-execution skills (Specify, Research, Plan, Implement, Review, Validate) |
| `fabrik` *(this one)* | Human's interactive Claude Code | PM buddy — talks about Fabrik with the user |

## Install

In Claude Code:

```
/plugin marketplace add handarbeit/fabrik
/plugin install fabrik@fabrik
```

The `fabrik` binary must be on your `PATH` (the plugin starts `fabrik mcp`), and a Fabrik daemon must be running in your project for the live tools to answer. Each Fabrik release pins the marketplace to its own tag, and the plugin's version equals the binary's, so you only ever receive what was released.

**Auto-update** is off by default for third-party marketplaces. To turn it on, open `/plugin`, go to **Marketplaces**, select `fabrik` and enable auto-update. Without it, run `/plugin marketplace update fabrik` after you upgrade the binary.

**Version notice.** At session start the plugin compares its version with `fabrik --version` and shows one line if they differ (a development build says it cannot be compared). It never blocks the session and is silent if `fabrik` is not installed. Set `FABRIK_PLUGIN_VERSION_CHECK=off` in your environment to silence it.

For local testing: `claude --plugin-dir /path/to/fabrik/plugin/fabrik`.

### Settings

Claude Code asks for these when you enable the plugin; both are optional.

| Setting | Meaning |
|---|---|
| **Subscriber name** | A stable name for this session (your topic or session name, never a PID). The overseer actions sign their audit comment with it and refuse without one; pushed events are delivered under it. Leave empty for read-only use. |
| **Fabrik directory** | The directory of the daemon to talk to. Empty means the daemon of the directory your Claude Code session is in. Set it if you run Claude Code somewhere other than the Fabrik project. One session addresses one daemon. |

### If you registered the server by hand

If you earlier ran `claude mcp add fabrik -- fabrik mcp`, remove it (`claude mcp remove fabrik`) — the plugin now provides the same server, and keeping both gives you two.

## What it provides

### MCP tools (from `fabrik mcp`, served by the running daemon)

| Tool | Purpose |
|---|---|
| `fabrik_status` | Everything the daemon knows about one issue: column, time in column, worker, PR and CI, blockers, attention state, next action |
| `fabrik_board` | The board's attention view (what needs you, ranked) or flow view, with filters |
| `fabrik_health` | Daemon version, uptime, poll and webhook freshness, worker slots, merge-train state |
| `fabrik_promote`, `fabrik_move_parked`, `fabrik_advance`, `fabrik_set_autonomy`, `fabrik_revalidate`, `fabrik_clear_claude_limit` | Audited overseer actions; each leaves a comment on the item naming your subscriber name. **No action lifts a pause or posts a free-form comment** — a paused item resumes when a human comments on it. |
| `fabrik_subscribe`, `fabrik_unsubscribe` | Choose which events are pushed into the session (see below) |

When no daemon is reachable, Claude says so and tells you how to start it; it does not fall back to scraping GitHub.

### Pushed events (opt-in)

With a subscriber name set, the daemon can push events into your session (a decision is pending, an item is wedged, a PR merged…). This uses Claude Code **channels**, a research-preview feature: it needs a claude.ai or Console login, a per-session flag and, on Team/Enterprise plans, an organization that allows channels. During the preview, start Claude Code with:

```
claude --dangerously-load-development-channels plugin:fabrik@fabrik
```

Without that, the read tools and actions still work; events are simply not delivered.

### Skills (ambient — no slash command needed)

| Skill | When it activates |
|---|---|
| `fabrik:fabrik-setup` | You want to install or bootstrap Fabrik in a project that doesn't yet have a `.fabrik/` directory |
| `fabrik:fabrik` | You are working in (or asking about) a project that uses Fabrik — `.fabrik/` exists, or the conversation references stages, the board, workers, or `fabrik:`-prefixed labels |

### Slash commands

| Command | Purpose |
|---|---|
| `/fabrik:status` | The board's attention view and daemon health, from the daemon |

## More information

- [Fabrik docs site](https://fabrik.handarbeit.io)
- [User Guide](https://fabrik.handarbeit.io/USER_GUIDE) — installation, configuration, labels
- [State Machine](https://fabrik.handarbeit.io/state-machine) — authoritative engine spec
- [Stage Lifecycle](https://fabrik.handarbeit.io/stage-lifecycle) — per-invocation lifecycle
