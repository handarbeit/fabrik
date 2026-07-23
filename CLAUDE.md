# Fabrik — Development Guide for Claude

## Project Overview

Fabrik is a Go CLI that orchestrates Claude Code through an SDLC pipeline defined on a GitHub Project board. Issues are the unit of work. The pipeline stages (Specify → Research → Plan → Implement → Review → Validate) are configured via YAML files.

## Build & Test

```bash
go build -o fabrik .     # Build
go test ./...            # Run all tests
go test -race ./...      # Run with race detector
go vet ./...             # Lint
```

## Documentation bundle (docs/llms-full.txt)

When you modify any of the canonical doc pages — `docs/USER_GUIDE.md`, `docs/state-machine.md`, `docs/stage-lifecycle.md`, or `docs/positioning.md` — you MUST regenerate `docs/llms-full.txt` in the same commit:

```bash
bash scripts/generate-llms-full.sh
git add docs/llms-full.txt
```

CI's `docs-drift` workflow (`.github/workflows/docs-drift.yml`) runs the regen and fails the PR if the committed bundle differs from the regen output. Forgetting the regen costs an extra round-trip; doing it consistently keeps PRs green on first push. This requirement applies to **every** stage (Specify, Research, Plan, Implement, Review, Validate) — wherever a doc edit happens, the bundle must follow.

## Architecture

- `cmd/root.go` — CLI entry point, flag parsing, .env loading
- `engine/engine.go` — Engine struct, Config, construction, Run() entry point
- `engine/poll.go` — Main poll loop, idle-upgrade, concurrent worker dispatch
- `engine/item.go` — Per-issue processing: stage runs, comment processing, blocking/pausing
- `engine/pr.go` — Output posting: issue comments, PR comments, summary extraction
- `engine/comments.go` — Comment detection and filtering logic
- `engine/context.go` — Context files (.fabrik-context/) and stage comment lookup
- `engine/repo.go` — Per-repo identity helpers (parseOwnerRepo, repoName, issueKey)
- `engine/claude.go` — Claude Code invocation, prompt building, marker extraction
- `engine/worktree.go` — Git worktree lifecycle (create, update, push, cleanup)
- `engine/merge_train.go` — Merge-train worker: trial branch assembly, inline conflict resolution, integration PR creation and CI polling (ADR-059 D3)
- `engine/interfaces.go` — GitHubClient and ClaudeInvoker interfaces (for testing)
- `github/project.go` — GraphQL board fetching (single query for all items + comments + linked PRs)
- `github/client.go` — HTTP client construction and shared request helpers
- `github/labels.go` — Label mutations (add, remove, ensure)
- `github/comments.go` — Comment mutations (add, update, reactions)
- `github/prs.go` — PR mutations (create draft, mark ready)
- `github/status.go` — Project board status updates
- `github/rest.go` — Low-level HTTP helpers
- `github/types.go` — Shared data types (ProjectItem, Comment, ReactionGroup)
- `stages/stages.go` — YAML stage config loading
- `stages/examples/` — Default stage YAML sources, embedded in binary via `//go:embed`
- `stages/embed.go` — Exposes embedded default stages as `stages.DefaultStages`
- `plugin/embed.go` — `FabrikPlugin` embed.FS; source of truth for all built-in skills
- `plugin/refresh.go` — `RefreshPlugin()` overwrites `.fabrik/plugin/` from the embedded source
- `cmd/init.go` — `fabrik init` subcommand; extracts embedded YAMLs to `.fabrik/stages/`
- `.fabrik/stages/` — Live stage configs for this project (tracked in git)
- `boardcache/boardcache.go` — `ReadClient` interface (9-method read-only subset of `GitHubClient`), `GitHubAdapter` (pass-through), `CacheImpl` (in-memory cache with delta, reconcile, pause/resume)
- `boardcache/delta.go` — Typed webhook delta functions: apply `issue_comment`, `issues`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `check_run`, and `projects_v2_item` payloads as state mutations

> **Editing built-in skills**: modify `plugin/fabrik-workflows/skills/<name>/SKILL.md` — the embedded source baked into the binary. `.fabrik/plugin/` is the local deployed copy written by `fabrik init` and refreshed by `fabrik upgrade`; it is not tracked in git and edits there will be silently overwritten on the next refresh.

## Key Patterns

### Reaction Flow
- 👀 (eyes) = comment acknowledged, processing started
- 🚀 (rocket) = comment processed successfully
- The rocket reaction is checked on restart to avoid reprocessing — it's durable state

### Markers in Claude Output
- `FABRIK_STAGE_COMPLETE` — Claude signals stage completion (must be on its own line)
- `FABRIK_BLOCKED_ON_INPUT` — Claude signals it needs user input before the stage can continue; mutually exclusive with `FABRIK_STAGE_COMPLETE`
- `FABRIK_ISSUE_UPDATE_BEGIN` / `FABRIK_ISSUE_UPDATE_END` — Updated issue body from comment processing
- `FABRIK_SUMMARY_BEGIN` / `FABRIK_SUMMARY_END` — Brief summary for issue when detailed output goes to PR

### Context Files

Before each stage invocation, the engine writes context documents to `.fabrik-context/` in the worktree:
- `.fabrik-context/issue.md` — the issue body (spec)
- `.fabrik-context/stage-<Name>.md` — output from prior stages
- `.fabrik-context/pr-description.md` — linked PR description (for `post_to_pr` stages)
- `.fabrik-context/codebase-changes.md` — files changed on `origin/<baseBranch>` since the last stage ran (generated by `context.go`; omitted when no prior stage or when SHAs match)

### Concurrency
- Workers dispatch via semaphore (`MaxConcurrent`, default 5)
- `processedSet` protected by `sync.Mutex`
- Worktree creation serialized by mutex (git config isn't concurrent-safe)
- In-flight issues tracked via `sync.Map` to prevent duplicate dispatch

### Worktrees
- Each managed repo is always bare-cloned to `.fabrik/repos/<owner>-<repo>.git` on first access
- Each issue gets `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N`
- `fabrikDir` (where `.fabrik/` config, stages, and plugin live) is always `os.Getwd()`
- NEVER destroy worktrees with existing content — they may have partial work
- `updateWorktreeFromMain` fetches and merges origin/main; leaves conflicts for Claude
- Dirty worktrees (uncommitted changes) skip the update

### PR Lifecycle
- Implement creates draft PR with `Closes #N` in body (links PR to issue)
- `closedByPullRequestsReferences` in GraphQL traverses issue → linked PRs → PR comments
- `post_to_pr` stages post detailed output on PR, summary on issue
- PR marked ready after Implement completes (triggers external review bots)

### Stage Config Options
```yaml
name: Research
order: 1
prompt: |
  ...
skill: fabrik-research          # Optional: plugin skill name to load for this stage
model: sonnet
max_turns: 50
comment_prompt: |               # Optional: prompt for processing user comments
  ...
comment_skill: fabrik-research-comment  # Optional: plugin skill for comment processing
comment_max_turns: 15           # Optional: max turns for comment review (default: min(max_turns, 15))
allowed_tools:                  # Optional: REPLACES the default tool set (not additive). When set,
  - Read                        # only the listed tools are allowed. When absent, Fabrik uses a
  - Grep                        # comprehensive default: Read, Edit, Write, Glob, Grep, TodoWrite, Skill, Task,
                                # Bash(git:*), Bash(gh:*), Bash(go:*), Bash(npm:*), Bash(npx:*), Bash(yarn:*),
                                # Bash(pnpm:*), Bash(make:*), Bash(cargo:*), Bash(python:*), Bash(pip:*),
                                # Bash(uv:*), Bash(pytest:*), Bash(ls:*), Bash(cat:*), Bash(rm:*), Bash(cp:*),
                                # Bash(mv:*), Bash(mkdir:*), Bash(find:*).
post_to_pr: true                # Post output to linked PR instead of issue
create_draft_pr: true           # Create draft PR before stage runs
mark_pr_ready_on_complete: true # Mark PR ready when stage completes
auto_advance: false             # Override global yolo setting
read_only: false                # Stash/restore worktree changes (for Specify/Research stages that don't write code)
cleanup_worktree: false         # Remove worktree when stage completes (for Done/cleanup stages)
max_wall_time: "45m"            # Optional: wall-clock deadline for a single Claude invocation (e.g. "30m", "1h").
                                # When exceeded, SIGTERM → 10s → SIGKILL sent to the process group. Output
                                # collected before the kill is scanned for FABRIK_STAGE_COMPLETE so completed
                                # stages are not re-run. Absent or zero = no cap. A hardcoded 15-minute
                                # inactivity timeout (no output received) applies to every invocation regardless.
disable_adaptive_thinking: true # Disable Claude Code's adaptive (auto-reduced) thinking budget. Default: true.
effort_level: max               # Claude Code thinking effort: low, medium, high, max. Default: high.
```

## Important Conventions

- **Don't commit directly to main from worktrees** — always work on the issue branch
- **Every PR must include `Closes #N`** in the body so Fabrik can discover PR comments
- **Commit frequently** during implementation — preserves progress if session is interrupted
- **Rebase onto the latest base branch** (default branch, or the branch specified by `base:<branch>` label) in Review and Validate stages before signaling completion
- **Check `git status` first** in any stage — there may be uncommitted work from a previous session
- **Labels are state**: `fabrik:locked:<user>`, `fabrik:editing`, `fabrik:paused`, `fabrik:awaiting-input`, `fabrik:awaiting-review`, `fabrik:awaiting-ci`, `fabrik:awaiting-done`, `fabrik:awaiting-member-close`, `fabrik:awaiting-placement`, `fabrik:blocked`, `fabrik:rebase-needed`, `fabrik:bot-reprompted`, `fabrik:children-spawned`, `fabrik:sub-issue`, `stage:<name>:in_progress`, `stage:<name>:complete`, `stage:<name>:failed`, `model:<name>`, `effort:<level>`, `fabrik:yolo`, `fabrik:cruise`, `fabrik:unrestricted`, `fabrik:extend-turns`, `base:<branch>`, `fabrik:revalidate`
  - `model:<name>` — set by user to select a specific model for this issue (e.g. `model:opus`)
  - `effort:<level>` — set by user to override the stage's configured thinking effort for this issue only; valid values: `low`, `medium`, `high`, `max`; if multiple `effort:` labels are present, precedence is `max > high > medium > low`
  - `fabrik:yolo` — set by user to force auto-advance even when `auto_advance: false` in stage YAML; also triggers auto-merge of the linked PR when Validate completes
  - `fabrik:cruise` — set by user to auto-advance through all stages without auto-merging the PR or advancing to Done at Validate completion; if both cruise and yolo are present, yolo takes precedence
  - `fabrik:awaiting-review` — set by engine when a stage with `wait_for_reviews: true` completes and outstanding PR reviewer requests remain; cleared when all reviewers submit or `FABRIK_REVIEW_WAIT_TIMEOUT` elapses
  - `fabrik:bot-reprompted` — single fixed label (22 chars); set by engine after Phase 1 of the bot-reviewer escalation ladder fires (1× `ReviewWaitTimeout` when all outstanding reviewers are bots); serves as idempotency guard for Phase 1 and timing anchor for Phase 2; cleared when the gate cycle ends (bot responds, Phase 2 fires, or gate clears naturally). Never persists beyond the gate cycle.
  - `fabrik:awaiting-ci` — set by engine when a stage with `wait_for_ci: true` emits `FABRIK_STAGE_COMPLETE`; **`stage:<name>:complete` is NOT added at that point** — it is deferred until CI passes (conjunctive gate). While this label is present, the dispatcher will not re-invoke the stage (only the catch-up loop evaluates CI). Cleared by the engine when all CI checks pass; also applied on confirmed CI failure to signal the CI-fix reinvocation path.
  - `fabrik:awaiting-done` — set by engine as the very first mutation in `handleNoWorkNeeded`, the instant a stage emits `FABRIK_STAGE_COMPLETE` + `FABRIK_NO_WORK_NEEDED`, so the decision survives even if the subsequent board move to Done or issue close fails (e.g. rate-limit exhaustion) or the engine restarts. While present, dispatch is suppressed for every non-cleanup stage regardless of board column. Retried every poll by an unconditional settle scan until the Done move and issue close both succeed, at which point the label is cleared; after `MaxRetries` failed settle passes the issue is escalated instead (`fabrik:paused` added, `fabrik:awaiting-done` removed, explanatory comment posted) — see ADR-060.
  - `fabrik:awaiting-member-close` — set by `landSingleton` (the merge-train one-at-a-time landing path) when its member-issue `CloseIssue` call fails, after the PR merge, Done-move, and member-PR close have already happened. Retried every poll by an unconditional settle scan (`settleMergeTrainMemberCloses`, independent of `merge_train: on/off` and of `itemMayNeedWork`/`itemNeedsWork` dispatch — the item has already reached Done by the time this label matters) until the issue is confirmed closed (by us or by GitHub's own `Closes #N` auto-close), at which point the label is cleared; after `MaxRetries` failed attempts the issue is escalated instead (`fabrik:paused` added, `fabrik:awaiting-member-close` removed, explanatory comment posted) — see ADR-061. Scoped to `landSingleton` only; the analogous call in `landMergeTrainBatch` is a separate, deliberately out-of-scope follow-up.
  - `fabrik:unrestricted` — passes `--dangerously-skip-permissions` instead of `--permission-mode dontAsk`; bypasses the default tool allowlist entirely. Use only when a stage needs tools outside the default set (e.g. non-standard toolchains). **Caution:** removes all tool restrictions.
  - `fabrik:extend-turns` — set by user as a manual override to pre-grant 2× the stage's `max_turns` budget for every invocation while the label is present; persists across all stages until the Done stage's cleanup runs (it is removed there, not on each individual stage completion); no-op when `max_turns == 0` (unlimited); subsequent extensions beyond 2× still require automatic progress detection; use as a safety valve when progress detection misfires — apply once and it covers all remaining stages
  - `base:<branch>` — set by user to override the worktree base branch for this issue; Fabrik will fork from, rebase onto, and target PRs at `<branch>` instead of the repository default; must be set before Research; multiple `base:` labels use the first and logs a warning; if the branch does not exist on the remote, Fabrik falls back to the default and posts a comment
  - `fabrik:blocked` — set by engine in `checkDependencies` when the issue has unresolved blocking dependencies (issues referenced via GraphQL `trackedIssues` / `blockedBy`); cleared idempotently when all dependencies are resolved; processing is suspended while the label is present
  - `fabrik:rebase-needed` — set by engine at Validate when `attemptMergeOnValidate` returns `ErrNotMergeable` (the linked PR no longer cleanly merges onto its base branch); engine dispatches `dispatchRebaseReinvoke` to retry; cleared on successful rebase; at `MaxRebaseCycles` Fabrik falls through to `pauseForRebaseCycleLimit` and the issue is paused for human intervention
  - `fabrik:children-spawned` — set by engine's `preImplement` step after all `FABRIK_SPAWN_CHILD_*` children are successfully created, added to the project board, and linked as `blockedBy` of the parent; idempotency guard — while present, `preImplement` is a no-op; remove manually (and close any orphaned children) to trigger a fresh spawn
  - `fabrik:sub-issue` — applied by `preImplement` to each spawned child issue; informational only (human-visible filtering); carries no engine-gate semantics
  - `fabrik:awaiting-placement` — set on a spawned **child** issue by `spawnChildren` when its initial project-board Status placement fails (call error, missing status-field metadata, or no suitable column found) — the child, board item, and `blockedBy` link already exist by this point, so this is recoverable in place rather than a spawn-abort condition. Retried every poll by a settle scan sourced directly from `board.Items` (not `deepFetchCandidates`, which a stranded child — sitting in a column with no configured stage — never reaches). Cleared on successful placement, or when the settle scan observes the child has been closed (no further placement is needed). After `MaxRetries` failed settle passes the child is escalated instead (`fabrik:paused` added, `fabrik:awaiting-placement` removed, explanatory comment posted on the child, plus a best-effort comment on the parent) — see ADR-062.
  - `fabrik:revalidate` — set by operator to force re-entry of the Validate stage; engine removes `stage:Validate:complete`, `stage:Validate:failed`, `fabrik:paused`, `fabrik:awaiting-input`, `fabrik:awaiting-ci`, `fabrik:auto-merge-enabled`, and the trigger label itself; Validate then dispatches on the next poll cycle; applied to non-Validate issues: only the trigger label is removed with a warning; safe to apply while Validate is in-flight (held until worker exits)

## Canonical Documentation

These files are the authoritative as-built specifications for Fabrik's engine behavior. They must be kept in sync with the code — any PR that changes behavior in the areas they cover must update the corresponding doc in the same change set.

- **`docs/state-machine.md`** — As-built specification for: engine state transitions, label semantics, `FABRIK_*` marker handling, comment processing lifecycle, review gate and review reinvoke, PR lifecycle coupling, progress-based turn extension, and guard/filter behavior in `itemMayNeedWork` / `itemNeedsWork`.
- **`docs/stage-lifecycle.md`** — As-built specification for the per-invocation lifecycle: what happens before, during, and after a single Claude invocation (context files, worktree setup, Claude invocation, progress baseline snapshot, extension loop, output handling).

These are **as-built docs** — they describe what the engine currently does. They are distinct from `adrs/*.md`, which record architectural decisions and design rationale, not current state. Do not put state-machine content into ADRs or vice versa.

PRs introducing new as-built behavioral docs should also add an entry here.

## Handling Community Bug Reports

**Never run a community- or human-filed bug report through the Fabrik pipeline directly.** The Specify stage rewrites the issue body (`FABRIK_ISSUE_UPDATE`), so pipelining a report would overwrite the reporter's repro and diagnosis with a bot-authored spec — and the pipeline machinery (👀/🚀 reactions, per-stage comments, label churn) would spam the reporter's thread. Report and work are different artifacts: the report is human-owned triage/discussion; the work is bot-driven engineering.

Instead:

- **Keep the report as the canonical thread** — human-owned. Confirm the repro there, then comment linking to the work issue; keep it open until the fix lands.
- **Create a separate spec-kit WORK issue** (authored as the bot identity, on the engine project board) that restates the report as Problem / Requirements / Scope / Acceptance and references it. **One report maps to 1..N work issues.**
- **Linkage:** the work issue's PR carries `Closes #<work-issue>` (Fabrik's own discovery relies on it) **and** `Fixes #<report>` (so GitHub auto-closes the report and the reporter sees the resolution land).
- **Multi-part fixes:** create a **chain of self-contained spec-kit work issues** (`blockedBy`-linked, **no epic/tracking issue**), all referencing the report. Do **not** rely on the in-pipeline child-spawn (`FABRIK_SPAWN_CHILD`) for a *known* decomposition — that path is for decomposition Plan *discovers* mid-flight; pre-decompose into chained issues when you already know the shape.
- **Exception:** only pipeline the issue itself when it is your own internal issue, already spec-kit-shaped, a single fix, and reshaping it is acceptable. Community-filed reports are always handled as a separate work issue.

**Board separation (recommended):** community reports belong on a separate, **public** triage/roadmap board with coarse columns (Triage → Accepted → In Progress → Shipped → Declined), giving the community a clean "reported → accepted → shipping" view. The engine work board stays private — it is thick with operational machinery (`fabrik:locked`, `stage:<name>:in_progress`, bot churn) that is noisy in public. A GitHub issue can belong to both boards at once (coarse on the public board, fine-grained on the engine board); keep the public board coarse to avoid status-sync overhead.

## Startup Board Validation

On every startup, Fabrik fetches the project board and compares stage names to board columns. If any non-cleanup stage is missing from the board, Fabrik exits with a detailed error message listing the mismatched names. Extra board columns (without a matching stage) produce a warning but don't block startup. This catches mismatches between stage YAML config and the GitHub Project board configuration early.

## Common Issues

- **Startup board validation failure**: Stage names in `.fabrik/stages/*.yaml` must match the column names on your GitHub Project board exactly. Check both for typos.
- **Max turns exceeded**: Increase `max_turns` in stage YAML or split the issue
- **Merge conflicts**: Left as conflict markers for Claude to resolve — check `git status`
- **Stale worktree**: `updateWorktreeFromMain` runs on each stage invocation; skip if dirty
- **SSH key expired**: `ssh-add ~/.ssh/<key>` — git operations fail silently with warning
- **processedSet is in-memory**: Rocket reactions provide durable "already processed" state across restarts
- **Stage drift warning at startup**: `[startup] warning: .fabrik/stages/<file>.yaml is missing fields present in v<version> defaults: <keys>` means your stage YAML predates fields added in a newer binary. The check is value-aware: a missing key is only reported when adding it would actually change the stage's effective behavior — a key whose embedded-default value is behaviorally identical to omission (e.g. `kill_grace: {sigint: 10s, sigterm: 10s}`, `completion: {type: claude}`) is never flagged. Run `fabrik refresh-stages` to preview what would be added, then `fabrik refresh-stages --apply` to add the missing keys (it uses the same value-aware comparison, so it never offers a key drift considers a no-op). Review with `git diff`, then commit. The warning is informational — the engine will still run with your existing config, but you may be missing new behavioral options (e.g. `wait_for_ci`, `wait_for_reviews`).
