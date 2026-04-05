# ADR 006: Git Worktrees for Issue Isolation

## Status

Accepted

## Context

When multiple issues are in flight, Claude Code needs to work on each issue's
code changes independently. Without isolation, one issue's edits would interfere
with another's.

## Decision

Create a git worktree for each issue at `.fabrik/worktrees/issue-N/` on branch
`fabrik/issue-N`, forked from the default branch.

## Rationale

- **True isolation**: Each issue has its own working directory and branch.
  File edits, commits, and test runs don't interfere.
- **Native git**: Worktrees are a built-in git feature. No copying repos,
  no Docker containers, no VMs.
- **Efficient**: Worktrees share the git object store with the main repo.
  Creating one is fast and uses minimal disk space.
- **Branch per issue**: Each issue's changes land on `fabrik/issue-N`,
  ready to become a PR.
- **Persistence**: Worktrees survive across polls, so Claude can resume
  work with the same file state.

## Implementation

- `WorktreeManager` creates/validates/cleans up worktrees.
- Branches fork from `origin/<default-branch>` (with local fallback).
- Stale worktree directories are detected and recreated.
- `.fabrik/` is gitignored.

### Multi-repo path structure (added in issue #134)

In multi-repo mode (omitting `--repo`), Fabrik manages multiple repos from a
single job-control directory. Worktree paths are namespaced by repo name to
prevent collisions when multiple repos have issues with the same number:

```
.fabrik/worktrees/<repo-name>/issue-N/
```

For example, issues in `liminis` and `liminis-framework` use:
```
.fabrik/worktrees/liminis/issue-42/
.fabrik/worktrees/liminis-framework/issue-42/
```

One `WorktreeManager` instance is created per repo (see ADR 014). Each WM has
its own mutex, so concurrent workers in different repos never contend on
worktree creation; same-repo workers serialize as before.

**Auto-migration**: At startup, Fabrik scans `.fabrik/worktrees/` for old-style
`issue-N/` entries and migrates them to the per-repo structure using
`git worktree move` (requires git ≥ 2.17). Worktrees whose remote URL cannot
be parsed are left in place with a warning.

**Bare clones**: In job-control mode (non-git directory), each repo is
bare-cloned to `.fabrik/repos/<owner>-<repo>.git` on first access, then a
`WorktreeManager` is registered for it. See `ensureRepoReady` in
`engine/engine.go`.

## Validation

When reusing an existing worktree directory, we verify it's a valid git
worktree on the expected branch (`git rev-parse --abbrev-ref HEAD`).
If stale or invalid, it's removed and recreated.

## Trade-offs

- **Disk space**: Each worktree is a full checkout. For large repos this
  adds up, though the object store is shared.
- **Cleanup**: Worktrees accumulate. Currently no automatic cleanup when
  issues reach Done (planned feature).
- **Repo root detection**: Fabrik uses `git rev-parse --show-toplevel` to
  find the repo root, so it works from subdirectories.
