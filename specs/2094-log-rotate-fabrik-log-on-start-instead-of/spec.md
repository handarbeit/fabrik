# Feature Specification: Rotate fabrik.log on start and open each run with a version banner

**Feature Branch**: `fabrik/issue-2094`
**Created**: 2026-10-10
**Status**: Specified
**Input**: User description: "log: rotate fabrik.log on start instead of truncating it, and open each run with a version banner

Report #2028: `Run()` opens `.fabrik/fabrik.log` with `O_CREATE|O_WRONLY|O_TRUNC` (`engine/poll.go:233-234`). So every start (a manual restart, the SIGHUP exec, the `--auto-upgrade` re-exec) destroys the previous run's log. That is exactly the before/after-upgrade boundary operators need, and the evidence incidents like #1980 depend on. `runLogJanitor` (`engine/janitor.go:305`) only prunes `.fabrik/logs/`.

The log also has no version or start-time banner. `[startup]` lines carry no timestamp (`poll.go:251-262`). Pruefer already does this properly: append plus size rotation (`pruefer/logfile.go`, #1428).

R1. Rotate on start, don't truncate. At startup, shift `fabrik.log.(N-1)` → `fabrik.log.N` … `fabrik.log` → `fabrik.log.1`, keeping N backups (default 5; Plan decides whether it's configurable), then open a fresh `fabrik.log`.

R2. Keep "one file per run". Don't switch to append. The live gate depends on `fabrik.log` holding only the current run: `tests/gate/backoff.go:16-21` scans the whole file, and `tests/gate/archive.go:34-39` detects restarts by the file's head bytes. Plan confirms both still hold after rotation, including across the SIGHUP and self-upgrade execs.

R3. Start banner. The first line of each run's log states the version, the commit if it's a dev build, the PID, an RFC3339 start time, and the reason for the start where it's known (fresh start, SIGHUP restart, self-upgrade re-exec).

R4. Optionally cap a single run's file by size, as Pruefer does. Plan decides; it isn't required.

R5. Document the rotation in `docs/USER_GUIDE.md` and regenerate `docs/llms-full.txt`.

Scope: In scope: engine log rotation, the banner, tests, docs. Out of scope: `.fabrik/logs/` per-invocation logs, and Pruefer's log. Mention #2028 in the PR body. Do NOT use a closing keyword (`Closes`/`Fixes`/`Resolves`) near #2028. This issue's own number gets `Closes`.

Acceptance: Unit tests: three consecutive starts leave `fabrik.log` (third run), `.1` (second) and `.2` (first), each beginning with its banner. Rotation is capped at N. A missing or unwritable backup slot doesn't prevent startup (logged). The gate's restart-detection and backoff scan tests still pass unchanged."

## Background

Today the engine's persistent poll log, `.fabrik/fabrik.log`, is opened with truncation on every startup, so it always holds only the current run. Every restart (a manual restart, the SIGHUP restart, the `--auto-upgrade` re-exec) therefore erases the previous run's log. That previous log is the evidence operators need when diagnosing what happened just before an upgrade or restart, and incidents such as #1980 depend on it. The existing log retention janitor only prunes the per-invocation logs under `.fabrik/logs/`, so nothing preserves `fabrik.log` across runs.

The log also gives no orientation: its first lines are `[startup]` warnings without timestamps, and nothing says which version or build produced the file, which process wrote it, when it started, or why. Once previous runs are kept, an operator reading several files needs that to tell them apart. Pruefer already solves both problems for its own log (append with size rotation, #1428); this work gives the engine log the equivalent properties while deliberately keeping its distinct "one file per run" shape. This work was reported in #2028.

The live e2e gate runner relies on the "one file per run" property: its rate-limit-backoff scan reads the whole current file, and its log archiver recognises an engine restart by the head bytes of the file. Both comments in the gate currently justify themselves by the truncation behaviour, so they must keep working (and their wording will need to match) after the change.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Previous runs' logs survive a restart (Priority: P1)

An operator restarts Fabrik (manually, via SIGHUP, or via a self-upgrade) and then needs to see what the previous run logged around the boundary.

**Why this priority**: This is the core defect — the evidence is destroyed on every start.

**Independent Test**: Start the engine three times in the same project directory. Afterwards `fabrik.log` holds the third run, `fabrik.log.1` the second and `fabrik.log.2` the first, byte-for-byte what each run wrote.

**Acceptance Scenarios**:

1. **Given** no existing log, **When** the engine starts, **Then** `fabrik.log` is created and no backup files exist.
2. **Given** `fabrik.log` from a previous run, **When** the engine starts, **Then** the old content is in `fabrik.log.1` and `fabrik.log` is a fresh file containing only the new run.
3. **Given** backups `.1` through `.N` already exist, **When** the engine starts, **Then** the oldest backup is discarded, each remaining one shifts up by one, and no more than N backups exist afterwards.
4. **Given** a gap in the backup chain (for example `.2` is missing), **When** the engine starts, **Then** the existing files still shift correctly and startup succeeds.

---

### User Story 2 - Each run's log opens with an identifying banner (Priority: P1)

An operator or maintainer opens a log file and wants to know which build wrote it, when it started, and why that run began.

**Why this priority**: Without a banner, retained logs cannot be told apart, and the before/after-upgrade boundary is the main reason for keeping them.

**Independent Test**: Start the engine; the first line of `fabrik.log` states version, PID, RFC3339 start time and start reason, plus the commit for a dev build.

**Acceptance Scenarios**:

1. **Given** a release build, **When** the engine starts fresh, **Then** line 1 of `fabrik.log` contains the version, the PID, an RFC3339 start time and the reason "fresh start".
2. **Given** a dev build, **When** the engine starts, **Then** the banner additionally contains the commit.
3. **Given** the engine restarts through SIGHUP, **When** the new process opens its log, **Then** the banner's reason says it was a SIGHUP restart.
4. **Given** the engine re-execs after a self-upgrade, **When** the new process opens its log, **Then** the banner's reason says it was a self-upgrade re-exec (ideally naming the previous and new version).
5. **Given** the start reason cannot be determined, **When** the engine starts, **Then** the banner still appears and states the reason as unknown (or omits it) rather than guessing.

---

### User Story 3 - Rotation never blocks startup and never breaks the live gate (Priority: P1)

A maintainer relies on the engine always starting and on the e2e release gate's log-based checks continuing to work.

**Why this priority**: Startup availability and the release gate are existing guarantees that this change must not weaken.

**Independent Test**: Make a backup slot unwritable or absent and start the engine; run the existing gate unit tests for the backoff scan and the restart detection unchanged.

**Acceptance Scenarios**:

1. **Given** a backup slot that cannot be renamed or removed, **When** the engine starts, **Then** the failure is logged and the engine still starts with a fresh `fabrik.log`.
2. **Given** the existing gate tests for the backoff scan and log restart detection, **When** they run after this change, **Then** they pass without modification.
3. **Given** a SIGHUP restart or self-upgrade exec within a gate leg, **When** the new process starts, **Then** `fabrik.log` holds only the new run's content and the gate still detects the restart as a new segment.

---

### User Story 4 - Operators can learn about the behaviour from the docs (Priority: P2)

An operator reading the user guide wants to know where previous logs go and how many are kept.

**Why this priority**: Behaviour change that is invisible without documentation; secondary to the behaviour itself.

**Independent Test**: The user guide's engine-log section describes rotation, the number of backups kept and the banner, and the docs bundle matches a fresh regeneration.

**Acceptance Scenarios**:

1. **Given** the user guide, **When** the engine-log section is read, **Then** it no longer says the file is truncated on each startup and instead describes rotation, backup naming, retention count and the banner.

---

### Edge Cases

- First ever start: there is no `fabrik.log` to rotate.
- Backups beyond N from an earlier, larger setting: they are not required to be cleaned up beyond what the rotation shift naturally discards (an assumption below).
- Rotation failure mid-chain (permissions, disk full, a directory in place of a file): logged, startup proceeds.
- An empty previous `fabrik.log` (a run that died before writing anything): rotated like any other (no special casing needed).
- Two engines in the same directory: prevented by the existing instance lock, and rotation happens after the lock is held so it cannot race.
- Startup where the log file cannot be opened at all keeps today's behaviour: a warning, and the engine runs without a log file.
- A very long-running single run: the file grows unbounded, as today (see Assumptions on the size cap).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: At engine startup, before opening the new log, the existing `.fabrik/fabrik.log` is preserved by rotation: `fabrik.log` becomes `fabrik.log.1`, `fabrik.log.K` becomes `fabrik.log.(K+1)`, and the oldest backup beyond the retention count is discarded.
- **FR-002**: The number of backups retained is 5 by default, and never more than the configured retention count exist after a start.
- **FR-003**: After rotation, a fresh `fabrik.log` is created containing only the new run's output; the engine never appends to a previous run's file.
- **FR-004**: The first line of each run's `fabrik.log` is a banner stating the Fabrik version, the PID, the start time in RFC3339 format and the start reason; for a dev build it also states the commit.
- **FR-005**: The banner's start reason distinguishes a fresh start, a SIGHUP restart and a self-upgrade re-exec when the engine can tell, and is clearly marked unknown otherwise.
- **FR-006**: A rotation step that fails (missing slot, unwritable slot, any rename or remove error) is logged and does not prevent the engine from starting or from opening a fresh `fabrik.log`.
- **FR-007**: Rotation takes place only after the engine has acquired its single-instance lock, so concurrent engines in one project directory cannot rotate against each other.
- **FR-008**: The rate-limit-backoff scan and log-restart detection in the live gate runner keep working unchanged in behaviour: `fabrik.log` holds only the current run, including after SIGHUP and self-upgrade execs. Any code comments there that justify themselves by the old truncation are updated to describe rotation.
- **FR-009**: Unit tests cover: three consecutive starts leaving the third, second and first runs in `fabrik.log`, `.1` and `.2` respectively, each beginning with its own banner; the cap on the number of backups; and startup surviving a missing or unwritable backup slot.
- **FR-010**: `docs/USER_GUIDE.md` describes the rotation (backup naming, how many are kept, one file per run) and the banner, no longer states that the file is truncated on startup, and `docs/llms-full.txt` is regenerated in the same change.

### Key Entities *(if applicable)*

- **Engine log (`.fabrik/fabrik.log`)**: the current run's persistent poll and startup log; exactly one run per file.
- **Rotated backup (`.fabrik/fabrik.log.N`)**: the log of the Nth most recent previous run; `N` starts at 1 for the most recent.
- **Start banner**: the first line of each run's log; identifies version, optional commit, PID, start time and start reason.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: After any number of consecutive starts, the logs of the most recent N previous runs are still available on disk, each as a complete file.
- **SC-002**: 100% of runs' log files begin with a banner containing version, PID, RFC3339 start time and start reason.
- **SC-003**: The total number of engine log files in `.fabrik/` never exceeds N + 1.
- **SC-004**: A deliberately failing rotation step produces a log message and zero startup failures.
- **SC-005**: The existing gate tests for the backoff scan and restart detection pass with no changes to their assertions.

## Assumptions

- The default retention is 5 backups. Whether it is operator-configurable (flag, env var, config key) is left to Plan; if made configurable it follows the project's existing configuration conventions, and the default stays 5.
- The optional per-run size cap (R4) is not required for this issue. Plan may include it if cheap and consistent with Pruefer's behaviour; nothing in this spec depends on it, and omitting it leaves a single run's file growing unbounded as today.
- Whether a restart reason is "known" depends on what the new process can learn about how it was started; where it cannot be determined the banner says so rather than guessing. The mechanism for passing the reason across the SIGHUP and self-upgrade execs is a Plan decision.
- Backups are plain uncompressed files named `fabrik.log.1`, `fabrik.log.2`, … in the same directory as `fabrik.log`, matching the naming used by the gate archiver's per-run numbered segments.
- Backups left over from a previously larger retention count are not actively purged beyond what normal rotation discards.
- The banner is a single line so that "first line" checks and head-byte comparisons stay simple; because it embeds PID and start time, consecutive runs have distinct heads.

## Out of Scope *(optional)*

- Per-invocation logs under `.fabrik/logs/` and their janitor.
- Pruefer's log file and its rotation.
- Switching the engine log to append mode or to a continuous multi-run file.
- Compression or off-host shipping of rotated logs.
- Adding timestamps to every existing `[startup]` line (the banner provides the run's start time).

## Source References *(optional)*

- Report #2028 (mention by bare number only in the PR body).
- Engine log open site: `engine/poll.go` (`Run()`, the `O_TRUNC` open of `fabrik.log`).
- Log retention janitor (only prunes `.fabrik/logs/`): `engine/janitor.go`.
- Pruefer's append-plus-size-rotation precedent: `pruefer/logfile.go` (#1428).
- Gate consumers of the one-file-per-run property: `tests/gate/backoff.go`, `tests/gate/archive.go`.
- Current documented behaviour to update: `docs/USER_GUIDE.md`, engine log section.
