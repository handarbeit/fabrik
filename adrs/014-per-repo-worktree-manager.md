# ADR 014: Per-Repo WorktreeManager with Always-Bare-Clone Strategy

## Status

Accepted (updated in issue #249)

## Context

Issue #134 extended Fabrik to support multi-repo GitHub Projects — a single
project board that tracks issues across multiple repos. To do this, Fabrik
needs to manage git worktrees for issues in repos it may not have checked out
locally.

The original design used a single `*WorktreeManager` (`e.worktrees`) shared
across all issues, pointing at one repo directory. For multi-repo, we need:

1. Separate worktree paths per repo (to prevent `issue-N` collisions)
2. Independent locking per repo (concurrent issues in different repos shouldn't
   serialize on a single mutex)
3. Lazy repo acquisition — we don't know which repos appear on the board until
   we fetch it

Issue #249 removed the dual-mode design (single-repo git mode vs. job-control
mode) and made Fabrik always use the bare-clone strategy. This simplifies the
engine by eliminating branching code and the bugs that came with it.

## Decision

Replace the single `e.worktrees *WorktreeManager` with a map:

```go
worktreeManagers map[string]*WorktreeManager  // key: "owner/repo"
```

Each entry is created lazily at first issue access via `ensureRepoReady`, which
always bare-clones the repo on first access. `fabrikDir` (where `.fabrik/` lives)
is always `os.Getwd()` — the directory where the user runs `fabrik`.

Engine code accesses WMs through `worktreesFor(nameWithOwner)`, which panics if
called before `ensureRepoReady` — the invariant is: call `ensureRepoReady` at
the top of `processItem`, then all downstream code can assume the WM exists.

## Rationale

- **Independent mutexes**: Each `WorktreeManager` has its own `sync.Mutex`
  protecting its worktree operations. Workers on different repos never contend,
  while workers on the same repo still serialize correctly.
- **Lazy acquisition**: The project board is the source of truth for which repos
  exist. We can't know at startup which repos will appear, especially as new
  issues are added to the project over time.
- **Uniform API**: All engine code uses the same `worktreesFor` accessor
  regardless of how many repos are in use. No `if single-repo` branches in the
  business logic.
- **Failure isolation**: If a repo can't be cloned (permission error, network
  issue), `ensureRepoReady` posts a comment, adds `fabrik:paused` +
  `fabrik:awaiting-input` labels, and returns `ErrSkipItem`. The issue is
  skipped for this poll cycle; other repos are unaffected.
- **Single code path**: Removing `jobControlMode`/`jobControlDir` and the
  pre-registration at startup eliminates dual-mode branching and the bugs that
  come with it.

## Implementation

**`Engine` struct:**
```go
worktreeManagers map[string]*WorktreeManager  // key: "owner/repo"
fabrikDir        string                        // always os.Getwd() at startup
```

**Key methods:**
- `worktreesFor(nameWithOwner string) *WorktreeManager` — lookup; panics on miss
- `registerWorktrees(nameWithOwner, baseDir, worktreeRoot string)` — idempotent
  registration; no-op if already registered
- `ensureRepoReady(ctx, item) error` — always calls `ensureBareClone` and
  `registerWorktrees` on first access; subsequent calls are no-ops

**Bare clone path:** `.fabrik/repos/<owner>-<repo>.git`  
Using `owner-repo` (hyphen-joined) avoids cross-owner collisions when two orgs
have repos with the same name.

## Trade-offs

- **Panic on unregistered access**: `worktreesFor` panics rather than returning
  an error. This is deliberate — a miss means a programming error (caller didn't
  call `ensureRepoReady` first). Panics are loud and catch bugs early; an error
  return would propagate silently through call stacks.
- **In-memory registration**: WM registrations are not persisted; they're
  rebuilt on each restart by `ensureRepoReady`. This is acceptable because the
  bare clones persist on disk — a restart just re-discovers them rather than
  re-cloning.
- **First-run latency for existing users**: Single-repo users upgrading from an
  older version trigger a bare-clone on first run. This is a one-time cost.
- **`NewWithDeps` convenience**: Tests still pass a `*WorktreeManager` directly
  to `NewWithDeps`, which registers it under the configured `owner/repo` key.
  This keeps existing tests working without requiring them all to use the new
  map API.
