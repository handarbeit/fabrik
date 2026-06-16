# ADR 055: Periodic Worktree Janitor

## Status

Accepted

## Context

ADR 006 noted that worktrees accumulate when issues exit the pipeline without
the Done stage running. Common stranding paths include:

- Issues manually archived or deleted from the project board.
- Issues moved off-board via the auto-archive pass.
- A restart window between Validate completing and Done being dispatched.
- Stage YAML changes that rename or remove the Done stage after existing issues
  already passed through it.

The poll loop only dispatches work for items currently on the board. Stranded
worktrees are never visited again and accumulate indefinitely.

## Decision

Introduce a periodic **worktree janitor** (`engine/janitor.go`) that:

1. Runs **once after startup** (after the first successful poll so board state
   is hydrated in the in-memory store).
2. Runs **hourly** thereafter on a `time.Ticker` goroutine, same pattern as
   `startWorkerDetector`.
3. Scans every `<fabrikDir>/.fabrik/worktrees/<owner>-<repo>/issue-N/`
   subdirectory and reaps each worktree that satisfies all four conditions of
   the reaping gate (see below).

## Reaping Gate

A worktree is **only reaped** when all four conditions hold simultaneously:

| # | Condition | Source |
|---|-----------|--------|
| 1 | Issue is **closed** | `itemstate.Store` (store hit), or `GitHubClient.FetchIssue` REST call (off-board fallback) |
| 2 | Issue is **off-board or at a cleanup-complete stage** | `itemstate.Store` snapshot: `snap.Status` empty (not on board) OR the active stage has `CleanupWorktree: true` |
| 3 | Worktree is **clean** | `isWorkingTreeDirty()` → `git status --porcelain` |
| 4 | **No in-flight worker** | `itemstate.Snapshot.Worker()` nil |

Conditions are checked in order (1→4) and the first failing condition skips
the worktree. The gate is deliberately conservative: any ambiguity (e.g. REST
error, unreadable git state) causes the janitor to leave the worktree alone
and log a warning.

## Rationale

### Conservative gate — why all four conditions?

- **Closed check**: avoids reaping worktrees for issues that are still open
  (e.g. temporarily moved off-board for triage).
- **Off-board or cleanup-complete**: avoids reaping when an issue is on an
  active stage (engine may dispatch work imminently).
- **Clean worktree**: preserves uncommitted work from a Claude session that
  ended before committing. This is the most important safety condition — dirty
  worktrees always indicate in-progress or abandoned work worth preserving.
- **No in-flight worker**: races between the janitor scan and the engine
  dispatcher are possible; the store's worker marker is the single source of
  truth for in-flight state.

### Fallback hierarchy for unregistered repos

When a worktree directory exists for a `<owner>-<repo>` pair that has no
registered `WorktreeManager` (e.g. the bare repo was never set up, or the
config drifted), the janitor:

1. Reads `git remote get-url origin` from the first issue subdirectory to
   recover the canonical `owner/repo` string.
2. Constructs a temporary `WorktreeManager` using `NewWorktreeManagerForRepo`
   pointing at `<fabrikDir>/.fabrik/repos/<owner>-<repo>.git`.
3. If the bare repo directory does not exist, falls back to `os.RemoveAll`
   with a warning log (the worktree is already disconnected from git).

This mirrors the approach used in `migrateWorktrees`.

### Closed-state lookup strategy

- **Primary**: check `itemstate.Store` — the in-memory board cache populated
  by each poll cycle. Zero latency, no API calls.
- **Fallback**: if the issue is absent from the store (off-board), call
  `GitHubClient.FetchIssue` (REST `GET /repos/{owner}/{repo}/issues/{number}`).
  Results are cached in a per-scan `closedCache` map to avoid redundant calls
  for multi-worktree scenarios.

## Configuration

Cadence is controlled by `JanitorIntervalHours` (engine `Config` field):

| Source | Precedence |
|--------|-----------|
| `--janitor-interval` CLI flag | Highest |
| `FABRIK_JANITOR_INTERVAL` env var | Second |
| `janitor_interval_hours` in `config.yaml` | Third |
| Default (`1` hour) | Lowest |

Setting the value to `0` disables the janitor entirely (no startup scan, no
ticker goroutine). Changing the value at runtime has no effect; restart Fabrik
to apply a new cadence (EC-6 caveat).

## Alternatives Considered

### Reap on every poll

Rejected: makes the poll loop more complex and adds per-poll git calls for
every worktree, even when they are being actively used. A separate goroutine
keeps concerns separated and allows independent scheduling.

### Reap immediately when issue leaves board

Rejected: the board cache may not have flushed yet; races with in-flight workers
are hard to gate on. The conservative four-condition gate run on a goroutine
with store reads is safer.

### No janitor — require manual cleanup

Rejected: stranded worktrees accumulate silently and disk usage grows without
bound. Operator burden for long-running Fabrik deployments is unacceptable.

## Trade-offs

- **REST call per off-board issue**: FetchIssue adds one API call per stranded
  worktree that isn't in the store. In practice, this is rare and the per-scan
  cache prevents duplicates.
- **Startup delay**: the post-first-poll janitor run adds latency before the
  engine enters steady-state. For large deployments with many stranded worktrees
  this could take seconds, but it runs concurrently with poll processing.
- **EC-6 restart caveat**: cadence changes require a restart. Operators must be
  informed (USER_GUIDE.md note added).
