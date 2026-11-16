# Feature Specification: Configurable workspace root for worktrees, clones, sessions and logs

**Feature Branch**: `fabrik/issue-2470`
**Created**: 2026-11-15
**Status**: Specified
**Input**: User description: the original issue request, verbatim:

> ## Problem
>
> Everything Fabrik writes at runtime lives under `<bed>/.fabrik/`, on the same volume as the main
> checkout. Reported in #2070 (@verveguy). On macOS that is costly: Time Machine takes hourly
> whole-volume local snapshots of the home volume and keeps each for 24 hours, and every snapshot pins
> data deleted after it. Fabrik's workload is mostly create-and-delete (a worktree plus `node_modules`
> per issue, build and test output). On one busy host a day of snapshots pinned **133 GB**. Excluding a
> directory doesn't help, because snapshots are whole-volume. A separate, excluded APFS volume does, but
> Fabrik cannot write outside `<bed>/.fabrik/`.
>
> The same need: a faster scratch disk, a quota-capped volume so a runaway agent can't fill the system
> disk, or a volume excluded from Spotlight or a backup agent.
>
> ## Requirements
>
> **R1 — A configured workspace root.** `workspace_root` in `config.yaml`; unset means `<bed>/.fabrik/`,
> so behaviour is byte-identical.
>
> **R2 — What moves:** `worktrees/`, `repos/` (kept together, so `git worktree` stays on one
> filesystem), `sessions/`, `logs/`, `debug/`, and stage state. **What stays in the bed:** `config.yaml`,
> `.env`, `fabrik.lock` and the local API socket under `state/`, since the CLI and TUI find a running
> daemon through them.
>
> **R3 — One resolver.** The root is rebuilt independently today: `engine/engine.go` (three sites),
> `cmd/resume.go`, `tui/commands.go` (`worktreePathForIssue`), `watch/model.go`, and the session and log
> helpers. Every site goes through one function (for example `cfg.WorkspaceDir("worktrees")`), so the
> setting cannot be honoured in some places and missed in others. Add a check that no site builds the
> path by hand.
>
> **R4 — The `.env` symlink.** `engine/symlink_env.go` uses a relative target (`filepath.Rel`). When the
> worktree root is outside the bed, link to the absolute source. `symlinkEnvIfEnabled` skips an
> existing `.env` (`Lstat`), so a broken link is never repaired. Detect and repair one.
>
> **R5 — Path comparisons.** Everything that matches worktree paths compares resolved paths
> (`filepath.EvalSymlinks`) or consistently unresolved ones: the janitor, `migrateSessions`,
> `findWorktreeForIssue`, the cwd-based process reaper (ADR 063), and Pruefer's worktree handling.
> Otherwise moved worktrees are skipped or orphaned.
>
> **R6 — Migration safety.** Changing `workspace_root` on a bed that already has worktrees must not
> orphan them: refuse to start while the old root holds active worktrees, or move them
> (`git worktree move` + re-link `.env`). Decide which, and document it.
>
> ## Scope
>
> **In scope:** R1–R6, config plumbing per the documented precedence, `docs/USER_GUIDE.md`, an ADR, tests.
> **Out of scope:** moving `state/` (the report suggests a separate option if ever needed); managing the
> volume itself.
>
> ## Acceptance
>
> 1. Unset `workspace_root` → every path identical to today.
> 2. Set → worktrees, clones, sessions and logs land under it, and config/lock/socket stay in the bed.
> 3. No site builds a workspace path outside the resolver, pinned by a test.
> 4. A worktree outside the bed gets a working absolute `.env` link, and a broken one is repaired.
> 5. The janitor and the reaper treat moved worktrees correctly, with no orphan and no wrong reap.
> 6. Changing the root with live worktrees follows R6.
> 7. Tests demonstrated non-vacuous; `go test -race ./...` and `go vet ./...` pass.
>
> ## Notes
>
> Reported in #2070 — a human-owned community thread. Mention it in the PR body by bare number only. Do
> not place any closing keyword (`Closes`/`Fixes`/`Resolves`) adjacent to that number in any form,
> including inside a negated or disclaiming sentence. Closed by hand once a release ships.

## Background

Everything Fabrik writes at runtime today lives under `<bed>/.fabrik/`, on the same volume as the main checkout. This was reported in community thread #2070 by an operator running Fabrik on macOS.

- **The cost.** Time Machine takes hourly whole-volume local snapshots of the home volume and keeps each for 24 hours. Every snapshot pins data that was deleted after it was taken.
- **Why Fabrik is hit hard.** Its workload is mostly create-and-delete: a worktree plus `node_modules` per issue, and build and test output. On one busy host, a day of snapshots pinned **133 GB**.
- **Why excluding a directory doesn't help.** Snapshots are whole-volume. A separate, excluded APFS volume does help, but Fabrik cannot currently write outside `<bed>/.fabrik/`.

The same need shows up elsewhere:

- a faster scratch disk;
- a quota-capped volume, so a runaway agent cannot fill the system disk;
- a volume excluded from Spotlight or a backup agent.

The bed's own location does not change. Only where the high-churn runtime data lives does. The files a running daemon is found through (config, `.env`, lock, local API socket) must stay in the bed.

Related work: #2070 is the human-owned report thread. It is mentioned here for context only.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Put runtime churn on a separate volume (Priority: P1)

An operator sets `workspace_root` to a directory on a separate, snapshot-excluded volume. They start Fabrik, and every per-issue worktree, bare clone, session record and log lands under that directory. Config, `.env`, the instance lock and the local API socket stay in the bed. The CLI, TUI and MCP tools still find the running daemon.

**Why this priority**: This is the whole point of the feature. Without it the reported problem (snapshot bloat) is unsolved.

**Independent Test**: Start an engine with `workspace_root` set and process one issue through a stage. Assert that the worktree, clone, session and log files exist only under the configured root, and that the lock and socket exist in the bed.

**Acceptance Scenarios**:

1. **Given** `workspace_root` points to an existing empty directory, **When** the daemon processes an issue, **Then** the worktree, bare clone, session files, logs and debug output are created under that root and nothing new of those kinds appears under `<bed>/.fabrik/`.
2. **Given** `workspace_root` is set, **When** `fabrik` CLI, TUI or watch commands run in the bed, **Then** they locate the daemon, its worktrees, sessions and logs correctly.
3. **Given** `workspace_root` is set, **When** the daemon is running, **Then** `fabrik.lock` and the local API socket are in `<bed>/.fabrik/state/`.

---

### User Story 2 - Unset means no change (Priority: P1)

An operator who does not set `workspace_root` sees no difference in any path, in behaviour or in on-disk layout.

**Why this priority**: The feature must not regress or disturb existing beds. This is a hard compatibility constraint.

**Independent Test**: Run the existing suites with `workspace_root` unset. Additionally, compare the set of resolved workspace paths against the previous hard-coded layout.

**Acceptance Scenarios**:

1. **Given** `workspace_root` is unset (flag, env var and config all absent), **When** any workspace path is resolved, **Then** it equals the path used before this change.
2. **Given** an existing bed with worktrees in `<bed>/.fabrik/worktrees/`, **When** the new binary starts with no setting, **Then** those worktrees are found and used exactly as before.

---

### User Story 3 - Every tool agrees on where the workspace is (Priority: P1)

One single place decides the root, so the daemon, CLI, TUI, watch view, resume command, janitor, session helpers and log helpers can never disagree.

**Why this priority**: Today the root is rebuilt independently at many sites. A single missed site would silently write to, or read from, the wrong location and orphan work.

**Independent Test**: A static check fails when any production code outside the resolver builds a workspace path by hand. A deliberate violation is demonstrated to fail it.

**Acceptance Scenarios**:

1. **Given** all current path-building sites, **When** the static check runs, **Then** it passes only if every one obtains its path from the resolver.
2. **Given** a new hand-built workspace path is added to production code, **When** the static check runs, **Then** it fails and names the offending site.

---

### User Story 4 - Worktrees outside the bed still get a working `.env` (Priority: P2)

When a worktree lives outside the bed, its `.env` link must resolve to the bed's `.env`. If a link already exists but is broken, for example after the root changed or the bed moved, it is repaired rather than skipped.

**Why this priority**: Stage workers rely on the `.env` link. A relative link computed for an in-bed worktree would dangle outside the bed, and today an existing broken link is never repaired.

**Independent Test**: Create a worktree under a root outside the bed and run the link step. Assert the link resolves. Then break the link and run it again. Assert it is repaired.

**Acceptance Scenarios**:

1. **Given** the worktree root is outside the bed, **When** the `.env` link is created, **Then** it targets the bed's `.env` and resolves to a readable file.
2. **Given** an existing `.env` link in a worktree whose target does not exist, **When** the link step runs, **Then** the link is replaced with a working one.
3. **Given** a real (non-link) `.env` file in a worktree, **When** the link step runs, **Then** it is left untouched.

---

### User Story 5 - Moved worktrees are matched, kept and reaped correctly (Priority: P2)

Components that match worktree paths (the janitor, session migration, issue-to-worktree lookup, the cwd-based process reaper, and Pruefer's worktree handling) treat worktrees under a relocated root, including a root reached through a symlink, as the same worktrees. Active worktrees are never orphaned or wrongly reaped.

**Why this priority**: Path-comparison mismatches (symlinked volumes, `/var` vs `/private/var` on macOS, an external-volume mount path) would otherwise make cleanup skip real worktrees or kill unrelated processes.

**Independent Test**: Run each matcher against a worktree root that is reached through a symlink, and against one reached through its resolved path. Assert identical decisions.

**Acceptance Scenarios**:

1. **Given** a worktree under a symlinked root, **When** the janitor runs, **Then** it recognises the worktree as managed and applies its normal retention rules.
2. **Given** a process whose working directory is inside a moved worktree, **When** the reaper evaluates it, **Then** it is attributed to that worktree.
3. **Given** a process outside any Fabrik worktree, **When** the reaper evaluates it, **Then** it is not reaped.

---

### User Story 6 - Changing the root cannot orphan live work (Priority: P2)

An operator changes `workspace_root` on a bed that already has active worktrees. Fabrik refuses to start and tells them exactly what to do. It never silently abandons work in the old location.

**Why this priority**: A silent switch would strand in-flight worktrees (unpushed commits, partial work) in a directory Fabrik no longer looks at.

**Independent Test**: Create a bed with live worktrees under the old root, change the setting, and start the daemon. Assert it refuses with an actionable message and has changed nothing on disk.

**Acceptance Scenarios**:

1. **Given** the old root contains active worktrees and the configured root differs, **When** the daemon starts, **Then** it exits with an error naming the old root and the active worktrees, and what to do about it. No file or setting is modified.
2. **Given** the old root contains no active worktrees, **When** the root is changed, **Then** startup proceeds and the new root is used.
3. **Given** the same setting value as the previous run, **When** the daemon restarts, **Then** the check passes without any message.

---

### Edge Cases

- The configured root does not exist: startup fails with a message naming the path. Fabrik never creates the root itself (see the Assumptions); it creates only the subdirectories inside an existing root. If the root is not a directory or is not writable, startup fails the same way.
- The configured root is a symlink, or lies inside a symlinked directory: behaviour is identical to using the resolved path (see User Story 5).
- The configured root is a relative path: it is interpreted relative to the bed directory, not the process's current directory.
- Two beds configured with the same root would collide on `worktrees/<owner>-<repo>/issue-N`. Each bed needs its own root; the documentation says so.
- The bed's `.env` is itself a symlink or lives on another volume: the absolute link target is still correct.
- An external volume is unmounted or unavailable at startup: startup fails clearly instead of silently recreating the workspace on the system disk.
- The root is changed back to unset after having been set: the same refusal rules apply in reverse, with the old root now the external location.
- The user's home or bed path contains symlinks (for example `/var` vs `/private/var` on macOS): path matching still works.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: A `workspace_root` setting is accepted through the same layers as other daemon settings (command-line flag, then environment variable, then `config.yaml`), and the documented precedence applies. When it is unset in all layers, the workspace is `<bed>/.fabrik/` and every path and behaviour is identical to today.
- **FR-002**: When `workspace_root` is set, the following are located under it: per-issue worktrees, bare repository clones, session files, logs, debug output, and the per-issue session sidecars that live beside the sessions. Worktrees and clones are always kept together under the same root, so `git worktree` operations stay on one filesystem.
- **FR-003**: When `workspace_root` is set, `config.yaml`, `.env`, `fabrik.lock` and the local API socket under `state/` stay in the bed, so the CLI, TUI and tools still find a running daemon through them.
- **FR-004**: Exactly one resolver decides workspace paths. Every production site that needs a workspace path (the engine, resume, the TUI, the watch view, the session helpers, the log helpers, and any other) obtains it from that resolver.
- **FR-005**: A test fails if production code builds a workspace path outside the resolver. The test is shown to fail against a deliberate violation and to pass against the compliant tree.
- **FR-006**: The `.env` link in a worktree is created with an absolute target when the worktree root is outside the bed, and resolves to the bed's `.env`.
- **FR-007**: An existing `.env` link whose target is missing is detected and repaired. A real (non-link) `.env` file in a worktree is never overwritten.
- **FR-008**: Every component that matches or compares worktree paths gives the same answer for a path whether it is reached through a symlink or through its resolved path. This covers the janitor, session migration, issue-to-worktree lookup, the cwd-based process reaper, and Pruefer's worktree handling.
- **FR-009**: Changing `workspace_root` while the previous root still holds active worktrees causes startup to refuse with an error that names the old root and the worktrees, and that states how to proceed. Nothing is moved, deleted or rewritten by this refusal. When the previous root holds no active worktrees, the change is accepted.
- **FR-010**: The behaviour in FR-009 is documented in `docs/USER_GUIDE.md`, along with `workspace_root` itself, what moves and what stays in the bed, and the one-bed-per-root rule. The decision is recorded in an ADR.
- **FR-011**: An unusable configured root (cannot be created, not writable, not a directory) fails startup with a message naming the path. It never silently falls back to the in-bed location.
- **FR-012**: The setting is treated as a daemon-only operational value. It is not forwarded to stage workers, and its environment variable is classified the same way as other engine-read `FABRIK_*` variables.

### Key Entities

- **Bed**: The directory in which `fabrik` runs and which holds `.fabrik/config.yaml`, `.env`, and the daemon's lock and socket. It always stays where it is.
- **Workspace root**: The directory under which worktrees, clones, sessions (with their sidecars), logs and debug output live. It defaults to `<bed>/.fabrik/` and may be set elsewhere.
- **Workspace resolver**: The single function that maps a workspace subdirectory name to a path under the workspace root.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With `workspace_root` unset, 100% of workspace paths resolved equal the pre-change paths, and the existing test suite passes without modification of its expectations.
- **SC-002**: With `workspace_root` set, after processing an issue through a stage, zero worktree, clone, session, log or debug files exist under `<bed>/.fabrik/` that were created by that run, and the lock and socket are present in the bed.
- **SC-003**: A static check shows zero production sites building a workspace path outside the resolver, and it fails when one is introduced.
- **SC-004**: A worktree outside the bed has a `.env` that resolves to a readable file, including after a deliberately broken link is repaired.
- **SC-005**: The janitor and the reaper give identical decisions for symlinked and resolved forms of the same worktree path, with no orphaned worktree and no unrelated process reaped.
- **SC-006**: Changing the root with active worktrees under the old one leaves the old worktrees untouched, and the operator receives a message naming the old root and the next step.
- **SC-007**: Each new check is shown non-vacuous (it fails when its protection is removed). `go test -race ./...` and `go vet ./...` pass.

## Assumptions

- Precedence for the setting follows the layering used for other daemon settings: flag, then `FABRIK_`-prefixed environment variable, then `config.yaml`. The flag and variable are named `--workspace-root` and `FABRIK_WORKSPACE_ROOT`.
- A relative value is resolved against the bed directory, and a leading `~` is expanded.
- The root directory must already exist; Fabrik never creates it, with or without its parents. Creating it would silently recreate the workspace on the system disk when the volume is not mounted, including when the root is itself the mount point. Fabrik creates only the `worktrees`, `repos`, `sessions`, `logs` and `debug` directories inside it. This supersedes the earlier assumption that a missing root is created (ADR 2470, decision 7).
- **Migration policy (R6): refuse to start rather than move worktrees.** Refusing is lossless, reversible, and requires no git surgery on in-flight work. An automatic `git worktree move` is more convenient but riskier, since it touches live branches and links. The error message tells the operator how to proceed (let the work finish, or clean up the old worktrees, then restart). Automatic move can be a later feature.
- "Active worktrees" means worktrees of items still in flight, not leftovers already eligible for the janitor.
- "Stage state" under the root means only the per-issue session sidecars (`.session`, `.session.meta`, `.resumefails`, `.dispatches`) that live in `sessions/`. It does not include decision-state files the daemon itself reads at startup from `state/`, which stay where they are (see Out of Scope).
- Pruefer is only in scope for how it matches worktree paths. Whether Pruefer gets its own `workspace_root` setting is not part of this issue.
- Each bed uses its own workspace root. Detecting two beds sharing one root is not required.

## Out of Scope

- Moving `state/` (lock, local API socket, decision state). The reporter suggests a separate option if that is ever needed.
- Creating, mounting, sizing or excluding the volume itself.
- Automatically moving existing worktrees to a new root (`git worktree move`); a possible follow-up.
- Detecting two beds that share the same workspace root.
- A new setting for Pruefer's own workspace location.

## Source References

- Community report #2070 (human-owned thread; mentioned for context only).
- ADR 063 (cwd-based process reaper), relevant to FR-008.
- `docs/USER_GUIDE.md` configuration-precedence sections (for example the `--claude-config-dir` and GHES settings), as the model for FR-001.
