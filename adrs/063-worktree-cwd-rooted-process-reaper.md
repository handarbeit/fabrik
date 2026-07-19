# ADR 063: Worktree cwd-Rooted Process Reaper

**Date**: 2026-07-19
**Status**: Accepted
**Issue**: #975 — bug(cleanup): orphaned child processes survive stage teardown — PGID-scoped 'grandchild cleanup' misses setsid'd descendants (e.g. dev servers)

## Context

Fabrik's existing subprocess cleanup (`killProcGroup`, `engine/procattr_unix.go`, documented in ADR 054) sends `SIGKILL` to the Claude worker's entire process group — `kill(-workerPGID, SIGKILL)` — immediately before a worktree directory is removed. This reaches any grandchild that stayed in the worker's process group (e.g. `tail -f` from the Monitor tool).

It does not reach a descendant that calls `setsid()`. This is exactly what happens when a Claude worker backgrounds a long-lived process to verify the app under test — `npm run dev`, `vite`, `next dev` — via Claude Code's background-bash tool, which detaches the process into its own session (and therefore its own process group) so it survives across tool calls. A production run against `verveguy/concept-maps` issue #155 confirmed this exact failure mode: the worker's PGID (73452) was killed as logged, but the dev server's PGID (76028) — a different group entirely, its leader already dead, members still running — survived, reparented to `launchd` (PID 1 equivalent), `cwd` still pointing at a worktree directory removed 25 minutes earlier, port 5174 still bound.

`git worktree remove` deletes the directory; it does not touch processes that happen to have their cwd rooted there. These orphans accumulate — potentially one per issue whose worker starts a long-lived server — most visible on a developer machine running the TUI locally across many issues, manifesting as port exhaustion and unbounded resource growth on a long-running engine.

## Decision

Add `reapWorktreeProcesses(wtDir string, issueNumber int, logf func(int, string, string, ...any))`, a cwd-rooted process reaper that enumerates every live process whose current working directory is the given worktree path or a subdirectory of it, and sends `SIGKILL` to each. It runs immediately before every worktree directory removal, as a complement to (not a replacement for) the existing PGID-scoped `killProcGroup` — the PGID kill still handles the common case of grandchildren that stayed in the worker's process group; this reaper extends coverage to the `setsid`'d case by keying off cwd instead of process-group membership.

### Package-level function, not a `WorktreeManager` method

Of the four worktree-directory-removal call sites in the codebase, one — the worktree janitor's `os.RemoveAll` fallback (`engine/janitor.go`), which fires when the bare repo for a stranded worktree is missing — has no `WorktreeManager` instance available at all; that is exactly why it falls back to raw `os.RemoveAll` instead of `WorktreeManager.CleanupWorktree`. A method on `WorktreeManager` structurally cannot be called from this site. `reapWorktreeProcesses` is therefore a free function, parameterized by a `logf func(int, string, string, ...any)` callback — a signature both `WorktreeManager.logf` and `Engine.logf` already share, so both call it directly with no adapter.

### Every removal call site routes through the same primitive

Rather than reaping inline inside just `CleanupWorktree` and `CleanupTrainWorktree` (the two sites the issue's own requirements text named), all four removal call sites call `reapWorktreeProcesses` immediately before their own removal operation:

1. `WorktreeManager.CleanupWorktree` (`engine/worktree.go`) — issue worktree teardown, called from both the Done-stage teardown (`engine/item.go`) and the periodic janitor (`engine/janitor.go`).
2. `WorktreeManager.ensureTrainWorktreeFromRef`'s stale-worktree removal (`engine/worktree.go`) — crash-recovery cleanup of a leftover trial worktree before a merge-train trial branch is recreated under the same name. A crashed trial's detached server (e.g. from a build/CI command) would otherwise still be running with cwd rooted in the worktree that is about to be reused.
3. `WorktreeManager.CleanupTrainWorktree` (`engine/worktree.go`) — merge-train trial worktree teardown.
4. The worktree janitor's `os.RemoveAll` fallback (`engine/janitor.go`) — the site that motivated the package-level-function decision above.

This was a load-bearing finding from a hard-look steering pass before implementation: the issue's own requirements text enumerated only sites 1 and 3, but sites 2 and 4 are equally directory-removal paths where orphaned processes are expected. A narrower fix — reaping only inside `CleanupWorktree`/`CleanupTrainWorktree` — would have left both gaps open. Routing every site through one shared primitive, rather than hand-maintaining a longer enumeration, satisfies "every code path that removes a worktree directory" by construction: a future removal site added elsewhere in the codebase without also calling `reapWorktreeProcesses` would be an obvious, greppable omission rather than a silent gap.

### Platform implementation

Two-way build tag split (`engine/reaper_unix.go` tagged `!windows`, `engine/reaper_windows.go` tagged `windows`), matching the existing `procattr_unix.go`/`procattr_windows.go` precedent exactly rather than introducing a three-way darwin/linux/windows split with no precedent in this package. `reaper_unix.go` branches internally on `runtime.GOOS` (already used elsewhere in the engine package, e.g. `engine/upgrade.go`) between the Linux and macOS implementations:

- **Linux**: scans `/proc/*/cwd` symlinks via `os.ReadDir("/proc")` + `os.Readlink`, prefix-matching each resolved cwd against the worktree directory. No external dependency, fast. Before killing a match, it re-reads `/proc/<pid>/cwd` one more time to close the TOCTOU window in which the pid could have been recycled for an unrelated process between the initial enumeration and the kill.
- **macOS**: runs `lsof -a -d cwd -n -P -Fpcn`. This was a deliberate choice over the issue's own suggested `lsof +D <worktree>`: `+D` recursively inspects every open file descriptor of every process under the entire directory tree, which is slow and prone to hanging on a `node_modules`-heavy worktree or a stale mount — unacceptable on a synchronous teardown path that runs on every worktree removal. `-d cwd` inspects only each process's cwd file descriptor. `-F` (with `pcn`: pid, command, name/path fields) gives stable, parseable field-per-line output instead of fragile column-width parsing. macOS has no `/proc`-equivalent cheap per-pid re-check primitive, so unlike Linux, the reaper does not re-verify cwd immediately before killing on this platform — the small PID-reuse TOCTOU window here is accepted and documented rather than mitigated.
- **Windows**: no-op, matching `killProcGroup`'s existing documented Windows precedent (`procattr_windows.go`) — process-group and cwd-enumeration primitives here are Unix-specific with no direct Windows equivalent wired up.

Both platform paths resolve symlinks in the worktree directory once, via `filepath.EvalSymlinks`, before matching. This was found necessary during implementation: `/proc/*/cwd` (Linux) and `lsof`'s reported path (macOS) both report the fully-resolved real path — e.g. macOS resolves `/tmp` → `/private/tmp` and `/var` → `/private/var` — which silently defeated a literal string-prefix match against `t.TempDir()`-style paths (themselves under `/var/folders/...` on macOS, without the `/private` prefix) in initial testing.

### Non-fatal by construction

`reapWorktreeProcesses` returns nothing and never propagates an error. Enumeration failures (`lsof` not installed, a `/proc/<pid>/cwd` read racing process exit, permission errors) and kill failures other than `ESRCH` (already-exited, an expected race) are logged as warnings via the `logf` callback; the caller's subsequent `git worktree remove` / `os.RemoveAll` always proceeds regardless of the reap's outcome, per the issue's explicit requirement that this must never block or fail worktree teardown.

### Logging

Each kill reuses the existing `[#N kill] ...` log tag (`"kill"`), not a new tag, so it appears in the same grep/log-scanning workflows as `killProcGroup`'s grandchild-cleanup line: `[#N kill] sending SIGKILL to PID <pid> (<comm>) rooted in <wtDir> (worktree cwd cleanup)` — the `(worktree cwd cleanup)` parenthetical mirrors the existing `(grandchild cleanup)` style so the two mechanisms are visually distinguishable but recognizably related in the log stream.

## Rationale

### Why cwd, not a full process-tree walk from the worker's PID?

A descendant-tree walk (e.g. via `PR_SET_CHILD_SUBREAPER` on Linux, reparenting orphans to the engine instead of `PID 1`) would also catch fd-only holders and processes that `chdir` away after starting — a strictly larger class of leak. It was considered and explicitly deferred: it is Linux-only, the reported evidence for this issue is macOS, and cwd-based detection is portable and directly matches the reported failure mode (a process whose cwd is the deleted worktree). Noted here as possible future hardening, not undertaken in this issue.

### Why not extend `killProcGroup` itself?

`killProcGroup` operates on a single already-known PGID (the worker's own). Reaching a `setsid`'d descendant requires system-wide process enumeration keyed on a different property (cwd) entirely — a fundamentally different operation, not a variant of a PGID kill. Keeping them as two separate, composable mechanisms (one PGID-scoped and immediate, one cwd-scoped and worktree-removal-scoped) keeps each simple and independently testable, and preserves `killProcGroup`'s existing, unrelated call sites (kill escalation during `max_wall_time`/inactivity timeouts) untouched.

## Consequences

**Positive:**
- Detached dev servers and similar `setsid`'d descendants no longer survive worktree teardown and outlive their deleted worktree directory — the specific, evidenced leak class this issue reports is closed for all newly-removed worktrees.
- All four worktree-directory-removal call sites are covered by construction (one shared primitive, not a hand-maintained enumeration), so a future removal site is unlikely to silently reintroduce the gap.
- Fully non-fatal: a missing `lsof`, a permission error, or a `/proc` race degrades gracefully to "no reap, just a warning" rather than blocking or failing worktree cleanup.

**Negative / Trade-offs:**
- **PID-reuse TOCTOU window on macOS**: accepted, not mitigated (no cheap re-check primitive without `/proc`). In practice the window is the time between one `lsof` invocation's output line and the immediately following `kill` call — very small, but not zero.
- **`chdir`'d descendants are not caught**: a dev-server framework that opens resources in the worktree and then `chdir`s elsewhere still leaks under this design. Explicitly out of scope per the issue; the portable, evidence-matching cwd approach was chosen deliberately over a broader (and Linux-only) descendant-tree walk.
- **Not a retroactive sweep**: this only prevents *new* leaks going forward. Processes already orphaned by worktrees removed before this fix shipped are not reaped by it; operators must clean those up manually. Explicitly scoped out by the issue.
- **Worker-side hygiene is a separate, complementary fix**: #976 tracks having workers avoid leaving detached servers running in the first place. This engine-side reaper is the more robust fix because it catches leaks regardless of worker behavior, but the two are independent and both worth having.

## Related Work

- [ADR 054: SIGINT Kill Escalation and Reason Propagation](054-sigint-kill-escalation-and-reason-propagation.md) — the PGID-scoped mechanism this reaper complements; see `docs/stage-lifecycle.md` § Subprocess Cleanup.
- [ADR 055: Periodic Worktree Janitor](055-worktree-janitor.md) — reaps orphaned worktree *directories*; this reaper is its immediate-teardown counterpart for orphaned *processes*, and automatically benefits the janitor's own `CleanupWorktree`/`os.RemoveAll` paths.
- Companion issue **#976** — worker-side hygiene (verify steps shouldn't leave detached servers running), filed as defense-in-depth alongside this engine-side fix.

**References:** [docs/stage-lifecycle.md § Worktree Teardown Process Reaping](../docs/stage-lifecycle.md), [docs/state-machine.md § 11.3 Reaping Gate](../docs/state-machine.md)
