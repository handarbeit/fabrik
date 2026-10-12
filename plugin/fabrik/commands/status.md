---
description: Summarise what Fabrik is doing right now — what needs you, what is in flight, and daemon health — from the running daemon
---

Report the current state of this project's Fabrik pipeline. Everything comes from the running daemon's tools; do not read it from GitHub, process lists or worktrees.

## 1. Ask the daemon

Call these two tools (in parallel):

- `fabrik_board` with `view=attention` — the items that need looking at, ranked, each with its attention state, a one-line reason and the next action.
- `fabrik_health` — daemon version and uptime, last poll, webhook and reconcile freshness, worker slots, merge-train partitions.

If the user named a column, label, repo or milestone, pass it as a `fabrik_board` filter. If the user asked about a flow or throughput picture, call `fabrik_board` with `view=flow` as well.

## 2. If the daemon is not running

A tool result saying the Fabrik daemon is not running at some socket path means exactly that. Say so plainly, and tell the user to start it by running `fabrik` in the Fabrik project directory (the one with `.fabrik/`). If this session is not in that directory, point out the plugin's "Fabrik directory" setting. Stop there: do not fall back to scraping GitHub, `gh` or `ps` for board or worker state, because the answer would not match what the engine believes.

## 3. Summarise

Lead with what needs the user, then the rest. Keep it short enough to read at a glance.

1. **Needs you** — items in `needs-human`, `escalated` and `suspended` (a stage finished and awaits their decision). For each: `#N title`, the reason, and what they can do (use the tool's `next_action` and `options`; don't invent steps).
2. **Stalled** — items in `stalled`, with how long without progress.
3. **In flight** — a count by column of `working` and `waiting` items, naming what the waiting ones wait for when it is a single common cause (CI, reviewers, a free worker slot, the next poll).
4. **Daemon health** — one line: version, uptime, time since the last successful poll. Call out anything stale or overdue that `fabrik_health` flags (poll overdue, webhooks quiet, a merge-train partition idle with queued items).

If nothing needs the user, say so in one line before the rest.

For a closer look at one item, use `fabrik_status` with its issue number. Offer the audited overseer actions (`fabrik_promote`, `fabrik_advance`, `fabrik_set_autonomy`, `fabrik_revalidate`, `fabrik_clear_claude_limit`) where one fits, but never take one without the user asking. No action resumes a paused item; that takes a human comment on the item.
