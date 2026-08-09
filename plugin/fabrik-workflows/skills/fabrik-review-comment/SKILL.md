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

Comments without `**File:**` / `**Diff context:**` headers are regular PR body or issue comments — handle them as before, **unless** they carry a `[Bot Review Finding]` marker (see below).

## Bot-Authored Findings (Not a User Decision)

A comment marked `[Bot Review Finding]` — e.g. `**@copilot-pull-request-reviewer[bot]** (2026-01-15 10:30) [Bot Review Finding]:` — is **not** a human's decision on a prior finding. It is a bot review's own content, surfaced to you in one of two ways:

- A plain PR body or issue comment with no formal review submission at all (e.g. `@copilot review` posting its findings as a regular comment instead of a review).
- The body of a `COMMENTED` or `APPROVED` review from a reviewer whose body carries substantive findings (e.g. Pruefer).

The rest of this skill's "act on the user's decision" framing — fix / dismiss / defer / clarify — does not apply to these. There is no user decision to act on yet; the bot's content **is** the finding. Treat it the same way you treat an inline review thread comment:

1. **Evaluate each finding on its merits** — read the bot's comment and judge whether it identifies a real, actionable problem.
2. **Fix valid findings autonomously** — apply the minimal targeted fix, the same as you would for a `**File:**`-tagged thread comment. Do not wait for a human to confirm the bot is right first; that is the gap this marker exists to close (a prior version of this skill refused with "this is new feedback from a bot, not an explicit decision from you," which left real findings unaddressed until the review gate timed out).
3. **No actionable findings → see the No-Op Contract below.** A generic "Pull request overview" or "LGTM" summary with nothing concrete to act on is common from some bots (Copilot, Gemini) — recognize it as such and do not manufacture a response.

This carve-out is scoped to comments carrying the `[Bot Review Finding]` marker only. A comment from a human, or a bot comment without the marker (older engine builds, or a delivery path that doesn't apply it), still follows the ordinary "act on the user's decision" rules above.

## No-Op Contract

If, after evaluating a bot review's findings (marked or unmarked) or a user's comment, you conclude there is **nothing actionable** — no valid finding to fix, no change the codebase needs — then **change nothing and complete**. Do not:

- Invent a plausible-sounding fix for feedback that didn't actually ask for one.
- Make a speculative change "just in case" it's what the reviewer meant.
- Push a commit to demonstrate activity when the correct action is no action.

A confabulated commit on a PR that's about to merge is worse than doing nothing: it can draw a fresh bot review with a new `DatabaseID`, which bypasses dedup and consumes another review cycle on feedback that was never real in the first place. "I reviewed this and found nothing to change" is a complete, correct response — say so in your output and stop there.

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

### Verifying with a live server or a long-running command

If confirming a fix needs a running instance of the managed app (e.g. a `npm run dev` dev server), do not start it in the background and continue in a later tool call. Claude Code's background-bash detaches the process into its own session (`setsid`), so it survives across tool calls — and outlives the stage. The engine's stage-end teardown kill is process-group scoped and cannot reach a `setsid`'d process, so a backgrounded server left running this way becomes an orphan holding a port on the host indefinitely.

In preference order:

1. **Prefer one-shot verification.** Use the framework's build or check command instead of a long-lived dev server — e.g. `npm run build` (or the framework's equivalent), or a bounded-lifetime preview command like `vite preview`.
2. **If a live server is genuinely needed** (e.g. an HTTP health check), bracket it in a single command with guaranteed teardown:
   ```bash
   npm run dev --port "$PORT" & DEV=$!
   trap 'pkill -P "$DEV"; kill "$DEV" 2>/dev/null' EXIT
   # health-check / curl / run the verification here
   ```
3. **If a persistent server is unavoidable, bound it with a timeout** so it self-terminates:
   ```bash
   timeout --signal=KILL <N> npm run dev …
   ```
4. **The same discipline applies to test suites, benchmarks, and CI waits.** Run them synchronously in the foreground with the framework's own timeout flag (e.g. `go test -timeout`, `pytest --timeout`, `jest --testTimeout`) so the outcome is known before the turn ends. Prefer this over `timeout(1)` — it's GNU coreutils and is absent on stock macOS, so relying on it can fail with `command not found` and tempt a fallback to backgrounding.
5. **If it won't fit in one turn even with a timeout, reduce scope** — fewer tests, a subset of the suite — rather than backgrounding it.
6. **If backgrounding is truly unavoidable, "wait for a completion notification" is never a valid terminal strategy in a headless stage.** There is no interactive session to deliver it, so the stage ends without `FABRIK_STAGE_COMPLETE`. Poll a concrete completion marker (an exit-code file, a `.rc` file, an explicit `wait $PID`) against a wall-clock deadline, and produce output every poll cycle rather than going silent.

**Never end a turn waiting on a background task or a CI run.** Never wait for CI — emit `FABRIK_STAGE_COMPLETE`; the engine gates on CI via `wait_for_ci` and `fabrik:awaiting-ci`. The same applies to a backgrounded local task: if its result is genuinely required, poll for it within the same turn against a wall-clock deadline instead of ending the turn to wait for it.

This paragraph's `FABRIK_STAGE_COMPLETE` reference describes the engine's general CI-wait mechanism — it does not override the "Completion" rule below, which governs whether comment processing may emit that marker at all.

## Numbering findings in your output

When you list or summarize multiple review findings (e.g., distinguishing one Copilot comment from another, or grouping Gemini suggestions), **do not use bare `#N` ordinals**. GitHub's issue renderer interprets any bare `#N` token in a comment body as a cross-reference to issue/PR N in the same repository. Unrelated issues get auto-linked with their titles appearing in hovercards or inlined in reader views, which looks like you're quoting work that has nothing to do with the current issue.

Use bracketed or descriptive numbering instead:

- ✅ `Gemini [1]`, `Gemini finding 1`, `Copilot (thread 2)`
- ❌ `Gemini #1`, `Copilot #2`

The same rule applies any time you number something in output that posts to a GitHub comment — threads, files, findings, or list items.

## Labels You Interact With

- **`fabrik:awaiting-review`** — the review-gate label; may already be present if you're processing a comment from a reviewer rather than a user decision on findings. You don't act on it directly.
- **`fabrik:bot-reprompted`** — may be present if the engine already re-prompted an unresponsive bot reviewer this gate cycle; informational only from this skill's perspective.

See `../../LABELS.md` for the full label reference.

## Completion

Do NOT output `FABRIK_STAGE_COMPLETE`. Comment processing in Review returns control to the engine without advancing the pipeline. The Review stage continues until all findings are resolved and the main Review workflow signals completion.

## If You Hit the Turn Limit

Comment processing runs on a smaller budget than a full stage (`comment_max_turns`, default `min(max_turns, 15)`), so it is easy to reach. That budget is a **time-slicer**, not a failure threshold: if you run out of turns the engine preserves your work and the next invocation **resumes this same session**. Continue from where you stopped rather than restarting.

Prefer committing incremental progress over trying to finish everything in one slice.

## What You Do NOT Do

- **Do not signal stage completion** — never output `FABRIK_STAGE_COMPLETE`
- **Do not apply fixes the user did not request** — act only on what was explicitly decided. This does not apply to `[Bot Review Finding]`-marked content (see the "Bot-Authored Findings" section above): those are evaluated and fixed autonomously on their merits, not gated on a user decision.
- **Do not confabulate a fix for a bot comment with no actionable findings** — see the No-Op Contract above; change nothing and say so.
- **Do not leave uncommitted changes** — always commit and push before returning
- **Do not re-run the full review** — focus on the specific findings the user addressed
- **Do not make unrelated changes** while applying fixes
- **Never background a dev server, test suite, benchmark, or CI wait and continue in a later tool call (or wait for a completion notification) to verify a fix** — a backgrounded dev server detaches via `setsid` and outlives the stage, becoming an orphaned process holding a port; a backgrounded long-running command left to "wait for a completion notification" simply ends the stage silently, since there is no interactive session to deliver that notification. See "Verifying with a live server or a long-running command" above.
- **Never post stage output directly to GitHub using `gh pr comment`, `gh issue comment`, `gh pr review`, or any equivalent tool that creates a comment on the issue or linked PR.** Doing so bypasses Fabrik's engine-side comment formatting, produces duplicate comments, and triggers a self-review loop on the next poll (the engine treats your directly-posted comment as new user input).

  Write all stage output to stdout only. The Fabrik engine captures stdout and posts it as a properly formatted `🏭 **Fabrik — stage: <Name>**` comment.

  **Exception — review thread resolution**: Resolving a PR review thread via `gh api GraphQL` (e.g., the `resolveReviewThread` mutation) is permitted. Only *comment creation* is prohibited, not *thread resolution*.
