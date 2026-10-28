# Feature Specification: `fabrik upgrade` refuses on-disk changes while a daemon holds the directory

**Feature Branch**: `fabrik/issue-2458`
**Created**: 2026-10-28
**Status**: Specified
**Input**: User description: "## Problem

`fabrik upgrade` never checks the per-directory instance lock (`.fabrik/fabrik.lock`, taken by `acquireInstanceLocks` in `engine/instance_lock.go`). Run while a daemon is live in the same directory, its workflow migration (#2139) rewrites `.fabrik/stages/*.yaml` skill references and moves `.fabrik/plugin` into `.fabrik/plugins/local-workflows` underneath the running engine.

Observed on 2026-10-13 during the 0.0.84 upgrade test on a downstream project: the upgrade ran under a daemon its operator had not noticed was running. About 21 s later a worker loaded during that window failed to load `fabrik:fabrik-review-comment` (the pre-migration name) before the change was reverted. Nothing told the operator a daemon held the directory.

The PM plugin's upgrade guidance already says to stop the daemon first; the command should enforce it.

## Requirements

- **FR-001** When `fabrik upgrade` would apply the workflow migration (i.e. not `--no-migrate`, not `--reconcile`) and the instance lock for the current Fabrik directory is held by another live process, it refuses before changing any file, exits non-zero, and names the holding PID and the remedy (stop the daemon, then run `fabrik upgrade` again).
- **FR-002** `fabrik upgrade --no-migrate` is unaffected: the daemon itself runs it as a child process while holding the lock (`engine/upgrade.go`, the dev-build auto-upgrade hook), and that must keep working.
- **FR-003** `--force` (which overwrites `.fabrik/plugin`) is refused under a held lock in the same way as the migration.
- **FR-004** The lock check is non-destructive: it must not take, break or replace a lock held by someone else, and must not leave a lock file behind when none existed. An unreadable or stale lock (no live holder) does not block the upgrade.
- **FR-005** The release-binary download/re-exec half of `fabrik upgrade` keeps its current behaviour; the check concerns only the on-disk workflow changes.

## Scope

In: `cmd/upgrade.go`, reuse of the existing lock-probe logic (the e2e harness and gate already read the lock's holder). Out: any change to how the daemon takes its lock; `fabrik init`; `refresh-stages`.

## Acceptance

- With a daemon running in the directory, `fabrik upgrade` on a project that needs migration exits non-zero, prints the PID and remedy, and `git status` shows no change.
- With no daemon running, the same command migrates as today.
- `fabrik upgrade --no-migrate` succeeds while the lock is held (test with a held lock).
- Tests are shown non-vacuous by neutralising the check."

## Background

`fabrik upgrade` can move a customised `.fabrik/plugin` into `.fabrik/plugins/local-workflows` and rewrite the skill references in `.fabrik/stages/*.yaml` (the workflow migration). A running daemon in the same directory reads exactly those files when it loads a worker. Changing them underneath it creates a window in which a worker can reference a skill name that no longer resolves.

This was seen on 2026-10-13 during the 0.0.84 upgrade test on a downstream project. The operator had not noticed a daemon was running. About 21 s after the upgrade started, a worker loaded in that window failed to load `fabrik:fabrik-review-comment` (the pre-migration name) before the change was reverted. Nothing told the operator that a daemon held the directory.

The PM plugin's upgrade guidance already tells the operator to stop the daemon first. This change makes the command enforce that guidance instead of relying on it being read.

Each daemon takes a per-directory instance lock (`.fabrik/fabrik.lock`). Other tooling (the e2e harness and the gate runner) already reads that lock's holder, so "is a daemon live here, and which PID?" is an established question.

The daemon itself runs `fabrik upgrade --no-migrate` as a child process while holding the lock, in its dev-build auto-upgrade hook. That path must keep working.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator is stopped from migrating under a live daemon (Priority: P1)

An operator runs `fabrik upgrade` in a Fabrik directory where a daemon is running. The command declines to change any file, says which process holds the directory, and says what to do.

**Why this priority**: This prevents the observed failure, where a worker hit a broken skill reference mid-migration.

**Independent Test**: Hold the directory's instance lock from another live process, in a project that needs the workflow migration. Run `fabrik upgrade`. It exits non-zero, the output names the holder's PID and the stop-the-daemon-then-rerun remedy, and the working tree and `.fabrik/` are byte-for-byte unchanged.

**Acceptance Scenarios**:

1. **Given** a live process holds the instance lock and the project needs migration, **When** the operator runs `fabrik upgrade`, **Then** the command exits non-zero, names the holding PID and the remedy, and changes no file.
2. **Given** the same state, **When** the operator runs `fabrik upgrade --force`, **Then** the command refuses in the same way and `.fabrik/plugin` is untouched.
3. **Given** no process holds the lock, **When** the operator runs `fabrik upgrade` on a project that needs migration, **Then** the migration is applied as it is today.

---

### User Story 2 - The daemon's own `--no-migrate` upgrade keeps working (Priority: P1)

The daemon's dev-build auto-upgrade hook runs `fabrik upgrade --no-migrate` as a child process while the daemon holds the lock. That invocation must not be blocked by the daemon's own lock.

**Why this priority**: Blocking it would break the dev-build auto-upgrade for every dev daemon.

**Independent Test**: With the instance lock held by a live process, run `fabrik upgrade --no-migrate`. It behaves exactly as it does with no lock held.

**Acceptance Scenarios**:

1. **Given** a live process holds the instance lock, **When** `fabrik upgrade --no-migrate` runs, **Then** it proceeds as it does today and is not refused.
2. **Given** a live process holds the instance lock, **When** `fabrik upgrade --reconcile` runs, **Then** it prints its reconciliation prompt as today, because it changes no file.

---

### User Story 3 - The check never disturbs the lock (Priority: P2)

The operator or an automated caller runs `fabrik upgrade` in a directory whose lock is held, stale, unreadable, or absent. The check observes the lock without changing it.

**Why this priority**: A probe that took, broke or created the lock would itself damage the daemon or confuse later starts.

**Independent Test**: Run the command against each lock state (held by a live process, left behind with no live holder, unreadable, absent). Afterwards, compare the lock file and its holder before and after: nothing changed, and no lock file exists where none existed before.

**Acceptance Scenarios**:

1. **Given** a lock file exists but its recorded holder is not alive, **When** `fabrik upgrade` runs, **Then** the upgrade proceeds as if no daemon were present.
2. **Given** the lock file is unreadable, **When** `fabrik upgrade` runs, **Then** the upgrade is not blocked by it.
3. **Given** no lock file exists, **When** `fabrik upgrade` runs, **Then** no lock file exists afterwards.
4. **Given** a live process holds the lock, **When** `fabrik upgrade` refuses, **Then** that process still holds the lock, unchanged.

---

### Edge Cases

- A lock whose holder has died is stale and does not block.
- A lock file that cannot be read or parsed does not block; the check only refuses on positive evidence of a live holder.
- The holder is the current process or its parent (for example the daemon invoking its own upgrade). The only such path in the codebase today is `--no-migrate`, which is exempt by FR-002.
- The release-binary download and re-exec half of `fabrik upgrade` is unchanged, even when the directory's lock is held (FR-005).
- `--no-migrate` combined with `--force` is still refused by FR-003, because `--force` overwrites `.fabrik/plugin`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: When `fabrik upgrade` would apply the workflow migration (neither `--no-migrate` nor `--reconcile`) and the instance lock for the current Fabrik directory is held by another live process, it MUST refuse before changing any file, exit non-zero, and name the holding PID and the remedy (stop the daemon, then run `fabrik upgrade` again).
- **FR-002**: `fabrik upgrade --no-migrate` MUST be unaffected by a held lock, so the daemon's own dev-build auto-upgrade child process keeps working.
- **FR-003**: `fabrik upgrade --force` (which overwrites `.fabrik/plugin`) MUST be refused under a held lock in the same way as the migration.
- **FR-004**: The lock check MUST be non-destructive. It MUST NOT take, break or replace a lock held by someone else, and MUST NOT leave a lock file behind when none existed. An unreadable or stale lock (no live holder) MUST NOT block the upgrade.
- **FR-005**: The release-binary download/re-exec half of `fabrik upgrade` MUST keep its current behaviour. The check concerns only the on-disk workflow changes.
- **FR-006**: `fabrik upgrade --reconcile` MUST keep working under a held lock. It only prints a prompt and changes no file.

### Key Entities *(if applicable)*

- **Instance lock**: The per-directory lock (`.fabrik/fabrik.lock`) a running daemon holds for the Fabrik directory. It identifies a live holder by PID.
- **On-disk workflow changes**: The writes `fabrik upgrade` makes to `.fabrik/stages/*.yaml` and `.fabrik/plugin` / `.fabrik/plugins/local-workflows` (migration, or overwrite under `--force`).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With a live holder of the instance lock and a project that needs migration, `fabrik upgrade` exits non-zero, its output contains the holder's PID and the remedy, and the working tree and `.fabrik/` show no change.
- **SC-002**: With no live holder of the lock, the same command migrates exactly as it does today.
- **SC-003**: With the lock held, `fabrik upgrade --no-migrate` completes successfully, as it does with no lock held.
- **SC-004**: After any run, in any lock state, the lock file's holder is unchanged and no lock file has been created where none existed.
- **SC-005**: The tests fail when the check is neutralised, so they are shown to exercise it rather than pass vacuously.

## Assumptions

- "Held by another live process" is decided by the same notion of liveness the existing lock readers use (the gate and e2e harness). This spec does not define a new one.
- The refusal applies whenever the on-disk half would run without `--no-migrate` or `--reconcile`. It does not require first proving that migration work is actually pending, because the command is documented as an operator action to run with the daemon stopped.
- The refusal output goes to the user as an ordinary error with a non-zero exit, matching how the command reports other refusals (for example the existing customisation refusal).
- Only the lock for the current Fabrik directory matters. A daemon in a different directory does not block.

## Out of Scope *(optional)*

- Any change to how the daemon takes, holds or releases its instance lock.
- `fabrik init` and `refresh-stages`.
- Stopping the daemon automatically on the operator's behalf.
- Changes to the PM plugin's upgrade guidance.

## Source References *(optional)*

- `cmd/upgrade.go`: the `--force`, `--reconcile` and `--no-migrate` flags and the migration/refusal flow.
- `engine/instance_lock.go`: `acquireInstanceLocks`, the daemon-side lock.
- `engine/upgrade.go`: the dev-build auto-upgrade hook that runs `fabrik upgrade --no-migrate`.
- Workflow migration: #2139.
