# Feature Specification: Board-scoped host instance lock, worktree-nesting refusal, and per-poll lock verification

**Feature Branch**: `fabrik/issue-2097`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "instance lock: a board-scoped host lock, refuse to start inside a worktree, and re-verify the lock every poll

Report #1980: two v0.0.82 engines processed one board concurrently for about 56 hours despite the flock instance lock. The board was GES/zen, issue #75, per the sibling report #1981. The results were: unholdable pauses (18 Fabrik comments in an hour of no-op-breaker ping-pong); duplicated stage and comment work; doubled polling.

Investigation of v0.0.82 rules out the reported re-exec window as a way to *keep polling* unlocked: `Run()` is the only entry to polling, and a failed acquire always exits; the SIGHUP re-exec gap can only make the loser exit; `fabrik upgrade` neither signals, re-execs nor touches `fabrik.lock`. So the older process held its lock on a **different inode**. Two explanations remain (the incident's machine and logs aren't available to settle it): (1) (Most likely) two directories on one board. The lock is per directory: `fabrikDir` = `os.Getwd()`, lock at `fabrikDir/.fabrik/fabrik.lock`. Every issue worktree of a repo that tracks `.fabrik/` contains a complete `config.yaml` for the same board, and so do sibling clones and worktrees. A `fabrik` started in any of them runs with its own lock. In PAT mode both engines share `fabrik:locked:<user>`, and the tie-break lets both proceed. (2) The lock file was replaced under the running engine by something outside Fabrik. `tests/e2e/harness.go`'s comment wrongly says Fabrik unlinks the lock on shutdown. Its skip message advises 'remove and restart' on a stale lock, which is a vector for exactly that.

R1. Board-scoped host lock. In addition to the per-directory lock, an engine takes an exclusive flock on a host-level file keyed on the board (GitHub host + project owner + project number). It lives in a per-user location outside any repo (Plan picks it, e.g. os.UserCacheDir()/fabrik/locks/). A second engine for the same board on the same host, from any directory, refuses to start with an error that names the other instance's pid and directory. Record both in the lock file. The lock is held across the SIGHUP and self-upgrade re-execs the same way the per-directory lock is today (released immediately before exec and re-acquired in Run()). Plan confirms the loser exits.

R2. Refuse to start inside another instance's worktree. If fabrikDir is inside any .fabrik/worktrees/ directory, refuse to start with a clear error.

R3. Per-poll lock verification. Each poll cycle, cheaply verify the held per-directory lock and the board lock are still the files on disk: fstat of the held fd against stat of the path (dev+inode), plus the recorded pid. On a mismatch, stop dispatching and exit loudly ('lost the instance lock; another Fabrik may own this board').

R4. Fix the harness's lock advice. Correct the tests/e2e/harness.go comment about unlinking, and replace its 'remove and restart' skip advice with guidance that checks the pid first.

R5. Document the board lock in docs/USER_GUIDE.md (and §7 of docs/state-machine.md if it describes the instance lock), and regenerate docs/llms-full.txt."

## Background

Fabrik's existing instance lock is an exclusive flock on `.fabrik/fabrik.lock` inside the directory the engine was started from. It therefore only prevents two engines in the *same directory*. It does not prevent two engines serving the *same GitHub Project board* from different directories.

Report #1980 documents the cost of that gap. Two engines processed one board for roughly 56 hours, which produced:
- pauses that could not be held (18 Fabrik comments in an hour of no-op-breaker ping-pong);
- duplicated stage and comment work;
- doubled polling.

Both engines ran the same board while holding locks on different inodes. The incident's machine and logs are unavailable, so the root cause cannot be confirmed. Two explanations remain:

1. **(Most likely) Two directories on one board.** The lock is per directory, and many directories carry a full copy of the board configuration:
   - every issue worktree of a repo that tracks `.fabrik/`;
   - sibling clones;
   - sibling worktrees.

   A `fabrik` started in any of them takes its own lock. In PAT mode, both engines also share the `fabrik:locked:<user>` label, and its tie-break lets both proceed, so nothing else stops them.
2. **The lock file was replaced under a running engine** by something outside Fabrik. The live-e2e harness contains a comment that wrongly claims Fabrik unlinks the lock on shutdown, and a skip message that tells the operator to "remove and restart" a stale lock. Both mislead operators into a procedure that can create exactly this situation.

Investigation of v0.0.82 ruled out the re-exec windows (SIGHUP restart, `fabrik upgrade`) as a way for a second engine to *keep polling* unlocked. A failed lock acquire always exits, and a re-exec gap can only make the loser exit.

The fix has three layers: a lock keyed on the board rather than the directory, refusal to start where the directory is itself an issue worktree, and continuous verification that the held locks are still the ones on disk.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - A second engine on the same board is refused (Priority: P1)

An operator, or a stray process, starts `fabrik` from a different directory (another clone, a sibling directory) configured for a board that another Fabrik on the same host is already serving. The second engine refuses to start and says which instance already owns the board.

**Why this priority**: This is the most likely root cause of #1980 and the only fix that prevents the double-engine condition outright, rather than detecting it afterwards.

**Independent Test**: Start an engine configured for board B from directory X. Start a second engine configured for board B from directory Y. The second exits non-zero before polling, with an error naming the first engine's pid and directory X.

**Acceptance Scenarios**:

1. **Given** an engine running for board B from directory X, **When** a second engine for board B is started from directory Y on the same host, **Then** the second refuses to start and its error names the first engine's pid and directory X.
2. **Given** an engine running for board B, **When** an engine for a *different* board is started on the same host, **Then** both run.
3. **Given** an engine holding the board lock that then exits (cleanly, by crash, or by SIGKILL), **When** a new engine for the same board starts, **Then** it acquires the lock without manual cleanup.
4. **Given** an engine that performs a SIGHUP restart or a self-upgrade re-exec, **When** the process re-execs, **Then** the re-executed engine re-acquires the board lock and continues. A competing engine that started during the gap exits.

---

### User Story 2 - Starting inside an issue worktree is refused (Priority: P1)

An operator runs `fabrik` from inside a Fabrik-managed issue worktree. This is a likely accident, since every such worktree contains a complete copy of the board config. The engine refuses to start.

**Why this priority**: It closes the most common path into the incident class even before the board lock is considered, and gives a clear, immediate message instead of a later, subtler conflict.

**Independent Test**: Run startup with the working directory at or below a `.fabrik/worktrees/<owner>-<repo>/issue-N/` path. Startup is refused with a clear error. Run it from an ordinary directory. Startup proceeds.

**Acceptance Scenarios**:

1. **Given** a working directory inside a `.fabrik/worktrees/` directory, **When** the engine starts, **Then** it refuses with an error saying it will not run inside another instance's worktree.
2. **Given** a working directory with no `.fabrik/worktrees/` ancestor, **When** the engine starts, **Then** this check does not block it.

---

### User Story 3 - A lost lock stops the engine (Priority: P2)

A running engine's lock file is deleted or replaced by something outside Fabrik, so the engine's held lock no longer protects anything. The engine notices on its next poll, stops dispatching, and exits with a loud explanation, instead of continuing to run unprotected.

**Why this priority**: It is the defense for the second hypothesis (lock file replaced) and for any future way the locks might be bypassed. It is cheap, but P2 because stories 1 and 2 prevent the more likely cause.

**Independent Test**: Start an engine, replace or unlink its lock file (per-directory or board lock) while it runs, and let one poll cycle elapse. The engine stops dispatching and exits with the "lost the instance lock" message.

**Acceptance Scenarios**:

1. **Given** a running engine whose per-directory lock file is replaced by a different file at the same path, **When** the next poll cycle runs, **Then** the engine stops dispatching work and exits with a message that it lost the instance lock and another Fabrik may own the board.
2. **Given** a running engine whose board lock file is replaced or removed, **When** the next poll cycle runs, **Then** the same shutdown occurs.
3. **Given** a running engine whose lock files are untouched, **When** polls run, **Then** verification passes silently and adds no meaningful cost or log noise.

---

### User Story 4 - Operators are not told to delete a live lock (Priority: P3)

A developer running the live e2e suite sees the harness skip because the bed's lock looks stale. The advice they get checks whether the recorded pid is alive before suggesting any action, and the harness comment correctly states that Fabrik does not unlink the lock on shutdown.

**Why this priority**: Documentation-level fix of a known misleading vector, independent of the engine changes.

**Independent Test**: Read the harness comment and trigger its stale-lock skip message; the text matches the actual lock behavior and tells the operator to check the pid first.

**Acceptance Scenarios**:

1. **Given** the harness detects a lock file whose recorded pid is dead, **When** it skips, **Then** the message tells the operator to confirm the pid is not alive (and that no other Fabrik owns the board) before touching the lock, and no longer says simply to "remove and restart".
2. **Given** the harness source, **When** read, **Then** its comment does not claim that Fabrik unlinks the lock on shutdown.

---

### User Story 5 - The live e2e gate beds do not contend (Priority: P2)

The live e2e gate runs several beds concurrently on one host (ADR-1976), each with its own board, and restarts a bed's engine between legs (`TestSwitchTrainMode`). The new board lock must not make beds contend with each other, and a restarted bed must re-acquire its lock cleanly.

**Why this priority**: Without this the release gate itself could break, though the fix to the engine is the primary deliverable.

**Independent Test**: Start engines for two bed boards on one host; both run. Stop and restart one bed's engine; it re-acquires its board lock without waiting or manual cleanup.

**Acceptance Scenarios**:

1. **Given** multiple beds, each on its own board, **When** their engines run concurrently on one host, **Then** none is refused by the board lock.
2. **Given** a bed engine restarted by the gate, **When** it starts, **Then** it acquires the board lock cleanly.

---

### Edge Cases

- **Same board referenced differently.** Differences in letter case of the owner or host, or a user-owned versus an organisation-owned board, must not let two engines for one board appear as two boards. Host, owner and project number are the identity.
- **GitHub Enterprise Server.** The GitHub host is part of the board identity, so two boards with the same owner and number on different hosts are different boards.
- **Stale lock after a crash or SIGKILL.** The flock disappears with the process. A leftover file naming a dead pid must not block a new engine.
- **Other instance's recorded pid/directory unreadable or missing.** The refusal still happens (the flock is held), and the error says the owner could not be identified rather than failing differently.
- **Symlinked or relocated directories.** The worktree-nesting check and the recorded directory must behave sensibly when `fabrikDir` is reached through a symlink.
- **Lock location unavailable.** If the per-user lock location cannot be determined or created, the engine must not silently run without the board lock.
- **Re-exec windows.** During SIGHUP or self-upgrade restart the lock is released just before exec and re-acquired after. An engine that cannot re-acquire exits.
- **Verification races with legitimate re-exec.** Per-poll verification must not report a lost lock because of the engine's own release/re-acquire sequence.
- **Different engine versions.** An older Fabrik without the board lock can still be started on the same board. This feature cannot prevent that, which is accepted.
- **Cross-host.** Two hosts serving one board are not detected (out of scope).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: At startup, in addition to the existing per-directory lock, the engine MUST acquire an exclusive host-level lock keyed on the board, defined as the combination of GitHub host, project owner and project number. The lock file MUST live in a per-user location outside any repository.
- **FR-002**: If another engine on the same host already holds the lock for the same board, the starting engine MUST refuse to start, MUST exit non-zero before any polling or dispatch, and MUST name the holder's pid and directory in the error.
- **FR-003**: The board lock file MUST record the holding engine's pid and its Fabrik directory, so that FR-002's error can name them.
- **FR-004**: Engines configured for different boards on the same host MUST NOT contend on the board lock.
- **FR-005**: The board lock MUST be released automatically when the holding process ends by any means, including crash or SIGKILL, so that no manual cleanup is required after an unclean shutdown.
- **FR-006**: The board lock MUST be handled across SIGHUP restart and self-upgrade re-exec the same way as the per-directory lock: released immediately before the exec and re-acquired in `Run()`. An engine that cannot re-acquire it MUST exit.
- **FR-007**: The engine MUST refuse to start when its Fabrik directory is inside any `.fabrik/worktrees/` directory, with a clear error stating that it will not run inside another instance's worktree.
- **FR-008**: On every poll cycle the engine MUST verify, for both the per-directory lock and the board lock, that the file at the lock path is still the file it holds locked (device and inode of the held descriptor compared with those of the path), and that the recorded pid is still its own.
- **FR-009**: When verification (FR-008) fails, the engine MUST stop dispatching work and MUST exit with a loud message stating that it lost the instance lock and another Fabrik may own this board.
- **FR-010**: Verification (FR-008) MUST be cheap enough to run every poll without measurable load, and MUST NOT log anything on success.
- **FR-011**: The comment in `tests/e2e/harness.go` describing the lock MUST correctly state that Fabrik does not unlink the lock file on shutdown.
- **FR-012**: The harness's stale-lock skip message MUST replace the "remove and restart" advice with guidance that first checks whether the recorded pid is alive (and that no other Fabrik owns the board) before any change to the lock.
- **FR-013**: The live e2e gate's concurrent beds, each with its own board, MUST NOT be refused by the board lock, and a bed engine restart (such as `TestSwitchTrainMode`) MUST re-acquire its lock cleanly.
- **FR-014**: If the board lock location cannot be determined or created, the engine MUST NOT silently proceed without the board lock; the failure MUST be reported at startup.
- **FR-015**: `docs/USER_GUIDE.md` MUST document the board lock, covering what it prevents, where it lives, the refusal message, the worktree-nesting refusal and the per-poll verification. `docs/state-machine.md` §7 MUST be updated where it describes the instance lock. `docs/llms-full.txt` MUST be regenerated in the same change.
- **FR-016**: Unit tests MUST cover the four acceptance behaviours: same board and different directories (second refuses, naming first's pid and directory); different boards (both start); start inside `.fabrik/worktrees/...` (refused); lock file replaced under a running engine (next poll's verification stops it). Each test MUST be shown to be non-vacuous by neutralising its fix and observing the test fail.

### Key Entities *(if applicable)*

- **Board identity**: The tuple of GitHub host, project owner and project number that identifies one Project board. It is the key of the board lock.
- **Board lock file**: A host-level, per-user file holding an exclusive flock for the board, recording the holder's pid and Fabrik directory.
- **Per-directory lock file**: The existing `.fabrik/fabrik.lock`, unchanged in purpose, now also subject to per-poll verification.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With one engine running on a board, every attempt to start another engine for that board on the same host, from any directory, exits before its first poll and names the first engine's pid and directory.
- **SC-002**: Engines for different boards on the same host start and run concurrently with no refusals, including all concurrent beds in a live e2e gate run.
- **SC-003**: Starting the engine from within any `.fabrik/worktrees/` directory is refused 100% of the time, and starting from outside one is never refused by this check.
- **SC-004**: When a held lock file is replaced or removed under a running engine, the engine stops dispatching and exits within one poll cycle.
- **SC-005**: After any unclean engine exit, a new engine for the same board starts with no manual lock cleanup.
- **SC-006**: Each of the four unit-test behaviours fails when its corresponding fix is neutralised, and passes with the fix.
- **SC-007**: `docs/USER_GUIDE.md` describes the board lock, and the `docs-drift` check passes with the regenerated `docs/llms-full.txt`.

## Assumptions

- Board identity is host + project owner + project number, normalised so that case differences in owner or host do not create two identities.
- The lock location is per-user and outside any repository. The exact path (for example under the user cache directory) is a Plan decision.
- If the per-user location cannot be established, the engine fails visibly rather than running unlocked. Whether a documented fallback location exists is a Plan decision, but a silent skip is not acceptable.
- Both the board lock and the per-directory lock are verified each poll. The recorded pid is the engine's own, and a mismatch counts as a lost lock.
- The lock behaves like the per-directory lock across re-exec: the file descriptor is not kept open across exec; the lock is released just before exec and re-acquired in `Run()`.
- An engine losing the lock exits the process, rather than only pausing, so that it cannot resume unprotected.
- The worktree-nesting check applies to the engine's Fabrik directory (its working directory). It is a refusal only. It does not attempt to locate or manage the parent instance.
- The live e2e gate gives each bed its own board (ADR-1976), so beds have distinct board identities and need no special exemption. Plan confirms this and confirms that a bed restart re-acquires cleanly.
- Older Fabrik versions without the board lock cannot be forced to honour it.

## Out of Scope *(optional)*

- A cross-host lease, such as a heartbeat on the board.
- Stamping an instance id on Fabrik comments.
- Keeping the lock file descriptor open across exec.
- Any change to PAT-mode lock labels (`fabrik:locked:<user>`) or their tie-break.
- Detecting or repairing the double-engine condition after the fact, beyond a lost-lock exit.

## Source References *(optional)*

- Report #1980 (the incident) and sibling report #1981; mention #1980 in the PR body by bare number only, with no closing keyword near it.
- ADR-1976 (multi-bed live e2e gate).
- `docs/USER_GUIDE.md` (instance lock note and "Multi-instance locking"), `docs/state-machine.md` §7.4.
- `tests/e2e/harness.go` lock detection and skip text.
