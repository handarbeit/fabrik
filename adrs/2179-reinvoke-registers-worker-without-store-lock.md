# ADR 2179: A reinvoke registers a worker, not a store lock

## Status

Accepted

## Context

`dispatchReinvoke` (`engine/reinvoke.go`) used `itemstate.LocalLockAcquired{Worker: ...}` to replace its `WorkerEntered` placeholder with a handle whose `StartedAt` is reset after the semaphore wait and repo preparation. That mutation also sets `Lock.HeldByThis = true`. A reinvoke never adds a `fabrik:locked:<id>` label and nothing applied `LocalLockReleased`, so every reinvoked item kept a store lock that GitHub did not have. `cleanupLockedIssues` at shutdown then tried to remove the absent label and drew a burst of 404s (observed at the 2026-10-11 shutdowns; #2159 suppresses the symptom). Readers of `snap.Lock()` saw a stale "held by this instance" lock.

Research found that nothing in the reinvoke path uses `Lock`: in-process exclusion is `snap.Worker() != nil`, the cross-instance marker is `fabrik:editing`, heartbeat and liveness read only `Worker`, and the only non-test reader of `Lock.HeldByThis` is `cleanupLockedIssues`.

## Decision

`Lock` mirrors the `fabrik:locked:<id>` label and is set only by stage dispatch. A reinvoke registers a `Worker` only. A new narrow mutation, `WorkerRefreshed{Repo, Number, StageName, StartedAt}`, replaces the Worker handle (`LastSignAt = StartedAt`), returns `WorkerChanged` only and never touches `Lock`. The deferred `WorkerExited` is unchanged and already covers every exit path.

## Alternatives considered

- **Acquire and release the real label** like stage dispatch: two extra API calls and the `lockVerifyDelay` sleep per reinvoke, and a changed cross-instance contract, for a lock nothing needs.
- **Re-apply `WorkerEntered`**: re-emits `WorkerLifecycleChanged`, causing an extra poll wake.
- **Drop the refresh**: reopens the #1303/#1779 PID<=0 stale-clear window after a long semaphore or clone wait.
- **Defensive `LocalLockReleased` in the reinvoke defer**: unnecessary once nothing is acquired, and it could clear a legitimate stage lock if the `Worker()` guard were ever bypassed.

## Consequences

- After any reinvoke the store lock is unchanged (nil unless a real stage lock exists); shutdown attempts no lock-label removal for reinvoked items.
- `LocalLockAcquired`/`LocalLockReleased` keep their semantics for stage dispatch and `cleanupStaleWorker`.
- #2159's shutdown handling stays as defence in depth. ADR 2159's "Follow-up (not fixed here)" note is resolved by this ADR; that file lives on `fabrik/issue-2159` (PR #2167) and is not on this base, so the note must be updated when #2167 lands.

See `docs/state-machine.md` §7 (worker lifecycle).
