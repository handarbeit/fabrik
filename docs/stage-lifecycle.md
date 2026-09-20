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

Session file: `.fabrik/sessions/issue-<N>/<stageName>.session`, relative to the process working directory — or `.fabrik/sessions/<owner>-<repo>/issue-<N>/<stageName>.session` on multi-repo projects (`engine/claude.go:227-242`). On retry, loaded via `--resume` to restore conversation context.

The session ID is captured even when the invocation ends by hitting `max_turns` — `parseClaudeJSON` accepts a response whose `result` is empty as long as `session_id` is present (`engine/claude.go:1163-1166`), because a turn-cap kill produces exactly that shape. This is deliberate: it is what allows a turn-capped stage to be resumed rather than restarted.

#### Retry after a turn-cap kill

When a stage exceeds `max_turns`, the Claude CLI self-terminates with a non-zero exit, so `completed = false` and `err != nil`. The progress-based extension loop requires `err == nil`, so this falls through to the cooldown/retry path. A later poll re-invokes the same stage with `resume = true` (`engine/item.go:682` — `resume := !lastAttempt.IsZero()`).

As of #1178, the CLI's own result classification (`subtype: "error_max_turns"`) is captured alongside this exit and threaded into `InvocationRecorded.TurnLimited`, so `history.json`/the TUI can render this case as incomplete-and-resumable rather than as a generic failure. This changes only the classification/rendering surfaced from the invocation — the retry/cooldown mechanics described above are unchanged.

**The retry continues the same Claude session.** It is not a fresh agent inheriting a dirty worktree — it retains the killed run's conversation context, including the output of every command that run executed before the cap.

Two consequences follow, and both routinely look like defects to someone reading only the posted comment:

1. **A retry may legitimately report on work performed before the kill.** If the predecessor ran a test suite at turn 12 and was killed at turn 51, the successor can summarise those results without re-running anything. The work was genuinely performed, by the same session — this is continuation, not fabrication.
2. **A retry completing in very few turns is the expected shape, not a warning sign.** The work is already done; the continuation only needs enough turns to write it up. A two-turn completion following a capped predecessor is a resume working correctly.

Conversely, a successor that *announces* the situation (e.g. "found the implementation already complete from a prior interrupted session") is accurately describing its own resumed state.

**Distinguishing a legitimate few-turn resume from a stall.** The prior paragraph describes a *healthy* capped-then-short-retry shape — a resume that only needs a few turns to write up work already done. A *stalled* retry looks superficially similar (short, incomplete) but never completes and, critically, is followed by a *further* decline rather than a completion. §7.10 of `docs/state-machine.md` covers the engine-side detection of that pattern (a turn-capped attempt followed by a strictly-declining, still-incomplete one) and the one-shot corrective hint it arms for the next invocation (#1146). That detection is purely additive to the resume mechanics described here — it does not change `resume`, session-file handling, or output attribution, which is what keeps it clear of the successor-reports-phantom-work problem #1081 fixed.

**Failure modes — the session ID is not always honoured, but the fallback is now logged.** Before building args, `InvokeClaude` and `InvokeClaudeForComments` each call `resolveResumeSessionID(issue.Number, stage.Name, sessFilePath, resume)` (`engine/claude.go`), which reads and classifies the session file via `classifySessionFile` — trimming **before** checking for emptiness, so whitespace-only content is never mistaken for a usable ID. `buildClaudeArgs` itself is a pure formatter: it receives the already-resolved `resumeSessionID string` and appends `--resume <id>` only when it's non-empty.

`classifySessionFile` produces one of four outcomes:

| Outcome | Condition | Behaviour |
|---|---|---|
| `resumeFound` | Trimmed content is non-empty | `--resume <id>` appended; **no log output** (the healthy path stays silent) |
| `resumeAbsent` | Session file does not exist (`os.IsNotExist`) | `--resume` omitted → fresh session; warning logged naming the issue, stage, and path |
| `resumeUnreadable` | Session file exists but `os.ReadFile` errors for another reason (e.g. it's a directory) | `--resume` omitted → fresh session; warning logged, including the underlying error |
| `resumeBlank` | File reads successfully but its trimmed content is empty (zero bytes **or** whitespace-only) | `--resume` omitted → fresh session; warning logged |

Unlike the earlier untrimmed-length-check bug, a whitespace-only file can no longer produce `--resume ""` — it is folded into `resumeBlank` and treated identically to a zero-byte file. Both `InvokeClaude` and `InvokeClaudeForComments` go through the same `resolveResumeSessionID` call, so neither call site can silently regress independently.

`engine.ReadSessionID` (`engine/claude.go`, used by `cmd/resume.go`) now shares the same `classifySessionFile` classifier internally (discarding the status), so it and the invocation path agree on what counts as a usable session ID.

#### Dead-session detection and self-heal

A session file can point at a conversation Claude Code itself has since reaped (the CLI's own `cleanupPeriodDays` retention, default 30 days) — distinct from #1117's load failures above: the file is present and well-formed, but the ID no longer resolves to anything. `--resume <dead-id>` then fails structurally, and the CLI **echoes the requested `session_id` back unchanged** in its error response, so a naive unconditional resave would rewrite the exact same dead pointer every retry, making the failure self-renewing rather than merely stale.

`interpretClaudeResult` (`engine/claude.go`) detects this from the parsed response, requiring both signals together:

- `resp.Subtype == "error_during_execution"` (structural)
- an entry in `resp.Errors` containing `"No conversation found with session ID"` (substring)

On detection, it removes the session file immediately and skips the resave of `resp.SessionID` for that invocation — the one place `saveSessionIDDirect` would otherwise persist the dead ID right back to disk. The next invocation reads no session file, `buildClaudeArgs` omits `--resume`, and a fresh session starts and is saved normally. No retry counter or label is involved: because the specific dead ID is never rewritten, the identical failure cannot recur for that pointer, which bounds the loop without any additional machinery.

This applies uniformly to both the stage-invocation and comment-processing call paths, since both flow through `interpretClaudeResult`.

#### Consecutive resume-failure abandonment (#1414)

The dead-session self-heal above only fires for the one specific error signature it structurally matches (`error_during_execution` + the "No conversation found" substring). Any *other* resume failure — most notably, a session that has simply grown too large for Claude Code to reload — falls through to a generic error and, before #1414, re-saved the identical session ID for the next attempt: every retry re-triggered the same condition and appended to the transcript that was already the cause. The reporter's workaround (moving the 36-byte `.session` pointer aside so the next run started cold) worked precisely because `resolveResumeSessionID` already treats an absent file as a fresh session; this mechanism automates that workaround.

`interpretClaudeResult` maintains a second, durable sidecar file next to the session pointer — `<stageName>.session.resumefails`, a plain-text integer, read/written with the same idiom `saveSessionIDDirect` uses for the session ID itself — counting **consecutive** failures where the invocation itself was a resume attempt (`resumeSessionID != ""`) and none of the more specific classifiers above claimed the failure. The counter is deliberately file-based rather than living in `itemstate.Store` (confirmed entirely in-memory, resetting on every engine restart): this is the one counter that must survive a restart, or a restart would silently re-enable the indefinite-retry loop this mechanism exists to close.

The counter resets to 0 on any evidence the session is healthy — a clean exit, `FABRIK_STAGE_COMPLETE` found despite a trailing error, or a turn-cap exit (real turns and cost consumed is the strongest possible evidence against a poisoned transcript). It is left untouched by a usage-limit or `api_error` exit, since the stage never ran and those outcomes say nothing about session health either way. A cold start's own failure (`resumeSessionID == ""`) also resets it, clearing any stale count left over from a since-abandoned session lineage.

Once the count reaches `MaxResumeFailures` (`--max-resume-failures` / `FABRIK_MAX_RESUME_FAILURES`, default 2), the session file is removed — the same `os.Remove` call the dead-session path above already uses — and the sidecar resets to 0. No new "force cold start" signal is threaded anywhere: the removed file is itself the cold-start signal, since `resolveResumeSessionID` already treats an absent file identically for both `InvokeClaude` and `InvokeClaudeForComments`. Because both invocation paths (and, transitively, `merge_train.go`'s conflict-resolution invocation) compute the identical session file path for a given (issue, stage), the counter and the abandonment are shared across all three — a session poisoned by a long stage run is abandoned for the next comment-review invocation too, and vice versa, and an alternating stage/comment-review failure sequence still reaches the threshold.

Unlike the dead-session self-heal (a structural fault with no ambiguity), a resume failure caught by this mechanism is exempted from `max_retries` accounting entirely — `StageAttempted` is still recorded (so the normal dispatch cooldown applies), but `StageRetryIncremented` never fires for it, for every consecutive resume failure up to the threshold, not only the one that triggers abandonment. This mirrors the `fabrik:claude-limit` precedent and exists so the guaranteed cold-start attempt cannot be starved by the very failures it is counting. Nothing is posted to the issue and no label is applied — only a log line (session id, stage, consecutive-failure count, threshold, last error) — since the mechanism is self-healing by construction; if the cold-started attempt also fails, that failure carries no `--resume` session ID and is therefore a genuine, unexempted failure that flows into the normal `max_retries` → `fabrik:paused` path. See `docs/state-machine.md` §2.17 and ADR-1414 for the full per-outcome classification table and design rationale.

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

- **Plan stage comment body** — read via `findStageComment(item.Comments, "Plan")`. If no Plan comment is found, the outcome depends on the `stage:Plan:complete` label:
  - **Label absent** — true no-op; Plan hasn't run yet. `preImplement` returns immediately.
  - **Label present** — an inconsistency (#982): the label says Plan finished, but the comment the spawn logic reads from is missing from `item.Comments` (a stale deep-field snapshot; see #957). `preImplement` does not silently no-op here — it recovers the true spawn intent via a live, uncached re-read before deciding. See [State Machine §6.7](state-machine.md#67-pre-implement-spawn-path) for the full detect → recover → three-way-outcome flow.
- **`FABRIK_SPAWN_CHILD_BEGIN/END` blocks** in the Plan comment — structured declarations of child issues to create:
  ```
  FABRIK_SPAWN_CHILD_BEGIN owner/repo
  TITLE: <single-line title>
  DEPENDS_ON: <n>                  # optional — forward-only 1-based index into this block list

  <scoped spec body>
  FABRIK_SPAWN_CHILD_END
  ```
  Parsed by `ParseSpawnBlocks()`. If no blocks are found, `preImplement` returns immediately. `DEPENDS_ON:`, when present, must immediately follow `TITLE:` (no blank line between) — see [State Machine §6.7](state-machine.md#67-pre-implement-spawn-path) for the full grammar and validation rules (ADR-1337).
- **`fabrik:children-spawned` label** — idempotency guard. If present on the parent issue, `preImplement` returns immediately without making any mutations.

### Flow (when spawn blocks are present and guard label is absent)

1. **`DEPENDS_ON` validation**: `validateSpawnDependsOn` checks every declared index upfront, before any GitHub mutation — a purely structural forward-reference check (`1 <= DEPENDS_ON < ownIndex`). Any invalid value (out-of-range, non-forward, or malformed) is a hard failure: post an error comment (`Created so far: none`), add `fabrik:paused`, and stop — no children are created.
2. **Repo validation**: Call `ensureRepoReady(owner, repo)` for each unique target repo across all blocks. If any repo is not in Fabrik's managed set (clone fails), post an error comment listing the unmanaged repos, add `fabrik:paused`, and stop — no children are created.
3. **Per-child mutations** (for each block in document order — the block-index → child node-ID mapping is retained for step 4):
   - `CreateIssue(owner, repo, title, body)` — REST `POST /repos/{owner}/{repo}/issues`; body = block body + engine-appended back-reference footer
   - `AddProjectV2ItemById(board.ProjectID, childNodeID)` — adds child to the same project board; returns `childItemID`
   - `AddBlockedByIssue(parent.NodeID, childNodeID)` — links child as a `blockedBy` dependency of the parent
   - `AddLabelToIssue(childNodeID, "fabrik:sub-issue")` — informational; no engine semantics
   - `UpdateProjectItemStatus(board.ProjectID, childItemID, sf.FieldID, specifyOptionID)` — moves child to the `Specify` column (or first non-Backlog, non-terminal column as fallback). **Non-fatal**: if `e.statusField` is nil or no viable column exists, child lands in Backlog and a warning is logged.
   - Conditional `AddLabelToIssue` for `fabrik:yolo` if the parent has `fabrik:yolo`; conditional `AddLabelToIssue` for `fabrik:cruise` if the parent has `fabrik:cruise`. Both are **non-fatal**. `base:<branch>` labels are **not** inherited.
   - On any failure in the fatal steps (CreateIssue, AddProjectV2ItemById, AddBlockedByIssue): post error comment naming completed and failed children, add `fabrik:paused` to parent, stop; `fabrik:children-spawned` is NOT added
4. **Sibling-wiring pass** (after all children from step 3 exist): for each block that declared `DEPENDS_ON`, call `AddBlockedByIssue(childNodeID, blockerChildNodeID)` linking it to the earlier sibling it referenced. On failure: post an error comment listing children created so far, add `fabrik:paused` to parent, stop; `fabrik:children-spawned` is NOT added.
5. **After all children and sibling edges succeed**: Add `fabrik:children-spawned` label to the parent.

### After spawn

`preImplement` returns `(spawned=true, nil)`. `processItem` returns without invoking Claude. On the next poll cycle, `checkDependencies` sees the new `blockedBy` edges — parent edges and any sibling edges alike — and adds `fabrik:blocked`, gating the parent's Implement until all children close. No changes were needed to `checkDependencies` or `PushUnblockObserver` to support this: both already operate generically over `item.BlockedBy` regardless of edge origin.

### Idempotency and retry

`fabrik:children-spawned` is the durable idempotency guard, applied only after both the creation pass (step 3) and the sibling-wiring pass (step 4) succeed. If pre-Implement fails after creating some but not all children, or after creating all children but failing to wire some `DEPENDS_ON` edges, it pauses the parent without adding `fabrik:children-spawned`. On retry (after user removes `fabrik:paused`), `preImplement` re-runs all steps from the start — v1 does not skip already-created children. The error comment names the orphaned children so the user knows what to close before re-advancing.

To trigger a fresh spawn (e.g., after Plan is revised), the user must manually remove `fabrik:children-spawned` and close any orphaned children.

### Recursive decomposition

A child issue created by `preImplement` runs the full Fabrik pipeline. If the child's own Plan emits `FABRIK_SPAWN_CHILD_*` blocks, the child's Implement dispatch triggers another `preImplement` — grandchildren are created by the same mechanism. There is no depth limit.

**References:** [ADR-048: Engine-Side Pre-Implement Spawn](../adrs/048-spawn-child-engine-side.md), [State Machine §6.7](state-machine.md#67-pre-implement-spawn-path)

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
--disallowedTools <tool> ...  (always: ScheduleWakeup, Workflow, Monitor, CronCreate — see below)
--resume <sessionID>          (if retry)
--model <override>            (if label or stage config)
--max-turns <N>               (if configured)
--allowedTools <tool> ...     (if restricted)
--name <sentinel>             (if claude supports --name; probed once at startup)
```

`--disallowedTools` is emitted unconditionally, outside the `--dangerously-skip-permissions`/`--permission-mode dontAsk` branch, so it applies on both invocation paths — it is a construction-time exclusion from the tool schema Claude is offered, not a call-time permission check like `--allowedTools`. `ScheduleWakeup`, `Workflow`, `Monitor`, and `CronCreate` are suppressed because each promises delivery to a future turn, session, or external callback a headless stage cannot receive — the membership test and full sweep (adds and rejects) are recorded in ADR-1365's #1558 amendment (see `disallowedTools` in `engine/claude.go`). `Agent` is deliberately not suppressed — subagents complete within the parent turn.

### Worker Session Naming (`--name`)

Every worker invocation carries a `--name <sentinel>` flag of the form `fabrik:<owner>/<repo>#<issue>:<stage>` (e.g. `fabrik:handarbeit/fabrik#1284:Implement`), built by `sessionNameSentinel` (`engine/claude.go`) from `issue.Repo`, `issue.Number`, and `stage.Name`. This gives every Fabrik worker a self-describing identity in `ps` output — a `ps aux | grep -- "--name fabrik:"` finds every worker on the host, and the sentinel itself names which repo, issue, and stage each one is serving, without fingerprinting incidental flags (`--output-format`, `--plugin-dir`) or recovering `cmd.Dir` via `lsof`/`--add-dir`.

The stage-name component is passed through `sanitizeSentinelComponent`, which collapses any run of whitespace to a single `-` and replaces any `:` or `#` with `-`. The whitespace rule keeps the value a single token, since args are passed as an argv slice (never through a shell) and the sole hard constraint is that naive `ps`-based parsing not break; the `:`/`#` rule keeps a custom stage name (e.g. `Review: Final` or `Review #2`, set via `.fabrik/stages/*.yaml`) from colliding with the sentinel's own `fabrik:<repo>#<issue>:<stage>` field delimiters, which would otherwise make the rendered sentinel ambiguous to split back into fields by position. An empty `issue.Repo` (not expected in production; GraphQL always populates it for real board items) falls back to the literal `unknown/repo` rather than producing a malformed sentinel. The sentinel is identical for a stage run and its comment-review invocations (both are built from the same `stage.Name`), and is deterministic for a given (repo, issue, stage) so repeated and resumed invocations produce the same value.

**Capability probe.** Older `claude` binaries reject unrecognized flags outright, which would kill every in-flight worker — a far worse failure than the `ps`-identification problem this feature solves. To avoid that, `--name` is gated on `claudeNameFlagSupported` (`engine/claude.go`), a package-level boolean probed exactly once, in `engine.New()`, by running `claude --help` with a 5s timeout and checking the output for the literal `--name <name>` flag documentation (`probeClaudeNameFlagSupport`/`parseNameFlagSupport`). This mirrors the existing `claudePluginDir` pattern: computed once at engine construction, read on every `buildClaudeArgs` call, never re-probed per invocation. The probe's zero value is `false`, and any ambiguity — the binary is missing from `PATH`, `--help` exits non-zero, the probe times out, or the output doesn't mention `--name` — fails safe to `false` (flag omitted, workers still run) rather than risking a fleet-wide outage. When unsupported, a single `[startup]` log line explains why and workers proceed exactly as they did before this feature. `buildClaudeArgs` itself stays a pure formatter here too: it receives the already-resolved `sessionName string` and appends `--name <sessionName>` only when both the probe passed and a non-empty sentinel was supplied.

**Interaction with `--resume`.** `--name` is passed unconditionally whenever the capability probe passes, resume or not — live verification during Research confirmed `--name` and `--resume` combine cleanly on the installed CLI (same, unforked `session_id` across a resumed invocation), so there is no conditional-omit branch on the resume path.

**Also a liveness-verification signal (#1779).** The sentinel never appears in prompts or context files, and its original human-facing uses — `ps`/the session picker/terminal title — are unchanged. But the engine itself now reads it back in two places, both in `engine/worker_liveness.go`/`engine/sentinel_probe*.go`:

- `runWorkerDetectorScan`'s `PID<=0` branch: before clearing a worker whose PID was never recorded (the pre-#1779 timeout-only clear from #1303 — see `docs/state-machine.md` §9.7), it probes for a live process still carrying that worker's sentinel via `ps`, exact-argv-token match. A live sentinel is treated like a live PID (log and wait, adopting the discovered PID so ordinary signal-0 liveness governs afterward); an affirmatively-absent sentinel clears exactly as before; a probe that itself cannot run (unsupported platform, `ps` error) defers the clear for a bounded number of scan cycles before falling back to the plain clear, logged as unverified.
- `dispatchCandidates` (`engine/poll.go`): independently refuses to start a second worker for an (issue, stage) whose sentinel is still live, even if a prior worker's handle was already cleared — the exact "two workers on one worktree" shape the mechanism exists to close, asserted at the dispatch site itself rather than relying solely on the scan above having caught it first. Unlike the scan (which checks one worker's sentinel at a time via `probeSentinelLive`/`sentinelProbeFn`), this site may need to check many candidates in a single call, so it fetches the live process table at most once per `dispatchCandidates` call (`listProcessArgvFn`, lazily on first need) and matches every candidate's sentinel against that one snapshot in-process (`matchSentinelInArgvList`) — not once per item, which would otherwise serialize one `ps` subprocess spawn per dispatch-eligible item ahead of the first dispatch in a poll with many of them at once.

Both consumption patterns build on the same underlying `ps`-invocation and exact-token-matching code (`listProcessArgv`/`matchSentinelInArgvList` in `engine/sentinel_probe*.go`) and are gated on `claudeNameFlagSupported` — an older `claude` binary that never got `--name` means no worker in this process could carry a sentinel, so the probe is skipped rather than spawning `ps` for a guaranteed miss.

### Worker Environment: GitHub Identity

The Claude worker subprocess's environment is assembled as `mergeEnv(os.Environ(), extraEnv)` — the base is the engine process's own environment, with `extraEnv` entries taking precedence on key collision (last-wins). Alongside `CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING` and `CLAUDE_CODE_EFFORT_LEVEL`, `extraEnv` includes `GH_TOKEN` and `GITHUB_TOKEN`. In PAT mode (the default), both are set to the engine's resolved GitHub token (`Config.Token`, the same value used for the engine's own `gh.NewClient` calls). Under GitHub App auth (#1713), `claudeGHTokenOverrideFn` is non-nil and `buildClaudeEnv` prefers it over `claudeGHToken`: both env vars are instead set to the live installation token, read directly off the engine's own `*gh.Client` at env-build time — riding that client's background refresh loop rather than a value copied once at startup. Either way this guarantees the worker's `gh` invocations (available via the default `Bash(gh:*)` allowed tool) always authenticate as the same identity as the engine itself, regardless of what `GH_TOKEN`/`GITHUB_TOKEN` the launching shell happens to export. Injection is skipped only when the resolved token is empty, which should not occur in practice since a token (PAT or App-auth installation token) is mandatory at startup.

Under App auth, the installation token is granted `contents:read` but not `contents:write`, so it authorizes worker `gh` CLI calls and worker `git fetch` but not `git push` over an HTTPS remote whose credential helper resolves from `GH_TOKEN`/`GITHUB_TOKEN`. `Run()`'s startup preflight (`RefuseHTTPSWorkerGitUnderAppAuth`, `engine/github_app_auth.go`) refuses to start in that combination — see ADR-1756 and `docs/USER_GUIDE.md`'s "Worker git under App auth" — so a running worker never actually hits this env's git push path un-refused.

`extraEnv` also includes `GH_HOST`, set to the engine's resolved GHES host (`Config.GHESHost`, the same value used for the engine's own client construction via `gh.NewClientForHost` — see ADR-1391), immediately after `GH_TOKEN`/`GITHUB_TOKEN` — but only when `Config.GHESHost` is non-empty. This is the `gh` CLI's own established convention for pointing it at a non-github.com host, so it requires no extra wiring on the worker side: `fabrik-validate`'s Pre-Completion Gate (which runs `gh pr view --json baseRefName` on every Validate invocation) and any other stage's ambient `gh` calls automatically target the same GHES instance as the engine rather than silently falling through to github.com. Unlike `GH_TOKEN`/`GITHUB_TOKEN`, absence is the default and expected case — when no GHES host is configured, `GH_HOST` is omitted from `extraEnv` entirely (not emitted as an empty value), preserving today's behavior byte-for-byte. `GH_HOST` is a Fabrik-computed override like `GH_TOKEN`, not part of the Anthropic auth namespace described below, so it is never subject to the scrub or passthrough machinery.

### Worker Environment: Anthropic Auth Namespace Scrub

`mergeEnv` gained a bare-key removal sentinel (#1346, R1): an entry in `extraEnv` with no `=` (just `KEY`) strips that key from `baseEnv` without supplying a replacement, in addition to the pre-existing `KEY=VALUE` add/shadow/last-wins semantics, which are unchanged. `buildClaudeEnv` uses this to scrub the Anthropic/Claude-Code auth namespace out of the worker's environment by default, so no ambient credential the engine process happens to have (e.g. from a `.env` a human or an agent added locally) can silently redirect a Fabrik invocation from subscription billing to metered API billing.

**Default-deny over a namespace, not a deny-list.** Every inherited variable whose name starts with `ANTHROPIC_` is removed unconditionally (`scrubAnthropicAuthEnv`, `engine/claude.go`) — matching on the exact parsed key, the same "up to the first `=`" extraction `mergeEnv` itself uses, never a substring, so `FANTASY_ANTHROPIC_API_KEY` passes through untouched (R5). This is deliberately wildcard-wide: a newly-introduced upstream `ANTHROPIC_*` billing variable is denied automatically, with no Fabrik code change required (R2). It also means non-auth `ANTHROPIC_*` variables (`ANTHROPIC_MODEL`, `ANTHROPIC_CONFIG_DIR`, etc.) are scrubbed too — an accepted, deliberate side effect of "namespace, not deny-list," documented in `docs/USER_GUIDE.md` and [ADR-1346](../adrs/1346-scrub-anthropic-auth-env-namespace.md).

`CLAUDE_CODE_*` is a much broader general-configuration namespace — it already carries Fabrik's own non-auth `CLAUDE_CODE_EFFORT_LEVEL`/`CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING` — so it is not wildcard-scrubbed. Instead, `claudeCodeAuthSelectors` enumerates the specific `CLAUDE_CODE_*` names verified (against the installed Claude Code binary) to select a non-subscription auth path or supply raw credentials: `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`, `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR`, and the `CLAUDE_CODE_USE_*` provider selectors (`BEDROCK`, `VERTEX`, `FOUNDRY`, `ANTHROPIC_AWS`, `ANTHROPIC_GOOGLE_CLOUD`, `MANTLE`, `GATEWAY`). A not-yet-enumerated future selector would require a Fabrik code change to be scrubbed — an accepted residual risk, since wildcard-scrubbing all of `CLAUDE_CODE_*` would also block legitimate non-billing configuration.

**Explicit API-billing opt-in.** `FABRIK_ANTHROPIC_API_KEY`, resolved once at engine construction into the package-level `claudeAnthropicAPIKey` (mirroring the existing `claudeGHToken` pattern), is translated into an explicit `ANTHROPIC_API_KEY=<value>` override when non-empty (R6) — the only supported way to obtain API billing through this variable. When unset or empty, `ANTHROPIC_API_KEY` never reaches the worker regardless of what the engine inherited (R7). `FABRIK_ANTHROPIC_API_KEY` itself is never forwarded (R8) — `buildClaudeEnv` emits it as a bare removal token, since it is itself present in `os.Environ()`, the very `baseEnv` the worker would otherwise inherit unfiltered (the same ambient-leak reasoning `FABRIK_REPO`'s always-emitted override already documents above). When active, a one-time `[startup]` notice (`logAnthropicAPIKeyOptIn`) states that invocations will be billed to the Anthropic API — never logging the value (R9).

**Explicit passthrough allow-list, for the long tail.** `FABRIK_ANTHROPIC_ENV_PASSTHROUGH`, resolved once into `claudeAnthropicEnvPassthrough` via `parseAnthropicEnvPassthrough` (comma-separated exact variable names), names keys to re-inherit from the ambient environment unchanged, overriding the scrub for only those names (R14). A named variable absent from the ambient environment is a no-op, not an error (R15); a variable not named remains scrubbed even if present (R16); a named variable outside the scrubbed namespace (e.g. `PATH`) is a redundant no-op, not an error (R19) — it was never going to be removed. `FABRIK_ANTHROPIC_ENV_PASSTHROUGH` itself is never forwarded, mirroring R8 (R17). The re-add loop is itself restricted to `isAnthropicAuthNamespaceKey` (`ANTHROPIC_*`-prefixed or a `claudeCodeAuthSelectors` entry) — this is what makes R19's "no-op" claim actually hold for *every* key outside the namespace, including Fabrik's own computed overrides (`GH_TOKEN`, the `FABRIK_*` invocation facts, `CLAUDE_CODE_EFFORT_LEVEL`, `CLAUDE_CODE_DISABLE_ADAPTIVE_THINKING`): naming one of those in the passthrough list is a no-op rather than letting a stale or attacker-controlled ambient value silently win over Fabrik's own value via `mergeEnv`/`os/exec`'s last-occurrence-wins duplicate-key resolution. This exists specifically for Bedrock/Vertex-style non-subscription auth: their actual credential chains (`AWS_ACCESS_KEY_ID`, `GOOGLE_APPLICATION_CREDENTIALS`, etc.) already fall outside the scrubbed namespace and need no passthrough entry — only the `ANTHROPIC_*`/`CLAUDE_CODE_*`-namespaced selectors do, e.g. `FABRIK_ANTHROPIC_ENV_PASSTHROUGH=CLAUDE_CODE_USE_BEDROCK,ANTHROPIC_AWS_API_KEY`. When non-empty, a one-time `[startup]` notice (`logAnthropicEnvPassthrough`) names which variables were passed through and warns that invocations may not be subscription-billed as a result — never logging values (R18).

**Ordering.** `buildClaudeEnv` emits the scrub removals, then the passthrough re-adds, then the `FABRIK_ANTHROPIC_API_KEY` translation last — so if a passthrough entry and the translation both name `ANTHROPIC_API_KEY` (an edge case R7 permits but nobody is expected to actually hit), the translation wins deterministically via `mergeEnv`/`os/exec`'s last-occurrence-wins duplicate-key resolution.

**`apiKeyHelper` is refused outright, not scrubbed.** `apiKeyHelper` is a `settings.json` key, not an environment variable — a command Claude Code shells out to for credentials — so no amount of environment scrubbing can prevent it from supplying an API key. `checkAPIKeyHelper` (`engine/startup.go`) fails startup (non-zero exit) if it is set anywhere in the resolved managed-policy/user/`fabrikDir`-project settings chain; see "apiKeyHelper Detection Path (Worktree)" below for the structurally identical per-invocation check against a worktree's own repo-resident settings, and `docs/USER_GUIDE.md`/[ADR-1346](../adrs/1346-scrub-anthropic-auth-env-namespace.md) for the full rationale and accepted residual risks.

### Worker Environment: Invocation Facts (`FABRIK_*`)

Alongside GitHub identity, `buildClaudeEnv` (`engine/claude.go`) also injects five facts Fabrik uniquely holds about the current invocation, so repo-side scripts running inside the worktree can provision or namespace a resource (a database schema, a port, a fixture namespace, a preview environment) without guessing. This is deliberately **expose-the-facts**, not a lifecycle-hook mechanism: Fabrik publishes what it knows; the consuming repo owns what happens, when, and how (see ADR-1288). No credential is added to this set, and nothing in the engine reads or branches on these variables — they exist solely for repo-side consumption.

| Variable | Value | Presence |
|---|---|---|
| `FABRIK_ISSUE` | The issue number (bare integer, e.g. `1085`) | Always |
| `FABRIK_REPO` | The `owner/repo` this invocation belongs to — the **item's** repo, not the engine's configured default (multi-repo aware) | Always |
| `FABRIK_WORKTREE` | Absolute path to the issue's worktree (the process's working directory) | Always |
| `FABRIK_ROOT` | Absolute path to `fabrikDir` (where `.fabrik/` config, stages, and plugin live) | Always |
| `FABRIK_PR` | The linked pull request number | Only once a PR exists |

`FABRIK_ISSUE` and `FABRIK_WORKTREE` are derived directly from the `issue.Number`/`workDir` values `buildClaudeEnv`'s callers already hold — `workDir` is guaranteed non-empty on every real invocation, since it also becomes the subprocess's `cmd.Dir`. `FABRIK_ROOT` and `FABRIK_PR` require `Engine`-only state (`e.fabrikDir`, `e.readClient`) and are resolved by the shared `Engine.resolveFabrikEnvOpts` helper (`engine/repo.go`), called from every `InvokeOptions`-constructing site — stage invocation (`item.go`), comment processing (`comments.go`), and merge-train conflict resolution (`merge_train.go`, `FABRIK_ROOT` only; see below) — so the worker environment is identical across invocation paths.

**Re-resolved on every extend-turns iteration, not once per call.** `runInvocationWithExtension`'s Turn-Limit Extension Loop (below) can invoke Claude multiple times within a single call, flipping its local `resume` variable to `true` partway through once a turn-capped attempt shows progress. `resolveFabrikEnvOpts` is called fresh at the top of every loop iteration (using the iteration's current `resume` value), not once before the loop — otherwise a PR that came to exist between iterations (e.g. pushed externally to `fabrik/issue-N` while the loop was still running) would stay invisible to `FABRIK_PR` for the rest of that call, since the resume-aware cost-control gate above would never re-evaluate.

**`FABRIK_REPO` and the not-yet-backfilled item case.** `FABRIK_REPO` prefers `issue.Repo`, which is populated from every board fetch and deep-fetch (`github/project.go`) and is therefore set on every item that has gone through Fabrik's normal dispatch path. The one exception is an item freshly constructed from a `projects_v2_item.created` webhook delta, which initially carries only a node ID — `issue.Repo` stays empty there until a subsequent `FetchItemDetails` backfills it (`github/project.go`'s `FetchItemDetails` explicitly comments on this path). To keep `FABRIK_REPO`'s "Always" guarantee true even in that window, `buildClaudeEnv` falls back to `opts.FabrikRepo` — the engine's configured default repo (`e.defaultRepo()`), set at all three `InvokeOptions`-constructing call sites — whenever `issue.Repo` is empty.

`FABRIK_REPO` is always added to `buildClaudeEnv`'s returned overrides, even in the (practically unreachable) case where both `issue.Repo` and `opts.FabrikRepo` are empty — the resolved value is then an empty string, but the key is still present. This is deliberate: `mergeEnv` only strips a key from the base environment (`os.Environ()`) when that key appears in `overrides`. Omitting `FABRIK_REPO` entirely in that case would leave `mergeEnv` with nothing to strip, letting an ambient `FABRIK_REPO` already present in the engine process's own environment — e.g. the distinct engine-startup-config `FABRIK_REPO` (see `docs/USER_GUIDE.md`'s "Not the same `FABRIK_REPO`" note) — pass straight through to the worker unmodified. Always emitting the key, even empty, guarantees the worker-injected value (or its deliberate absence) always wins over anything the launching shell exported.

**`FABRIK_PR` resolution and the `base:<branch>` case.** The board-sourced `item.LinkedPRNumber` is populated from GraphQL `closedByPullRequestsReferences`, which GitHub leaves structurally empty for a PR targeting a non-default base branch. `resolveFabrikEnvOpts` therefore trusts `item.LinkedPRNumber` when non-zero, and otherwise falls back to `FetchLinkedPR` via REST — the identical fallback pattern the review gate already uses (`handleBrokenReviewLinkage` in `reviews.go`). A result that errors, is `nil`, or isn't an open, unmerged PR is treated as "no PR": non-fatal, logged at warn, and never delaying or failing the invocation.

`FetchLinkedPR` matches by branch name (`fabrik/issue-N`) alone, which is never sufficient confirmation on its own: a stale or repurposed `fabrik/issue-N` branch could carry someone else's unrelated open PR — on a `base:<branch>` repo especially, since `closingIssuesReferences` is structurally empty there for *every* PR, not just the linked one, but the same risk exists on a default-branch repo too. `resolveFabrikEnvOpts` therefore **always** confirms the branch-name match via `FetchPRClosingIssues` before trusting it — the same confirmation `handleBrokenReviewLinkage` performs for both its base-label and non-base-label branches — rather than skipping the check on a default-branch repo. `FetchPRClosingIssues` parses the PR body directly via REST, independent of any GraphQL cross-reference field, so it resolves correctly on both repo shapes; every Fabrik-created PR carries a closing keyword (`Closes #N`, per repo convention), so this confirmation succeeds transparently for the common case — including a draft PR that was just created moments ago, before the next GraphQL board refetch has repopulated `item.LinkedPRNumber`. A transient `FetchPRClosingIssues` error fails open (the branch-name match is still trusted, mirroring `handleBrokenReviewLinkage`'s own fail-open behavior) — only a *successful* fetch that doesn't list this issue withholds the PR number.

**Cost control.** The REST fallback only fires when a PR could plausibly exist yet, which is both stage-config- and attempt-aware, not stage-config-alone:
- A stage with neither `PostToPR` nor `CreateDraftPR` (Specify/Research/Plan in the default stage set) never calls the fallback — structurally, no PR can exist yet.
- A stage that only posts to an already-existing PR (`PostToPR` without `CreateDraftPR`, e.g. Review/Validate) calls the fallback from its very first attempt, since an earlier stage (Implement) already created the PR.
- A stage that creates its own draft PR (`CreateDraftPR`, e.g. Implement) calls the fallback only on a *resumed* attempt (`resume == true`, i.e. this issue has a prior invocation attempt already on record) — its own first attempt has no PR to find yet, since `ensureDraftPR` only runs after Claude completes. Gating on the stage flags alone (ignoring `resume`) would burn a REST call on every first Implement invocation that can never succeed — exactly the "network call on every invocation" the cost-control requirement forbids.

Comment processing (`comments.go`) always passes `resume = true`: reaching the comment-review path already implies the stage produced at least one prior attempt.

**Absent is absent, never a misleading zero.** `buildClaudeEnv` omits `FABRIK_PR` entirely when the resolved PR number is `0` — it never emits `FABRIK_PR=0`, which would read as a real PR number to a naive consumer.

**Merge-train conflict resolution is a deliberate partial case.** `merge_train.go`'s inline conflict-resolution invocation (`resolveConflictWithClaude`) sets `FabrikRoot` for consistency but leaves `PRNumber` unset: that invocation resolves a merge conflict on a trial branch, not the member issue's own PR, so there is no single PR for `FABRIK_PR` to name.

### Output Logging

Each invocation writes one NDJSON stream file to `.fabrik/logs/<owner>-<repo>/issue-<N>/` as `<stage>-<timestamp>-<nanos>.log`. This file is written live during execution (tee'd from Claude's stdout) and is the sole on-disk copy of the stream. Viewable via `cat <file> | fabrik stream-filter | less -R` or through the TUI's `l` key.

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

   Claude is started as its own session leader (`Setsid: true` on Unix, #1798 — previously `Setpgid: true` only). `setsid()` also makes the caller its own process-group leader (PGID = own PID), exactly as `Setpgid: true` already provided, so this section's PGID-scoped signaling is unaffected: all three signals are still sent to the entire process group via `syscall.Kill(-pgid, sig)`. The session-leader change exists for an independent mechanism — see "Session-Scoped Descendant Reaping" below.

   Each step is logged as `[#N kill] sending SIG<X> to PGID <pid> (reason=<reason>)`. Reason codes: `max_wall_time`, `inactivity_timeout`, `daemon_shutdown`, `supplant_by_new_invocation`, `context_cancel`.

   A step is skipped if the grace duration is zero (via per-stage `kill_grace:` with `"0s"`). The liveness probe (`kill -0`) short-circuits the remaining sequence if the process group is already empty.

   **Wrapper contract**: The SIGINT grace window exists specifically so that shell wrappers spawned by stage skills (e.g. a CI test runner that posts a final Commit Status on interrupt) can catch SIGINT, complete their cleanup, and exit normally before SIGTERM arrives. The default SIGINT grace is 10 seconds. Wrappers must be designed to finish their cleanup within this window.

   **Grace window configuration**: Engine-wide defaults are set via `--kill-grace-sigint` / `--kill-grace-sigterm` flags (or `FABRIK_KILL_GRACE_SIGINT` / `FABRIK_KILL_GRACE_SIGTERM` env vars, default 10s each). Per-stage overrides are expressed in stage YAML (`kill_grace:`); an omitted field inherits the engine default; `"0s"` skips that signal step entirely.

   After `cmd.Wait()` returns, a second unconditional `SIGKILL` is sent to the process group to clean up any grandchildren (e.g. `tail -f` from the Monitor tool) that survived the Claude subprocess exit. This grandchild cleanup fires regardless of how the main subprocess exited.

2. **WaitDelay bound**: `cmd.WaitDelay` is set to 30s (configurable via `--claude-wait-delay` / `FABRIK_CLAUDE_WAIT_DELAY`). When grandchild processes hold the stdout pipe open after Claude exits, Go's `cmd.Wait()` would otherwise block indefinitely. With `WaitDelay`, Go forcibly closes its end of the pipe after the deadline and returns `exec.ErrWaitDelay`. The engine detects this error, logs a diagnostic warning, clears the error, and processes the buffered output normally — including any `FABRIK_STAGE_COMPLETE` marker. This prevents the worker goroutine from being permanently stuck when Claude uses `run_in_background` or the Monitor tool.

### Session-Scoped Descendant Reaping

The PGID-scoped grandchild cleanup above (item 1) and the cwd-rooted worktree-teardown reaper (below) each leave a gap: a descendant that leaves the worker's process group (e.g. a job-control-enabled shell's `setpgid()` call on a backgrounded job — confirmed by direct reproduction: `set -m; nohup cmd & disown` produces a child in a brand-new PGID) escapes the first, and a descendant whose cwd is outside the worktree (a Go test's `t.TempDir()` fixture, the dominant real-world shape per #1798) escapes the second regardless of when it runs. Neither gap is closed by running either mechanism more often or earlier — the target simply isn't reachable by either primitive.

`engine/descendant_reap_unix.go` (`!windows`) adds a third, independent mechanism keyed on session ID (SID) rather than process-group membership or cwd:

- **Discovery, live during the invocation**: `trackWorkerDescendants` runs as a goroutine parallel to the inactivity watchdog (same lifecycle, stopped via the same `watchdogCtx`), ticking every `descendantScanInterval` (3s in production, test-overridable). Each tick, `sessionScopedDescendantsFromProcs` filters a process-table snapshot down to every live process whose session ID equals the worker's own PID — via `golang.org/x/sys/unix.Getsid`, called per candidate PID rather than parsed from `ps` output (see below for why) — and records any not already seen into a durable registry with an identity fingerprint (`comm` + `lstart`, from a single-PID `ps -p <pid> -o comm=,lstart=` query). A PID is only marked "seen" once its `upsertTrackedDescendant` write actually succeeds — a transient registry-write failure (e.g. `.fabrik/state/` momentarily unwritable) leaves it unmarked so a later tick retries the write, rather than silently and permanently dropping that descendant from tracking for the rest of the invocation.

  The process-table snapshot itself comes from `sharedProcessTableScan`, a package-level cache (TTL = `descendantScanInterval`) rather than a fresh `ps -eo pid=,args=` call per tick: one `trackWorkerDescendants` goroutine runs per in-flight Claude invocation, and without sharing, `MaxConcurrent` concurrent invocations meant that many redundant full-table scans every tick — the same class of `ps` cost found responsible for real CPU spikes elsewhere in this mechanism (#1805). The first goroutine to tick after the cache goes stale performs the real scan and repopulates it; every other goroutine ticking within the same window reuses that result. Mirrors `dispatchCandidates`'s existing "fetch once, reuse across many checks" shape (`poll.go`, via `listProcessArgvFn`) but as a time-based cache rather than a single-call memoization, since sharing here spans concurrently-running, independently-ticking goroutines over the life of several invocations rather than one function call. `sessionScopedDescendants` (an uncached, always-fresh variant used directly by this mechanism's own tests) is unaffected.

  This must observe the tree *while the invocation is live*, not just once at teardown: a `nohup`/`disown`-detached descendant is reparented to init on a sub-second timescale — well before any post-hoc walk could see the transient worker→…→child PPID chain. Session ID does not have this problem: POSIX guarantees it is assigned once at process creation and inherited unchanged across `fork()`, changing only via an explicit `setsid()` call by the descendant itself (reparenting does not touch it) — so a descendant discovered at any later tick is still correctly attributed to the worker, no matter how quickly its intermediate parent exited.

  **Why `getsid(2)` directly, not `ps`'s own session-ID column**: on at least one macOS release, `ps -eo pid=,ppid=,sess=` (the BSD equivalent of GNU/Linux's `sid=`) reports `0` for every process unconditionally, apparently masked at the `ps`-output layer. The underlying `getsid(2)` syscall is not masked — verified directly on the same host — so discovery goes through it instead. See ADR 1798.

- **Reap at invocation end (R2)**: immediately after the pre-existing unconditional grandchild-cleanup `SIGKILL` (item 1 above) — the same already-unconditional call site, not a new hook — `reapTrackedDescendants` loads every registry entry recorded against the worker's PID, re-verifies each one's `comm`+`lstart` fingerprint immediately before killing it (a mismatch means the process already exited or the PID was reused for something unrelated since it was recorded — the entry is dropped, never signalled), and `SIGKILL`s the survivors. Logged via the existing `[#N kill] sending SIGKILL to PID <pid> (<comm>) — session-scoped descendant of worker PID <workerPID> (reason=invocation_end)` line.

  A registry entry is only ever dropped on a *positive* signal: confirmed dead via a cheap signal-0 probe (`isProcessAlive`), or confirmed a mismatch via a successful fingerprint lookup whose `comm`/`lstart` differ. A fingerprint lookup that itself errors for a PID `isProcessAlive` still reports as alive — e.g. the `ps`-subprocess timeout expiring under the same host contention this reaper exists to handle — is treated as an inconclusive, transient probe failure: the entry is left in the registry for a later attempt rather than being dropped, mirroring `trackWorkerDescendants`'s own transient-failure handling (below). `sweepStaleDescendants` (R3) applies the identical rule to its own per-descendant fingerprint check.

  Before this reap runs, `runClaude` waits (via a `sync.WaitGroup`) for the `trackWorkerDescendants` goroutine to actually observe `watchdogCtx`'s cancellation and return — not merely for `watchdogCancel()` to have been called. Cancelling the context doesn't interrupt a tick already in progress: if the goroutine is blocked inside a `pidFingerprintFn` call for a just-discovered descendant when `cmd.Wait()` returns, it can still be about to persist that descendant to the registry. Reaping before that write lands would miss the entry entirely — silently deferring that descendant's reap from "unconditional at invocation end" to the next R3 backstop sweep (bounded by `JanitorIntervalHours`, potentially hours later). The wait is bounded by `pidFingerprintFn`'s own internal timeout, so it cannot hang.

- **Backstop sweep (R3)**: `runProcessSweepJanitor` (`engine/janitor.go`) is a fourth periodic janitor, wired into the same `JanitorIntervalHours`-gated call sites (startup, and the hourly ticker) as the worktree/log/session janitors — no new config knob. It calls `sweepStaleDescendants`, which loads the *entire* durable registry (every worker, including ones from a previous engine run — the registry lives at `.fabrik/state/descendants.json` and survives a restart) and, for each entry whose recorded worker PID is confirmed dead, re-verifies its fingerprint and kills it exactly as the invocation-end reap does. "Confirmed dead" is a live signal-0 probe *plus* an identity re-check: `trackWorkerDescendants` also stamps each entry with the worker's own `comm`/`lstart` fingerprint at discovery time, and a live PID whose fingerprint no longer matches (the worker exited and the OS reused its PID) is treated as dead too — otherwise such an entry would pass the signal-0 probe forever and never be swept. An entry whose worker is still alive and still the same worker is left alone — that invocation is still in flight and R2 will reap it — so the sweep only ever acts on orphans that genuinely escaped R1/R2, including ones left behind by a prior engine run or orphaned by worker-PID reuse.

- **Registry-independent worker-session sweep (#1814)**: every path above starts from a registry entry, so a descendant that was *never recorded* — spawned within the last `descendantScanInterval` before its worker exited, or during a transient `ps` failure — is invisible to all of them, and nothing would ever reap it. `runClaude` therefore also writes a durable **worker record** (`.fabrik/state/workers.json`, `engine/worker_registry.go`) synchronously right after `cmd.Start()`, when the worker's PID belongs to exactly one process, capturing its `comm`/`lstart` fingerprint (retried in the background if that first lookup fails). `engine/worker_sweep_unix.go` then finds orphans by enumerating live processes and calling `unix.Getsid` on each: any process whose SID equals the PID of a recorded, confirmed-dead worker is reaped. Ownership is by session lineage alone — no `comm`/args allow-list — so an orphaned Bash-tool shell with zero children is reaped like any test binary. It runs (a) at invocation end, immediately after `reapTrackedDescendants`, using a **fresh, uncached** process scan (a cached snapshot could predate the racing descendant), rescanning briefly (≤2s) to catch members still visible after `SIGKILL`; and (b) from the proc-janitor (see state-machine.md §11.7), reading every record from disk so an orphan from a previous engine run is still reapable. The per-descendant registry stays as the fast path; this is additive.

  A process is signalled only when it is provably a descendant of a dead recorded worker — a dead session leader alone is never enough, and no record means no kill. A worker is "dead" when its PID is not alive, or when the live process at that PID has a different `lstart` than recorded (a recycled PID). In the recycled case SID equality alone would also match the *new* holder's descendants, so a member is reaped only if it started at or after the dead worker and strictly before the current holder; equal or unparseable timestamps skip. Every ambiguity fails open: a fingerprint lookup error, an empty recorded fingerprint while the PID is alive, or a scan error signals nothing and keeps the record. Each candidate is re-verified (fresh fingerprint, `Getsid` still equal) immediately before a single-PID `SIGKILL`, logged as `[#N kill] sending SIGKILL to PID <pid> (<comm>) — orphaned session member of dead worker PID <W> (reason=session_sweep_invocation_end|session_sweep_periodic)`. A record is pruned only once the worker is confirmed dead and two consecutive successful scans find no live member and nothing was skipped. See ADR 1814.

**Kill scope**: both reap paths signal the descendant's own PID directly (`syscall.Kill(pid, SIGKILL)`), not a process group — matching the worktree-teardown reaper's own `killWorktreeProcess` precedent.

**Known gaps** (see ADR 1798 Consequences): a descendant that calls its own `setsid()` is not caught (its SID permanently diverges from the worker's — this is the cwd-rooted reaper's domain instead); a detached descendant that itself forks further descendants is reaped only at the recorded leaf, not recursively; a descendant that was never recorded is invisible to the registry paths above (closed by the worker-session sweep, #1814, for any worker that has a record). Still not covered by the worker-session sweep (ADR 1814 Consequences): orphans that predate worker records, other Fabrik instances' orphans, and — by design, failing open — a member whose lookups are inconclusive or whose recorded worker fingerprint could not be captured while its PID is alive.

### Worktree Teardown Process Reaping

*(Complementary to, not superseded by, "Session-Scoped Descendant Reaping" above — the two catch different failure classes: this one is cwd-based and runs only at worktree removal; the other is session-based and runs on every invocation end plus a periodic backstop. See ADR 1798.)*

The grandchild cleanup above is **PGID-scoped**: it can only reach descendants that stayed in the worker's process group. A descendant that calls `setsid()` — exactly what Claude Code's background-bash tool does to keep a backgrounded process (e.g. `npm run dev`) alive across tool calls — leaves the worker's process group entirely, taking on a fresh PGID equal to its own PID. `kill(-workerPGID, SIGKILL)` cannot reach it, so it survives Claude's exit and outlives the worktree directory it was started in.

`reapWorktreeProcesses` (`engine/reaper_unix.go`) closes this gap with a **cwd-rooted** reaper, run immediately before every worktree directory removal: it enumerates live processes whose current working directory is the worktree path or a subdirectory of it — regardless of process-group membership — and sends SIGKILL to each. It is a package-level function (not a `WorktreeManager` method), so it can be called from contexts with no `WorktreeManager` instance available.

**Platform implementations:**
- **Linux**: scans `/proc/*/cwd` symlinks and prefix-matches each against the worktree directory. Immediately before killing a match, it re-reads `/proc/<pid>/cwd` to close the TOCTOU window in which the pid could have been recycled for an unrelated process between enumeration and kill.
- **macOS**: runs `lsof -a -d cwd -n -P -Fpcn`, which inspects only each process's cwd file descriptor (not every open fd, unlike `lsof +D`) — kept fast and non-hanging on the synchronous teardown path even for a `node_modules`-heavy worktree. macOS has no `/proc`-equivalent cheap re-check primitive, so the same TOCTOU window is accepted rather than mitigated here.
- **Windows**: no-op (`engine/reaper_windows.go`), matching `killProcGroup`'s existing Windows precedent.

Both platform paths resolve symlinks in the worktree directory before matching (`filepath.EvalSymlinks`), since `/proc/*/cwd` and `lsof`'s reported path are both fully-resolved real paths (e.g. macOS resolves `/tmp` → `/private/tmp`), which would otherwise defeat a literal prefix match.

Each kill is logged via the existing `[#N kill] ...` convention: `[#N kill] sending SIGKILL to PID <pid> (<comm>) rooted in <wtDir> (worktree cwd cleanup)`, alongside the `(grandchild cleanup)` PGID-kill log line so both are visible in the same log stream. Enumeration and kill failures (missing `lsof`, a `/proc/<pid>/cwd` read racing process exit, permission errors) are always non-fatal: logged as a warning, never blocking the caller's own worktree removal.

**Call sites** (every path that removes a worktree directory):
- `WorktreeManager.CleanupWorktree` — issue worktree teardown (Done-stage teardown, periodic janitor)
- `WorktreeManager.ensureTrainWorktreeFromRef`'s stale-worktree removal — crash-recovery cleanup before a merge-train trial worktree is recreated under the same name
- `WorktreeManager.CleanupTrainWorktree` — merge-train trial worktree teardown
- The worktree janitor's `os.RemoveAll` fallback (`engine/janitor.go`), which fires when no `WorktreeManager` is available for the repo (bare repo missing) — this is why the reaper is a free function rather than a `WorktreeManager` method

**Documented residual risks** (see ADR 063):
- **PID-reuse TOCTOU on macOS**: unlike Linux, there is no cheap re-check between enumeration and kill, so a small window exists where a recycled pid could receive an unintended SIGKILL.
- **`chdir`'d descendants are not caught**: this reaper is cwd-based by design (matching the issue's reported failure mode); a process that opened a file in the worktree and then `chdir`'d elsewhere is out of scope and will still leak.

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

1. `opts.MaxTurnsOverride` is set to `currentBudget` (first iteration: `stage.MaxTurns`, or `2 × stage.MaxTurns` if `fabrik:extend-turns` is present). `opts.FabrikRoot`/`opts.PRNumber` are also re-resolved here via `resolveFabrikEnvOpts` (#1288), using the current `resume` value — see "Worker Environment: Invocation Facts" above.
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

### Assistant-Turn Artifact Harvest (R1/R2)

The CLI's terminal `result` field is whichever text the agent emitted in its *very last* turn — not necessarily the same turn that produced the stage's real artifact. If the agent makes one more tool call after emitting its output (a common shape: emit the Plan/Research/Review content, then run one more verification command before signaling done), `result` ends up carrying only the wrap-up, sometimes nothing but the bare `FABRIK_STAGE_COMPLETE` marker itself. Before this harvest existed, that wrap-up was trusted verbatim and the real artifact was silently discarded — the stage was still labelled `stage:<name>:complete`, with no comment and no `.fabrik-context/stage-<Name>.md` behind it, discovered only when a later stage found nothing to read (#1632, #1782).

`interpretClaudeResult` (`engine/claude.go`) detects this via `artifactMissingOnComplete`: `FABRIK_STAGE_COMPLETE` is present, `FABRIK_NO_WORK_NEEDED` is not (see the exclusion below), and the text carries nothing beyond Fabrik's own bare control-marker lines (`hasArtifactContent` — the same five markers `FABRIK_STAGE_COMPLETE`/`FABRIK_BLOCKED_ON_INPUT`/`FABRIK_NO_WORK_NEEDED`/`FABRIK_SUMMARY_BEGIN`/`FABRIK_SUMMARY_END` that Marker Stripping below removes before posting). When that fires, `extractLastSubstantialAssistantTurn` re-scans the raw NDJSON output (the same `forEachAssistantText` line-scanner that already powers the narrower `FABRIK_ISSUE_UPDATE_BEGIN`-specific fallback immediately preceding it in the function) for the **last assistant turn whose text is non-empty after that same stripping** — not "the turn containing the marker," since the reported case's marker-bearing turn contains only the marker itself. The recovered turn is prepended to the CLI's own result text, so the marker itself is always preserved for the completion check that follows.

This only ever *adds* content ahead of an already-thin result — an ordinary completion where the artifact is already present in `result` never triggers the scan (`hasArtifactContent(resp.Result)` is already true) and is byte-identical to before this existed.

### No-Artifact Completion Guard (R3)

`FABRIK_STAGE_COMPLETE` being present is not, by itself, sufficient evidence that the stage produced anything — the harvest above can come back empty too (no assistant turn had any content beyond control markers). `artifactMissingOnComplete` gates every place `interpretClaudeResult` would otherwise convert "marker present" into `completed = true` (the marker-found-despite-a-trailing-error path, and the ordinary clean-exit path); when it holds, `completed` is forced `false` and the invocation falls through to the stage's normal non-completion handling instead — for a stage dispatch, that is the existing retry/escalate machinery (`finalizeStageOutcome`, `engine/item.go`): the attempt counts against `MaxRetries` (mirroring the pre-existing degenerate-output guard, #1065 — the invocation did real work, so this is not exempted the way a usage-limit or tools-denied exit is), a one-time first-detection comment ("no artifact harvested") posts on the first occurrence, and the eventual escalation at `MaxRetries` names this cause distinctly from the unrelated bare-file-reference one. For the comment-review path (`processComments`/`publishCommentOutput`, `engine/comments.go`), `completed = false` flows through unchanged into the same no-progress/no-op-cycle handling comment processing already has — no separate guard was needed there, since both invocation paths bottom out in this same `interpretClaudeResult` call.

**Exclusion:** a stage that legitimately produces no artifact — `FABRIK_STAGE_COMPLETE` co-occurring with `FABRIK_NO_WORK_NEEDED` — is never treated as this defect. Both the harvest scan and the completion guard check `!CheckNoWorkNeeded(text)` before doing anything, so a no-work-needed completion's established "nothing posted, stage skipped straight to Done" behavior is unaffected — including never risking a scan pulling unrelated earlier-turn reasoning into it.

This does not change or duplicate `recoverMissingPlanComment` (#982): that recovery targets a stale-cache race (a fresh Plan comment existing on GitHub but not yet visible in `item.Comments`), a different root cause producing the same *symptom* (`stage:Plan:complete` present, no Plan comment). This guard targets the harvest defect directly, at the point the artifact is first assembled, so new occurrences of the #1632 shape should no longer reach that recovery path at all.

See ADR-1782.

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

### Spawn Receipt Note

If the Plan stage's posted output contains N > 0 well-formed spawn blocks — as counted by `ParseSpawnBlocks`, never by string-matching the marker text — a deterministic note is appended stating that N sub-issues are declared and will be created when the parent advances to the **Implement** stage. The note is gated to `stage.Name == "Plan"`, matching exactly what `preImplement` itself reads (`findStageComment(item.Comments, "Plan")`, engine/spawn.go): a note on any other stage's comment would promise a spawn that mechanism never performs — for example, a later stage's Claude quoting a spawn block back verbatim from its own context (later stages receive the Plan comment through `.fabrik-context/stage-Plan.md`). The gate applies to both output-posting paths above. The note is folded into the same `footer` value that also carries the stats footer below, computed once and threaded unchanged through every posting path — so it renders identically whether output goes straight to the issue or through `post_to_pr`. N == 0, or a non-Plan stage, produces no note and byte-identical output to before this existed. See ADR-048 and #1338.

### Token Stats Reporting

Every invocation reports token usage in two places: an operator-facing log line (`e.logf(..., "stats", ...)`, emitted from `finalizeStageOutcome` in `engine/item.go` and from comment-processing finalization in `engine/comments.go`) and a human-facing footer appended to the posted comment/PR output (`formatStatsFooter`, `engine/claude.go`). Both are built from the same `TokenUsage` struct, which carries four independent token counts per invocation: `InputTokens` (raw, uncached input), `OutputTokens`, `CacheReadTokens` (context served from Claude's prompt cache), and `CacheCreationTokens` (new context written to the cache this turn).

**Why cached input dominates.** With prompt caching active — which it always is once a conversation has any history — `InputTokens` alone is structurally near-zero: almost all context is served from cache, not resent as fresh input. Reporting only `InputTokens` (as Fabrik did before this reporting was added) makes every stage look like it consumed `0k input` regardless of actual cost, and a short resumed invocation that replays a large accumulated context looks artificially cheap. Cache tokens are not a rounding error; on long-running or resumed invocations they routinely dwarf raw input by two or three orders of magnitude.

**Log line format** (`formatStatsLogLine`, shared by `item.go` and `comments.go`) uses raw, unscaled numbers in the same `key: value | key: value` convention as the pre-existing cumulative line at `engine/poll.go:1172`:

```
used 41/250 turns | in: 476 | out: 171009 | cache_read: 25003551 | cache_write: 993820
```

**Footer format** (`formatStatsFooter`, posted into the GitHub comment/PR a human reads) is k/M-scaled and leads with an "effective input" total (`InputTokens + CacheReadTokens + CacheCreationTokens`) so a reader can't mistake the raw figure for total input consumed, with a raw/cache-read/cache-write breakdown in parentheses when cache activity is non-zero. Cache reads and cache writes are broken out separately rather than folded into one "cached" figure, since Anthropic prices them differently per token:

```
Used 41/250 turns, 26.0M input (476 raw + 25.0M cache-read + 993k cache-write) / 171k output tokens.
```

**Emptiness guards.** Both formatters return an empty string — suppressing the line entirely — only when *all five* fields (`TurnsUsed`, `InputTokens`, `OutputTokens`, `CacheReadTokens`, `CacheCreationTokens`) are zero. An invocation with meaningful cache activity but zero raw input/output still reports, since caching means that combination is a real, common case rather than a signal of an empty invocation.

`TokenUsage.InputTokens` keeps its existing meaning (raw uncached input) everywhere, including in `internal/itemstate` and the poll-level cumulative log — this is a display change only, not a change to what the field means or how cost (`CostUSD`) is calculated.

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

**Validate + yolo**: At Validate completion, if the issue carries `fabrik:yolo` (and not `fabrik:cruise`), and the Validate stage's review gate is either not opted in (`wait_for_reviews` unset) or already satisfied — including, when `review_authority: authoritative` is set, no outstanding `CHANGES_REQUESTED` review (see `docs/state-machine.md` §6.1.1) — Fabrik calls `enablePullRequestAutoMerge` on the linked PR and applies `fabrik:auto-merge-enabled` rather than calling `MergePR` directly. GitHub then merges the PR atomically once branch-protection requirements are satisfied. The post-Validate convergence monitor (`checkAutoMergeConvergence`) tracks the PR in subsequent poll cycles until it reaches a terminal state or the convergence budget expires. If `enablePullRequestAutoMerge` fails (PR already in a terminal GitHub state — CLEAN, UNSTABLE, or similar), Fabrik falls back to a direct `MergePR` call — the only merge path with no independent `mergeable_state` gate upstream of it (reached whenever `wait_for_ci: false`). `MergePR` self-gates regardless of `enforce_admins`: it refuses to merge unless `mergeable_state ∈ {clean, unstable}`, returning the distinct sentinel `gh.ErrNotMergeableCI` when a required check is still `blocked` or pending (ADR-072). This is not a merge conflict — `fabrik:rebase-needed` is not applied and no rebase cycle is consumed; the item simply retries via a full Validate re-dispatch on the next poll. See `docs/state-machine.md` §5.4–5.5 for full details.

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
5. Stall detection (`detectAndArmStallHint`, #1146, #1767): this attempt's turn usage is compared against the previous incomplete attempt's. A clean incomplete predecessor — capped or not — followed by a strictly-declining, still-incomplete, uncapped attempt arms a one-shot-per-episode corrective hint — consumed by the *next* invocation of this stage, injected into its prompt via `InvokeOptions.CorrectiveHint` — and posts an informational comment. See `docs/state-machine.md` §7.10 for the full detection and injection rule.
6. Retry count incremented; after `max_retries`: `fabrik:paused` + `stage:<name>:failed`, lock released

### Claude Usage-Limit Path

A fourth outcome, distinct from Completion/Blocked-on-Input/Incomplete above: Claude exits non-zero
because the account's usage limit was hit, not because the stage genuinely failed.
`interpretClaudeResult` (`engine/claude.go`) detects this **structurally**, from the CLI's own parsed
result object only — never from anything the assistant wrote: `classifyUsageLimitExit` checks
`resp.TerminalReason == "blocking_limit"` (the same `terminal_reason` field the turn-cap check
consults) or `resp.TerminalReason == "api_error"` with `api_error_status == 429` (the CLI's other
session-limit shape, #1811 / ADR-1811; other `api_error` statuses stay transient and never suspend),
and only when a result object actually parsed. The `result` text is never matched. When Claude exited non-zero without a
`FABRIK_STAGE_COMPLETE` marker, no turn cap, and `TerminalReason` matches, it returns a
`*claudeUsageLimitError` sentinel in place of the generic error; an unparseable-JSON invocation is
never classified as a usage-limit exit by any means. `finalizeStageOutcome` classifies it via
`errors.As` after stash-restore (which must run regardless of outcome) but immediately following the
engine-shutdown guard — before the normal `claudeRan`/retry classification logic runs — and routes to
`handleUsageLimitExit`:

1. `StageAttempted` recorded — the normal cooldown (`pollSeconds * 10`) applies, so the item does not
   retry on the very next poll and hammer the limit in a tight loop.
2. Retry count is **not** incremented — the stage never ran, so this does not consume `max_retries`.
3. If `fabrik:claude-limit` is absent, an explanatory comment naming the condition is posted and the
   label is added — gated on the label's own absence so a repeated hit within the same episode does not
   repost the comment. Neither `fabrik:paused` nor `stage:<name>:failed` is applied. The comment
   names the reset time (`(resets <local time>)`) only when it was sourced from the CLI's structured
   `unifiedWindows.*.resetsAt` (ADR-1815); it is never parsed from prose, and is omitted on the
   one-hour fallback.
4. No partial-progress commit, no branch push, no `markCommentsSeenByStage` — nothing was produced.
5. Lock released.

`fabrik:claude-limit` clears per-issue on the next invocation that is not itself classified as a
usage-limit exit (success, blocked-on-input, incomplete/genuine-failure, or PR-creation failure alike),
and account-wide via `settleClaudeLimitLabelSweep`, a per-poll settle scan that removes it from every
open item once the account-wide suspension has lifted — so an issue that is paused, blocked, or simply
never redispatched no longer keeps the label indefinitely. An operator can also end an active suspension
early, without restarting the engine, by applying `fabrik:clear-claude-limit` to any open board item;
`settleClaudeLimitClearRequests` reads it each poll and clears the suspension. See
`docs/state-machine.md` §7.3, [ADR-1119](../adrs/1119-claude-usage-limit-detection.md), and
[ADR-1183](../adrs/1183-structural-claude-usage-limit-detection.md) for the full rationale, including
why this reuses the same `StageAttempted`-without-`StageRetryIncremented` split as the Post-Run Boundary
Audit above (with the opposite pause/fail outcome — a usage limit is transient and self-resolving, a
boundary violation is not).

### apiKeyHelper Detection Path (Worktree)

A fifth outcome, checked in `runInvocationWithExtension` immediately alongside the account-wide
usage-limit suspension gate above — before `InvokeOptions` is built or the stall hint consumed, and
before Claude is ever invoked. `findAPIKeyHelper` (`engine/startup.go`, shared with the engine-startup
preflight — see "Worker Environment: Anthropic Auth Namespace Scrub" above) checks the worktree's own
`.claude/settings.json` and `.claude/settings.local.json` (mirroring the startup preflight's file
coverage for the user/project layers): a **repo-resident** setting Fabrik cannot see at engine startup,
since the worktree doesn't exist yet (#1346, R13). If either sets `apiKeyHelper`, the invocation is skipped and
`*apiKeyHelperDetectedError` is returned in place of invoking Claude at all; `finalizeStageOutcome`
classifies it via `errors.As` (alongside the usage-limit check) and routes to
`handleAPIKeyHelperDetected`, which mirrors `handleUsageLimitExit` exactly:

1. `StageAttempted` recorded — the normal cooldown applies, so the item does not retry on the very
   next poll.
2. Retry count is **not** incremented — the stage never ran.
3. If `fabrik:api-key-helper-detected` is absent, an explanatory comment naming the offending file is
   posted and the label is added — gated on the label's own absence, matching `fabrik:claude-limit`'s
   non-spamming behavior. Neither `fabrik:paused` nor `stage:<name>:failed` is applied.
4. No partial-progress commit, no branch push — nothing was produced.
5. Lock released.

`fabrik:api-key-helper-detected` clears on the next invocation that is not itself classified as a
usage-limit exit or an `apiKeyHelper` detection (the same unconditional label-clear site as
`fabrik:claude-limit`, later in `finalizeStageOutcome`) — a human removing `apiKeyHelper` from the
worktree's `.claude/settings.json` and letting the next poll reach Claude successfully is enough to
self-resolve; no manual label removal is required. Unlike `fabrik:claude-limit`, there is no
account-wide settle sweep for this label — the condition is inherently per-worktree, not
account-wide. See [ADR-1346](../adrs/1346-scrub-anthropic-auth-env-namespace.md).

### Toolchain Declaration Drift Detection (Worktree)

A sixth check, `checkToolchainDrift` (`engine/toolchain.go`), runs at both invocation entry points —
`runInvocationWithExtension` (stage dispatch, immediately after the `apiKeyHelper` check above) and
`processComments` (comment review, right after context files are written) — closing the gap the
`apiKeyHelper` check leaves on the comment-review path (#1786 R5 explicitly names both cycles).

**Unlike every check above, this one never skips the invocation.** A long-lived daemon inherits `PATH`
once, from whatever shell hosted it at startup; a worktree can declare a toolchain version
(`.nvmrc`, `package.json` `engines.node`, `go.mod`'s `toolchain`/`go` directive, or `.tool-versions`)
newer than what that inherited `PATH` resolves, and nothing before this check ever notices — workers
keep running the stale toolchain silently, indefinitely, restarting `fabrik` itself does not help
(it re-inherits the same `PATH` from the same parent shell). Re-resolving `PATH` or driving a version
manager (`nvm use`, `asdf install`, …) is explicitly out of scope; this is a warn-and-continue signal,
not a gate, so it returns nothing — `finalizeStageOutcome` is never involved, `StageAttempted`/
`StageRetryIncremented` bookkeeping is untouched, and the invocation proceeds exactly as it would have.

1. `detectToolchainDrift` reads whichever declaration files exist in `workDir` and, for each
   comparable declaration, resolves the corresponding binary (`node --version` / `go version`) via
   `exec.LookPath` against the daemon's own inherited `PATH`. A binary entirely absent from `PATH` is
   "cannot compare" (a distinct, pre-existing condition — not this check's concern), never a mismatch.
   A successful resolution is cached process-wide per tool for the daemon's lifetime
   (`resolveToolVersion`) — since the entire premise of the bug is that this `PATH` is invariant until
   a restart, a second check for the same tool is a cache hit, not a second shell-out. A resolution
   *failure* (subprocess timeout, transient exec error) is never cached, since unlike `PATH` itself a
   failure carries no invariance guarantee — caching it would permanently and silently disable
   detection for that tool after a single bad moment.
2. Comparison is deliberately narrow and fails closed: `.nvmrc`, `package.json` `engines.node`, and
   `.tool-versions` are compared at **major-version granularity only** (an alias like `lts/*`, a
   complex semver range using `||`/`x`/multiple clauses, or any other unparseable content is "not
   comparable" — silence, never a false-positive warning); `go.mod`'s `toolchain`/`go` directive is
   compared at **major.minor granularity**, using minimum-version (`>=`) semantics, since a `go.mod`
   declares a floor, not a pin (and `GOTOOLCHAIN=auto`, the Go 1.21+ default, already self-corrects in
   the common case — this only catches the `GOTOOLCHAIN=local` / auto-download-disabled edge case).
3. On any comparable mismatch, if `fabrik:toolchain-stale` is absent: an explanatory comment naming
   every mismatched declaration (source, declared version, resolved version) is posted and the label
   is applied — gated on the label's own absence, so the warning fires once per episode (per issue),
   not once per invocation (#1786 R5), matching `fabrik:claude-limit`/`fabrik:api-key-helper-detected`.
4. If no comparable mismatch is found and `fabrik:toolchain-stale` is present, the label is cleared —
   self-resolving once a worktree's declaration file is fixed (or, in practice, the daemon is restarted
   with a corrected `PATH`), exactly like `fabrik:api-key-helper-detected`. No dedicated settle scan:
   this is a fire-once informational label, added to `transientLifecycleLabels` for closed-issue sweep.

A daemon/worktree whose resolved toolchain already satisfies every declaration sees no new behavior and
no new output at all (#1786 R4) — no comment, no label, no added latency worth noticing beyond a few
small file reads and, at most once per tool per daemon lifetime, a version-check subprocess. See
[ADR-1786](../adrs/1786-toolchain-declaration-drift-detection.md).

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

## Phase 6b: Unmanaged Stage (Backlog)

An `unmanaged: true` stage (the default `Backlog`, `stages/examples/backlog.yaml`) declares a
"parking column" Fabrik recognizes but runs no workflow for:
- No Claude invocation, no lock, no in-progress label management — same as a cleanup stage,
  but no worktree action either (nothing to clean up; the item never had one)
- `itemMayNeedWork` and `itemNeedsWork` both return `false` for it, so it is never dispatched
- The yolo/cruise catch-up loop skips it, so it is never auto-advanced out of the column
- `runProbeAndDeepFetch`'s stage-membership guard treats it as unconfigured for deep-fetch
  purposes — items sitting in an unmanaged column are never `FetchItemDetails`-fetched, even
  though a matching `Stage` exists (preserving the pre-existing Backlog deep-fetch avoidance)
- Items sit here until a human moves them to a real stage's column

---

## Markers Reference

| Marker | Direction | Purpose | Where Checked |
|--------|-----------|---------|---------------|
| `FABRIK_STAGE_COMPLETE` | Claude -> Engine | Stage finished successfully. **Intended behavior:** honored even on non-zero Claude exit in both stage runs and comment processing — the non-zero exit is recorded separately (as an `Err`/`ExitOK` field on the invocation record) and does not veto completion; engine logs a warning. **Current reality:** stage runs honor this marker on non-zero exit, but `comments.go:168` currently gates `completed` on `err == nil`, so comment processing does NOT honor `FABRIK_STAGE_COMPLETE` when the process exits non-zero (regression introduced by commit `5acf2609`). A code fix restoring parity is tracked in a separate follow-up PR. | Stage runs (both exit paths); comment processing (non-zero exit vetoes marker — known divergence) |
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
holding_stage: false        # Engine-managed batch holding pen (e.g. Queued) — no per-item dispatch
unmanaged: false            # Parking column (e.g. Backlog) — recognized but never dispatched or auto-advanced
kill_grace:
  sigint: 10s               # Grace window after SIGINT before SIGTERM (empty = engine default; "0s" = skip SIGINT)
  sigterm: 10s              # Grace window after SIGTERM before SIGKILL (empty = engine default; "0s" = skip SIGTERM)
completion:
  type: claude              # Only supported type
```

Either `skill` or `prompt` is required (unless `cleanup_worktree`, `holding_stage`, or `unmanaged` is true — these three flags mark stages that are never dispatched to Claude). When `skill` is set, the engine sends a directive prompt and the skill is loaded via `--plugin-dir`.
