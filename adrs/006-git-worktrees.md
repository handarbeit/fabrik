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
