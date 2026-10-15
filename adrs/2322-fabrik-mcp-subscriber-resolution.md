# ADR 2322: `fabrik mcp` subscriber from the environment, then `CLAUDE_TOPIC`, then a fixed default

Status: Accepted. Amends ADR 2141 (decisions 2 and 3) and supersedes ADR 1966-b's "no generated default" rule and ADR 1966-c's "every action requires `--subscriber`".

## Context

`fabrik mcp` needs a subscriber name for two things: the daemon-side identity of the session's channel stream (held queue and subscriptions, which must survive restarts, #1968) and the audit name the overseer actions write into their GitHub comments. Until now it came only from `--subscriber` or `FABRIK_SUBSCRIBER`, with no default, and the plugin filled `FABRIK_SUBSCRIBER` from one plugin-wide `subscriber` setting.

1. A user who installed the plugin and filled in nothing got no pushes and no actions.
2. One plugin setting cannot tell sessions apart, and the `env` entry in `.mcp.json` overrides what a session inherited. A per-session `FABRIK_SUBSCRIBER`, or the `CLAUDE_TOPIC` that `topic` (verveguy/claude-topics#7) exports on each session it launches, was silently replaced.

## Decision

1. **The plugin sets no subscriber.** `FABRIK_SUBSCRIBER` is removed from `plugin/fabrik/.mcp.json` and `userConfig.subscriber` from `plugin.json`. The name reaches `fabrik mcp` only when it is really in the session's environment. `fabrik_dir` / `FABRIK_MCP_DIR` and `CLAUDE_PROJECT_DIR` are unchanged. A test pins the absence of both.
2. **Resolution order** (`cmd/mcp.go` `resolveSubscriber`): `--subscriber`, `FABRIK_SUBSCRIBER`, `CLAUDE_TOPIC`, then the OS username (`$USER`, else `os/user`). An unexpanded `${...}` placeholder or a blank value counts as unset at every step; the value is trimmed. The server carries the winning source (`mcpstdio.NameSource`).
3. **A fixed default replaces the "no generated default" rule.** That rule existed to forbid *unstable* names (PIDs, session ids), which would orphan held queues. The OS username is stable. Its cost, two default-named sessions sharing one name, is handled by decisions 4 and 5.
4. **The default never starts the push stream (part 4 of the request, FR-007/FR-008).** Any `claude` process that loads the plugin, such as a `claude -p` script, would otherwise attach under the shared default and supersede the interactive session. Investigation of what `fabrik mcp` can observe:
   - `initialize` carries `protocolVersion`, `clientInfo` and `capabilities`. Claude Code's documentation (channels reference, plugins reference, MCP) names no field in them that separates interactive from print mode, or channel-enabled from not. A real payload was **not captured** here (no Claude Code on the implementing host); it is on the live-check list below.
   - Channel enablement is one-way: the server declares `experimental["claude/channel"]`, and the client sends nothing back to say it registered the listener. Per the docs, a session that did not load the server as a channel drops pushes silently, and `--dangerously-load-development-channels` is ignored under `-p`, so a print-mode session cannot receive pushes. The shim cannot see that.
   - Plugin servers do start under `claude -p`. No documented environment variable marks print mode, and parent-process inspection (tty, argv) is non-portable and fragile.

   Conclusion: no reliable signal. The fallback in the request applies: a default-sourced name is the action tools' audit name and `fabrik_subscribe`'s default name, and never starts the stream (`Server.StreamName()` is empty for `SourceDefault`). Names from `--subscriber`, `FABRIK_SUBSCRIBER` and `CLAUDE_TOPIC` are deliberate or tool-assigned and start the stream as before. The #2207 rule (no stream without a `.fabrik/` directory) is unchanged. The server deliberately reads nothing from `clientInfo` / `capabilities`, and tests assert three handshake shapes behave identically. If the live capture finds a reliable signal, the default may stream in sessions that pass it.
5. **A collision explains itself.** When a session whose name came from the default is superseded, its single `stream-superseded` notice also says to set `FABRIK_SUBSCRIBER` per session or run sessions under `topic`. Notices for explicitly named sessions are unchanged. Because of decision 4 a default-named session does not stream today, so this branch is reached only if a later signal lets the default stream; it is unit-tested directly.
6. **`fabrik_subscribe` is honest.** With a default-sourced (or no) name its result says the subscription is held by the daemon but this server holds no push connection, and how to get one.

## Residual hazards (accepted, documented)

- A `claude -p` child started from a session whose shell exports `CLAUDE_TOPIC` or `FABRIK_SUBSCRIBER` inherits it, so it starts a stream under the same name and takes it from the parent. Remedy: unset the variable or set a different `FABRIK_SUBSCRIBER` in the child. Fabrik's own workers are unaffected: they run with the PM plugin disabled (ADR 2140).
- For a user relying on the default, pushes are not delivered out of the box; only the actions work. They export `FABRIK_SUBSCRIBER` or run under `topic`.
- The OS username becomes visible in the audit comments on public threads (after `sanitizeRequester` reshaping). Users for whom that matters set `FABRIK_SUBSCRIBER`.
- A user who set the plugin's old `subscriber` setting silently falls to the default and loses pushes. Mitigation: the release note below, and the `fabrik_subscribe` note.

## Release note (for `scripts/cut-release.sh`)

> The `fabrik` plugin no longer has a "Subscriber name" setting. `fabrik mcp` now takes the name from `--subscriber`, then `FABRIK_SUBSCRIBER`, then `CLAUDE_TOPIC`, then your OS username, so the overseer actions work with no configuration. The OS-username default does not start the push stream; if you set the old setting, export `FABRIK_SUBSCRIBER` instead to keep receiving pushed events.

## Live checks CI cannot prove — NOT yet verified

Added to ADR 2141's list (and repeated there):

- [ ] A session's own environment reaches a **plugin's** stdio MCP server once `.mcp.json` no longer sets `FABRIK_SUBSCRIBER`, so an exported `FABRIK_SUBSCRIBER` / `CLAUDE_TOPIC` arrives. If it does not, the fallback is an explicit `"FABRIK_SUBSCRIBER": "${FABRIK_SUBSCRIBER}"` / `"CLAUDE_TOPIC": "${CLAUDE_TOPIC}"` passthrough (an empty value counts as unset, so a per-session value still wins), and the plugin test's absence assertion changes with it.
- [ ] Removing a `userConfig` key from an installed plugin does not break the upgrade, and a stored old `subscriber` value does not still reach the server.
- [ ] Capture the real `initialize` payload (`clientInfo`, `capabilities`) from an interactive session, `claude -p` and the Agent SDK. If they differ reliably, decision 4 can let the default stream in interactive sessions only.

## Consequences

- Fresh installs get working overseer actions without configuration.
- Per-session names (`topic`, direnv, shell) work with the plugin and are never overridden by it.
- The plugin's `subscriber` setting is a breaking removal for people who set it.
