---
description: Summarise what Fabrik is doing right now — what needs you, what is in flight, and daemon health — from the running daemon
---

Report the current state of this project's Fabrik pipeline. Everything comes from the running daemon's tools; do not read it from GitHub, process lists or worktrees.

## 1. Ask the daemon

Call `fabrik_situation`. It is the daemon's rolled-up answer to what needs the user: PRs waiting for a merge decision, items held at a gate, items waiting for input (with how to resume them), pending decisions, stalled items, merge-train and poll health, and active anomalies. Each entry carries its options to act.

Add `fabrik_health` (in parallel) for the daemon line: version, uptime, last poll, webhook and reconcile freshness, worker slots.

If the user named a column, label, repo or milestone, call `fabrik_board` with `view=attention` and that filter as well. If the user asked about a flow or throughput picture, call `fabrik_board` with `view=flow`.

## 2. If the daemon is not running

A tool result saying the Fabrik daemon is not running at some socket path means exactly that. Say so plainly, and tell the user to start it by running `fabrik` in the Fabrik project directory (the one with `.fabrik/`). If this session is not in that directory, point out the plugin's "Fabrik directory" setting. If the user says a daemon is running, it is probably older than 0.0.84 (no local API; it needs upgrading), or it could not open its socket (the warnings panel has a "Local API unavailable" entry, and `.fabrik/fabrik.log` has a `local API unavailable` line). If the `fabrik_*` tools are not available at all, `fabrik` is missing from `PATH` or older than this plugin. Stop there: do not fall back to scraping GitHub, `gh` or `ps` for board or worker state, because the answer would not match what the engine believes.

## 3. Summarise

Lead with what needs the user, then the rest. Keep it short enough to read at a glance.

1. **Needs you** — merge decisions, items waiting for input and pending decisions from `fabrik_situation`. For each: `#N title`, the reason, and what they can do (use the entry's options; don't invent steps).
2. **Held and stalled** — items held at a gate (which gate, how long) and stalled items.
3. **Anomalies** — any active anomaly the situation reports.
4. **Daemon health** — one line: version, uptime, time since the last successful poll. Call out anything stale or overdue (poll overdue, webhooks quiet, a merge-train partition idle with queued items).

If `nothing_needs_you` is true, say so in one line before the rest. A section's `omitted` count means entries were cut; say how many. Report "unknown" values as unknown.

For a closer look at one item, use `fabrik_status` with its issue number. Offer the audited overseer actions (`fabrik_promote`, `fabrik_move_parked`, `fabrik_advance`, `fabrik_set_blocked_by`, `fabrik_set_autonomy`, `fabrik_revalidate`, `fabrik_clear_claude_limit`) where one fits, but never take one without the user asking. No action resumes a paused item; that takes a human comment on the item after the pause, or the user removing `fabrik:paused` (and `fabrik:awaiting-input`).
