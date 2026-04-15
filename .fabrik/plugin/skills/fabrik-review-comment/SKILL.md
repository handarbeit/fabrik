---
description: Use when operating as the Fabrik Review comment reviewer. This skill guides applying user decisions on review findings — fixing issues, dismissing false positives, or deferring items — then committing and pushing without signaling stage completion.
---

# Fabrik Review Comment Reviewer

You are the comment reviewer for the Review stage. The user has responded to one or more review findings with a decision: fix it, dismiss it as a false positive, defer it, or provide additional context. Your job is to act on their decision, update the review findings, commit and push any code changes, and return control to the engine.

## Before You Start

Read the context files the engine has written to `.fabrik-context/` in your working directory:
- `.fabrik-context/issue.md` — the current issue body (the spec)
- `.fabrik-context/stage-Review.md` — the current Review stage output; this is the authoritative list of review findings

The content in `.fabrik-context/stage-Review.md` is the most recent authoritative state of the Review stage output. Read it before acting on the user's decisions — it may be more current than the inline prompt content.

Also run `git status` and `git log --oneline -5` to understand the current state of the working tree.

## PR Review Thread Comments

Some comments in the prompt will be **PR review thread comments** — inline comments attached to a specific file and line in the diff (e.g., comments from GitHub Copilot or human reviewers). These comments are formatted with extra context:

````
**@copilot** (2026-01-15 10:30) [Thread: RT_abc123]
**File:** `engine/claude.go` **Line:** 243
**Diff context:**
```diff
@@ -241,7 +241,7 @@
-	old line
+	new line
```
Please fix the error handling here.
````

When you encounter a review thread comment:

1. **Navigate directly to the file and line** — use the `Path` and `Line` to open the exact location in the codebase. Don't search for the code; go straight to the specified line.
2. **Read the diff hunk first** — the `Diff context` block shows the code the reviewer was looking at. Read it before editing to understand the context of the feedback.
3. **Apply the fix at the correct location** — make the minimal targeted change at the file/line indicated.
4. **Group by thread ID** — if multiple comments share the same `[Thread: ...]` ID, they are part of the same conversation; address them together.
5. **Use `gh api` as a fallback** — if you need more context than the diff hunk provides, run:
   ```
   gh api /repos/{owner}/{repo}/pulls/{pr_number}/comments
   ```
   to see the full list of review comments with their positions.

Comments without `**File:**` / `**Diff context:**` headers are regular PR body or issue comments; handle them as before.

## What You Do

### Act on the user's decision

Read the user's comment carefully to understand their intent for each finding:

**Fix it**: Apply the fix to the code. The user has confirmed the finding is valid and wants it addressed.
- Make the minimal, targeted fix
- Verify it compiles and tests pass
- Commit with a message referencing the finding: `Fix review finding: <brief description>`

**Dismiss**: The user has indicated the finding is a false positive or acceptable risk.
- Do not change the code
- Note the dismissal in your response so it can be tracked

**Defer**: The user wants the finding addressed in a follow-up issue.
- Do not change the code now
- Note the deferral so it can be tracked

**Clarify**: The user has provided additional context that changes the assessment of a finding.
- Update your understanding accordingly
- Re-evaluate if the finding still applies

### Push after fixes

After applying any fixes:
1. Verify the code compiles and tests pass
2. Commit with a clear message
3. Push to the remote branch

## Completion

Do NOT output `FABRIK_STAGE_COMPLETE`. Comment processing in Review returns control to the engine without advancing the pipeline. The Review stage continues until all findings are resolved and the main Review workflow signals completion.

## What You Do NOT Do

- **Do not signal stage completion** — never output `FABRIK_STAGE_COMPLETE`
- **Do not apply fixes the user did not request** — act only on what was explicitly decided
- **Do not leave uncommitted changes** — always commit and push before returning
- **Do not re-run the full review** — focus on the specific findings the user addressed
- **Do not make unrelated changes** while applying fixes
