# Fabrik Stage Lifecycle

This document describes what the Fabrik engine does before, during, and after each stage invocation. It is intended as a reference for writing and refining stage skills — understanding what Claude inherits and what the engine expects helps craft better instructions.

---

## Phase 1: Pre-Stage Setup

### Item Qualification

Before any work begins, two filters run:

1. **Shallow pre-filter** (`itemMayNeedWork`): Uses board data only (no comments). Skips items that have no matching stage, haven't changed since last poll (unless cooldown retry), are paused, or are locked by another user. Does NOT filter on stage completion labels — completed items may still have new comments.

2. **Deep filter** (`itemNeedsWork`): After fetching comments via `FetchItemDetails` for items that pass the shallow check. New comments trigger processing even on completed stages. PRs only support comment processing (no stage invocation).

The two-phase approach minimizes GitHub GraphQL rate limit cost — only items that might need work get the expensive detail fetch (~2 points each vs ~1,200 for fetching everything).

### Lock & Label Acquisition

- `fabrik:locked:<user>` label added — prevents other Fabrik instances from picking up the issue
- `stage:<name>:in_progress` label added — signals active work on the board
- Both labels held through cooldown retries (not released until completion or permanent failure)

### Unpause Detection

If the issue was previously paused by max retries (`fabrik:paused` + `stage:<name>:failed`), and the user has since removed `fabrik:paused`, the engine:
- Removes the failed label
- Resets retry count and cooldown timer
- Allows immediate retry

### Worktree Setup

Each issue gets an isolated git worktree:
- **Path**: `.fabrik/worktrees/issue-<N>`
- **Branch**: `fabrik/issue-<N>`
- **First run**: Branch created from `origin/main`, then rebased onto latest `origin/main`
- **Retry** (`attempted=true`): Worktree returned as-is — no rebase, no fetch. This preserves Claude's context and avoids pulling in unrelated changes mid-session.
- **Rebase conflicts**: Silently aborted (rebase --abort) — Claude works from current base, a later stage can rebase.

### Read-Only Stage Stashing

If the stage is marked `read_only: true`:
- Any dirty state (modified + untracked files) is auto-stashed before Claude runs
- Claude sees a clean worktree for analysis
- Stash is restored after Claude finishes

### Session Resume

- Session file: `~/.fabrik/sessions/issue-<N>/<stageName>.session`
- On retry (`attempted=true`): The saved session ID is loaded and passed to Claude via `--resume`, restoring the multi-turn conversation context
- On first run: No resume flag — fresh conversation

### Model Override

- Labels matching `model:<name>` on the issue override the stage's configured model
- First match wins; multiple labels log a warning

---

## Phase 2: Claude Invocation

### Prompt Construction

When a stage has a `skill:` field set (recommended), the prompt is a minimal directive:

```
You are operating as the Fabrik <StageName> agent for issue #<N>.
Follow the instructions in the <skill-name> skill exactly.

---
# Issue #N: <title>
URL: <url>

## Spec / Issue Body
<full issue body>

## Labels
<comma-separated labels>

## Prior Discussion
<all comments — author, timestamp, body>

## New Comments
<only unprocessed comments>

---
[If post_to_pr]: Instructions for FABRIK_SUMMARY markers
Completion instruction: "end your response with FABRIK_STAGE_COMPLETE"
```

When a stage uses an inline `prompt:` field (legacy), the prompt text replaces the skill directive at the top.

The skill itself is auto-loaded by Claude Code via the `--plugin-dir` mechanism. It contains the detailed methodology, quality checklists, scope boundaries, and common pitfalls for the stage.

**Key implication for skills**: Claude receives the full issue body, all prior comments, and any new comments in the prompt. The skill should focus on *methodology and quality standards*, not on describing what context is available.

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

The `--plugin-dir` is auto-detected from `.fabrik/plugin/` in the repo root (created by `fabrik init`) and resolved to an absolute path since Claude runs in the worktree.

### Execution Mode

Claude runs directly via `exec.CommandContext` with stdin piped and stdout captured:
- Uses `--output-format json`, output captured in memory
- Stderr goes to log file only (TUI mode) or stderr + log file (plain mode)

### Logging

- Session logs saved to: `~/.fabrik/logs/issue-<N>/<label>-<timestamp>.log`
- Debug output (if enabled): `.fabrik/debug/issue-<N>_<epoch>_<label>.log`

---

## Phase 3: Post-Stage Handling

### Output Parsing

Three JSON formats are supported (tried in order):
1. Single result object: `{"result": "...", "session_id": "..."}`
2. JSON array: `[{"type":"system",...}, ..., {"type":"result","result":"..."}]`
3. NDJSON (stream-json): One JSON object per line, last `type: "result"` used

If parsing fails: A short error message is posted instead of the raw output. Full output is in the log files. Raw JSON is never posted as a comment.

### Issue Body Update

Before posting output, the engine checks for `FABRIK_ISSUE_UPDATE_BEGIN`/`END` markers:
- If found: Extracts the content and updates the issue body via the GitHub API
- The markers and their content are stripped from the output before posting as a comment
- This allows stages like Specify, Research, and Plan to refine the issue body

### Git Metadata Capture

After Claude finishes, the engine captures:
- Current branch name
- Short commit SHA (8 chars)
- UTC timestamp

These appear in the comment header for traceability.

### Output Posting

**If `post_to_pr: true`**:
- Full output posted on the linked PR
- Brief summary (from `FABRIK_SUMMARY_BEGIN`/`END` markers) posted on the issue
- Falls back to posting on issue if no PR found

**Otherwise**:
- Full output posted directly on the issue

**Format**: `🏭 **Fabrik — stage: <name>** *branch: X | commit: Y | timestamp*`

**Footer**: Stats appended — turns used (with max), input/output tokens, completion status.

**Truncation**: Output capped at ~60K characters (GitHub comment limit).

### When Claude Completes Successfully

The engine detects completion by matching `^FABRIK_STAGE_COMPLETE$` (on its own line) in Claude's output. When found:

1. **Lock released** — `fabrik:locked:<user>` and `stage:<name>:in_progress` removed
2. **Retry tracking cleared** — count and paused state reset
3. **Draft PR creation** (if `create_draft_pr: true`):
   - Creates PR with title matching issue title
   - Body includes `Closes #<N>` to link issue
   - Idempotent — skips if PR already exists
4. **Mark PR ready** (if `mark_pr_ready_on_complete: true`):
   - Transitions draft to ready-for-review
   - Triggers external review bots/CI
5. **Stage completion label** — `stage:<name>:complete` added
6. **Auto-advance** (if `auto_advance: true` or global `yolo`):
   - Moves issue to next stage column on the project board
   - Otherwise logs "waiting for human to advance"

### When Claude Does Not Complete (Max Turns / Error)

1. **WIP commit** (unless read-only stage):
   - `git add -A && git commit -m "WIP: <stageName> stage incomplete (partial progress)"`
   - Preserves partial work
2. **Branch pushed** — even on failure, work is preserved on remote
3. **Cooldown timer set** — `pollSeconds * 10` seconds before retry
4. **Lock held** — issue stays locked through cooldown (prevents duplicate dispatch)
5. **Retry count incremented**
6. **If max retries exceeded** (`count >= max_retries`):
   - `fabrik:paused` label added
   - `stage:<name>:failed` label added
   - Explanatory comment posted
   - Lock released (permanently giving up)

### Branch Pushing

The branch is **always pushed** after Claude runs (success or failure):
- `git push --force-with-lease -u origin fabrik/issue-<N>`
- Ensures work is never lost, even on crashes

---

## Phase 4: Comment Processing

Comment processing is triggered when new comments from the configured user are found on an issue or its linked PRs. It runs independently of stage processing — even completed stages can process new comments.

### Detection

A comment is "new" if it:
- Is authored by the configured user
- Is not in the processed set
- Doesn't start with `🏭 **Fabrik` (skip Fabrik's own output)
- Doesn't have a ROCKET reaction (skip already-processed)

### Flow

1. **Eyes reaction** added to all new comments
2. **`fabrik:editing` label** added to issue
3. **Worktree prepared** (fresh rebase, not a retry)
4. **Claude invoked** with comment-review prompt:
   - Uses `stage.CommentPrompt` if defined, otherwise default prompt
   - Default prompts instruct Claude to: read comments, perform actions, update issue/PR body
   - Always resumes existing session
5. **Parse output** for `FABRIK_ISSUE_UPDATE_BEGIN`/`END` markers:
   - If found: Update issue/PR body with extracted content
   - If not found: Post Claude's output as a comment
6. **`fabrik:editing` label** removed
7. **Rocket reaction** added to processed comments
8. **Comments marked processed** in memory

### Comment Prompt Construction

```
1. Comment prompt (stage config or default)
2. ---
3. # Issue/PR #N: <title>
   URL: <url>
4. ## Current Issue/PR Body
   <full body>
5. ## New Comments to Process
   <each comment with author, timestamp, body>
6. ---
7. Instructions for body updates (FABRIK_ISSUE_UPDATE markers)
8. Note: "Include ENTIRE body in update, not just changed parts"
```

### Key Differences from Stage Processing

| Aspect | Stage Processing | Comment Processing |
|--------|-----------------|-------------------|
| Session | Fresh or resume on retry | Always resume |
| Worktree update | Skip on retry | Always rebase |
| Completion marker | `FABRIK_STAGE_COMPLETE` required | Not required |
| Issue body update | Via `FABRIK_ISSUE_UPDATE` markers (extracted and applied) | Via `FABRIK_ISSUE_UPDATE` markers (extracted and applied) |
| Reaction flow | Labels only | Eyes -> editing -> Rocket |
| Lock | `fabrik:locked:<user>` | `fabrik:editing` |

### Known Limitation

Comments processed via the Rocket reaction flow are not visible to subsequent stage retries. The stage prompt only includes `newComments` (unprocessed ones) — previously-processed comments are filtered out. This means a stage retry after comment processing may not see the user's answers. This will be addressed by context files (#99).

---

## Phase 5: Cleanup Stages

Cleanup stages (`cleanup_worktree: true`) are terminal stages like Done:
- No Claude invocation, no lock, no in_progress label
- Remove the worktree directory (unconditionally — dirty state is discarded since the issue has completed all active stages)
- Add `stage:<name>:complete` label
- Respect `fabrik:paused` — skip if paused

---

## Markers Reference

| Marker | Direction | Purpose |
|--------|-----------|---------|
| `FABRIK_STAGE_COMPLETE` | Claude -> Engine | Signals stage finished successfully (must be on its own line) |
| `FABRIK_SUMMARY_BEGIN` / `END` | Claude -> Engine | Brief summary for issue when `post_to_pr: true` |
| `FABRIK_ISSUE_UPDATE_BEGIN` / `END` | Claude -> Engine | Updated issue body (works in both stage runs and comment processing) |

## Labels Reference

| Label | Set by | Purpose |
|-------|--------|---------|
| `fabrik:locked:<user>` | Engine | Lock during stage processing |
| `fabrik:editing` | Engine | Lock during comment processing |
| `fabrik:paused` | Engine or User | Pause processing (engine sets on max retries) |
| `stage:<name>:in_progress` | Engine | Stage actively running |
| `stage:<name>:complete` | Engine | Stage completed successfully |
| `stage:<name>:failed` | Engine | Stage hit max retries |
| `model:<name>` | User | Override Claude model for this issue |

## Stage YAML Options

```yaml
name: Research              # Required: matches board column name
order: 2                    # Required: processing priority (lower = earlier)
skill: fabrik-research      # Plugin skill name (recommended, alternative to prompt)
prompt: |                   # Inline prompt (legacy, used when skill not set)
  ...
model: sonnet               # Optional: Claude model
max_turns: 50               # Optional: turn limit per invocation
allowed_tools:              # Optional: restrict Claude's tools
  - Read
  - Grep
comment_prompt: |           # Optional: custom prompt for comment processing
  ...
read_only: false            # Stash/restore worktree (for analysis stages)
post_to_pr: false           # Route output to linked PR
create_draft_pr: false      # Create draft PR on completion
mark_pr_ready_on_complete: false  # Mark PR ready on completion
auto_advance: null          # Override global yolo (true/false/null)
cleanup_worktree: false     # Terminal stage — remove worktree instead of running Claude
completion:
  type: claude              # Only supported type
```

Either `skill` or `prompt` is required (unless `cleanup_worktree` is true). When `skill` is set, the engine sends a directive prompt and the skill is loaded via the `--plugin-dir` mechanism.
