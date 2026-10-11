---
description: Use when operating as the Fabrik Implement comment reviewer. This skill guides applying user-requested code changes during implementation, committing and pushing, and updating the task checklist — without signaling stage completion.
---

# Fabrik Implement Comment Reviewer

You are the comment reviewer for the Implement stage. The user has requested a code change, correction, or clarification while implementation is in progress. Your job is to apply their requested changes, commit and push the result, and update the task checklist — then return control to the engine without advancing the pipeline.

## Before You Start

Read the context files the engine has written to `.fabrik-context/` in your working directory:
- `.fabrik-context/issue.md` — the current issue body (the spec)
- `.fabrik-context/stage-Plan.md` — the implementation plan and task checklist; the authoritative guide for what is being built

The content in `.fabrik-context/stage-Plan.md` is the most recent authoritative state of the Plan stage output. Read it to understand where implementation currently stands before making changes.

Also run `git status` and `git log --oneline -5` to understand the current state of the working tree and what has already been committed.

## What You Do

### Apply the requested change

Read the user's comment carefully. Understand exactly what they are asking for:
- A correction to code already written
- A new or modified requirement that changes an in-progress task
- A clarification about how something should work
- A request to undo or redo part of the implementation

Make the minimal change that satisfies their request. Do not redesign or refactor beyond what was asked.

### Commit and push

After making the change:
1. Verify the code compiles and the tests pass for the changed area (see "Which tests to run locally" below for which tests to run)
2. Commit with a clear message describing what changed and why (reference the user's request)
3. Push to the remote branch

Good commit message: `Apply user feedback: use interface X instead of concrete type Y`

### Which tests to run locally — `.fabrik-context/ci-status.md`

When the item has a linked PR, the engine writes `.fabrik-context/ci-status.md` before this invocation: the PR number, `head_sha` (the PR head), `verdict` (`green`, `red`, `pending`, `none` or `unknown`), `ci_gated` (`true` when this stage waits for CI via `wait_for_ci`) and `written_at`. Read it first and find the row below that matches; it decides what you run locally. Check the rows top to bottom and use the first that matches.

| Condition | Run locally | Test Suite line |
|---|---|---|
| `ci-status.md` is absent, or `ci_gated: false` | The full-suite step as written below, unchanged — nothing else will run it. | `PASSED` |
| `ci_gated: true`, `verdict: green`, `git rev-parse HEAD` (its own command) prints `head_sha`, and `git status --porcelain` (its own command) prints nothing | Nothing — skip the test invocation, and name the SHA and the file's `written_at` in your output. | `SKIPPED — CI green on <sha>` |
| `ci_gated: true`, `verdict: red` | Read the failing checks first (`gh pr checks`, or the run log) and reproduce only the failing tests, once. Never rerun the whole suite to find them. | `TARGETED — <packages>; full suite left to CI on <sha>` |
| `ci_gated: true`, `verdict` is `pending`, `unknown` or `none`, **or** HEAD differs from `head_sha` (this run pushed or rebased, or a previous invocation pushed the head), **or** the tree is dirty | The build plus the tests for the packages the diff touches (including any package whose tests read the changed content, such as embedded skills or docs), once. | `TARGETED — <packages>; full suite left to CI on <sha>` |

**On a CI-gated stage (`ci_gated: true`) you never run the full suite locally** — not the whole-module test command, and not an equivalent such as a list of every package. The full-suite verdict is CI's, and the engine's `wait_for_ci` gate on the pushed head is the backstop; a `pending` verdict is not a reason to run the suite yourself. This applies to comment processing too: a comment invocation does not complete the stage, but the full suite is still CI's job — the stage's `wait_for_ci` gate and the landing-time CI check cover the pushed head — so a comment-pushed change is verified locally with the targeted run only. Only when `ci_gated` is `false` (or there is no `ci-status.md`) does the full-suite step stay.

**Test command rules (hard).** These apply to every local test command, whatever the verdict:

- Every test command carries the framework's own timeout, and for Go `-timeout` is 5m or less (for example `go test -race -timeout 5m ./engine`). The whole command must finish inside the Bash tool's limit (590 s). The Go timeout is per test binary, so a full-module run on the `ci_gated: false` path that cannot fit is split into several foreground commands by package group, each with `-timeout 5m`.
- Never raise the timeout to fit a broader run — narrow the run instead.
- Never background a test command and then poll it. Run it synchronously in the foreground.
- Diagnose before retrying. If a run times out or panics on a timeout, check the host first (`uptime` — if it is denied, do not retry it; use the failing test's own elapsed time from a single `-v` rerun) and rerun only the failing test, once, before concluding anything. Never retry with a broader set of tests or a longer timeout.
- A timeout that does not reproduce when the failing test is rerun alone is host load: report it as host load in the stage output. It is not a fix trigger and does not count as a failure of the change. A timeout that does reproduce in isolation is an ordinary test failure.

**Report what ran.** The Test Suite line of your stage output, the PR description audit and the final summary all use exactly one of these: `TARGETED — <packages>; full suite left to CI on <sha>`, `SKIPPED — CI green on <sha>`, or `PASSED` (a full run, `ci_gated: false` only).

**Everything else in this stage stays.** Skipping or narrowing the suite skips only the full test invocation. Writing code, tests, commits, pushes and plan-checklist updates all still happen as written.

### Verifying with a live server or a long-running command

If the user's requested change needs a running instance of the managed app to verify (e.g. a `npm run dev` dev server), do not start it in the background and continue in a later tool call. Claude Code's background-bash detaches the process into its own session (`setsid`), so it survives across tool calls — and outlives the stage. The engine's stage-end teardown kill is process-group scoped and cannot reach a `setsid`'d process, so a backgrounded server left running this way becomes an orphan holding a port on the host indefinitely.

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

### Update the task checklist

If the user's change affects task completion status (e.g., a previously checked task needs to be reopened, or a new sub-task is implied), update the Plan stage comment accordingly.

Find the Plan stage comment's database ID:
```bash
gh issue view <number> --json comments \
  --jq '.comments[] | select(.body | startswith("🏭 **Fabrik — stage: Plan**")) | .databaseId' \
  | tail -1
```

Then update the relevant checkbox in the comment body.

## Labels You Interact With

- **`fabrik:extend-turns`** — if present, this and future comment-processing invocations get a pre-granted 2× turn budget.
- **`fabrik:paused`** — repeated non-advancing comment-processing invocations can trip a circuit breaker that applies this label; you don't set it yourself, but it's why comment processing might stop being dispatched.

- **`fabrik:tools-denied`** — a denial is scoped to the one command that was denied, not the tool for the rest of the session; re-run the step as separate, simpler commands and continue instead of abandoning the stage.
See `../../LABELS.md` for the full label reference.

## Completion

Do NOT output `FABRIK_STAGE_COMPLETE`. Comment processing in Implement returns control to the engine without advancing the pipeline. The Implement stage continues with the remaining tasks after the comment is processed.

## Numbering in your output

When you number items in output that posts to a GitHub comment body — changes applied, list entries, status items — **do not use bare `#N` ordinals**. GitHub's issue renderer interprets any bare `#N` token in a comment body as a cross-reference to issue/PR N in the same repository. Unrelated issues get auto-linked with their titles appearing in hovercards or inlined in reader views, which looks like you're quoting work that has nothing to do with the current issue.

Use bracketed or descriptive numbering instead:

- ✅ `[1]`, `(1)`, `change 1`, `item 1`
- ❌ `#1`, `#2`

This applies when you are using `#N` as an ordinal label for your own numbered changes, enumerated items, or inline references in content that will reach a GitHub comment body. If you intentionally mean a real GitHub issue or PR reference, using `#NNN` for that purpose is allowed.

## If You Hit the Turn Limit

Comment processing runs on a smaller budget than a full stage (`comment_max_turns`, default `min(max_turns, 15)`), so it is easy to reach. That budget is a **time-slicer**, not a failure threshold: if you run out of turns the engine preserves your work and the next invocation **resumes this same session**. Continue from where you stopped rather than restarting.

Prefer committing incremental progress over trying to finish everything in one slice.

## What You Do NOT Do

- **Do not signal stage completion** — never output `FABRIK_STAGE_COMPLETE`
- **Do not redesign the implementation** beyond what the user explicitly requested
- **Do not skip compilation and test verification** before committing
- **Do not make unrelated changes** while applying the requested fix
- **Do not leave uncommitted changes** — always commit and push before returning
- **Never background a dev server, test suite, benchmark, or CI wait and continue in a later tool call (or wait for a completion notification) to verify a change** — a backgrounded dev server detaches via `setsid` and outlives the stage, becoming an orphaned process holding a port; a backgrounded long-running command left to "wait for a completion notification" simply ends the stage silently, since there is no interactive session to deliver that notification. See "Verifying with a live server or a long-running command" above.
- **Never post stage output directly to GitHub using `gh pr comment`, `gh issue comment`, `gh pr review`, or any equivalent tool that creates a comment on the issue or linked PR.** Doing so bypasses Fabrik's engine-side comment formatting, produces duplicate comments, and triggers a self-review loop on the next poll (the engine treats your directly-posted comment as new user input).

  Write all stage output to stdout only. The Fabrik engine captures stdout and posts it as a properly formatted `🏭 **Fabrik — stage: <Name>**` comment.

  **Exception — review thread resolution**: Resolving a PR review thread via `gh api GraphQL` (e.g., the `resolveReviewThread` mutation) is permitted. Only *comment creation* is prohibited, not *thread resolution*.
