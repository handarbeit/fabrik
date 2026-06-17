---
layout: docs
title: Stage Lifecycle
---

# Fabrik Stage Lifecycle

This document describes what the Fabrik engine does before, during, and after each stage invocation, including comment processing. It is intended as a reference for writing and refining stage skills.

---

## Pipeline Overview

```
Backlog → Specify → Research → Plan → Implement → Review → Validate → Done
```

| Stage | Order | Read-Only | PostToPR | CreateDraftPR | MarkPRReady | MaxTurns |
|-------|-------|-----------|----------|---------------|-------------|----------|
| Specify | 0 | Yes | No | No | No | 50 |
| Research | 1 | Yes | No | No | No | 50 |
| Plan | 2 | Yes | No | No | No | 50 |
| Implement | 3 | No | Yes | Yes | Yes | 50 |
| Review | 4 | No | Yes | No | Yes | 50 |
| Validate | 5 | No | Yes | No | No | 50 |
| Done | 99 | N/A | No | No | No | N/A |

---

## Phase 1: Item Qualification (Poll Loop)

### Two-Phase Filtering

1. **Shallow pre-filter** (`itemMayNeedWork`): Uses board data only (no comments). Checks stage exists, `updatedAt` changed, not paused (unless awaiting-input), not locked by another user. Does NOT filter on completion labels — completed items may have new comments.

2. **Deep fetch** (`FetchItemDetails`): Only for items that pass the shallow filter. Fetches comments and linked PR `updatedAt` from GitHub GraphQL (~2 points each).

3. **Full check** (`itemNeedsWork`): With comments loaded. New comments trigger processing even on completed stages. PRs only support comment processing. Awaiting-input items only pass if new comments exist (the resume trigger).

### Rate Limit Cost

Shallow query: ~16 points/poll. Deep fetch: ~2 points per active item. Typical poll: ~20-30 points, well within the 5,000/hour GraphQL limit.

---

## Phase 2: Pre-Stage Setup

### Lock & Label Acquisition

- `fabrik:locked:<user>` — prevents other instances from picking up the issue
- `stage:<name>:in_progress` — signals active work on the board
- Both held through cooldown retries (not released until completion or permanent failure)

### Worktree Setup

Each issue gets `.fabrik/worktrees/issue-<N>` on branch `fabrik/issue-<N>`:
- **First run**: Created from `origin/main`, rebased onto latest
- **Retry** (`attempted=true`): Returned as-is — no rebase, preserves Claude's context
- **Rebase conflicts**: Silently aborted (`git rebase --abort`) — Claude works from current base

#### Dependency Install Responsibility Split

The engine's `updateWorktreeFromMain` rebases the worktree onto main but does not run any dependency install. The Review and Validate skills are responsible for prompting Claude to run the project's install step after the rebase step completes. The project's `CLAUDE.md` is the authoritative source for the install command. This split keeps Fabrik package-manager-agnostic. See `.fabrik/plugin/skills/fabrik-validate/SKILL.md` and `.fabrik/plugin/skills/fabrik-review/SKILL.md` for the skill-side instruction.

### Read-Only Stage Stashing

For `read_only: true` stages (Specify, Research, Plan): dirty state is auto-stashed before Claude runs and restored after. Claude sees a clean worktree.

### Context Files

Before each Claude invocation, the engine writes context documents to `.fabrik-context/` in the worktree. These files are excluded from git by two mechanisms: a `.gitignore` file written inside `.fabrik-context/` that excludes all files in the directory, and a pre-rebase step that runs `git rm -rf --cached .fabrik-context/` to remove any accidentally tracked context files before rebasing.

| File | Content |
|------|---------|
| `.fabrik-context/issue.md` | The issue body (spec) — always written |
| `.fabrik-context/stage-Specify.md` | Specify stage comment output |
| `.fabrik-context/stage-Research.md` | Research stage comment output |
| `.fabrik-context/stage-Plan.md` | Plan stage comment output |
| `.fabrik-context/stage-Implement.md` | Implement stage comment output |
| `.fabrik-context/stage-Review.md` | Review stage comment output |
| `.fabrik-context/pr-description.md` | Linked PR description (for `post_to_pr` stages) |
| `.fabrik-context/codebase-changes.md` | Files changed on `origin/<baseBranch>` since the prior stage ran (omitted on first stage or when no changes) |

**Stage invocation**: Writes only stages *prior* to the current stage. Implement sees Specify, Research, Plan but not its own output.

**Comment processing**: Writes prior stages *and* the current stage. Claude needs to see the current stage output to build upon it.

### Session Resume

Session file: `~/.fabrik/sessions/issue-<N>/<stageName>.session`. On retry, loaded via `--resume` to restore conversation context.

### Model Override

Labels matching `model:<name>` on the issue override the stage's configured model.

### Worktree Boundary Enforcement

For non-read-only, non-unrestricted stages (Implement, Review, Validate, and any custom stage with `read_only: false`), the engine replaces the bare `Edit` and `Write` entries in `--allowedTools` with path-scoped variants:

```
--allowedTools Edit(<workDir>/**)
--allowedTools Write(<workDir>/**)
```

This proactively restricts Claude Code's file-editing tools to the assigned worktree directory. If Claude attempts to edit or write a file outside the worktree, it receives an error from Claude Code and the stage continues running (the attempt is blocked, not the whole stage).

**Scope:** Enforced for all stages where `read_only: false`. Skipped for read-only stages (Specify, Research, Plan by default) — they do not write files and receive bare `Edit`/`Write` entries (or none, if `allowed_tools:` is overridden in stage YAML).

**Bypass:** When `fabrik:unrestricted` is present on the issue, `--dangerously-skip-permissions` is passed instead of `--allowedTools`, bypassing this restriction entirely (consistent with the existing semantics of that label).

**Known gap:** `Bash` shell commands that write files (e.g., `cat > /other/path`) cannot be path-restricted at the tool-permission layer — `Bash(cmd:*)` restricts command name, not argument paths. The post-run boundary audit (Phase 3) covers the primary remaining attack surface.

---

## Phase 2.5: Pre-Implement Step (Implement Stage Only)

Before the Claude invocation on every Implement dispatch, the engine calls `preImplement()` (`engine/spawn.go`). For most issues this is an instant no-op; for issues whose Plan stage output contains `FABRIK_SPAWN_CHILD_BEGIN/END` blocks, it performs the GitHub mutations that create child issues and link them as `blockedBy` dependencies of the parent.

### Inputs

- **Plan stage comment body** — read via `findStageComment(item.Comments, "Plan")`. If no Plan comment exists, `preImplement` returns immediately (no-op).
- **`FABRIK_SPAWN_CHILD_BEGIN/END` blocks** in the Plan comment — structured declarations of child issues to create:
  ```
  FABRIK_SPAWN_CHILD_BEGIN owner/repo
  TITLE: <single-line title>

  <scoped spec body>
  FABRIK_SPAWN_CHILD_END
  ```
  Parsed by `ParseSpawnBlocks()`. If no blocks are found, `preImplement` returns immediately.
- **`fabrik:children-spawned` label** — idempotency guard. If present on the parent issue, `preImplement` returns immediately without making any mutations.

### Flow (when spawn blocks are present and guard label is absent)

1. **Repo validation**: Call `ensureRepoReady(owner, repo)` for each unique target repo across all blocks. If any repo is not in Fabrik's managed set (clone fails), post an error comment listing the unmanaged repos, add `fabrik:paused`, and stop — no children are created.
2. **Per-child mutations** (for each block in document order):
   - `CreateIssue(owner, repo, title, body)` — REST `POST /repos/{owner}/{repo}/issues`; body = block body + engine-appended back-reference footer
   - `AddProjectV2ItemById(board.ProjectID, childNodeID)` — adds child to the same project board; returns `childItemID`
   - `AddBlockedByIssue(parent.NodeID, childNodeID)` — links child as a `blockedBy` dependency of the parent
   - `AddLabelToIssue(childNodeID, "fabrik:sub-issue")` — informational; no engine semantics
   - `UpdateProjectItemStatus(board.ProjectID, childItemID, sf.FieldID, specifyOptionID)` — moves child to the `Specify` column (or first non-Backlog, non-terminal column as fallback). **Non-fatal**: if `e.statusField` is nil or no viable column exists, child lands in Backlog and a warning is logged.
   - Conditional `AddLabelToIssue` for `fabrik:yolo` if the parent has `fabrik:yolo`; conditional `AddLabelToIssue` for `fabrik:cruise` if the parent has `fabrik:cruise`. Both are **non-fatal**. `base:<branch>` labels are **not** inherited.
   - On any failure in the fatal steps (CreateIssue, AddProjectV2ItemById, AddBlockedByIssue): post error comment naming completed and failed children, add `fabrik:paused` to parent, stop; `fabrik:children-spawned` is NOT added
3. **After all children succeed**: Add `fabrik:children-spawned` label to the parent.

### After spawn

`preImplement` returns `(spawned=true, nil)`. `processItem` returns without invoking Claude. On the next poll cycle, `checkDependencies` sees the new `blockedBy` edges and adds `fabrik:blocked`, gating the parent's Implement until all children close.

### Idempotency and retry

`fabrik:children-spawned` is the durable idempotency guard. If pre-Implement fails after creating some but not all children (partial spawn), it pauses the parent without adding `fabrik:children-spawned`. On retry (after user removes `fabrik:paused`), `preImplement` re-runs all steps from the start — v1 does not skip already-created children. The error comment names the orphaned children so the user knows what to close before re-advancing.

To trigger a fresh spawn (e.g., after Plan is revised), the user must manually remove `fabrik:children-spawned` and close any orphaned children.

### Recursive decomposition

A child issue created by `preImplement` runs the full Fabrik pipeline. If the child's own Plan emits `FABRIK_SPAWN_CHILD_*` blocks, the child's Implement dispatch triggers another `preImplement` — grandchildren are created by the same mechanism. There is no depth limit.

**References:** [ADR-048: Engine-Side Pre-Implement Spawn](../adrs/048-spawn-child-engine-side.md), [State Machine §6.6](state-machine.md#66-pre-implement-spawn-path)

---

## Phase 3: Claude Invocation

### Prompt Construction

When `skill:` is set (recommended), the prompt is a minimal directive:

```
You are operating as the Fabrik <StageName> agent for issue #<N>.
Follow the instructions in the <skill-name> skill exactly.

---
# Issue #N: <title>
URL: <url>

## Spec / Issue Body
<full issue body>

## Labels
<comma-separated>

## Prior Discussion
<all comments>

## New Comments
<unprocessed comments only>

---
Context files are available in .fabrik-context/ in your working directory:
- .fabrik-context/issue.md — the issue body (spec)
- .fabrik-context/stage-{Name}.md — output from prior stages
- .fabrik-context/pr-description.md — the linked PR description (if applicable)

When you have completed all work for this stage, end your response with:
FABRIK_STAGE_COMPLETE

If you have unresolved questions that must be answered before the stage can proceed:
FABRIK_BLOCKED_ON_INPUT

These two markers are mutually exclusive.
```

### Comment Review Prompt

When `comment_skill:` is set, comment processing uses a similar directive:

```
You are operating as the Fabrik <StageName> comment reviewer for issue #<N>.
Follow the instructions in the <comment-skill> skill exactly.

---
# Issue #N: <title>
URL: <url>

## New Comments to Process
<each comment with author, timestamp, body>

---
Context files are available in .fabrik-context/
...
```

### Claude Arguments

```
--plugin-dir <absolute-path-to-.fabrik/plugin>
--output-format json
--verbose
--resume <sessionID>          (if retry)
--model <override>            (if label or stage config)
--max-turns <N>               (if configured)
--allowedTools <tool> ...     (if restricted)
```

### Output Logging

Raw Claude output saved to `~/.fabrik/logs/issue-<N>/<stage>-output-<timestamp>.json` after every invocation. Viewable through the TUI's `l` key (piped through `fabrik _stream-filter` for human-readable display).

### Turn Progress Emission

During each Claude invocation, the engine counts logical turns in real time via a `turnCountingWriter` wrapping the stdout pipe. Each time a `{"type":"user"}` NDJSON line is detected (one per logical turn — the initial prompt or a tool-result round), the writer increments a per-invocation counter and fires the `claudeTurnProgress` callback (set during engine construction), which emits a `TurnProgressEvent` to the TUI channel. The event carries:

- `IssueNumber` — the issue being processed
- `TurnsUsed` — the current per-invocation logical-turn count
- `MaxTurns` — the effective budget for this invocation (accounts for `opts.MaxTurnsOverride` from the extension loop)

This is a purely additive display mechanism — it does not affect Claude's execution, the output buffer, or any engine state. In plain-text mode and tests, `claudeTurnProgress` is nil and no events are emitted.

### Subprocess Cleanup

After `cmd.Run()` returns, two cleanup steps run unconditionally:

1. **Kill escalation and the wrapper contract**: When the engine needs to stop a Claude invocation (max_wall_time, inactivity timeout, daemon shutdown, or supplant-by-new-invocation), it uses a three-signal escalation sequence rather than an immediate SIGKILL:

   ```
   SIGINT  → sleep(sigintGrace)  → liveness probe
   SIGTERM → sleep(sigtermGrace) → liveness probe
   SIGKILL
   ```

   Claude is started in its own process group (`Setpgid: true` on Unix). All three signals are sent to the entire process group via `syscall.Kill(-pgid, sig)`.

   Each step is logged as `[#N kill] sending SIG<X> to PGID <pid> (reason=<reason>)`. Reason codes: `max_wall_time`, `inactivity_timeout`, `daemon_shutdown`, `supplant_by_new_invocation`, `context_cancel`.

   A step is skipped if the grace duration is zero (via per-stage `kill_grace:` with `"0s"`). The liveness probe (`kill -0`) short-circuits the remaining sequence if the process group is already empty.

   **Wrapper contract**: The SIGINT grace window exists specifically so that shell wrappers spawned by stage skills (e.g. a CI test runner that posts a final Commit Status on interrupt) can catch SIGINT, complete their cleanup, and exit normally before SIGTERM arrives. The default SIGINT grace is 10 seconds. Wrappers must be designed to finish their cleanup within this window.

   **Grace window configuration**: Engine-wide defaults are set via `--kill-grace-sigint` / `--kill-grace-sigterm` flags (or `FABRIK_KILL_GRACE_SIGINT` / `FABRIK_KILL_GRACE_SIGTERM` env vars, default 10s each). Per-stage overrides are expressed in stage YAML (`kill_grace:`); an omitted field inherits the engine default; `"0s"` skips that signal step entirely.

   After `cmd.Wait()` returns, a second unconditional `SIGKILL` is sent to the process group to clean up any grandchildren (e.g. `tail -f` from the Monitor tool) that survived the Claude subprocess exit. This grandchild cleanup fires regardless of how the main subprocess exited.

2. **WaitDelay bound**: `cmd.WaitDelay` is set to 30s (configurable via `--claude-wait-delay` / `FABRIK_CLAUDE_WAIT_DELAY`). When grandchild processes hold the stdout pipe open after Claude exits, Go's `cmd.Wait()` would otherwise block indefinitely. With `WaitDelay`, Go forcibly closes its end of the pipe after the deadline and returns `exec.ErrWaitDelay`. The engine detects this error, logs a diagnostic warning, clears the error, and processes the buffered output normally — including any `FABRIK_STAGE_COMPLETE` marker. This prevents the worker goroutine from being permanently stuck when Claude uses `run_in_background` or the Monitor tool.

### Progress Baseline Snapshot

Immediately before the first invocation, `snapshotBaseline` captures observable progress state for this stage:

| Stage | Baseline fields captured |
|-------|--------------------------|
| **Implement** | `gitHeadSHA` — `git rev-parse HEAD` in the worktree |
| **Review** | `gitHeadSHA` + `resolvedThreadCount` — `LinkedPRResolvedThreadCount` from the poll cycle's `FetchItemDetails` |
| **Validate** | `commentCount` — `len(item.Comments)` from the poll cycle's `FetchItemDetails` |
| **All others** | (empty — no extension possible) |

The baseline is purely in-memory; it is lost on engine restart (an acceptable risk per ADR 030).

### Turn-Limit Extension Loop

The `e.claude.Invoke()` call runs inside an extension loop. On each iteration:

1. `opts.MaxTurnsOverride` is set to `currentBudget` (first iteration: `stage.MaxTurns`, or `2 × stage.MaxTurns` if `fabrik:extend-turns` is present).
2. Claude is invoked. Output is appended to `totalOutput`; usage is accumulated into `totalUsage`.
3. Turn-limit check: `!completed && err == nil && stage.MaxTurns > 0 && invUsage.TurnsUsed >= currentBudget`.
4. If turn limit was NOT hit (or stage completed), exit the loop.
5. If `totalMultiple >= 3` (hard cap), exit the loop (fail as turn-limit).
6. Call `detectProgress`. If progress → `totalMultiple++`, set `currentBudget = stage.MaxTurns`, set `resume = true`, log `[#N extend-turns]`, loop.
7. If no progress or progress check fails → exit the loop (fail as turn-limit).

**`detectProgress` per stage:**
- **Implement**: `git rev-parse HEAD` in worktree; progress if SHA changed.
- **Review**: `git rev-parse HEAD`; if SHA same → `FetchItemDetails` re-fetch; progress if `LinkedPRResolvedThreadCount` increased. One GraphQL call only when no new commits.
- **Validate**: `FetchItemDetails` re-fetch; progress if `len(Comments)` increased. One GraphQL call per check.
- **All others**: return `false` immediately.

**Output accumulation:** Each `--resume` invocation produces only the delta output for that session continuation. The engine concatenates all invocations' output before posting. The empty-output check (`strings.TrimSpace(output) == ""`) applies to the accumulated total.

**Deferred WIP commit and push:** The `commitWIP` and `PushBranch` calls happen AFTER the extension loop completes, not between invocations. This preserves worktree state across extensions.

**Stats footer:** After the loop, `usage.MaxTurns` is set to `totalMultiple × stage.MaxTurns`, so the stats line reflects the total budget (e.g., `used 130/150 turns`).

### Post-Run Boundary Audit

After the extension loop completes, a cross-repo ref audit runs for non-read-only, non-unrestricted stages **when `e.cfg.WorktreeBoundaryAudit` is `true`** (default: `false`). The audit detects git-layer mutations in any repository other than the active worktree's own repo.

> **Default off (pending #808):** The audit is disabled by default because routine `git fetch origin` in sibling bare clones produces false-positive violations. Enable it with `worktree_boundary_audit: true` in `.fabrik/config.yaml`, `--worktree-boundary-audit` on the CLI, or `FABRIK_WORKTREE_BOUNDARY_AUDIT=true`.

**How it works:**

1. **Pre-audit snapshot** (taken immediately before the extension loop): For each registered `WorktreeManager` in the engine, run `git for-each-ref --format=%(refname) %(objectname) refs/heads/ refs/tags/` in its bare-clone directory. Capture `repo → (refname → SHA)` for locally-authored refs only. `refs/remotes/` is intentionally excluded — remote-tracking refs are passively-observed upstream state updated by `git fetch` for reasons unrelated to Claude's activity; including them would cause false-positive violations when a concurrent fetch updates a sibling bare clone. **Skipped entirely when `WorktreeBoundaryAudit` is `false`.**
2. **Post-audit snapshot** (taken immediately after the extension loop): Same operation. Skipped when the pre-audit snapshot was not taken (i.e., `WorktreeBoundaryAudit` is `false`).
3. **Violation check** (`crossRepoViolations`): Compare before/after for every repo key *except* the active issue's repo. Any ref that is new or has a changed SHA is a violation.

**On violation:**
- `[#N audit]` log line names the stage and count of mutations.
- A comment is posted on the issue listing the specific refs mutated (names and SHAs). No automatic cleanup.
- `fabrik:paused` is added so `itemNeedsWork` skips the issue until the user investigates.
- `stage:<name>:failed` label is added. `StageAttempted` is recorded (cooldown applies). `MaxRetries` is NOT consumed — violations require human investigation, not auto-retry.
- `EnginePaused` is recorded in the store so that `clearFailedStage` fires (removing the failed label and resetting state) when the user removes `fabrik:paused`.
- The stage returns without posting output or advancing to the next stage.
- **To retry**: remove `fabrik:paused`. The engine will clear `stage:<name>:failed` and re-run the stage on the next poll.

**No violation:** The audit is silent. Stage proceeds to normal output posting and completion.

**Bypass:** Skipped when `WorktreeBoundaryAudit` is `false` (default), `stage.ReadOnly == true`, or `fabrik:unrestricted` is present on the issue.

**Limitation — Bash shell writes:** The audit checks git refs, not the filesystem. A Claude session that writes files outside the worktree via raw shell commands (e.g., `cat > /other/path`) would not be caught unless those files were also committed and pushed to another repo. The `Edit`/`Write` path restriction (Phase 2) is the primary mitigation for direct file writes.

**Limitation — unregistered repos:** Only repos in `worktreeManagers` at the time of the snapshot are audited. Repos that Claude navigated into but that are not registered in Fabrik's managed set are not detected.

---

## Phase 4: Post-Stage Handling

### Output Parsing

Three JSON formats supported (tried in order):
1. Single result object: `{"result": "...", "session_id": "..."}`
2. JSON array: `[{"type":"system",...}, ..., {"type":"result","result":"..."}]`
3. NDJSON (stream-json): One JSON object per line

Empty result with valid session ID is accepted (max turns hit — Claude was mid-tool-use).

If parsing fails: error message posted instead of raw output. Full output in log files.

### Issue Body Update

Before posting output, checks for `FABRIK_ISSUE_UPDATE_BEGIN`/`END` markers:
- When present, the issue body is updated unconditionally
- By convention, only the Specify stage produces these markers
- Markers are always stripped from output before posting

### Marker Stripping

All Fabrik markers are stripped from output before posting:
- `FABRIK_STAGE_COMPLETE`
- `FABRIK_BLOCKED_ON_INPUT`
- `FABRIK_SUMMARY_BEGIN` / `FABRIK_SUMMARY_END`
- `FABRIK_ISSUE_UPDATE_BEGIN` / `FABRIK_ISSUE_UPDATE_END`

### Output Posting

**If `post_to_pr: true`** (Implement, Review, Validate):
- Detailed output posted on the linked PR
- Brief summary (from `FABRIK_SUMMARY` markers) posted on the issue
- Falls back to issue if no PR found

**Otherwise** (Specify, Research, Plan):
- Full output posted directly on the issue as a stage comment

### Comments Marked as Seen

After a stage runs, any pre-existing user comments get a rocket reaction via `markCommentsSeenByStage`. They were included in the prompt as context and should not trigger the awaiting-input unblock logic on subsequent polls.

### Completion Path

When `FABRIK_STAGE_COMPLETE` is detected (regardless of Claude's exit code — as of v0.0.26, a non-zero exit is treated as a warning, not a failure, when the marker is present):
1. Lock released (`fabrik:locked:<user>` and `stage:<name>:in_progress` removed)
2. Retry tracking cleared
3. Draft PR created (if `create_draft_pr: true`)
4. PR marked ready (if `mark_pr_ready_on_complete: true`)
5. `stage:<name>:complete` label added
6. **Validate only:** `ValidateCompletedAtSHA` mutation applied with the worktree's current `HEAD` SHA (`git rev-parse HEAD`). This records the exact post-commit SHA so the SHA-invalidation scan (`docs/state-machine.md` §2.16) can detect future SHA changes (force-push, external commits) and automatically re-enter Validate. On error (e.g. bare git call fails), the SHA is left empty — the SHA-invalidation scan's FR-5 guard treats empty completion SHA as "do nothing," preserving safe degraded behavior.
7. Auto-advance to next stage (if `auto_advance: true` or global `yolo`)

**Validate + yolo**: At Validate completion, if the issue carries `fabrik:yolo` (and not `fabrik:cruise`), Fabrik calls `enablePullRequestAutoMerge` on the linked PR and applies `fabrik:auto-merge-enabled` rather than calling `MergePR` directly. GitHub then merges the PR atomically once branch-protection requirements are satisfied. The post-Validate convergence monitor (`checkAutoMergeConvergence`) tracks the PR in subsequent poll cycles until it reaches a terminal state or the convergence budget expires. See `docs/state-machine.md` §5.4–5.5 for full details.

Note: `fabrik:extend-turns` is **not** removed here. It persists across all intermediate stages and is removed only during the Done stage's cleanup path (see Cleanup Stage below).

### Blocked-on-Input Path

When `FABRIK_BLOCKED_ON_INPUT` is detected (and Claude ran without error):
1. `fabrik:paused` + `fabrik:awaiting-input` labels added
2. Lock released
3. Retry count NOT incremented, no `stage:<name>:failed` label
4. Issue waits until user comments (auto-detected, see Comment Processing)

### Incomplete Path (No Marker)

1. Partial-progress commit (unless read-only): `git add -A && git commit -m "chore: partial <StageName> stage progress (incomplete)"`
2. Branch pushed
3. Cooldown timer: `pollSeconds * 10` seconds
4. Lock held through cooldown
5. Retry count incremented; after `max_retries`: `fabrik:paused` + `stage:<name>:failed`, lock released

### Branch Pushing

Always pushed after Claude runs (success or failure): `git push --force-with-lease -u origin fabrik/issue-<N>`

---

## Phase 5: Comment Processing

Comment processing is triggered when new comments from the configured user are found. It runs independently of stage processing — even completed stages can process new comments.

### Comment Detection

A comment is "new" if it:
- Is authored by the configured user
- Is not in the in-memory processed set
- Doesn't start with `🏭 **Fabrik` (skip Fabrik's own output)
- Doesn't have a ROCKET reaction (durable "processed" marker)

### Comment Processing Flow

1. **Eyes reaction** added to all new comments
2. **`fabrik:editing` label** added
3. **Worktree prepared** (fresh rebase, not a retry)
4. **Context files written** (prior stages + current stage)
5. **Claude invoked** with `comment_skill` (or default comment prompt)
   - Always resumes existing session
6. **Output processed**:
   - `FABRIK_ISSUE_UPDATE` markers: applied unconditionally when present, then stripped
   - All Fabrik markers stripped
   - Stage comment rewritten (or created) via `findStageComment` + `UpdateComment`
   - Exception: `post_to_pr` stages post a new "(comment review)" comment on the issue
7. **`fabrik:editing` label** removed
8. **Rocket reaction** added to processed comments
9. **Completion check**: If `FABRIK_STAGE_COMPLETE` was in the output, `handleStageComplete` fires — the stage completes directly from comment processing without needing an extra stage invocation

### Awaiting-Input Auto-Resume

When a user comments on an issue with `fabrik:paused` + `fabrik:awaiting-input`:
1. `itemMayNeedWork` lets it through (special exception for awaiting-input)
2. `itemNeedsWork` checks `findNewComments` — returns true only if new comments exist
3. `processItem` calls `unblockAwaitingInput` → removes both labels, clears cooldown
4. Routes to `processComments` with the new comments
5. Comment processing can signal `FABRIK_STAGE_COMPLETE` to complete the stage immediately

### Stage Comment Rewriting

For non-`post_to_pr` stages, comment processing rewrites the existing stage comment:
- `findStageComment` scans for the most recent comment matching `🏭 **Fabrik — stage: {Name}**`
- If found: `UpdateComment` replaces its body
- If not found: `AddComment` creates a new stage comment

For `post_to_pr` stages, comment processing posts a new "(comment review)" comment on the issue (not the PR).

### Key Differences: Stage Run vs Comment Processing

| Aspect | Stage Run | Comment Processing |
|--------|-----------|-------------------|
| Session | Fresh or resume on retry | Always resume |
| Worktree update | Skip on retry | Always rebase |
| Completion | Checked, honored | Checked, honored |
| Blocked-on-input | Checked, honored | Not checked |
| Issue body update | When markers present | When markers present |
| Output destination | Stage comment or PR | Rewrite stage comment or new issue comment |
| Lock | `fabrik:locked:<user>` | `fabrik:editing` |
| Reaction flow | Comments marked seen (rocket) | Eyes → editing → rocket |

---

## Phase 6: Cleanup Stage (Done)

The Done stage (`cleanup_worktree: true`) is terminal:
- No Claude invocation, no lock, no in-progress label management
- Skipped entirely if `stage:Done:complete` is already present
- Removes worktree directory when it exists (for non-PR items)
- Adds `stage:Done:complete` label
- Removes `fabrik:extend-turns` label if present (this is the designated removal site; the label is not removed during any earlier stage completion)
- Respects `fabrik:paused` (skips if paused)

---

## Markers Reference

| Marker | Direction | Purpose | Where Checked |
|--------|-----------|---------|---------------|
| `FABRIK_STAGE_COMPLETE` | Claude -> Engine | Stage finished successfully; honored even on non-zero Claude exit (v0.0.26+) — engine logs a warning | Stage runs AND comment processing |
| `FABRIK_BLOCKED_ON_INPUT` | Claude -> Engine | Stage needs user input | Stage runs only |
| `FABRIK_ISSUE_UPDATE_BEGIN/END` | Claude -> Engine | Updated issue body | Stage runs AND comment processing |
| `FABRIK_SUMMARY_BEGIN/END` | Claude -> Engine | Brief summary for issue | Stage runs with `post_to_pr: true` |

## Labels Reference

| Label | Set by | Purpose |
|-------|--------|---------|
| `fabrik:locked:<user>` | Engine | Lock during stage processing |
| `fabrik:editing` | Engine | Lock during comment processing |
| `fabrik:paused` | Engine or User | Pause processing |
| `fabrik:awaiting-input` | Engine | Paused waiting for user comment (auto-resumes) |
| `stage:<name>:in_progress` | Engine | Stage actively running |
| `stage:<name>:complete` | Engine | Stage completed successfully |
| `stage:<name>:failed` | Engine | Stage hit max retries |
| `model:<name>` | User | Override Claude model |

## Stage YAML Options

```yaml
name: Research              # Required: matches board column name
order: 2                    # Required: processing priority (lower = earlier)
skill: fabrik-research      # Plugin skill name (recommended)
comment_skill: fabrik-research-comment  # Plugin skill for comment processing
prompt: |                   # Inline prompt (legacy, used when skill not set)
  ...
comment_prompt: |           # Inline comment prompt (legacy)
  ...
model: sonnet               # Optional: Claude model
max_turns: 50               # Optional: turn limit per invocation
comment_max_turns: 15       # Optional: max turns for comment review (default: min(max_turns, 15))
allowed_tools:              # Optional: restrict Claude's tools
  - Read
  - Grep
read_only: false            # Stash/restore worktree (for analysis stages)
post_to_pr: false           # Route output to linked PR
create_draft_pr: false      # Create draft PR on completion
mark_pr_ready_on_complete: false  # Mark PR ready on completion
auto_advance: null          # Override global yolo (true/false/null)
cleanup_worktree: false     # Terminal stage — remove worktree
kill_grace:
  sigint: 10s               # Grace window after SIGINT before SIGTERM (empty = engine default; "0s" = skip SIGINT)
  sigterm: 10s              # Grace window after SIGTERM before SIGKILL (empty = engine default; "0s" = skip SIGTERM)
completion:
  type: claude              # Only supported type
```

Either `skill` or `prompt` is required (unless `cleanup_worktree` is true). When `skill` is set, the engine sends a directive prompt and the skill is loaded via `--plugin-dir`.
