# ADR 2458: `fabrik upgrade` refuses on-disk changes while a daemon holds the directory

## Status
Accepted

## Context
`fabrik upgrade` rewrites `.fabrik/stages/*.yaml` and moves `.fabrik/plugin` (workflow migration, ADR 2139) or overwrites it (`--force`). A daemon in the same directory reads those files when it loads a worker; during the 0.0.84 upgrade test a worker failed to load a pre-migration skill name. The guidance to stop the daemon first was only documentation.

## Decisions
1. **Probe the flock, not the PID.** `internal/lockprobe` opens `.fabrik/fabrik.lock` read-only and takes a non-blocking shared `flock`. `EWOULDBLOCK` is the positive evidence of a live holder (the daemon holds `LOCK_EX` for all of `Run()`; the OS drops it when the process dies). The PID in the file is a display label only, read with a short retry because the daemon writes it just after taking the lock; an unreadable PID still refuses ("pid unknown"). This is the technique `tests/gate`'s `osProcs.LockHeld` already uses; that function is left untouched.
2. **Non-destructive and fail-open.** The probe never creates, writes, truncates or unlinks. A missing, unreadable or stale file, a failed flock for any other reason, or a platform without flock reports "not held".
3. **Placement.** The check runs after `--reconcile` returns and after the release-binary download/re-exec step, immediately before the first on-disk write (FR-005 keeps the binary half unchanged; the re-exec'd binary checks again). Condition: `force || !noMigrate`.
4. **`--no-migrate` is an unconditional bypass**, because the daemon's dev-build hook (`engine/upgrade.go`) runs it as a child while holding the lock. No parent-PID exception is needed. `--force` is checked even with `--no-migrate`.
5. **Refusal does not depend on pending work**; the command is an operator action to run with the daemon stopped.

## Consequences
- Residual risk: a shared lock taken in the microseconds between a starting daemon's `open` and its `LOCK_EX` could make that start fail; the gate's probe has the same exposure.
- On filesystems without working `flock` (some NFS/FUSE) the check fails open, as the daemon's own lock does.
- The daemon's lock-taking must stay a flock on that file, with the PID as a label.
- `plugin/fabrik/skills/fabrik/SKILL.md` still says `fabrik upgrade` does not check for a running daemon; that guidance was out of scope for this change and should be updated separately.
