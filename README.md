# Fabrik

Automated Claude Code SDLC driver powered by GitHub Issues and Projects.

Fabrik watches a GitHub Project board and drives Claude Code through configurable
workflow stages. Issues are the unit of work — the issue body is the spec, comments
are user input, and the board columns define the workflow.

## Quick Start

**Option A: Install binary (requires `gh`)**

```bash
# Requires: gh auth login (with access to tenaciousvc/fabrik)
cd ~/bin  # or any directory on your PATH
gh release download --repo tenaciousvc/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
# Platform-specific alternatives:
#   darwin/arm64:  --pattern "fabrik_*_darwin_arm64.tar.gz"
#   darwin/amd64:  --pattern "fabrik_*_darwin_amd64.tar.gz"
#   linux/amd64:   --pattern "fabrik_*_linux_amd64.tar.gz"
#   linux/arm64:   --pattern "fabrik_*_linux_arm64.tar.gz"
```

**Option B: Build from source (requires Go)**

```bash
# Build
go build -o fabrik .

# Initialize stage configs, plugin, and project config template
# Pass your GitHub Project URL to auto-populate owner, project, and owner_type:
./fabrik init --user you https://github.com/orgs/your-org/projects/5
# Or run without a URL for full interactive prompts (TTY) / blank template (non-TTY)
./fabrik init
# Creates .fabrik/stages/, .fabrik/plugin/, and .fabrik/config.yaml

# Edit .fabrik/config.yaml with your project settings (commit this file)
# Add your GitHub token to a gitignored .env:
echo 'FABRIK_TOKEN=ghp_...' >> .env
echo '.env' >> .gitignore

# Run (settings come from .fabrik/config.yaml)
./fabrik

# Or override specific values with flags
./fabrik --stages ./.fabrik/stages --yolo
```

**Configuration summary:**
- `.fabrik/config.yaml` — non-secret project settings (commit to git)
- `.env` — secrets only (`FABRIK_TOKEN` / `GITHUB_TOKEN`; must be gitignored)
- Precedence: `CLI flag > env var > .env > config.yaml > defaults`

### Auto-upgrade

Use `--auto-upgrade` to have Fabrik self-upgrade from GitHub Releases when idle:

```bash
./fabrik --auto-upgrade ...
```

After 2 idle polls, Fabrik checks GitHub Releases for a newer version, downloads it, and re-execs.

## How It Works

```
GitHub Project Board (source of truth)
        | GraphQL poll
    Fabrik (Go CLI, runs locally)
        | stage config match
    Claude Code (invoked per stage, in isolated worktree)
        | output
    GitHub Issue comments + labels + body updates
```

1. **Poll** — Fabrik fetches the entire project board via a single GitHub GraphQL query.
2. **Match** — Each issue's board status is matched to a stage config (YAML file).
3. **Worktree** — An isolated git worktree is created for each issue (`fabrik/issue-N` branch).
4. **Invoke** — Claude Code runs in the worktree with the stage's prompt, model, and tool configuration.
5. **Complete** — When Claude signals completion, the issue is labeled `stage:<name>:complete`.
6. **Advance** — In yolo mode, the issue auto-advances to the next stage. Otherwise, a human moves it.

### Comment Processing

When a user comments on an issue in an active stage:

1. Fabrik reacts with :eyes: to each new comment (marks as "in review")
2. Adds `fabrik:editing` label to lock the issue
3. Invokes Claude with a stage-specific comment review prompt
4. Claude performs any requested actions (e.g., linking PRs, running commands)
5. If the issue body needs updating, parses the updated body from Claude's output
6. Updates the issue body on GitHub (or posts output as a comment)
7. Removes `fabrik:editing` label
8. Reacts with :rocket: to each processed comment (also used to skip already-processed comments on restart)

This allows iterative refinement — comment to answer questions, provide feedback,
or steer the work, and Fabrik incorporates your input into the issue.

### Review and Fix

Stages with `post_to_pr: true` (like the default Review stage) post detailed
output on the linked PR and a brief summary on the issue. The Review stage
also rebases onto latest main and resolves merge conflicts before reviewing,
keeping the PR branch clean.

If a stage doesn't complete (e.g., unfixable issues found), it retries after
a cooldown period rather than being permanently skipped.

### Steering with Comments

Fabrik responds to natural language. Comment on an issue to steer the work:

- *"Please link the PR to this issue"* — Claude performs the action
- *"Please process PR feedback on PR #18"* — Claude reads the PR reviews and fixes the code
- *"Let's use approach B instead"* — Claude updates the plan accordingly

Each stage sees the full conversation history — previous stage outputs, user
comments, and all — so context carries forward through the pipeline.

## Default Pipeline

The example stages implement a full SDLC pipeline:

| Stage | Purpose |
|-------|---------|
| **Backlog** | Parking lot (no processing) |
| **Specify** | Refine rough issues into clear, unambiguous specs (Q&A with user) |
| **Research** | Explore codebase, identify scope, surface questions |
| **Plan** | Design approach, break into tasks, document decisions |
| **Implement** | Write code, tests, commit to issue branch |
| **Review** | Rebase, review, fix, and push to PR |
| **Validate** | Run tests, verify requirements met |
| **Done** | Terminal state (no processing) |

## Stage Configuration

Each stage is a YAML file in your stages directory:

```yaml
name: Research            # Must match a Project board column name
order: 1                  # Processing order (lower = earlier)
skill: fabrik-research    # Plugin skill (recommended; alternative to inline prompt)
prompt: |                 # Inline prompt (used when skill is not set)
  You are a research agent...
comment_skill: fabrik-research-comment  # Skill for comment review (overrides comment_prompt)
comment_prompt: |         # Inline comment-review prompt (used when comment_skill is not set)
  You are reviewing user comments...
model: sonnet             # Optional: claude model to use
max_turns: 50             # Optional: max conversation turns per stage invocation
comment_max_turns: 15     # Optional: max turns when processing user comments (default: min(max_turns,15))
read_only: false          # Optional: stash/restore worktree; use for analysis stages (Specify, Research)
update_issue_body: false  # Optional: allow FABRIK_ISSUE_UPDATE markers to modify issue body (Specify only)
post_to_pr: true          # Optional: post output on linked PR instead of issue
create_draft_pr: true     # Optional: push branch and create draft PR before Claude runs
mark_pr_ready_on_complete: true  # Optional: mark PR ready after stage completes
auto_advance: false       # Optional: override global yolo setting for this stage
                          #   true = always auto-advance; false = never; omit = inherit yolo
cleanup_worktree: false   # Optional: remove worktree instead of invoking Claude (terminal stages)
allowed_tools:            # Optional: restrict available tools
  - Read
  - Grep
  - Glob
completion:
  type: claude            # "claude" (default and only supported type)
```

## Configuration

Fabrik resolves settings in this order (highest to lowest priority):

```
CLI flag  >  shell env var  >  .env file  >  .fabrik/config.yaml  >  built-in default
```

Run `fabrik init` to generate `.fabrik/config.yaml` in your project. This file holds
all non-secret project settings and should be committed to git. See the
[Configuration Reference](docs/USER_GUIDE.md#2-configuration-reference) in the User
Guide for a full field-by-field description with examples and defaults.

**Secrets** (GitHub tokens) belong in a gitignored `.env` file only:

```
# .env (gitignored — keep secrets here)
FABRIK_TOKEN=ghp_...    # Preferred
GITHUB_TOKEN=ghp_...    # Fallback
```

**Safety:** If `.env` exists but is not listed in `.gitignore`, Fabrik refuses to start
to prevent accidental token leaks.

## Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--owner` | GitHub repo owner | required |
| `--repo` | GitHub repo name | required |
| `--project` | GitHub project number | required |
| `--user` | Your GitHub username | required |
| `--token` | GitHub token | `$GITHUB_TOKEN` |
| `--stages` | Stage configs directory | `./.fabrik/stages` |
| `--yolo` | Auto-advance through stages | `false` |
| `--auto-upgrade` | Self-upgrade from origin/main when idle | `false` |
| `--poll` | Poll interval in seconds | `30` |
| `--notui` | Disable the interactive TUI dashboard | TUI on by default |
| `--max-concurrent` | Maximum number of concurrent issue workers | `5` |
| `--max-retries` | Max failed stage attempts before pausing the issue (0 = unlimited) | `3` |
| `--debug-output` | Save Claude stage output to `.fabrik/debug/` for debugging | `false` |
| `--plugin-dir` | Path to Fabrik plugin directory (overrides installed plugin) | `""` |

## Subcommands

| Command | Description |
|---------|-------------|
| `fabrik init` | Initialize `.fabrik/stages/`, `.fabrik/plugin/`, and `.fabrik/config.yaml` in the current repo |
| `fabrik watch <issue-number>` | Open a real-time TUI for a single issue — live Claude output, stage history, PR/CI status |
| `fabrik stream-filter` | Read NDJSON Claude output from stdin and render it as human-readable text |
| `fabrik resume <issue-number>` | Resume an interrupted Claude session for an issue |
| `fabrik upgrade` | Self-update the Fabrik binary (release builds) and refresh plugin skills in `.fabrik/plugin/` from embedded defaults |

## Built-in Skills

Fabrik ships two user-invocable Claude Code skills (type `/skill-name` in any Claude Code session in the Fabrik repo):

| Skill | Description |
|-------|-------------|
| `/cut-release` | Pre-flight checks, curated release notes, commit/tag/push, and auto-files a doc-update issue — see [§5 of the User Guide](docs/USER_GUIDE.md#built-in-skill-cut-release) |
| `/audit-documentation` | Scans recently shipped issues, identifies documentation gaps, closes covered issues, and files new gap issues — see [§5 of the User Guide](docs/USER_GUIDE.md#built-in-skill-audit-documentation) |

### `fabrik watch`

Monitor a single issue in real time without running the engine:

```bash
fabrik watch 42
# or with explicit credentials if not in .fabrik/config.yaml:
fabrik watch 42 --owner myorg --repo myrepo
```

The watch TUI displays: issue title and labels, live Claude output (streamed from the log file), stage history with duration and cost, linked PR status, CI check results, and comment count. Press `i` to open an interactive Claude session in the issue's worktree (resumes the current stage's session). Press `q` to exit.

## Labels

Fabrik uses labels to track state:

| Label | Purpose |
|-------|---------|
| `fabrik:locked:<user>` | Issue is being processed by this user's Fabrik instance |
| `fabrik:editing` | Issue body is being updated (prevents concurrent processing) |
| `fabrik:paused` | Issue is skipped entirely — no stage processing or comment processing occurs |
| `fabrik:awaiting-input` | Stage is paused waiting for user input; auto-clears when a new comment from the configured user (`--user`) is received |
| `fabrik:blocked` | Issue is waiting for one or more blocking issues to close; managed automatically by the engine |
| `stage:<name>:complete` | Stage has been completed |
| `stage:<name>:in_progress` | Stage is actively running |
| `stage:<name>:failed` | Stage hit max retries and was paused |
| `model:<name>` | Use this model for the issue (e.g. `model:opus` overrides the stage YAML default) |
| `fabrik:yolo` | Force auto-advance even when `auto_advance: false`; also triggers auto-merge of the linked PR when Validate completes |
| `fabrik:unrestricted` | Pass `--dangerously-skip-permissions` to Claude Code for this issue only; use when an issue needs to write to paths not covered by `.claude/settings.json` (e.g. `.claude/skills/`). **Caution:** bypasses the permission system. |

## Multi-User

Multiple people can run Fabrik against the same project board. Each instance
only processes changes made by its `--user`. Labels provide lightweight locking
to prevent conflicts.

## Git Worktrees

Fabrik always bare-clones each managed repository on first access. Run Fabrik from any directory — no need to be inside a git checkout of a managed repo:

1. Repos are discovered lazily from project board items
2. Each repo is bare-cloned to `.fabrik/repos/<owner>-<repo>.git` on first access
3. Each issue gets an isolated worktree at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N`
4. One worktree manager per discovered repository handles its own lifecycle

This means:

- Multiple issues can be worked on simultaneously without conflicts
- Each issue's changes are on their own branch, ready for PR
- Worktrees persist across polls for Claude session continuity

```bash
mkdir my-fabrik-dir && cd my-fabrik-dir
fabrik init
./fabrik --owner myorg --project 5 --user me
# Fabrik bare-clones each repo as needed into .fabrik/repos/
```

Use `--repo owner/repo` to restrict processing to a single repository.

## Formations

Fabrik supports **dependency-based sequencing** of issues using GitHub's native "Blocked by" relationships. A **formation** is a coordinated set of issues that execute in parallel where possible and respect ordering constraints automatically — no polling, no manual unblocking.

When all blocking issues are closed, Fabrik detects the change within one poll cycle and resumes the blocked issue automatically. The `fabrik:blocked` label is managed entirely by the engine (created on first use, no pre-creation needed). The first stage (Specify) always runs regardless of blockers, so an entire formation can be fully specified before execution begins.

**Recipe:** file issues → add blocked-by edges in GitHub → label all `fabrik:yolo` → move to Specify → watch it run.

**Validated on the Ambient project:** 9 issues, 4 parallel starts, 7 dependency edges — all pipeline constraints respected automatically, ~88 minutes wall-clock, $31 total cost.

See the [USER_GUIDE §3 — Dependency-Based Sequencing (Formations)](docs/USER_GUIDE.md#dependency-based-sequencing-formations) for the full recipe, diagram, and behavior callouts.

When all issues in a formation are closed, Fabrik archives the completed project board items after a 24-hour grace period — keeping the board clean automatically. Archived items remain accessible via GitHub's Archive view and the grace period survives engine restarts.

## The Self-Evolving Factory

Fabrik is used to develop Fabrik. Issues filed against this repo go through
the same Research → Plan → Implement → Review → Validate pipeline that Fabrik
orchestrates. When we filed an issue to add PR comment processing, Fabrik
researched its own codebase, planned the GraphQL changes, and will eventually
implement the feature that lets it read PR comments — gaining a capability it
needs by building it for itself. Ouroboros-as-a-service.

The human's role is product manager: file issues, answer questions when the
Research stage surfaces them, drag cards across the board, and occasionally
comment "please process PR feedback" when Copilot has opinions. The factory
does the rest.

## Migration from `./stages`

If you used Fabrik before `v0.2`, your stage configs were in `./stages` (the old default)
or `./stages/mystages/` if you used that convention.

```bash
mkdir -p .fabrik/stages
# If you used the default ./stages path:
cp ./stages/*.yaml .fabrik/stages/
# If you used ./stages/mystages/:
cp ./stages/mystages/*.yaml .fabrik/stages/
# Or keep the old path by setting FABRIK_STAGES in your .env:
# FABRIK_STAGES=./stages
```

## Documentation

- [User Guide](docs/USER_GUIDE.md) — full configuration reference for the pre-v0.2 `./stages` workflow (see "Quick Start" and "Migration from `./stages`" above for the current `./.fabrik/stages` default), workflow patterns, stage details, labels, and troubleshooting

## Architecture Decision Records

See [adrs/](adrs/) for documented decisions and their rationale.

## Requirements

- Go 1.26.1+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub personal access token with `repo` and `project` scopes
- A GitHub Project (v2) with board columns matching your stage names
