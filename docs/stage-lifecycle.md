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

| Stage | Order | Read-Only | UpdateIssueBody | PostToPR | CreateDraftPR | MarkPRReady | MaxTurns |
|-------|-------|-----------|-----------------|----------|---------------|-------------|----------|
| Specify | 0 | Yes | Yes | No | No | No | 20 |
| Research | 1 | Yes | No | No | No | No | 50 |
| Plan | 2 | Yes | No | No | No | No | 50 |
| Implement | 3 | No | No | Yes | Yes | Yes | 50 |
| Review | 4 | No | No | Yes | No | Yes | 30 |
| Validate | 5 | No | No | Yes | No | No | 50 |
| Done | 99 | N/A | N/A | No | No | No | N/A |

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
- **Only applied if `update_issue_body: true`** (Specify only)
- Other stages: warning logged, markers stripped, issue body untouched
- Enforced by the engine — skills can't override this

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

When `FABRIK_STAGE_COMPLETE` is detected:
1. Lock released (`fabrik:locked:<user>` and `stage:<name>:in_progress` removed)
2. Retry tracking cleared
3. Draft PR created (if `create_draft_pr: true`)
4. PR marked ready (if `mark_pr_ready_on_complete: true`)
5. `stage:<name>:complete` label added
6. Auto-advance to next stage (if `auto_advance: true` or global `yolo`)

### Blocked-on-Input Path

When `FABRIK_BLOCKED_ON_INPUT` is detected (and Claude ran without error):
1. `fabrik:paused` + `fabrik:awaiting-input` labels added
2. Lock released
3. Retry count NOT incremented, no `stage:<name>:failed` label
4. Issue waits until user comments (auto-detected, see Comment Processing)

### Incomplete Path (No Marker)

1. WIP commit (unless read-only): `git add -A && git commit -m "WIP: ..."`
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
   - `FABRIK_ISSUE_UPDATE` markers: applied if `update_issue_body: true`, stripped regardless
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
| Issue body update | If `update_issue_body: true` | If `update_issue_body: true` |
| Output destination | Stage comment or PR | Rewrite stage comment or new issue comment |
| Lock | `fabrik:locked:<user>` | `fabrik:editing` |
| Reaction flow | Comments marked seen (rocket) | Eyes → editing → rocket |

---

## Phase 6: Cleanup Stage (Done)

The Done stage (`cleanup_worktree: true`) is terminal:
- No Claude invocation, no lock, no labels
- Skipped entirely if no worktree exists for the issue — no worktree means there's nothing to clean up
- Removes worktree directory when it exists
- Adds `stage:Done:complete` label
- Respects `fabrik:paused` (skips if paused)

---

## Markers Reference

| Marker | Direction | Purpose | Where Checked |
|--------|-----------|---------|---------------|
| `FABRIK_STAGE_COMPLETE` | Claude -> Engine | Stage finished successfully | Stage runs AND comment processing |
| `FABRIK_BLOCKED_ON_INPUT` | Claude -> Engine | Stage needs user input | Stage runs only |
| `FABRIK_ISSUE_UPDATE_BEGIN/END` | Claude -> Engine | Updated issue body | Stage runs AND comment processing (gated by `update_issue_body`) |
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
update_issue_body: false    # Allow FABRIK_ISSUE_UPDATE to modify issue body (Specify only)
post_to_pr: false           # Route output to linked PR
create_draft_pr: false      # Create draft PR on completion
mark_pr_ready_on_complete: false  # Mark PR ready on completion
auto_advance: null          # Override global yolo (true/false/null)
cleanup_worktree: false     # Terminal stage — remove worktree
completion:
  type: claude              # Only supported type
```

Either `skill` or `prompt` is required (unless `cleanup_worktree` is true). When `skill` is set, the engine sends a directive prompt and the skill is loaded via `--plugin-dir`.
