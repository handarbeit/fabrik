# ADR 022: Per-Repo Singleflight Coordination for Bare Clones

## Status

Accepted

## Context

When multiple issues from the same new repository appear on the project board simultaneously, concurrent workers all enter `ensureRepoReady` for the same `owner/repo`. The function checks whether a `WorktreeManager` is registered for that repo, finds none, and proceeds to call `ensureBareClone`. With N workers, N concurrent `git clone --bare` calls target the same destination directory.

The first clone succeeds; subsequent clones fail with:

```
fatal: cannot copy '.../templates/info/exclude' to '.../shadoworg-fabrik.git/info/exclude': File exists
```

This causes those issues to receive `fabrik:paused` + `fabrik:awaiting-input` labels, requiring manual intervention to retry.

The existing `worktreeMu` on `WorktreeManager` serializes per-worktree git operations, but a `WorktreeManager` does not exist until after the bare clone completes — so that mutex cannot protect the clone itself. Holding `e.mu` (the engine's general mutex) for the duration of a network-bound `git clone` would serialize all engine operations on all repos, which is unacceptable.

## Decision

Implement a **singleflight-style pattern using `sync.Map` + channels** (stdlib only) to serialize `ensureBareClone` calls per repo within `ensureRepoReady`.

A `cloneInFlight sync.Map` field on `Engine` stores `*cloneCall` entries keyed by `"owner/repo"`. Each `cloneCall` holds:
- `done chan struct{}` — closed when the clone completes (success or failure)
- `dir string` — the bare clone directory on success
- `err error` — the clone error on failure

### Protocol

**Election**: `cloneInFlight.LoadOrStore(nameWithOwner, call)` elects exactly one goroutine as the owner. The first to store wins; all others receive the existing entry.

**Owner behavior**:
1. Call `ensureBareClone`
2. Store result in `call.dir` / `call.err`
3. On failure: `close(call.done)` → `cloneInFlight.Delete(nameWithOwner)` → post comment/labels → return `ErrSkipItem`
4. On success: `registerWorktrees(...)` → `close(call.done)` → return nil

**Waiter behavior**:
1. Block on `<-existing.done`
2. On failure: log a brief message, return `ErrSkipItem` (no duplicate GitHub API calls)
3. On success: call `registerWorktrees(existing.dir, ...)` (idempotent) → return nil

### Failure-path cleanup

On clone failure the entry is deleted from `cloneInFlight` before returning, so future poll cycles (after the user removes `fabrik:paused`) can retry by electing a new owner. If the entry were left in place, the next poll cycle would find a closed channel with a non-nil error and immediately return `ErrSkipItem` forever.

### Success-path no-cleanup

On success the entry is left in place (closed channel, nil error). Future callers exit at the fast-path `registered` check in `ensureRepoReady` (the `WorktreeManager` is now registered) before ever consulting `cloneInFlight`. The stale entry is harmless and is never read after the first successful clone.

### Ordering invariant on failure

`close(call.done)` happens before `cloneInFlight.Delete(nameWithOwner)`. Waiters that unblock on `close(call.done)` read `call.err` (which is set before `close`) and return `ErrSkipItem`. A re-entrant goroutine from the *next* poll cycle calling `LoadOrStore` after the delete sees no existing entry and becomes the new owner — correct behavior for retry.

## Alternatives Considered

### Map of `sync.Mutex` (one per repo)

A `sync.Map` of `*sync.Mutex` values could serialize clones per repo. However, locking a mutex for the duration of a network operation and then releasing it does not propagate the clone result to waiters. After the owner unlocks, each waiter would re-enter the clone path, check `os.Stat(bareDir)` (now non-nil because the clone succeeded), and call the fetch path — which is harmless but wasteful. For the failure case, each waiter would also attempt its own clone into the partially-written directory, reproducing the original bug. The channel-based approach broadcasts the result to all waiters atomically.

### Global mutex

A single `sync.Mutex` guarding all bare clones would be correct but would serialize clones for different repos, harming throughput in projects with many new repos added simultaneously. Per-repo coordination preserves full concurrency across different repos.

### `golang.org/x/sync/singleflight`

The `singleflight` package implements exactly this pattern and would work. However, it is an external dependency not currently used by Fabrik. The project convention is stdlib-only (currently only `gopkg.in/yaml.v3` as an external dep). The `sync.Map` + channel implementation achieves the same behavior with a modest amount of code.

## Consequences

- Concurrent workers for the same new repo no longer race on the bare clone directory.
- Exactly one `AddComment` + labels is posted per failed clone, not one per concurrent worker.
- The `cloneInFlight sync.Map` introduces new shared state on `Engine`; it is self-cleaning on failure and never grows without bound (repos are bounded by the project board).
- The pattern is not obvious from reading `worktreeManagers` or `inFlight`; this ADR documents it for future contributors.
