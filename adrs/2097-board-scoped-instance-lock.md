# ADR 2097: Board-scoped host instance lock, worktree refusal and per-poll lock verification

## Status

Accepted

## Context

Report #1980: two engines processed one board for about 56 hours despite the flock instance lock. The lock was per directory (`<fabrikDir>/.fabrik/fabrik.lock`), and many directories carry a full copy of a board's configuration: every issue worktree of a repo that tracks `.fabrik/`, sibling clones, sibling worktrees. An engine started in any of them took its own lock; in PAT mode both shared `fabrik:locked:<user>` and the tie-break let both proceed. A second candidate cause is the lock file being replaced under a running engine; the e2e harness comment wrongly said Fabrik unlinks the lock on shutdown and its skip text advised "remove and restart".

## Decision

1. **Board lock.** In addition to the directory lock, `Run()` takes an exclusive non-blocking flock on a per-user file keyed on GitHub host + project owner + project number (lower-cased; empty host = `github.com`; `OwnerType` excluded because user and organisation logins share a namespace). File name: readable slug plus 8 hex of the identity's sha256. Location: `$FABRIK_LOCK_DIR`, else `os.UserCacheDir()/fabrik/locks` (`0700`). `UserConfigDir` was rejected (TCC-protected on macOS). A cache cleaner removing the file is the "lock replaced" case that per-poll verification turns into a loud exit. If no directory resolves or it cannot be created, startup fails; there is no unlocked fallback. `FABRIK_LOCK_DIR` is classified `Scrubbed` (daemon-only) and is also how engine and cmd `TestMain` keep tests hermetic: many tests share one board identity and several suites run concurrently on one host.
2. **Record format.** The board file holds `{"pid","dir","started"}`, written in one write after the flock is owned, so the refusal can name the holder. The directory lock file stays exactly `"<pid>\n"` because `tests/gate/bed.go` (`lockedBedPID`) parses it as a bare pid. A refused loser retries reading the record for about 500 ms, then says the owner could not be identified.
3. **Order.** Worktree refusal, directory lock, board lock, then log rotation. A refused loser never rotates the winner's log. The worktree check lives in `Run()`, not `New()`, so engine tests that call `New()` from a Fabrik worker's worktree still run.
4. **R2.** Refuse when `fabrikDir` (symlinks resolved) has a `.fabrik`/`worktrees` component pair.
5. **R3.** `doPollCycle` verifies both locks (fstat vs stat dev+inode, recorded pid == own pid) before each poll, silently on success. On loss it stops, logs loudly, and `Run()` returns an error. `drainAndExit` skips `cleanupLockedIssues` because in PAT mode the other engine may legitimately hold `fabrik:locked:<user>`. It is not in `poll()`/`PollOnce` (ADR-1449).
6. **Re-exec.** Same as the directory lock: SIGHUP releases explicitly before exec, self-upgrade relies on close-on-exec, `Run()` re-acquires. No retry window: whoever wins the flock after the gap keeps it and the loser exits. The pid is stable across exec.
7. **R4.** The harness comments no longer claim an unlink and the stale-lock skip text tells the operator to check the pid first.

## Consequences

- Beds need no exemption: each has its own board (ADR-1976), so keys differ, and a restarted bed re-acquires cleanly because the flock dies with the process.
- Residual gaps by design: older binaries without the board lock, two hosts on one board, and the PAT-mode `fabrik:locked:<user>` tie-break are untouched.
- Pruefer's `acquireLock` has the same per-directory weakness; out of scope here, a follow-up candidate.
