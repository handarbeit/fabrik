---
description: Use when operating as the Fabrik Review comment reviewer. This skill guides applying user decisions on review findings — fixing issues, dismissing false positives, or deferring items — then committing and pushing. It also handles engine-triggered re-invocation to address PR review feedback from bots and human reviewers, signaling FABRIK_STAGE_COMPLETE when all review feedback is resolved.
---

# Fabrik Review Comment Reviewer

You may be invoked in two distinct modes. **Determine which mode you are in before acting.**

## Mode Detection

Examine the incoming comments passed to you:

- **Engine re-invocation mode**: Comments contain PR review bodies. These are identified by bodies starting with `[PR Review —` (e.g. `[PR Review — CHANGES_REQUESTED]`). This means the engine has waited for all pending reviewers to submit and is now asking you to address their feedback.

- **User decision mode**: Comments contain human replies to specific review findings (e.g., "fix this", "dismiss as false positive"). The body does NOT start with `[PR Review —`.

---

## Before You Start (both modes)

Read the context files the engine has written to `.fabrik-context/` in your working directory:
- `.fabrik-context/issue.md` — the current issue body (the spec)
- `.fabrik-context/stage-Review.md` — the current Review stage output; the authoritative list of findings

Run `git status` and `git log --oneline -5` to understand the current state of the working tree.

---

## Engine Re-invocation Mode

The engine has collected PR reviews from bots and human reviewers and is asking you to address them. Your job is to read all the review feedback, fix any valid issues, and signal completion.

### Step 1: Gather all review feedback

The synthetic comments passed to you contain the review bodies. Additionally, fetch inline code review comments via:

```bash
gh pr view --json reviewComments --jq '.reviewComments[] | {author: .author.login, path: .path, line: .line, body: .body}'
```

This surfaces inline diff-level comments (line-specific feedback) that are not included in the review body.

Also fetch the full reviews for context:

```bash
gh pr view --json reviews --jq '.reviews[] | {author: .author.login, state: .state, body: .body}'
```

### Step 2: Triage the feedback

For each piece of review feedback:

- **CHANGES_REQUESTED / specific issues**: These require code changes. Apply targeted fixes.
- **APPROVED with comments**: Comments are suggestions, not blockers. Use your judgment on whether to apply them.
- **COMMENTED**: Read carefully — these may flag real issues or be informational.

Inline comments (from `reviewComments`) are particularly important as they pinpoint specific code locations.

### Step 3: Apply fixes

For each valid finding:
1. Make the minimal, targeted fix
2. Verify the code compiles and tests pass
3. Commit with a clear message: `Fix review feedback: <brief description>`

### Step 4: Push

After applying all fixes:
```bash
git push
```

### Completion in Engine Re-invocation Mode

After addressing all review feedback (fixing valid issues, and noting any that are intentionally acceptable):

Output `FABRIK_STAGE_COMPLETE` on its own line to signal that review feedback has been processed. The engine will then re-check for any new pending reviewers before advancing the pipeline.

**Important**: Only output `FABRIK_STAGE_COMPLETE` when all PR review feedback has been addressed (fixes applied, or consciously accepted/dismissed with justification). If you could not address the feedback (e.g., the changes requested are unclear or contradictory), describe the blocker and do NOT output `FABRIK_STAGE_COMPLETE`.

---

## User Decision Mode

The user has responded to one or more review findings with a decision: fix it, dismiss it as a false positive, defer it, or provide additional context.

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

### Completion in User Decision Mode

Do NOT output `FABRIK_STAGE_COMPLETE`. Comment processing in Review returns control to the engine without advancing the pipeline. The Review stage continues until all findings are resolved and the main Review workflow signals completion.

---

## What You Do NOT Do (both modes)

- **Do not apply fixes the user/reviewer did not request** — act only on what was explicitly decided or requested
- **Do not leave uncommitted changes** — always commit and push before returning
- **Do not re-run the full review** — focus on the specific feedback provided
- **Do not make unrelated changes** while applying fixes
- **In user decision mode**: never output `FABRIK_STAGE_COMPLETE`
- **In engine re-invocation mode**: always output `FABRIK_STAGE_COMPLETE` when done (even if no fixes were needed — this advances the cycle)
