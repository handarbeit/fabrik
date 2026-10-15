# ADR 2207: `fabrik mcp` walks up to the project's `.fabrik/` and streams only inside a Fabrik project

## Status

Accepted. Amends ADR 2141 decision 2 (directory resolution) and the expectation that a session in a non-Fabrik project shows no misleading output.

## Context

The `fabrik` PM plugin starts `fabrik mcp` in every Claude Code session where it is installed. `resolveMCPDir` used `--dir`, `FABRIK_MCP_DIR`, `CLAUDE_PROJECT_DIR` or the working directory as given. Two consequences:

1. A session started in `repo/sub/` of a Fabrik project addressed `repo/sub/`, found no socket and reported "no daemon" although the project's daemon was running.
2. With `subscriber` set at user scope, a session in an unrelated project still started the channel stream shim, which retried forever and, once the channel was opted in, emitted a `daemon-unreachable` notice after the 10 s grace — about a daemon that cannot exist there.

Review of #2206 also found that the placeholder check in `settingValue` could not be neutralised without a compile error (its `strings` import had no other use), and that the hook's `command -v fabrik` guard was invisible to every stdout/exit-code test.

## Decision

1. **Walk up.** When the directory comes from `CLAUDE_PROJECT_DIR` or the working directory, `findFabrikRoot` returns the nearest ancestor (including the start) that contains a directory named `.fabrik`. A plain file of that name does not count. `--dir` and `FABRIK_MCP_DIR` stay literal. With no match, the original directory is kept, so the existing "Fabrik daemon not running at <path>" text is unchanged.
2. **Boundary.** If the start is inside `$HOME`, `$HOME` itself is checked and nothing above it is. Otherwise the walk runs to the filesystem root. If the home directory cannot be determined, only the root bounds the walk. The walk is `stat` calls only.
3. **Stream gate.** `Server.DisableStream` (zero value = today's behaviour) stops `startStream`. `newMCPServer` sets it when the final resolved directory — whichever source supplied it — has no `.fabrik/`. `Subscriber` is deliberately not blanked: it remains the audit name for the action tools and the default for `fabrik_subscribe`. Tools outside a project still dial the absent socket and fail at once with "daemon not running".
4. **Testability.** `isUnexpandedPlaceholder` is extracted from `settingValue` so that neutralising it compiles and the placeholder tests are shown to fail. The redundant `command -v fabrik` guard is deleted from the SessionStart hook: a missing binary yields an empty version, which the empty-version check already turns into silence, and removing the guard alone changes no observable output. The hook tests now guard the empty-version check (removing it fails both silent subtests).

## Consequences

- Subdirectory sessions reach the project's daemon.
- Known limit: a repository that tracks `.fabrik/` in git (Fabrik's own does) has one in every checkout and worktree, so a session inside such a worktree resolves to that checkout, not to the directory the daemon runs in. That is the same "no daemon" answer as before, not a regression; `--dir` or the plugin's "Fabrik directory" setting addresses the daemon explicitly. A stronger marker (`.fabrik/state`, `config.yaml`) was rejected as diverging from the specified rule.
- A `~/.fabrik` directory is treated as a project; the message then names that path.
- Fleet discovery across instances or machines remains out of scope.
