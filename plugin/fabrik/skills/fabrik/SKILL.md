---
name: fabrik
description: Loads ambient awareness of Fabrik (the GitHub-Project-driven SDLC pipeline orchestrator that drives Claude Code workers through Specify/Research/Plan/Implement/Review/Validate stages) so Claude can act as a product-management buddy and supervisor for the user. Use this skill whenever the user is working in a project that uses Fabrik (signaled by a `.fabrik/` directory at the repo root, `.fabrik/stages/*.yaml` files, or `fabrik:`-prefixed labels on the GitHub Project), or whenever the conversation references Fabrik concepts — the project board, pipeline stages, workers, attention states, `fabrik:yolo` / `fabrik:cruise` / `base:` labels, the issue-as-spec model, or worker output. Use this skill for: answering "what's the status of #N" and "what needs me" from the running daemon, helping the user author issues Fabrik can act on, deciding when to intervene vs let workers run, steering items with the audited overseer actions, interpreting stage failures, and discussing strategy for the project's pipeline. Do NOT use for: bootstrapping Fabrik in a new project (use `fabrik-setup` instead — typically when no `.fabrik/` directory exists yet) or for the worker-Claude itself executing pipeline stages (those workers load `fabrik-stages` skills, not this one).
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
| **Queued** | Only with `merge_train: on`: a holding column where the engine batches landed-ready items, tests them together and merges them | Nothing to do per item; `fabrik_health` and `fabrik_situation` show each merge-train partition |
| **Done** | Cleanup; worktree removed | Terminal — don't expect more work here. |

Stages can be customised per-project in `.fabrik/stages/*.yaml` — column names must match `name:` exactly or startup fails. If the user mentions stages that don't match the standard list, read their YAML files first.

### Reading state — ask the daemon, never scrape

The running Fabrik daemon already knows where every item is and what it will do next. This plugin starts `fabrik mcp`, which gives you these tools (their names are prefixed `mcp__plugin_fabrik_fabrik__` in Claude Code):

| Tool | Use it for |
|---|---|
| `fabrik_situation` | **Start here for "what needs me?"** One rolled-up view: PRs waiting for a merge decision, items held at a gate, items waiting for input (with how to resume them), pending decisions, stalled items, merge-train and poll health, and anomalies. Each entry carries the options to act and the tool that performs each |
| `fabrik_status` | One issue: column and time in column, worker, linked PR and CI, blockers, labels, its attention state and what the engine will do next |
| `fabrik_board` | The whole board. `view=attention` (default) lists only items that need looking at, ranked; `view=flow` shows throughput by column. Filter by `column`, `label`, `repo`, `milestone`, `has_open_blockers` |
| `fabrik_health` | Daemon version and uptime, last poll, webhook and reconcile freshness, worker slots, per-partition merge-train state |

"What's the status of #1067?" is one `fabrik_status` call. "What needs me?" is one `fabrik_situation` call; use `fabrik_board` when the user wants the board filtered or the flow view. Answer from what they return; do not rebuild the picture from GitHub, `gh`, process lists or the worktree directory. The daemon serves them from its own cache, so they cost no GitHub calls.

**If a tool answers that the daemon is not running**, say so plainly and tell the user how to start it: run `fabrik` in the Fabrik project directory (the one holding `.fabrik/`). If the session is in some other directory, the plugin's "Fabrik directory" setting points it at the right one. Do not fall back to scraping GitHub for board or worker state: the answer would not match what the engine believes.

**Attention states.** `fabrik_status` and `fabrik_board` classify every item. Read the `reason` and `next_action` fields rather than guessing from labels; they explain engine-managed state in full.

| State | Meaning | Your move |
|---|---|---|
| `needs-human` | The engine is paused or waiting on a person | Read why (`reason`); this is the list to bring the user |
| `escalated` | The engine gave up on a loop (retries, review/CI/rebase cycles) and handed the item over | Diagnose with the user; usually needs a comment or an edit |
| `suspended` | A finished stage is waiting on the user's decision, or the item waits behind blockers parked in a parking column | Put the decision to the user; the tool lists the options |
| `waiting` | Waiting on something the engine tracks: CI, reviewers, a retry timer, a free worker slot, the next poll | Usually nothing. Say what it waits for |
| `working` | A worker is running | Let it run |
| `stalled` | No progress for longer than the stall threshold | Look before intervening; it may just be a long stage |
| `idle` | Nothing to do right now | Nothing |

**Steering labels** — the ones the user (or you, on their behalf) applies. Engine-managed labels (`stage:*`, `fabrik:locked:*`, `fabrik:awaiting-*`, `fabrik:blocked`, `fabrik:rebase-needed` and the rest) are the engine's state: don't add or remove them by hand, and ask `fabrik_status` what one means instead of explaining it from memory.

| Label | Use it when |
|---|---|
| `fabrik:paused` | You want to halt processing. Add it to pause. To resume, a human comments on the item after the pause, or the user removes `fabrik:paused` (with `fabrik:awaiting-input` if present). |
| `fabrik:yolo` | Auto-advance through every stage **and** auto-merge the PR at Validate. Trust-the-pipeline mode. |
| `fabrik:cruise` | Like `fabrik:yolo` but **stops at Validate** — no auto-merge, no Done. Good when a human gates the merge. (`fabrik:cruise` wins if both are present.) |
| `fabrik:extend-turns` | The worker is making progress but ran out of `max_turns`. Pre-grants 2× budget while present. Safety valve. |
| `fabrik:unrestricted` | A stage needs tools outside the default allowlist. **Removes all tool restrictions**; use sparingly. |
| `model:opus` / `model:sonnet` | Override the model for this issue. |
| `effort:low` / `medium` / `high` / `max` | Override thinking effort for this issue (precedence max > high > medium > low). |
| `base:<branch>` | Fork from, rebase onto, and target PRs at `<branch>` instead of the default. **Apply before Research.** |
| `review-authority:<advisory\|authoritative>`, `expected-reviewers:<none\|declared>` | Per-issue overrides of the review gate (see the User Guide). |

For autonomy (`cruise`/`yolo`/none) prefer the `fabrik_set_autonomy` action below: it makes the item end up with exactly the requested mode and leaves an audit trail.

### Overseer actions — steering through the daemon

These tools write, and each one posts an audit comment on the item naming who asked:

| Tool | Does |
|---|---|
| `fabrik_promote` | Move an item out of a parking column (e.g. Backlog) into a pipeline column such as Specify |
| `fabrik_move_parked` | Move an item between parking columns (e.g. Triage → Backlog); starts no work |
| `fabrik_advance` | Move an item whose current stage is complete exactly one stage forward, through the engine's own advance (refused while a gate holds it) |
| `fabrik_set_autonomy` | Make the item `cruise`, `yolo` or `none` |
| `fabrik_revalidate` | Force a Validate re-run |
| `fabrik_clear_claude_limit` | Clear an account-wide Claude usage-limit suspension without restarting the daemon |

Use these instead of editing labels or board columns by hand. They need a **subscriber name** (the plugin's "Subscriber name" setting); without one they refuse and say so, and the read tools still work. Each refusal carries the item's current state, so relay that rather than retrying.

**Limits you must state plainly:** no action lifts a pause, and none posts a free-form comment. A paused item (`fabrik:paused`, usually with `fabrik:awaiting-input`) resumes when a **human comments on the item** after the pause, or when the user removes those labels. If the user asks you to resume a paused item, show what the engine is waiting for (`fabrik_status`): if it asked a question, the answer belongs in a comment; if nothing needs saying (for example the daemon was stopped mid-run and paused its in-flight items), removing the labels is enough. Also: `fabrik_revalidate` refuses a paused item, and `fabrik_advance` refuses Validate (the PR merge ends Validate).

### Events pushed into the session (opt-in)

With a subscriber name set and Claude Code started with channels enabled for this plugin, the daemon can push events (decision pending, item wedged, merged…) so you don't have to poll. This is a research-preview Claude Code feature that needs a per-session flag and claude.ai or Console login; never assume it is on. Use `fabrik_subscribe` / `fabrik_unsubscribe` to choose what arrives, and see the plugin README for the launch flag.

### Filing issue chains with `blocked_by` dependencies

When the user wants to create a group of related issues where B is blocked by A, the ordering of operations matters. **Wire the `blocked_by` dependency before moving the blocked issue into Specify** — or disaster follows.

**Why the order matters**: Once an issue lands in Specify with `fabrik:yolo`, the engine picks it up within one poll (`poll:` in `.fabrik/config.yaml`, 30 s by default) or at once when webhooks are on. `fabrik:blocked` is engine-managed: Fabrik adds it only when it detects an open `blocked_by` link at poll time. If that link isn't present when Fabrik fetches the issue's deep state, `checkDependencies` sees no open dependencies, adds no block label, and dispatches a Claude worker immediately — even if the user intends to wire the dependency a moment later.

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

Once the dependency is wired, Fabrik's engine will apply `fabrik:blocked` to B automatically on the next poll, and B will remain blocked until A is closed (normally when A's PR lands).

### Comments are the interactive channel

The user can comment on an in-flight issue to redirect the worker. Fabrik picks up comments on the next poll and re-invokes the stage with the comment as additional context. Comments on PRs work the same way (Review and Validate stages read PR comments).

How to steer well:

- **In the pipeline, comment; never edit the body.** Once an issue has entered Specify, the body is the spec the engine maintains, and Specify rewrites it. A hand edit is silently overwritten and leaves no record; a comment is processed and stays visible.
- **In Backlog, edit the body.** Before an issue first enters the pipeline, nothing has processed it yet, so rewrite the spec directly.
- **Don't comment on an issue whose current stage has not run yet.** A comment that is waiting when an item enters a stage is processed *instead of* the stage: the comment pass can complete the stage without the stage ever running (Specify not expanding the spec, Plan posting no plan). Wait until the stage has posted its output, then comment.
- **Never post approval comments** ("looks good", "approved", "go ahead"). Each comment costs a worker cycle and can spuriously pause the item. Advance with labels or the overseer actions instead: `fabrik_advance` for a completed stage, `fabrik_set_autonomy` to `yolo` to let a cruise item merge.

**The 👀 → 🚀 reaction flow** is durable processed-state:

- 👀 (eyes) means Fabrik has acknowledged the comment and started processing.
- 🚀 (rocket) means the comment was processed successfully.
- A comment with a 🚀 reaction is **not** reprocessed on restart. This is how Fabrik survives crashes without double-handling.

If the user is wondering whether their comment "landed," check for the 👀 / 🚀 reactions.

### Worktrees and the `.fabrik/` directory

- `.fabrik/repos/` — bare clones of every managed repo (gitignored, can be very large)
- `.fabrik/worktrees/<owner>-<repo>/issue-N/` — per-issue worktrees on branch `fabrik/issue-N`
- `.fabrik/stages/*.yaml` — stage configs (commit these)
- `.fabrik/plugin/` — the base worker plugin `fabrik-stages`, written from the binary; `fabrik upgrade` restores it, so never edit it
- `.fabrik/plugins/local-workflows/` — the project's own customised worker skills (commit these); see below
- `.fabrik/warnings.json` — the operator warnings shown in the TUI's warnings panel
- `.fabrik/config.yaml` — project config (commit this)
- `.fabrik/fabrik.log` — engine log
- `.fabrik/fabrik.lock` — single-instance lock

**Never destroy a worktree with uncommitted content** — it may carry partial worker progress that will resume on the next stage invocation. To re-run Validate, use `fabrik_revalidate`. For any other reset, read `fabrik_status` first and agree the step with the user rather than editing engine labels.

### Customising the worker skills

Stage YAML names its skills as `plugin:skill`, for example `skill: fabrik-stages:plan`. To change how a stage works, the project uses its own plugin rather than editing the shipped one:

- Put customised skills in `.fabrik/plugins/local-workflows/` (a Claude Code plugin whose manifest name is `local-workflows`) and point the stage at them, for example `skill: local-workflows:my-plan`. Fabrik loads it after the base plugin. Other plugin directories can be added with `FABRIK_PLUGIN_DIRS`.
- `.fabrik/plugin/` is the shipped base plugin. Edits there are not the way to customise: `fabrik upgrade` moves them into `local-workflows` and restores the base.
- Bare skill names (`skill: plan`) and the old `fabrik-plan` names still resolve, but are deprecated; `fabrik upgrade` rewrites them to qualified names.

### Upgrading a project

To move a project to a new Fabrik release:

1. **Stop the daemon first** (Ctrl-C, or SIGINT). It drains for up to about 30 seconds; any worker still running is stopped and its item is paused. `fabrik upgrade` does not check for a running daemon, and changing the plugin under one breaks the next worker's skill load.
2. **Run `fabrik upgrade`** in the Fabrik directory. It replaces the binary, then migrates the workflow: stage skill references become qualified, and edited skills in `.fabrik/plugin/` move to `.fabrik/plugins/local-workflows/`. It commits nothing.
3. **Review and commit** the result (`.fabrik/stages`, `.fabrik/plugin`, `.fabrik/plugins/local-workflows`).
4. **Repair the board** if the release added columns or fields: `fabrik repair-board` reports what is missing and `fabrik repair-board --apply` adds it without touching any item's Status.
5. **Start the daemon** and check the TUI's warnings panel; it names leftover config keys and stage fields `fabrik refresh-stages --apply` can add.
6. **Resume** the items the stop paused by removing `fabrik:paused` and `fabrik:awaiting-input` from them.

### Splitting work and handling bug reports

- **Chain issues, no epics.** Split known multi-part work into self-contained issues linked with `blocked_by` (see above), with no tracking issue. Each issue goes through the pipeline on its own.
- **Never run a community or human-filed bug report through the pipeline.** Specify would rewrite the reporter's text, and the pipeline's comments and labels would flood their thread. Keep the report as the human discussion, and file separate work issues that restate it as a spec and mention it by number.
- **Never put a closing keyword (`Closes`, `Fixes`, `Resolves`) near a report's number**, even inside a negation. GitHub would close the report when the PR merges, long before the fix is released. Close the report by hand once a release containing the fix has shipped, naming that release. Merged is not shipped.

### When to intervene

Default to **letting it run**. The pipeline is designed to recover from a lot of conditions (CI failures trigger CI-fix invocations, reviewers trigger review-reinvokes, mergeability flips trigger rebases). Premature intervention often makes things worse — you'll end up fighting the engine.

Intervene when:

- An issue is `needs-human` or `escalated`, for example paused with `fabrik:awaiting-input` because the worker asked a real question (read the comments).
- A stage has `failed` (hit max retries). Read the worker output, decide whether to comment+resume, edit the issue body, or close as wontfix.
- The plan in Plan stage is wrong. Comment on the issue (not the PR — PR doesn't exist yet) to redirect.
- The implementation in Implement is going down a wrong path. Comment on the **PR**.

Don't intervene when:

- The worker is mid-stage and `stage:X:in_progress` is set. Let it finish or fail; commenting concurrently can confuse the next invocation.
- A stage `failed` once and `fabrik:extend-turns` would obviously help. Apply the label rather than rewriting the issue.

### Reading stage output and comments

Stage output lives in issue comments the engine posts, starting with `🏭 **Fabrik — stage: <Name>**`. The daemon does not serve comment bodies, so for the text of a stage's output or a thread, read the issue directly: `gh issue view <N> --comments` (and `gh pr view <N> --comments` for PR discussion). That reads issue content, not board or worker state; for state, use the tools above.

## Pointers — link the user, don't paraphrase

These are the canonical sources. Link them; the docs evolve.

- **Setup** — https://fabrik.handarbeit.io/USER_GUIDE
- **State machine (label semantics, marker handling, comment lifecycle)** — https://fabrik.handarbeit.io/state-machine
- **Stage lifecycle (per-invocation: context files, worktree, output handling)** — https://fabrik.handarbeit.io/stage-lifecycle
- **Troubleshooting** — https://fabrik.handarbeit.io/troubleshooting
- **Labels reference** — https://fabrik.handarbeit.io/USER_GUIDE#6-labels-reference

## Things to avoid saying or doing

- **Don't tell the user to edit `.fabrik/plugin/`.** It is the shipped base plugin, which `fabrik upgrade` restores. Project customisations go in `.fabrik/plugins/local-workflows/` (see "Customising the worker skills").
- **Don't recommend `fabrik init` in an already-initialised project** — it'll write over their config. `fabrik upgrade` is the right verb for refreshes.
- **Don't promise behaviour without checking the docs.** Fabrik changes fast. If a question is non-trivial (especially around CI gates, review gates, rebase semantics), point the user at `state-machine.md` rather than reasoning from memory.
- **Don't conflate the two plugins.** This plugin (`fabrik`) talks to the user. `fabrik-stages` instructs the workers. They are not interchangeable.
