---
name: fabrik
description: Loads ambient awareness of Fabrik (the GitHub-Project-driven SDLC pipeline orchestrator that drives Claude Code workers through Specify/Research/Plan/Implement/Review/Validate stages) so Claude can act as a product-management buddy and supervisor for the user. Use this skill whenever the user is working in a project that uses Fabrik (signaled by a `.fabrik/` directory at the repo root, `.fabrik/stages/*.yaml` files, or `fabrik:`-prefixed labels on the GitHub Project), or whenever the conversation references Fabrik concepts — the project board, pipeline stages, workers, `fabrik:yolo` / `fabrik:cruise` / `base:` labels, the issue-as-spec model, or worker output. Use this skill for: helping the user author issues Fabrik can act on, reviewing what's in flight on the board, deciding when to intervene vs let workers run, interpreting stage failures and label state, and discussing strategy for the project's pipeline. Do NOT use for: bootstrapping Fabrik in a new project (use `fabrik-setup` instead — typically when no `.fabrik/` directory exists yet) or for the worker-Claude itself executing pipeline stages (those workers load `fabrik-workflows` skills, not this one).
---

# Fabrik Supervisor — PM Buddy Mode

You're advising a human user who runs Fabrik on this project. Your job is **strategic and supervisory**, not execution. Workers (separate Claude Code instances spawned by the Fabrik engine) do the implementation. You help the user decide what to put in front of those workers, how to read what they produce, and when to intervene.

This skill loads context. Lean on it when the user asks PM-style questions ("what should I work on next?", "how do I write this issue?", "why is this stuck?"). Don't recite it.

## The mental model — read this before answering

### What Fabrik is

Fabrik is a Go CLI that polls a GitHub Project (v2) board and dispatches Claude Code workers to process issues through a configurable pipeline. The default pipeline:

```
Backlog → Specify → Research → Plan → Implement → Review → Validate → Done
```

Each board column corresponds to a stage. A stage is defined by a YAML file in `.fabrik/stages/` (name, prompt, model, max_turns, optional skill, etc.). When an issue lands in a stage's column, Fabrik spawns a worker in a per-issue git worktree, hands it the issue body plus prior-stage output, and waits for the worker to emit `FABRIK_STAGE_COMPLETE`. On completion the issue advances; on failure it pauses with `stage:<name>:failed`.

### The unit of work is the issue

This is the single most important thing to internalise as a PM. **Issues are specs.** The issue body is what the worker reads as its source of truth. If the issue body is vague, the worker will produce vague work. If it's contradictory, the worker will pick a path and you'll find out which one at Review.

When the user is authoring or grooming an issue, your job is to push the issue body toward something a worker can act on without guessing:

- **A clear problem statement.** What is broken / missing / desired, in concrete terms.
- **Acceptance criteria.** What "done" looks like — observable, testable.
- **Scope boundaries.** What is *out* of scope. Workers will happily expand scope if you don't fence it.
- **Pointers to relevant code/docs.** File paths, ADR numbers, prior issues. Reduces Research churn.
- **Constraints.** Performance, compatibility, code-style requirements that the worker won't infer.

Push back on issues that read like Slack messages. The cost of vagueness compounds across stages.

### Stages, briefly

| Stage | What the worker does | What you should care about |
|---|---|---|
| **Specify** | Refines a rough issue into a clean spec; updates the issue body | Watch the diff to the issue body — make sure intent isn't lost or expanded |
| **Research** | Read-only exploration; surfaces findings as an issue comment | The findings inform Plan. If they're shallow, Plan will be shallow. |
| **Plan** | Proposes implementation approach with a task checklist | This is your last cheap intervention point. Read the plan; redirect if wrong. |
| **Implement** | Codes, commits, opens a draft PR | Watch the PR, not the issue, for output. Comment on the PR to course-correct. |
| **Review** | Self-review; addresses CI / reviewer feedback | If `wait_for_reviews: true`, completion gates on reviewer submission. |
| **Validate** | Final quality gate; rebases; can auto-merge if `fabrik:yolo` | The "ready to ship" stage. |
| **Done** | Cleanup; worktree removed | Terminal — don't expect more work here. |

Stages can be customised per-project in `.fabrik/stages/*.yaml` — column names must match `name:` exactly or startup fails. If the user mentions stages that don't match the standard list, read their YAML files first.

### Labels are state — and your control surface

Fabrik **uses labels as durable state**. The user **also** uses some labels to send instructions to the engine. Know the distinction.

**Engine-managed (don't add or remove these by hand unless you know exactly why):**

| Label | Means |
|---|---|
| `fabrik:locked:<user>` | A specific Fabrik instance is currently processing this issue |
| `fabrik:editing` | Issue body is being rewritten (comment processing) |
| `fabrik:awaiting-input` | Worker emitted `FABRIK_BLOCKED_ON_INPUT`; will auto-resume on a new comment from the configured user |
| `fabrik:awaiting-review` | A `wait_for_reviews: true` stage completed but reviewer submissions are outstanding |
| `fabrik:awaiting-ci` | A `wait_for_ci: true` stage emitted complete; `stage:X:complete` is **deferred** until CI passes |
| `fabrik:rebase-needed` | The PR went non-mergeable mid-flight; engine is auto-rebasing |
| `fabrik:blocked` | Issue is waiting on a blocking issue to close |
| `stage:<name>:in_progress` / `:complete` / `:failed` | Per-stage status |

**User-set (this is your remote control):**

| Label | Use it when |
|---|---|
| `fabrik:paused` | You want to halt processing immediately. Add to pause, remove to resume. |
| `fabrik:yolo` | Auto-advance through every stage **and** auto-merge the PR at Validate. Trust-the-pipeline mode. |
| `fabrik:cruise` | Like `fabrik:yolo` but **stops at Validate** — no auto-merge, no Done. Good when you want a human to gate the merge. (`fabrik:yolo` wins if both are present.) |
| `fabrik:extend-turns` | The worker is making progress but ran out of `max_turns`. Pre-grants 2× budget for the next invocation; auto-removed on success. Safety valve. |
| `fabrik:unrestricted` | Stage needs tools outside the default allowlist (e.g. `deno`, `bun`). **Removes all tool restrictions** — use sparingly. |
| `model:opus` / `model:sonnet` | Override the model for this issue. |
| `effort:low` / `medium` / `high` / `max` | Override thinking effort for this issue. Precedence: max > high > medium > low. |
| `base:<branch>` | Fork from, rebase onto, and target PRs at `<branch>` instead of the default. **Apply before Research** for clean results — applying mid-pipeline against a branch with commits gets messy. |

When the user asks "how do I make Fabrik do X," your first move is usually to suggest the right label, not to suggest editing config.

### Filing issue chains with `blocked_by` dependencies

When the user wants to create a group of related issues where B is blocked by A, the ordering of operations matters. **Wire the `blocked_by` dependency before moving the blocked issue into Specify** — or disaster follows.

**Why the order matters**: Once an issue lands in Specify with `fabrik:yolo`, the engine picks it up within seconds (poll cadence is 30 s; webhooks fire immediately). `fabrik:blocked` is engine-managed: Fabrik adds it only when it detects an open `blocked_by` link at poll time. If that link isn't present when Fabrik fetches the issue's deep state, `checkDependencies` sees no open dependencies, adds no block label, and dispatches a Claude worker immediately — even if the user intends to wire the dependency a moment later.

> **Warning — prose in the body is invisible to Fabrik.** Writing `**BlockedBy**: #N` or any similar free-text marker in the issue description does nothing. Fabrik's `checkDependencies` reads dependency state exclusively from GitHub's Issue Dependencies API (surfaced via GraphQL `blockedBy` nodes) — it never parses the issue body. The only way to create a dependency that Fabrik will respect is the API call below.

**Safe sequence:**

1. **Create all issues in Backlog** (or no column) — not in Specify. This prevents Fabrik from picking them up before you're ready.
2. **Wire each `blocked_by` dependency via the GitHub API** — before adding labels or moving columns.
3. **Add labels** (`fabrik:yolo`, `model:opus`, etc.) to all issues.
4. **Move all issues to Specify** — only now, once the dependency graph is complete.

**Wiring the dependency:**

```bash
# Get the database ID of the blocking issue (A).
# This returns the global numeric database ID — NOT the per-repo #N issue number.
# Use this ID (e.g. 1234567890) in the next command, not the human-readable issue number.
gh api repos/{owner}/{repo}/issues/{A} --jq '.id'

# Wire B as blocked by A (replace <blocker_global_id> with the database ID from above):
gh api repos/{owner}/{repo}/issues/{B}/dependencies/blocked_by \
  --method POST \
  -F issue_id=<blocker_global_id>

# Verify the link was created before proceeding:
gh api repos/{owner}/{repo}/issues/{B}/dependencies/blocked_by \
  --jq '.[] | {number, state}'
```

**The `-F` vs `-f` gotcha**: Use `-F` (uppercase), not `-f` (lowercase). `-F` sends a typed integer/ID field; `-f` sends a plain string. The GitHub dependencies API rejects string IDs and returns a confusing 422 error.

Once the dependency is wired, Fabrik's engine will apply `fabrik:blocked` to B automatically on the next poll, and B will remain blocked until A's linked PR is merged.

### Comments are the interactive channel

The user can comment on an in-flight issue to redirect the worker. Fabrik picks up comments on the next poll and re-invokes the stage with the comment as additional context. Comments on PRs work the same way (Review and Validate stages read PR comments).

**The 👀 → 🚀 reaction flow** is durable processed-state:

- 👀 (eyes) means Fabrik has acknowledged the comment and started processing.
- 🚀 (rocket) means the comment was processed successfully.
- A comment with a 🚀 reaction is **not** reprocessed on restart. This is how Fabrik survives crashes without double-handling.

If the user is wondering whether their comment "landed," check for the 👀 / 🚀 reactions.

### Worktrees and the `.fabrik/` directory

- `.fabrik/repos/` — bare clones of every managed repo (gitignored, can be very large)
- `.fabrik/worktrees/<owner>-<repo>/issue-N/` — per-issue worktrees on branch `fabrik/issue-N`
- `.fabrik/stages/*.yaml` — stage configs (commit these)
- `.fabrik/plugin/` — worker-side plugin (gitignored; `fabrik upgrade` refreshes from embedded)
- `.fabrik/config.yaml` — project config (commit this)
- `.fabrik/fabrik.log` — engine log
- `.fabrik/fabrik.lock` — single-instance lock

**Never destroy a worktree with uncommitted content** — it may carry partial worker progress that will resume on the next stage invocation. If the user wants to reset an issue, the right move is usually to remove the `stage:X:complete` labels and let the engine re-dispatch.

### When to intervene

Default to **letting it run**. The pipeline is designed to recover from a lot of conditions (CI failures trigger CI-fix invocations, reviewers trigger review-reinvokes, mergeability flips trigger rebases). Premature intervention often makes things worse — you'll end up fighting the engine.

Intervene when:

- An issue is `paused` with `fabrik:awaiting-input` and the worker has asked a real question (read the comments).
- A stage has `failed` (hit max retries). Read the worker output, decide whether to comment+resume, edit the issue body, or close as wontfix.
- The plan in Plan stage is wrong. Comment on the issue (not the PR — PR doesn't exist yet) to redirect.
- The implementation in Implement is going down a wrong path. Comment on the **PR**.

Don't intervene when:

- The worker is mid-stage and `stage:X:in_progress` is set. Let it finish or fail; commenting concurrently can confuse the next invocation.
- A stage `failed` once and `fabrik:extend-turns` would obviously help. Apply the label rather than rewriting the issue.

### Reading the board with the user

If the user asks "what's going on?" or similar, suggest `/fabrik:status` (this plugin's slash command). It summarises in-flight issues, worker processes, and worktrees with uncommitted changes. For deeper inspection: use the reusable helpers below, `gh project item-list ...`, or open the Project board in a browser.

## Reusable helpers — prefer these over hand-rolled `gh` + `jq`

This skill ships small bash scripts for the queries you'll run every session. **Use them.** Hand-rolling a GraphQL query for each request wastes tokens and drifts in shape between answers. All scripts auto-locate `.fabrik/config.yaml` by walking up from `$PWD` and handle both org- and user-owned projects. They live in `${CLAUDE_PLUGIN_ROOT}/skills/fabrik/scripts/` (or, if you can't resolve that, adjacent to this SKILL.md at `scripts/`).

| When the user asks… | Run |
|---|---|
| "what's on the board?", "what's in flight?" | `scripts/board.sh` |
| "show me `#N`", "status of `#N`", "which column is `#N` in?" | `scripts/issue.sh <N>` (add `--body` when you actually need the spec) |
| "what did `<stage>` say on `#N`?", "show me the Research output" | `scripts/stage-output.sh <N> <StageName>` |
| "what are the recent comments on `#N`?", "did my comment get processed?" | `scripts/comments.sh <N> --last 5` (add `--pr` for the linked PR's comments) |

**Design notes:**

- `board.sh` hides Done items and `stage:*:complete` labels by default (they're pipeline noise). Pass `--all` and `--raw-labels` when the user genuinely wants the full picture; pass `--column <Name>` or `--label <substring>` to slice.
- `issue.sh` shows column, labels, updated time, comment count with last-comment metadata, and linked PRs — enough to answer 90% of "what's happening with `#N`?" without a second call.
- `comments.sh` truncates bodies to 500 chars and shows reaction counts including 👀 and 🚀 so you can see comment-processing state at a glance. Pass `--full` when the user needs the whole thing.
- `stage-output.sh` finds the last comment starting with `🏭 **Fabrik — stage: <StageName>**` and prints it — this is how Fabrik marks stage output. Also matches variants like `Fabrik — stage: Review (review feedback addressed)`.
- All scripts support `--json` (or emit raw JSON in `stage-output.sh`'s case with jq itself) if you need to chain further. `board.sh --json` returns the full `gh project item-list` payload.
- Multi-repo mode (`repo:` unset in `config.yaml`): the scripts resolve the issue's repo from the project board automatically. If you already know the repo, pass `-r owner/repo` to skip the lookup — meaningful only on large boards.

**Fallback:** if a script fails (missing `gh`, unusual config), the underlying commands are simple enough to inline — but do the debugging *once* and fix the script rather than routing around it every session.

## Pointers — link the user, don't paraphrase

These are the canonical sources. Link them; the docs evolve.

- **Setup** — https://fabrik.handarbeit.io/USER_GUIDE
- **State machine (label semantics, marker handling, comment lifecycle)** — https://fabrik.handarbeit.io/state-machine
- **Stage lifecycle (per-invocation: context files, worktree, output handling)** — https://fabrik.handarbeit.io/stage-lifecycle
- **Troubleshooting** — https://fabrik.handarbeit.io/troubleshooting
- **Labels reference** — https://fabrik.handarbeit.io/USER_GUIDE#6-labels-reference

## Things to avoid saying or doing

- **Don't tell the user to edit `.fabrik/plugin/`.** It's gitignored and overwritten by `fabrik upgrade`. Skill changes belong in the Fabrik repo's `plugin/fabrik-workflows/` source tree.
- **Don't recommend `fabrik init` in an already-initialised project** — it'll write over their config. `fabrik upgrade` is the right verb for refreshes.
- **Don't promise behaviour without checking the docs.** Fabrik changes fast. If a question is non-trivial (especially around CI gates, review gates, rebase semantics), point the user at `state-machine.md` rather than reasoning from memory.
- **Don't conflate the two plugins.** This plugin (`fabrik`) talks to the user. `fabrik-workflows` instructs the workers. They are not interchangeable.
