# ADR 1136: Age-Based Session-File Pruning

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1136 — Session files are never pruned: 83% of stored session IDs point at reaped conversations

## Context

`.fabrik/sessions/` stores one `.session` file per (issue, stage) pair — a pointer to the Claude Code
conversation Fabrik resumes from on the next invocation (`engine/claude.go`'s `sessionFile`/
`saveSessionIDDirect`). Nothing in Fabrik ever deletes one. Claude Code itself reaps conversation
transcripts after `cleanupPeriodDays` (default 30, user-configurable), so the pointer's lifetime is
unbounded while the thing it points at has a 30-day expiry. Measured on a working install on
2026-07-26: 1,998 `.session` files on disk, only 338 pointing at a conversation that still exists —
83% pointing at nothing, spread across 293 issues, most long closed. Every other category of Fabrik
runtime state has a bound: stage logs are pruned by `LogRetentionDays` (`runLogJanitor`/`pruneLogs`,
`engine/janitor.go`), and worktrees are swept hourly by the worktree janitor. Sessions had neither.

This is the substrate that made #1128 possible: #1128 fixes the *symptom* (detect a dead session at
invocation time, clear the entry, start fresh); this issue removes the substrate so that recovery path
stays a rare event instead of the 83%-of-the-time steady state.

Two decisions were explicitly left to Research rather than picked silently by the original issue:
which pruning mechanism to use, and whether a new config key is needed.

## Decision

### Mechanism: age-based only, not liveness-checking

Prune `.session` files whose mtime exceeds a configurable retention window (`session_retention_days`,
new config key, default **14 days**, `0` disables), mirroring `pruneLogs`'s existing shape exactly:
scope-guarded directory walk, `now.AddDate(0, 0, -retentionDays)` cutoff, `now` injected for
deterministic tests, followed by best-effort empty-directory cleanup.

The alternative considered — **liveness-checking**, listing
`~/.claude/projects/<sanitized-cwd>/*.jsonl` and intersecting against stored session IDs — was
rejected. It would be Fabrik's first code path that reads Claude Code's internal on-disk transcript
layout. Nothing else in the codebase depends on that path today, not even #1128's own dead-session
recovery, which detects staleness structurally from the Claude CLI's own error response
(`"No conversation found with session ID"`) rather than by inspecting Claude Code's on-disk state.
Coupling to an internal, unversioned storage convention means an unannounced Claude Code layout change
would silently stop all session pruning fleet-wide, with no test able to catch it before it ships. A
hybrid (age-based plus liveness-checking as a second, tighter mechanism) was also rejected: liveness
checking only earns its complexity if it lets the retention window be safely *shorter* than a pure
age-based cutoff, and the coupling risk above outweighs that marginal benefit — age-based alone already
satisfies every line of the issue's Definition of Done.

A `.session` file's mtime already means the right thing with zero new bookkeeping:
`saveSessionIDDirect` rewrites the file on every stage invocation that produces a session ID, so mtime
is exactly "last time this (issue, stage) pair was invoked" — the correct staleness proxy.

**Default of 14 days** matches `LogRetentionDays`'s own precedent (no comparable upstream constraint
either) and sits comfortably under Claude Code's 30-day `cleanupPeriodDays` default, so Fabrik's own
prune generally lands ahead of — or not far behind — Claude's reap, keeping the rotten-pointer
population near zero in steady state rather than accumulating toward the 83% this issue measured.

### Scheduling: new pass on the existing janitor, no new scheduler

`runSessionJanitor(ctx)` is a third periodic pass in `engine/janitor.go`, invoked from the same two
`engine/poll.go` call sites as `runWorktreeJanitor`/`runLogJanitor` (the post-first-poll startup pass
and the hourly ticker goroutine), gated by the same `JanitorIntervalHours > 0` condition. No new
goroutine, ticker, or config surface for cadence — `session_retention_days` only controls the age
cutoff, exactly as `log_retention_days` does for the log janitor.

### In-flight guard: directory-wide, not stage-specific

The spec requires never deleting a session file for a stage currently in flight. The existing
worktree-janitor check — `snap.Worker() != nil && !isWorkerStale(snap.Worker(), e.workerStaleTimeout())`
(`engine/janitor.go`) — is reused, but widened: once an issue has *any* live worker registered, **every**
`.session` file under that issue's session directory is skipped for the cycle, not only the file
matching the worker's current `StageName`. This is a deliberate conservative widening: it costs one
`store.Get(ownerRepo, issueNumber)` call per issue directory — already the same cost the worktree
janitor pays per issue — and it avoids a race where the active stage transitions mid-cycle, between
the age check and the delete, in a way that a stage-specific check could miss.

### Unresolved multi-repo directories: skip entirely, never delete

Sessions dirs use the same `<owner>-<repo>` naming convention as worktree dirs
(`sessionDirForItem`/`strings.ReplaceAll(issue.Repo, "/", "-")`), so a multi-repo session directory
name is resolved back to `"owner/repo"` via the same reverse map the worktree janitor builds from
`e.worktreeManagers` (factored out as the shared `buildWMByDirName()` helper so both janitors stay in
sync). Unlike the worktree janitor, there is **no git-remote fallback** available for an unresolvable
directory (`janitorResolveOwnerRepo`'s `git remote get-url origin` requires an actual git checkout to
run inside, and a sessions-only directory has none). When a directory's name isn't found in the
reverse map, the entire directory is skipped for the cycle — counted, never deleted from — consistent
with this codebase's established "ambiguity → don't touch, log and move on" convention (the same
philosophy behind the worktree janitor's own conservative gate).

### Logging: one summary line per cycle

`session-janitor` tag, same shape as `runLogJanitor`'s summary line:

```
cycle complete: scanned N session files, removed M, skipped K (in-flight=I, unresolved-repo=U)
```

Never one line per file — a first run against an install like the one measured in the issue
(1,660 dead pointers) would otherwise flood the log.

## Rationale

### Why no REST calls or closed-state gate, unlike the worktree janitor?

The worktree janitor's four-condition gate exists because a worktree holds uncommitted work that must
never be destroyed while an issue could still be worked — that requires knowing whether the issue is
closed, which requires a REST fallback (plus a rate-limit pre-check) for off-board issues. A session
file carries no work-in-progress state; it is a resume pointer, and #1128 already makes losing one
gracefully recoverable — the next invocation just starts a fresh session. The spec's Definition of
Done reflects this asymmetry: it gates only on in-flight status and age, never on issue-closed state.
The session janitor is therefore pure store-lookup plus filesystem walk — no `FetchIssue` fallback, no
rate-limit gate, no closed-state cache — simpler than the worktree janitor and closer in shape to the
log janitor.

### Why factor out `buildWMByDirName()`?

Both the worktree and session janitors need the identical `e.worktreeManagers` → `"owner-repo"`
reverse map. Duplicating the ~8-line construction risks silent drift if the mapping logic ever
changes (e.g. how `owner-repo` names are derived); a single shared helper makes that structurally
impossible.

### Why is empty-directory cleanup included, given it was explicitly optional in scope?

It was called out as a nice-to-have, not a requirement, specifically to avoid blocking on it. In
practice it is a small bottom-up `os.Remove` pass over directories already collected during the main
walk (mirroring `pruneLogs`'s own Phase 3), so it was cheap enough to include rather than leave as a
coin flip.

## Consequences

**Positive:**
- The rotten-pointer population (measured at 83% of 1,998 files) is bounded by `session_retention_days`
  instead of growing forever; #1128's dead-session recovery path becomes a rare event rather than the
  steady state.
- No new coupling to Claude Code's internal on-disk layout — the janitor's correctness does not depend
  on an unversioned convention Fabrik doesn't own.
- Both session directory layouts (single-repo `issue-N/`, multi-repo `<owner>-<repo>/issue-N/`) are
  scanned unconditionally, so a project that switches between single- and multi-repo mode doesn't leave
  the old layout's directories to accumulate with nothing to clean them up.

**Negative / Trade-offs:**
- Age is a *proxy* for liveness, not exact. An issue that sits `fabrik:paused` (e.g. awaiting human
  input) for longer than `session_retention_days` has no in-flight worker, so it does not qualify for
  the in-flight guard — its session file can be pruned mid-pause even though the underlying Claude Code
  conversation may still be resumable for up to 30 days. This is spec-compliant (only "in flight" is
  protected, not "paused-but-will-resume") and the consequence is bounded — the next invocation just
  starts a fresh session, the same fallback used whenever a session pointer goes dead for any other
  reason — but it is a real, if minor, behavior change, documented in `docs/USER_GUIDE.md`'s Session
  Janitor section.
- An unresolvable multi-repo session directory (a permanently deregistered repo whose
  `WorktreeManager` is never re-created) is skipped every cycle and could accumulate indefinitely.
  This mirrors an existing, already-accepted risk in the worktree janitor's own unregistered-repo
  fallback chain — not a new class of problem introduced here.

**References:** [ADR-055: Worktree Janitor](055-worktree-janitor.md)
