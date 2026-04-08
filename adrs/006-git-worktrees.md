# ADR 006: Git Worktrees for Issue Isolation

## Status

Accepted

## Context

When multiple issues are in flight, Claude Code needs to work on each issue's
code changes independently. Without isolation, one issue's edits would interfere
with another's.

## Decision

Always bare-clone each managed repository to `.fabrik/repos/<owner>-<repo>.git`
on first access, then create git worktrees from the bare clone. Worktrees are
created at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch
`fabrik/issue-N`, forked from the default branch.

`fabrikDir` (where `.fabrik/` config, stages, and plugin live) is always
`os.Getwd()` — the directory where the user runs `fabrik`.

## Rationale

- **True isolation**: Each issue has its own working directory and branch.
  File edits, commits, and test runs don't interfere.
- **Native git**: Worktrees are a built-in git feature. No copying repos,
  no Docker containers, no VMs.
- **Efficient**: Worktrees share the git object store with the bare clone.
  Creating one is fast and uses minimal disk space.
- **Branch per issue**: Each issue's changes land on `fabrik/issue-N`,
  ready to become a PR.
- **Persistence**: Worktrees survive across polls, so Claude can resume
  work with the same file state.
- **Single code path**: Removing dual-mode (single-repo git mode vs.
  job-control mode) eliminates a source of bugs and simplifies the engine.
  Fabrik no longer needs to detect whether it's running inside a git
  checkout of a managed repo.

## Implementation

- `WorktreeManager` creates/validates/cleans up worktrees.
- Branches fork from `origin/<default-branch>` (with local fallback).
- Stale worktree directories are detected and recreated.
- `.fabrik/` is gitignored in managed repos (existing `.git/info/exclude`
  entries are harmless; new setups don't need them).

### Path structure

Worktree paths are namespaced by repo to prevent collisions when multiple
repos have issues with the same number:

```
.fabrik/worktrees/<owner>-<repo>/issue-N/
```

For example, issues in `liminis` and `liminis-framework` use:
```
.fabrik/worktrees/org-liminis/issue-42/
.fabrik/worktrees/org-liminis-framework/issue-42/
```

One `WorktreeManager` instance is created per repo (see ADR 014). Each WM has
its own mutex, so concurrent workers in different repos never contend on
worktree creation; same-repo workers serialize as before.

### Bare clone lifecycle

On first access for a repo, `ensureRepoReady` in `engine/engine.go`:

1. Bare-clones the repo to `.fabrik/repos/<owner>-<repo>.git`.
2. Creates and registers a `WorktreeManager` for subsequent worktree operations.

Subsequent calls to `ensureRepoReady` for the same repo are no-ops (the WM
is already registered). `ensureBareClone` itself is idempotent — it skips the
clone if the directory already exists.

**Auto-migration**: At startup, Fabrik scans `.fabrik/worktrees/` for old-style
`issue-N/` entries and migrates them to the per-repo structure using
`git worktree move` (requires git ≥ 2.17). Worktrees whose remote URL cannot
be parsed are left in place with a warning.

## Validation

When reusing an existing worktree directory, we verify it's a valid git
worktree on the expected branch (`git rev-parse --abbrev-ref HEAD`).
If stale or invalid, it's removed and recreated.

## Trade-offs

- **First-run latency**: Existing single-repo users trigger a bare-clone on
  first run after upgrade. For small/medium repos this takes a few seconds.
  This is a one-time cost.
- **Disk space**: Each worktree is a full checkout. For large repos this
  adds up, though the object store is shared with the bare clone.
- **Cleanup**: Worktrees accumulate. Currently no automatic cleanup when
  issues reach Done (planned feature).
