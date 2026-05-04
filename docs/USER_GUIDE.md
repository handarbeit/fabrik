---
layout: docs
title: User Guide
---

# Fabrik User Guide

Fabrik is an automated Claude Code SDLC driver powered by GitHub Issues and Projects.
This guide covers everything you need to set up, configure, and use Fabrik effectively.

For a quick overview see the [README](../README.md).
For details on the internal stage lifecycle, see [Stage Lifecycle](stage-lifecycle.md).
For the authoritative engine state-machine spec (label semantics, review gate transitions, marker handling), see [State Machine](state-machine.md).

---

## Table of Contents

1. [Getting Started](#1-getting-started)
2. [Configuration Reference](#2-configuration-reference)
3. [Workflow Patterns](#3-workflow-patterns)
4. [Stage Reference](#4-stage-reference)
5. [Plugin & Skills](#5-plugin--skills)
6. [Labels Reference](#6-labels-reference)
7. [Permissions](#7-permissions)
8. [TUI Dashboard](#8-tui-dashboard)
9. [Observability](#9-observability)
10. [Webhook Mode](#10-webhook-mode)
11. [Troubleshooting](#11-troubleshooting)

---

## 1. Getting Started

### Prerequisites

- Go 1.26.1+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- GitHub **classic** personal access token (`ghp_...`) with `repo`, `project`, and `workflow` scopes
  - Fine-grained tokens (`github_pat_...`) are **not supported** — GitHub Projects v2 GraphQL requires a classic PAT
  - Create one at: https://github.com/settings/tokens (select "Tokens (classic)")
  - If a fine-grained token is detected at startup, Fabrik prints a `[warn]` message and subsequent API calls include an actionable hint; see [GitHub API Returns 401 or Fine-Grained Token Warning](#github-api-returns-401-or-fine-grained-token-warning)
- A GitHub Project (v2) with board columns matching your stage names


### Initial Setup

**Option A: Install binary (requires `gh`)**

```bash
# Requires: gh auth login (with access to shadoworg/fabrik)
cd ~/bin  # or any directory on your PATH
gh release download --repo shadoworg/fabrik \
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
```

Then initialize:

```bash
# Initialize stage configs, plugin, and config template
./fabrik init
# Creates:
#   .fabrik/stages/       — stage YAML configs
#   .fabrik/plugin/       — Claude Code plugin
#   .fabrik/config.yaml   — project config template (edit this)
# Updates:
#   .git/info/exclude     — adds .fabrik/repos/, .fabrik/worktrees/, .fabrik/debug/,
#                           and .fabrik/history.json so they don't appear as untracked
#                           in git status (local excludes, not committed to the repo)
```

Pass your GitHub Project URL to auto-populate `owner`, `project`, and `owner_type` in
`config.yaml`. The `--user` flag enables fully non-interactive setup:

```bash
./fabrik init --user you https://github.com/orgs/your-org/projects/5
# or for a personal project:
./fabrik init --user you https://github.com/users/you/projects/3
```

Without a URL, `fabrik init` behaves as before: if your terminal is interactive it
prompts for owner, repo, project number, and username; otherwise it writes a
fully-commented template for you to fill in.

To refresh plugin skills without touching stages or config (e.g., after upgrading Fabrik):

```bash
./fabrik upgrade
```

Edit `.fabrik/config.yaml` with your project settings and commit it to git. Add your
GitHub token to a gitignored `.env` file:

```
# .env (gitignored — keep secrets here)
# Use a CLASSIC personal access token (ghp_...) — not a fine-grained token (github_pat_...)
# Required scopes: repo, project, workflow
# Create at: https://github.com/settings/tokens (select "Tokens (classic)")
FABRIK_TOKEN=ghp_...
```

> **Note:** When a `.git/` directory is present, Fabrik refuses to start if `.env` exists but is not listed in `.gitignore` (prevents accidental token leaks). In directories **without** `.git/` — containers, CI workspaces, or bare directories — this check is skipped and `.env` is loaded normally without requiring a `.gitignore` entry.

### Create a Project Board

Create a GitHub Project (v2) for your repository. Add board columns that correspond to
your stage names -- the column name must match the `name` field in each stage YAML file
exactly (case-sensitive). The default pipeline uses:

`Backlog` -> `Specify` -> `Research` -> `Plan` -> `Implement` -> `Review` -> `Validate` -> `Done`

### Compatibility Notes

> **Warning:** Global Claude Code plugins can interfere with Fabrik's headless sessions. Do not install global plugins on machines running Fabrik as a service. The `superpowers` plugin is a known offender — if installed globally, it injects unexpected behaviour into Fabrik's Claude sessions and causes duplicate comments on issues.
>
> To check for the problematic plugin:
> ```bash
> ls ~/.claude/plugins/cache/claude-plugins-official/
> ```
> If `superpowers` appears, remove it:
> ```bash
> rm -rf ~/.claude/plugins/cache/claude-plugins-official/superpowers
> ```
> See [§11 Troubleshooting → Duplicate Comments on Issues](#11-troubleshooting) for full details.

### First Run

With settings in `.fabrik/config.yaml`:

```bash
./fabrik
```

Or pass settings as flags:

```bash
./fabrik \
  --owner your-org \
  --repo your-repo \
  --project 1 \
  --user your-github-username
```

Fabrik polls the project board every 30 seconds by default (configurable with `--poll`
or `poll:` in `config.yaml`).

**Idle backoff**: When nothing needs work, Fabrik progressively slows down polling to conserve API rate limit budget. The backoff is time-based — after 5 minutes of idle polls, the interval doubles; after 10 minutes it quadruples; after 20 minutes it caps at 5 minutes. Any activity (items dispatched or deep-fetched) immediately resets the interval to the configured value. Press `w` in the TUI to wake polling instantly if you've just made changes on the board.

| Idle duration | Effective interval | At 30s base |
|---|---|---|
| 0–5 min | 1× (configured) | 30s |
| 5–10 min | 2× | 60s |
| 10–20 min | 4× | 120s |
| 20+ min | max (5 minutes) | 300s |

**Rate-limit backoff**: When Fabrik detects GraphQL API quota pressure, the same interval infrastructure adds a separate rate-limit component that escalates as quota depletes. There is no separate 5-minute ceiling for the rate-limit contributor — it can exceed 5 minutes when the configured base interval is large. The effective poll interval is `max(idle interval, rate-limit interval)`, so whichever is larger governs at any given moment.

| Remaining GraphQL quota | Multiplier | At 30s base | At 60s base |
|---|---|---|---|
| >=10% (incl. sticky zone 20%–50%) | 2× | 60s | 120s |
| >=5% and <10% | 4× | 120s | 240s |
| >=1% and <5% | 6× | 180s | 360s |
| <1% | 10× (cap) | 300s | 600s |

Rate-limit backoff uses two-threshold hysteresis to prevent thrashing: it **activates** when GraphQL remaining quota drops below **20%** of the hourly limit, and **clears only when quota rises above 50%** of the limit. While quota is recovering (20%–50%, sticky zone), the 2× tier applies. Activity detection (items deep-fetched or dispatched) resets idle backoff but does NOT reset rate-limit backoff — the two concerns are independent. When rate-limit backoff is active, Fabrik logs the effective poll interval each cycle so operators can observe the actual cadence.

**Board fetch resilience**: `FetchProjectBoard` automatically retries up to 3 times (with 1s/2s backoff) when GitHub returns an empty response (zero items), which occurs during transient Projects v2 indexer degradation that intermittently returns empty boards for non-empty projects (both returned node count and `totalCount` are zero in degraded responses). The log line:

```
[warn] project board fetch returned N items, totalCount=M (attempt X/3) — retrying in case of indexer hiccup
```

indicates the retry is occurring. It is transient and requires no user action unless it persists across all three attempts (in which case Fabrik accepts the last response and continues, which may appear as a temporarily idle engine).

Move an issue to `Specify` on the board to start processing it.

### Git Repositories and Worktrees

Fabrik always bare-clones each managed repository on first access. Run Fabrik from any directory (no need to be inside a git checkout of a managed repo):

```bash
mkdir ~/my-fabrik-dir && cd ~/my-fabrik-dir
fabrik init
./fabrik --owner myorg --project 5 --user me
# No --repo needed — Fabrik processes all repos on the board
```

Fabrik **always bare-clones** each managed repository on first access:
- Bare clones are stored at `.fabrik/repos/<owner>-<repo>.git`
- Worktrees are created at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N`
- One worktree manager runs per discovered repository, all sharing the same poll loop

**`.fabrik/repos/` is excluded from git tracking by default** (gitignored). Bare clone directories can be very large and should not be committed to your repository. When you run `fabrik init` inside a git repo, these paths are also added to `.git/info/exclude` automatically.

To restrict processing to a single repository, pass `--repo owner/repo`.

The `.fabrik/` directory (config, stages, plugin) always lives in the directory where you run `fabrik`.

### Multi-Repo Support

Fabrik can manage issues across every repository on the board in a single run. To enable multi-repo mode, omit (or comment out) the `repo:` field in `.fabrik/config.yaml` — Fabrik then discovers repositories lazily from the project board and processes issues from all of them. Each repository gets its own bare clone at `.fabrik/repos/<owner>-<repo>.git` and its own set of worktrees, all driven by the same poll loop.

### Git Clone Protocol (HTTPS vs SSH)

By default, Fabrik uses HTTPS to bare-clone managed repos (`https://github.com/<owner>/<repo>.git`). Users who authenticate via SSH keys can switch to SSH clone URLs instead.

#### Using SSH

Pass `--ssh` on the command line or add `git_ssh: true` to `.fabrik/config.yaml`:

```bash
# One-time: use SSH for this run
fabrik --ssh --owner myorg --project 5 --user me

# Persistent: add to .fabrik/config.yaml
git_ssh: true
```

The environment variable `FABRIK_GIT_SSH=true` is also supported (useful in CI).

When SSH mode is active, Fabrik uses `git@github.com:<owner>/<repo>.git` as the clone URL. This requires your SSH key to be registered with GitHub and accessible via `ssh-agent`.

> **Startup behavior with SSH mode:** When `--ssh` / `git_ssh: true` is set, Fabrik suppresses the HTTPS credential helper startup check (described below) — it is not applicable. No equivalent SSH agent availability check runs at startup. If your SSH key is absent or unloaded when a clone or fetch runs, git will fail at that point rather than at startup. To verify your agent before running Fabrik: `ssh -T git@github.com`.

#### HTTPS Credential Helper Warning

When using HTTPS (the default), Fabrik checks at startup whether a git credential helper is configured. If none is found, you'll see an advisory warning:

```
[startup] warn: no git credential helper configured; HTTPS cloning may prompt for credentials.
[startup] warn: configure one (e.g. git-credential-osxkeychain) or use --ssh / git_ssh: true in .fabrik/config.yaml.
```

This warning is non-fatal. If you use a GitHub token in your environment (`FABRIK_TOKEN` or `GITHUB_TOKEN`) and your git is configured to use it (e.g., via a credential helper or token-in-URL), cloning will work without further configuration.

To resolve the warning, either:
- Configure a credential helper: `git config --global credential.helper osxkeychain` (macOS), or `git config --global credential.helper store`
- Or switch to SSH mode with `--ssh` / `git_ssh: true`

#### Existing Bare Clones

**Switching between HTTPS and SSH does not re-clone existing bare repos.** Once Fabrik has cloned a repo, it reuses the existing bare clone with its original remote URL. The SSH/HTTPS setting only affects new clones.

If you switch to SSH mode after existing bare clones were created, those clones will continue to fetch via HTTPS. To switch an existing bare clone to SSH, update its remote URL directly:

```bash
git -C .fabrik/repos/owner-repo.git remote set-url origin git@github.com:owner/repo.git
```

#### URL Rewriting

If you have `url.<base>.insteadOf` configured in your global `~/.gitconfig` (a common way to redirect HTTPS to SSH globally), Fabrik will notice at startup and print an informational message. Git applies URL rewriting transparently, so Fabrik's HTTPS clone URLs will automatically use your SSH configuration — no additional Fabrik setting is needed.

### Auto-upgrade

The `--auto-upgrade` flag enables Fabrik to upgrade itself when idle. After 2
consecutive idle polls, Fabrik checks the GitHub Releases API for a newer version.
If one is found, it downloads the new binary, replaces the running executable,
runs `fabrik upgrade` to refresh plugin skills, and re-execs itself.

**Dev builds (built from source)** follow the same 2 idle poll threshold but use a
different upgrade path. Fabrik detects that it is a dev build (version string starts
with `dev`) and checks whether it is running from a `tenaciousvc/fabrik` or
`verveguy/fabrik` source checkout. If so, it compares the running binary's embedded
commit SHA against the local `HEAD`:

- **Local commits ahead of the binary**: rebuild immediately from the current working
  tree (`go build -o fabrik .`) without pulling — useful when you have local changes
  you've already compiled once.
- **Remote `origin/main` ahead of HEAD**: run `git pull --ff-only` to update, then
  rebuild.

After rebuilding, Fabrik runs `fabrik upgrade` (non-interactive, silent — see below)
and re-execs the new binary. This keeps dev builds current with the same hands-off
experience as release binaries.

> **`fabrik upgrade` in non-interactive mode**: Whether called automatically during
> auto-upgrade or run standalone, `fabrik upgrade` behaves differently depending on
> whether a TTY is attached. With no TTY (non-interactive), it auto-refreshes plugin
> skills silently — no prompt, no confirmation required. With a TTY (interactive), it
> prompts once before applying any skill updates. This means automated environments
> (CI, background processes, auto-upgrade re-exec) always get a clean, unattended
> skill refresh.

```bash
./fabrik --auto-upgrade --owner your-org --repo your-repo --project 1 --user you
```

> **Upgrading from v0.0.28 or earlier?** Before v0.0.29, `--auto-upgrade` fetched
> from the private `tenaciousvc/fabrik` repo. The v0.0.29 release switched the
> upgrade source to the public `shadoworg/fabrik` repo — but this change cannot
> self-apply: a binary built before v0.0.29 will never auto-receive it. If you are
> running an older binary, upgrade once manually:
> ```bash
> gh release download --repo shadoworg/fabrik \
>   --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
>   -O - | tar xz
> ```
> After that, `--auto-upgrade` will keep you current automatically.

### Startup Board Validation

On startup, Fabrik attempts to fetch the project board and status field so it can compare stage names in your YAML configs against the column names on the board. When that metadata is fetched successfully, any non-cleanup stage missing from the board causes Fabrik to exit with a detailed error listing the mismatched names — catching config drift before any work begins. Extra board columns without a matching stage produce a warning but do not block startup. If Fabrik cannot fetch the board or status field, it logs a warning and continues without blocking startup.

See [§11 Troubleshooting → Startup Board Validation Failure](#startup-board-validation-failure) if validation succeeds and reports a mismatch.

### Stage YAML Drift Warning

After loading your stage YAMLs from `.fabrik/stages/`, Fabrik compares each stage against the embedded default with the same `name:` field. If the embedded default contains top-level YAML keys that your file does not, Fabrik prints a warning to stderr at startup:

```
[startup] warning: .fabrik/stages/validate.yaml is missing fields present in v0.0.53 defaults: wait_for_ci, wait_for_reviews. Run `fabrik refresh-stages --apply` to add the missing keys.
```

This warning is **informational only** — the engine continues running with your existing config. The missing keys are behavioral options added in a newer binary that your stage file predates.

**What to do:**

1. Run `fabrik refresh-stages` to preview missing keys as a unified diff (no file changes).
2. Run `fabrik refresh-stages --apply` to add the missing keys to your stage YAMLs (additive only — never removes or overwrites existing values).
3. Run `git diff` to review the changes.
4. `git commit` to record the update.

**Common keys that trigger this warning:**

- `wait_for_ci: true` — enables the CI gate on Validate auto-advance (added in v0.0.49)
- `wait_for_reviews: true` — enables the reviewer gate on Validate auto-advance (added in v0.0.49)
- `completion:` — marks the stage completion type (added as an explicit top-level key; omitting it is safe for normal Claude-invoked stages because the engine defaults it to `{type: claude}`)

Custom stages (names not present in any embedded default) are silently skipped — no warning is produced for them.

### Instance Lock

> **Note:** When Fabrik starts, it creates a PID lock file at `.fabrik/fabrik.lock`. If a second instance attempts to start in the same directory, it reads the lock file, logs an error identifying the running process, and exits immediately. The lock is automatically released when the process exits — including on crash or SIGKILL — so there is no need to manually delete the file after an unclean shutdown.
>
> See [§11 Troubleshooting → Multiple Fabrik Instances](#11-troubleshooting) if you encounter a stale lock or need to run multiple instances against different projects.

---

## 2. Configuration Reference

### Settings Overview

Fabrik resolves settings in this order (highest to lowest priority):

```
CLI flag  >  shell env var  >  .env file  >  .fabrik/config.yaml  >  built-in default
```

Use `.fabrik/config.yaml` for non-secret project settings (commit it to git).
Use `.env` for secrets only (`FABRIK_TOKEN` / `GITHUB_TOKEN`).

### `.fabrik/config.yaml`

Generated by `fabrik init`. Commit this file — it carries project settings with the repo.

```yaml
# .fabrik/config.yaml
owner: your-org
# repo: your-repo  # omit for multi-repo mode (processes all repos on the board)
project: 1
user: your-github-username

# Optional settings (defaults shown):

# Path to stage YAML configs directory.
# stages: ./.fabrik/stages

# Polling interval in seconds. Lower values are more responsive; higher values
# reduce GitHub API usage. Tradeoff: 10s is very responsive but consumes ~360 REST
# requests/hour; 30s (default) is a good balance.
# poll: 30

# Maximum number of parallel Claude sessions. Tune based on your API tier capacity.
# Each active session counts against your Anthropic API concurrency limit.
# max_concurrent: 5

# Maximum stage failures before pausing an issue. When exceeded, fabrik:paused and
# stage:<name>:failed labels are applied. Set to 0 for unlimited retries.
# max_retries: 3

# Auto-advance issues through stages without human card moves on the board.
# When true, completed issues advance to the next stage automatically.
# Per-stage auto_advance: in stage YAML can override this setting per-stage.
# yolo: false

# When idle, check shadoworg/fabrik GitHub Releases for a newer version. After 2
# consecutive idle polls, Fabrik downloads the new binary, runs fabrik upgrade,
# and re-execs. Requires internet access to the GitHub Releases API.
# auto_upgrade: false

# Disable the interactive TUI dashboard (enabled by default when a real terminal is detected).
# tui: false

# Save raw Claude output to .fabrik/debug/ for diagnosing unexpected behavior
# or prompt issues. Files are named by issue number and stage.
# debug_output: false

# Project version shown in the TUI footer. Auto-inferred from package.json,
# go.mod (returns module path, not semver), Cargo.toml, or pyproject.toml.
# Set explicitly to override auto-inference (e.g., version: "1.2.0").
# version: ""
```

**Multi-repo mode:** When `repo:` is commented out or omitted, Fabrik processes issues from *all* repositories on the project board. Use this when your project board spans multiple repos (cross-org collaborations, monorepos with independent sub-repos, or a single board managing several distinct services). To restrict Fabrik to one repository, uncomment and set `repo:`.

**Note:** `.fabrik/config.yaml` should NOT be listed in `.gitignore`. Fabrik warns
(non-fatal) if it is.

### `.env` File

Keep only secrets here. When a `.git/` directory is present, Fabrik refuses to start if `.env` exists but is not listed in `.gitignore` (prevents accidental token leaks). In directories without `.git/` (containers, CI, bare directories), this check is skipped and `.env` is loaded normally.

```
# .env (gitignored)
# Classic personal access token (ghp_...) required — see https://github.com/settings/tokens
FABRIK_TOKEN=ghp_...         # Preferred token env var (needs repo, project, workflow scopes)
GITHUB_TOKEN=ghp_...         # Fallback token env var
```

For per-developer identity overrides (when your username differs from config.yaml):

```
FABRIK_USER=my-personal-username
```

### Command-Line Flags

| Flag | Description | Default |
|------|-------------|---------|
| `--owner` | GitHub repo owner (org or user) | required |
| `--repo` | GitHub repo name; omit to enable multi-repo mode (processes all repos on the board) | optional |
| `--project` | GitHub Project (v2) number | required |
| `--user` | Your GitHub username -- only processes comments by this user | required |
| `--token` | GitHub API token | `$GITHUB_TOKEN` |
| `--stages` | Directory containing stage YAML configs | `./.fabrik/stages` |
| `--yolo` | Auto-advance issues through stages without human approval; also auto-merges the linked PR when Validate completes | `false` |
| `--auto-upgrade` | When idle, check GitHub Releases for a newer version and self-upgrade; dev builds (built from source) rebuild from `origin/main` instead | `false` |
| `--notui` | Disable the interactive TUI dashboard | TUI on by default |
| `--plugin-dir` | Path to Fabrik plugin directory (overrides `.fabrik/plugin/`) | auto-detected |
| `--poll` | Poll interval in seconds | `30` |
| `--max-concurrent` | Maximum number of concurrent issue workers | `5` |
| `--max-retries` | Max failed stage attempts before pausing the issue (0 = unlimited) | `3` |
| `--review-wait-timeout` | Minutes to wait for all requested PR reviewers before advancing (0 = use default of 15; also `FABRIK_REVIEW_WAIT_TIMEOUT`) | `0` (15 min) |
| `--max-review-cycles` | Maximum number of review-and-fix cycles per issue (0 = use default of 5; also `FABRIK_MAX_REVIEW_CYCLES`) | `0` (5 cycles) |
| `--ci-wait-timeout` | Minutes to wait for CI checks to pass before pausing (0 = use default of 30; also `FABRIK_CI_WAIT_TIMEOUT`) | `0` (30 min) |
| `--max-ci-fix-cycles` | Maximum number of CI-fix re-invocation cycles per issue (0 = use default of 5; also `FABRIK_MAX_CI_FIX_CYCLES`) | `0` (5 cycles) |
| `--max-rebase-cycles` | Maximum number of rebase re-invocation cycles per issue before pausing (0 = use default of 3; also `FABRIK_MAX_REBASE_CYCLES`) | `0` (3 cycles) |
| `--claude-wait-delay` | Seconds to wait after Claude exits before recovering buffered output; prevents worker goroutines from blocking when Claude uses `run_in_background` or the Monitor tool, which can hold stdout open after the main Claude process exits (0 = use built-in default of 30 sec; also `FABRIK_CLAUDE_WAIT_DELAY`) | `0` (30 sec) |
| `--debug-output` | Save Claude stage output to `.fabrik/debug/` | `false` |

### Environment Variables

| Variable | `config.yaml` key | Description | Default |
|----------|-------------------|-------------|---------|
| `FABRIK_TOKEN` | *(secrets only)* | GitHub **classic** personal access token (`ghp_...`) with `repo`, `project`, `workflow` scopes (preferred) | required |
| `GITHUB_TOKEN` | *(secrets only)* | GitHub **classic** personal access token (`ghp_...`) — fallback when `FABRIK_TOKEN` is unset | required |
| `FABRIK_OWNER` | `owner` | GitHub repo owner | -- |
| `FABRIK_REPO` | `repo` | GitHub repo name; optional — omitting enables multi-repo mode (all repos on the board) | -- |
| `FABRIK_PROJECT_NUMBER` | `project` | GitHub Project (v2) number | -- |
| `FABRIK_USER` | `user` | Your GitHub username | -- |
| `FABRIK_STAGES` | `stages` | Stage configs directory | `./.fabrik/stages` |
| `FABRIK_YOLO` | `yolo` | Auto-advance (`true`/`1`/`yes`) | `false` |
| `FABRIK_POLL` | `poll` | Poll interval in seconds | `30` |
| `FABRIK_MAX_CONCURRENT` | `max_concurrent` | Max parallel Claude sessions | `5` |
| `FABRIK_MAX_RETRIES` | `max_retries` | Max retries before pausing (0 = unlimited) | `3` |
| `FABRIK_AUTO_UPGRADE` | `auto_upgrade` | Self-upgrade when idle (`true`/`1`/`yes`) | `false` |
| `FABRIK_TUI` | `tui` | Disable TUI dashboard (`false`/`0`/`no`) | `true` |
| `FABRIK_PLUGIN_DIR` | *(no config.yaml key)* | Override plugin directory | `.fabrik/plugin/` |
| `FABRIK_DEBUG_OUTPUT` | `debug_output` | Save raw Claude output for debugging | `false` |
| `FABRIK_REVIEW_WAIT_TIMEOUT` | *(no config.yaml key)* | Minutes to wait per review cycle at the review gate (whether waiting for requested reviewers to submit or for at least one review to exist) before pausing with `fabrik:awaiting-input` (positive integer; invalid or unset values default to 15) | `15` |
| `FABRIK_MAX_REVIEW_CYCLES` | *(no config.yaml key)* | Maximum number of review re-invocation cycles per issue before pausing with `fabrik:awaiting-input` (positive integer; invalid or unset values default to 5) | `5` |
| `FABRIK_CI_WAIT_TIMEOUT` | *(no config.yaml key)* | Minutes to wait for CI checks to pass before pausing with `fabrik:awaiting-input` (positive integer; invalid or unset values default to 30) | `30` |
| `FABRIK_MAX_CI_FIX_CYCLES` | *(no config.yaml key)* | Maximum number of CI-fix re-invocation cycles per issue before pausing with `fabrik:awaiting-input` (positive integer; invalid or unset values default to 5) | `5` |
| `FABRIK_MAX_REBASE_CYCLES` | *(no config.yaml key)* | Maximum number of rebase re-invocation cycles per issue before pausing with `fabrik:awaiting-input` (positive integer; invalid or unset values default to 3). Lower than CI/review because rebase either succeeds in one shot or needs human judgment for a semantic conflict. | `3` |
| `FABRIK_CLAUDE_WAIT_DELAY` | *(no config.yaml key)* | Seconds to wait after Claude exits before recovering buffered output (non-negative integer; `0` or unset uses the default of 30; invalid values default to 30). Prevents worker goroutines from blocking indefinitely when Claude uses `run_in_background` or the Monitor tool, which can hold stdout open after the main Claude process exits. | `30` |

Token precedence: `--token` flag > `FABRIK_TOKEN` > `GITHUB_TOKEN`

### Stage YAML Reference

Each stage is a YAML file in your stages directory. The filename is arbitrary; the
`name` field determines which board column it matches.

```yaml
name: Research            # Required. Must match a Project board column exactly.
order: 2                  # Required. Lower values processed earlier in the pipeline.
skill: fabrik-research    # Plugin skill to use (recommended; alternative to inline prompt).
                          #   When set, Fabrik sends a minimal directive and Claude loads
                          #   the skill methodology via the plugin system.
comment_skill: fabrik-research-comment  # Plugin skill for comment review (overrides comment_prompt).
prompt: |                 # Inline prompt (used when skill is not set; legacy but still supported).
  You are a research agent...
comment_prompt: |         # Inline comment-review prompt (used when comment_skill is not set).
  You are reviewing user comments...
model: sonnet             # Optional. Claude model: "opus", "sonnet", etc.
max_turns: 50             # Optional. Max conversation turns per main stage invocation.
comment_max_turns: 15     # Optional. Max turns when processing user comments. Defaults to
                          #   min(max_turns, 15) when max_turns is set, otherwise 15.
                          #   Keeps comment processing bounded independently of stage budget.
max_wall_time: "45m"      # Optional. Wall-clock deadline for a single Claude invocation.
                          #   Accepts Go duration strings (e.g. "30m", "1h", "1h30m").
                          #   When the deadline is reached, Fabrik sends SIGTERM to the
                          #   Claude process group, waits 10 s, then sends SIGKILL (reaping
                          #   any hung background children). Output collected before the kill
                          #   is processed normally — if FABRIK_STAGE_COMPLETE appears in
                          #   the streamed output, the stage is marked complete without a
                          #   retry. When absent or zero, no wall-clock cap is applied.
                          #   Recommended: "45m" for Implement and Review stages in
                          #   production use. The hardcoded 15-minute inactivity timeout
                          #   (see below) acts as a universal backstop even when this field
                          #   is not set.
read_only: true           # Optional. Stashes the dirty worktree before Claude runs and
                          #   restores it after. Use for analysis stages that should not
                          #   modify files (e.g., Specify, Research).
post_to_pr: true          # Optional. Routes detailed Claude output to the linked PR; a
                          #   brief summary is still posted on the issue. Falls back to
                          #   posting on the issue if no linked PR is found.
create_draft_pr: true     # Optional. Pushes the branch and creates a draft PR *before*
                          #   Claude runs. Idempotent if a PR already exists.
mark_pr_ready_on_complete: true  # Optional. Marks the draft PR as review-ready after the
                                 #   stage completes. Triggers external review bots.
auto_advance: false       # Optional. Per-stage override for the global yolo setting.
                          #   true  = always auto-advance this stage (even if yolo: false)
                          #   false = never auto-advance this stage (even if yolo: true)
                          #   omit  = inherit the global yolo setting
cleanup_worktree: false   # Optional. Removes the issue worktree instead of invoking Claude.
                          #   Use for terminal stages (e.g., Done) where no further work
                          #   is needed on the branch.
wait_for_reviews: false   # Optional. When true, Fabrik waits for all requested PR reviewers
                          #   to submit and re-invokes the stage agent via the comment-processing
                          #   path to address submitted inline feedback. Re-invocation is
                          #   unconditional — it fires regardless of the auto-advance setting.
                          #   Auto-advancement to the next stage after re-invocation still requires
                          #   auto-advance to be active. This loop repeats until no reviewers are
                          #   pending (up to FABRIK_MAX_REVIEW_CYCLES cycles). On timeout or cycle
                          #   limit, Fabrik pauses with fabrik:awaiting-input instead of advancing.
                          #   See §3 Pending Reviewer Gate for full details.
wait_for_ci: false        # Optional. When true, Fabrik gates auto-advance (and auto-merge for
                          #   Validate+yolo) on CI checks passing on the PR head. At
                          #   FABRIK_STAGE_COMPLETE, Fabrik immediately adds `fabrik:awaiting-ci`
                          #   and withholds `stage:X:complete` until the CI gate clears (covering
                          #   both pending and failed CI states). When CI fails, Fabrik re-invokes
                          #   the stage agent via ci_fix_skill (falls back to comment_skill) with a
                          #   structured report classifying each failed check as NEW REGRESSION or
                          #   pre-existing. Re-invocation repeats up to FABRIK_MAX_CI_FIX_CYCLES
                          #   times. On CI timeout or cycle limit, Fabrik pauses with
                          #   fabrik:awaiting-input. Enabled by default on the Validate stage.
                          #   See §3 CI Gate and CI-Fix Workflow for full details.
ci_fix_skill:             # Optional. Plugin skill name for CI-fix re-invocations (the synthetic
                          #   CI failure report comment). Defaults to comment_skill if unset.
allowed_tools:            # Optional. REPLACES the default tool set — not additive. When set,
  - Read                  #   only these tools are allowed; the default list is not added.
  - Grep                  #   When omitted, Fabrik uses a comprehensive default covering common
  - Glob                  #   SDLC tools: Read, Edit, Write, Glob, Grep, TodoWrite, Skill, Task,
                          #   Bash(git:*), Bash(gh:*), Bash(go:*), Bash(npm:*), Bash(npx:*),
                          #   Bash(yarn:*), Bash(pnpm:*), Bash(make:*), Bash(cargo:*),
                          #   Bash(python:*), Bash(pip:*), Bash(uv:*), Bash(pytest:*),
                          #   Bash(ls:*), Bash(cat:*), Bash(rm:*), Bash(cp:*), Bash(mv:*),
                          #   Bash(mkdir:*), Bash(find:*).
                          #   Use this to restrict read-only stages (Research, Plan, Specify)
                          #   or to limit Claude to project-specific tools.
disable_adaptive_thinking: true  # Optional. Disables Claude Code's adaptive (auto-reduced)
                                 #   thinking budget. Default: true.
effort_level: high        # Optional. Claude Code thinking effort: low, medium, high, max.
                          #   Default: high. Controls how much the model "thinks" before
                          #   responding. Higher values use more tokens.
completion:
  type: claude            # Only supported type (default).
```

Either `skill` or `prompt` is required (unless `cleanup_worktree` is true). When `skill`
is set, Fabrik sends a directive prompt telling Claude to follow the named skill; the
skill provides the detailed methodology via the plugin system. Prefer `skill` for complex
stages — it supports rich methodology, quality checklists, and scope boundaries. Use
`prompt` for simple single-purpose stages or quick overrides.

**`auto_advance` and `yolo` interaction:**

There are four cases:

1. Global `yolo: true` in `config.yaml` → all stages auto-advance after completion.
2. Per-stage `auto_advance: true` in stage YAML → this stage always auto-advances, regardless of whether global `yolo` is true or false.
3. Per-stage `auto_advance: false` in stage YAML → this stage never auto-advances, even when global `yolo: true`. This is a meaningful override — explicitly setting `false` is different from omitting the field.
4. Per-stage `auto_advance:` absent from stage YAML → the stage inherits the global `yolo` setting.

A fifth case applies at the issue level: adding the `fabrik:yolo` label to an issue forces auto-advance for that issue even when `auto_advance: false` is set in the stage YAML. The `fabrik:yolo` label also triggers auto-merge of the linked PR when the Validate stage completes (equivalent to running with `--yolo` globally, but scoped to a single issue).

**Thinking budget (`disable_adaptive_thinking`, `effort_level`):**

These two fields control how much Claude "thinks" before responding. `disable_adaptive_thinking: true` (the default) turns off Claude Code's adaptive thinking budget, which would otherwise auto-reduce thinking depth to save tokens. With adaptive thinking disabled, the `effort_level` field directly controls thinking intensity: `low`, `medium`, `high`, or `max`. The default `effort_level` is `high` (changed from `max` in v0.0.33 to reduce token usage without sacrificing quality). Use `effort_level: max` only for your most demanding stages — complex implementation or thorough review work — where higher token cost is justified.

**Timeout and inactivity protection (`max_wall_time` + 15-minute inactivity kill):**

Fabrik applies two complementary timeout mechanisms to every Claude invocation to prevent indefinite hangs from stuck background processes:

1. **`max_wall_time` (per-stage, opt-in):** A hard wall-clock deadline. When the deadline expires, Fabrik sends `SIGTERM` to the Claude process group (which includes any background children spawned during the session), waits 10 seconds for a graceful exit, then sends `SIGKILL` to any surviving processes. Recommended: `"45m"` for Implement and Review stages in production. Legitimately long stages (e.g., a 90-minute Review on a large PR) should either set a higher limit or leave this field unset.

2. **15-minute inactivity timeout (global, always active):** If no streamed output is received from Claude for 15 consecutive minutes, the process group is killed using the same SIGTERM → 10s → SIGKILL sequence. This catches sessions that are stuck on a hung background task even when `max_wall_time` is not set — as long as Claude is actively producing output, it continues indefinitely. The threshold is hardcoded and cannot be configured.

In both cases, if `FABRIK_STAGE_COMPLETE` was emitted before the kill (visible in the streamed `assistant` turns), the stage is treated as successfully completed without retrying. Both timeouts apply equally to main-stage invocations and comment-processing invocations.

---

## 3. Workflow Patterns

### How Issues Move Through the Pipeline

1. Create an issue and add it to your GitHub Project board.
2. Move the issue to a stage column (e.g., `Specify`).
3. Fabrik picks it up on the next poll, creates a worktree, and invokes Claude Code.
4. Claude works in the worktree and posts progress as issue comments.
5. When Claude completes the stage, the `stage:<name>:complete` label is applied.
6. In `--yolo` mode, the issue is automatically moved to the next stage column.
   Otherwise, a human reviews and drags the card.

### The Specify Stage

The Specify stage is the first active stage. It takes a rough backlog issue and
refines it into a clear, unambiguous spec:

- Surfaces missing requirements, ambiguities, and edge cases as questions
- Checks consistency with existing project features and documentation
- Researches prior art and established patterns on the web
- Rewrites the issue body with a structured spec

The user answers questions via comments. Claude incorporates the answers and updates
the issue body. Once all questions are resolved, the stage completes and the issue is
ready for Research.

### Steering with Comments

Fabrik responds to natural language comments you post on an issue. Claude sees the full
issue body and all prior comments, so context carries forward.

**Effective comment patterns:**

- *"Please link the PR to this issue"* -- Claude creates the PR link
- *"Let's use approach B instead"* -- Claude updates the plan and continues
- *"The answer to your question about X is Y"* -- Claude incorporates your answer
- *"Please push and link the PR"* -- Claude pushes the branch and creates a draft PR

When you post a comment:
1. Fabrik reacts with eyes to acknowledge the comment.
2. Claude is invoked with the stage's comment prompt (or a default).
3. Claude performs any requested actions.
4. If the issue body should be updated, Claude outputs the new body between
   `FABRIK_ISSUE_UPDATE_BEGIN` and `FABRIK_ISSUE_UPDATE_END` markers.
5. Fabrik updates the issue body and strips the markers from the posted comment.
6. Fabrik reacts with rocket to mark the comment as processed.

The rocket reaction is durable -- on restart, Fabrik skips comments that already have it.

Engine-posted comments are identified by **two dedup signals**: the `🏭 **Fabrik` header (primary) and the 🚀 rocket reaction (secondary). Both categories are skipped when scanning for new user comments to process.

### Reaction Flow

| Reaction | Meaning |
|----------|---------|
| Eyes | Comment received and queued for processing |
| Rocket | Comment has been fully processed |

### When to Intervene

You do not need to babysit the pipeline. The intended human role is:

- **File issues** with clear specs in the body (or let Specify refine them).
- **Answer questions** when Specify or Research surfaces unknowns.
- **Move cards** (or use `--yolo` to automate this).
- **Comment** to steer when the plan goes sideways or you want to redirect.
- **Review PRs** before merging -- Fabrik gets them review-ready, not merge-ready (unless `--yolo` or `fabrik:yolo` label is active, in which case Fabrik auto-merges after Validate).

### Yolo Mode and Auto-Merge

Pass `--yolo` to enable global auto-advance: Fabrik moves issues through every stage automatically without waiting for human approval, and auto-merges the linked PR once Validate completes. To scope the same behavior to a single issue, apply the `fabrik:yolo` label — Fabrik auto-advances and auto-merges that issue only.

For lighter automation without auto-merge, use `fabrik:cruise`: it auto-advances through all stages but stops at Validate, leaving the merge decision to you.

See [Stage YAML Reference](#stage-yaml-reference) for the `auto_advance` field, which controls auto-advance behavior per stage independent of the global `--yolo` flag.

### Draft PR Workflow

The Implement stage creates a **draft PR** linked to the issue when `create_draft_pr: true` is set (the default for Implement). This gives you a place to review incrementally. The Review stage then rebases, reviews, fixes, and pushes — turning the draft into a review-ready PR.

**PR body seeding**: When the draft PR is created, Fabrik seeds the PR body from context files already written in the worktree:

| Section | Source |
|---------|--------|
| `## Summary` | Extracted from the issue spec (`.fabrik-context/issue.md`) |
| `## Problem` | Extracted from the issue spec (`.fabrik-context/issue.md`) |
| `## Approach` | Extracted from the Plan stage output (`.fabrik-context/stage-Plan.md`); falls back to a placeholder if absent |
| `## Verification` | Placeholder text, auto-replaced by an extracted stage summary when available |

The `Closes #N` footer in the PR body links the PR to the issue so Fabrik can discover PR comments via GraphQL.

**Verification auto-update**: For draft PRs created with `create_draft_pr: true`, Fabrik updates the `## Verification` section only when it can extract a summary block delimited by `FABRIK_SUMMARY_BEGIN` and `FABRIK_SUMMARY_END` from stage output. This keeps the PR description current when a stage provides a structured summary for PR-body updates.

### Retry and Escalation

When a stage doesn't complete (Claude doesn't output `FABRIK_STAGE_COMPLETE`):

1. **Cooldown**: Fabrik waits `poll_interval x 10` seconds (default 5 minutes) before retrying.
2. **Resume**: On retry, Claude resumes the existing conversation session with full context.
   The worktree is left as-is (no rebase) to preserve Claude's context.
3. **WIP commit**: Partial work is committed and pushed to preserve progress.
4. **Max retries**: After `--max-retries` failures (default 3):
   - `fabrik:paused` and `stage:<name>:failed` labels are added
   - An explanatory comment is posted on the issue
   - The issue stops being processed until a human investigates

To resume after escalation: remove the `fabrik:paused` label. Fabrik will clear the
failed label, reset the retry count, and try again immediately.

### Stages Waiting for Input

A stage can signal that it needs user input before it can proceed by outputting
`FABRIK_BLOCKED_ON_INPUT`. This is different from a failure — the stage is not broken,
it just needs a question answered.

When a stage outputs `FABRIK_BLOCKED_ON_INPUT`:
1. `fabrik:paused` and `fabrik:awaiting-input` labels are added to the issue
2. The retry counter is **not** incremented — this does not count as a failure
3. The issue waits silently until the configured Fabrik user (`--user` / `FABRIK_USER`) posts a new comment

When the configured user posts a new comment:
1. Fabrik detects the comment and automatically removes both labels
2. Comment processing is triggered immediately (no manual card move needed)
3. The comment processing run can output `FABRIK_STAGE_COMPLETE` to finish the stage
   directly, without needing an additional stage invocation

This is the intended mechanism for Q&A in stages like Specify — Claude asks a question,
the configured user answers it in a comment, and the stage resumes automatically.

### Dependency-Based Sequencing (Formations)

Fabrik supports dependency-based sequencing of issues using GitHub's native "Blocked by" relationships. This enables **formations** — coordinated sets of issues that execute in parallel where possible and respect ordering constraints automatically.

#### Setting Up Dependencies

To mark one issue as blocked by another on GitHub.com:

1. Open the issue in GitHub
2. In the right sidebar, find the **Relationships** section
3. Click **"Mark as blocked by"**
4. Search for and select the blocking issue (same repo or cross-repo)

Repeat for each dependency. This uses GitHub's native `blockedBy` GraphQL field (available on all GitHub plans since August 21, 2025).

#### How Fabrik Detects and Respects Dependencies

Fabrik uses the `fabrik:blocked` label to track blocked issues. The label lifecycle is fully automatic:

1. **Detection**: Fabrik re-evaluates blocked items periodically via a cooldown timer (every `10 × poll interval`, typically ~5 minutes at the default 30s poll). When the timer fires, Fabrik deep-fetches the blocked item and checks whether its dependencies have closed. If GitHub also bumps the blocked item's `updatedAt` when a dependency closes (common but not guaranteed), unblocking is detected sooner — on the next poll cycle.
2. **First block**: When Fabrik first detects that an issue is blocked, it posts a comment listing the open blocking issues and adds the `fabrik:blocked` label automatically. Fabrik creates this label on first use — no pre-creation needed.
3. **While blocked**: The issue is skipped silently each poll cycle (no duplicate comments).
4. **Automatic unblocking**: When all blocking issues are closed, Fabrik removes `fabrik:blocked` and resumes processing on the next poll — no human action required.

**Key behavior callouts:**

- **The first stage (Specify) always runs regardless of blockers** — dependencies only gate subsequent stages. This allows a formation to be fully specified before execution begins.
- **Independent issues start in parallel** — issues with no blockers are dispatched concurrently up to the configured `MaxConcurrent` limit.
- **Failed issues retry independently** — a failure in one formation member does not affect siblings.
- **Cross-repo dependencies are supported** — a blocking issue can be in a different repository. Fabrik displays cross-repo blockers as `owner/repo#N` in log output.

#### Formation Recipe

1. **File all issues** for the formation. Write specs at the right level of granularity — each issue should be independently implementable.
2. **Add blocked-by edges** in GitHub using the Relationships sidebar (see above).
3. **Label all issues `fabrik:yolo`** — this makes the formation hands-free. Without `fabrik:yolo`, each stage requires a manual card move on the project board.
4. **Move all issues to Specify** on the project board. Fabrik will pick them up on the next poll.
5. **Watch it run** — Fabrik executes Specify for all issues in parallel, then gates subsequent stages on dependency resolution automatically.

#### Example Formation

```mermaid
graph TD
    A["#1 Data model design"] --> C["#3 API layer"]
    B["#2 Auth design"] --> C
    C --> D["#4 Frontend integration"]
    C --> E["#5 Background jobs"]
```

In this 5-issue formation:
- Issues #1 and #2 start immediately in parallel (no blockers)
- Issue #3 starts after both #1 and #2 are closed
- Issues #4 and #5 start after #3 is closed (in parallel with each other)

**Real-world validation:** A 9-issue formation with 7 dependency edges was run on the Ambient project — 4 issues started in parallel, all pipeline constraints were respected automatically, completing in ~88 minutes wall-clock time at $31 total cost.

### Issue Decomposition

When Plan determines that an issue is too broad for a single Implement cycle, it can autonomously split it into focused sub-issues — each small enough to implement cleanly and independently.

**No user configuration required.** Decomposition is Plan's judgment call. If the issue is well-scoped, Plan produces a normal implementation plan. If it's too broad, Plan decomposes it.

#### What Happens

1. **Plan creates sub-issues** via `gh` CLI, labels them `fabrik:sub-issue`, and adds them to the project board.
2. **Plan sets up "blocked by" edges** between sub-issues where sequential ordering is required.
3. **Plan outputs `FABRIK_DECOMPOSED`** and stops — no `FABRIK_STAGE_COMPLETE` is emitted.
4. **The engine adds `stage:Plan:complete`** to the parent issue and moves it to Done.
5. **Sub-issues appear in Research** and flow through the full pipeline independently.

Sub-issues form a [formation](#dependency-based-sequencing-formations) automatically — dependency edges set by Plan are enforced during execution.

#### What You Observe

- The parent issue moves to Done on the project board (its label set shows `stage:Plan:complete`).
- New issues appear in Research, each labeled `fabrik:sub-issue`.
- If Plan set up blocking edges, sub-issues that depend on each other will wait as a formation.

#### Depth Limit

Sub-issues (those labeled `fabrik:sub-issue`) are **never decomposed further**. If Plan encounters an issue with that label, it produces a normal implementation plan regardless of scope. Maximum decomposition depth is 1.

### Pending Reviewer Gate

When a stage has `wait_for_reviews: true` set, Fabrik waits for all requested PR reviewers to submit their reviews, then re-invokes the stage agent to address any submitted inline feedback. **Re-invocation is unconditional** — it fires for any issue with submitted inline review thread comments, regardless of whether auto-advance is active. Auto-advancement to the next stage after re-invocation still requires auto-advance to be active (global `yolo: true`, per-stage `auto_advance: true`, or the `fabrik:yolo` label on the issue).

For the authoritative spec on label lifecycle and gate transitions, see [State Machine](state-machine.md).

The Review and Validate stages ship with `wait_for_reviews: true` enabled by default.

#### Enabling the Gate

Add `wait_for_reviews: true` to the relevant stage YAML:

```yaml
name: Review
order: 4
wait_for_reviews: true
...
```

The reviewer wait and re-invocation cycle fire unconditionally — they activate whenever `wait_for_reviews: true` and inline review feedback is present, regardless of whether auto-advance is active. If you're manually dragging cards through the board, re-invocation still happens; the issue will not advance automatically after the cycle completes — you still move the card manually.

#### Label Lifecycle

When the gate is active, Fabrik adds the `fabrik:awaiting-review` label to the issue. This label:

- Makes the wait state visible on the project board
- Is cleared automatically either when no requested reviewers are outstanding **and** at least one review has been submitted — this dual condition catches bot reviewers (Copilot, Gemini) that self-trigger via webhook without ever appearing in the formal requested-reviewer list — **or** when the review-wait timeout expires and Fabrik removes the label before pausing with `fabrik:awaiting-input`
- Triggers a re-invocation of the stage agent (via the comment-processing path) to address the submitted review feedback
- After re-invocation, if new reviewers are assigned (e.g. bots triggered by a fresh push), the label is re-applied and the cycle continues

#### Three-Phase Mechanism

The gate uses a three-phase design:

1. **Phase 1 (always-gate):** On stage completion, Fabrik immediately adds `fabrik:awaiting-review` and skips auto-advance. This fires even before reviewer assignments propagate.
2. **Phase 2 (gate evaluation):** On subsequent poll cycles, Fabrik re-fetches the PR with fresh GraphQL data and evaluates the dual condition: the gate clears only when no requested reviewers are outstanding **and** at least one review has been submitted. Requiring at least one review (not just an empty pending list) is what catches bot reviewers like Copilot and Gemini that self-trigger via webhook without ever appearing in the formal requested-reviewer list — if only the pending list were checked, the gate would race through while bots were still processing. If still pending → wait. If timed out → pause with `fabrik:awaiting-input`.
3. **Phase 3 (re-invocation):** When the gate clears with submitted reviews present, Fabrik re-invokes the stage agent via the comment-processing skill (`comment_skill`) with the unresolved inline review thread comments as input. Top-level PR review bodies are not included, so a review that only contains general feedback without inline thread comments does not provide re-invocation input. Each inline thread comment is enriched with its **file path** and, when available, **line number** and **raw diff-hunk context** (line number and hunk may be absent for file-level or outdated comments) so the agent understands where in the code the reviewer's feedback applies. The agent addresses the feedback, commits, and signals `FABRIK_STAGE_COMPLETE`. This re-applies `fabrik:awaiting-review`, and the cycle returns to Phase 2 until the gate clears again under the same dual condition — no requested reviewers outstanding **and** at least one review submitted — or, if that does not happen before the wait limit, Fabrik falls back to `fabrik:awaiting-input`. **As of v0.0.39, re-invocation is unconditional** — it fires for any issue with `wait_for_reviews: true` and submitted inline feedback, regardless of whether auto-advance is active.

This means there is always at least one extra poll cycle delay after stage completion — typically 30 seconds.

#### No Inline-Thread Feedback Skip

If a submitted-review batch leaves no unresolved inline PR review thread comments to process, Fabrik skips re-invocation entirely and advances the issue normally. Top-level review bodies are ignored for this decision, so a review body containing text like "APPROVED" does not trigger re-invocation unless there is unresolved line-level thread feedback to address.

This prevents spurious re-invocation cycles when reviews contain no actionable inline thread feedback for the agent to address.

#### PR Summary Comment

After each re-invocation cycle, Fabrik posts a summary comment on the **linked PR** (not the issue) with the following format:

```
🏭 **Fabrik — stage: {StageName} (review feedback addressed)**
*branch: {branch} | commit: {sha} | main: {mainSHA} | {timestamp}*

{Claude output}

---
**Threads addressed:**
- `path/to/file.go:42` — resolved
- `other/file.go` — resolved

Resolved N review thread(s) across M comment(s).
```

The "Threads addressed" footer lists each inline thread by file path and line number (when available); file-level or outdated threads show only the file path. This comment is posted only when Claude produces output and a linked PR is found. It carries the `🏭 **Fabrik` header so it is not reprocessed as user input on subsequent polls.

#### Cycle Limit

To prevent infinite loops (e.g., a bot that always posts new reviews after every push), Fabrik caps the number of re-invocation cycles per issue per stage per engine session. When the limit is reached with reviewers still pending, Fabrik pauses the issue with `fabrik:awaiting-input` and posts a comment explaining the situation.

```bash
FABRIK_MAX_REVIEW_CYCLES=3  # Limit to 3 re-invocation cycles per issue per stage (default: 5)
# or equivalently:
fabrik --max-review-cycles=3
```

The cycle count resets on engine restart.

#### Timeout Configuration

`FABRIK_REVIEW_WAIT_TIMEOUT` sets the per-cycle timeout in minutes for the review gate. If the gate is still waiting within the timeout — whether for requested reviewers to submit or simply for at least one review to exist — Fabrik **pauses** the issue with `fabrik:awaiting-input` (rather than auto-advancing) so a human can investigate. This timeout also acts as the fallback when no reviews ever arrive — for example, if all bot reviewers fail to post — since the dual-condition gate requires at least one submitted review and would otherwise wait indefinitely.

```bash
FABRIK_REVIEW_WAIT_TIMEOUT=30  # Wait up to 30 minutes per cycle for reviewers (default: 15)
# or equivalently:
fabrik --review-wait-timeout=30
```

#### Restart Persistence

The timeout is based on the timestamp of when the `fabrik:awaiting-review` label was added to the issue, which is stored in GitHub's event history. If Fabrik restarts while waiting, it recalculates the remaining wait time from the label timestamp rather than resetting the clock. The cycle count is in-memory and resets on restart.

---

### CI Gate and CI-Fix Workflow

When a stage has `wait_for_ci: true` set, Fabrik gates auto-advance (and auto-merge for Validate+yolo) on CI checks passing on the PR head. If checks fail, Fabrik re-invokes the stage agent with a structured failure report so it can fix the regression and push again. **Re-invocation is unconditional** — it fires for any issue with `wait_for_ci: true` and a failing CI check, regardless of whether auto-advance is active.

For the authoritative spec on label lifecycle and gate transitions, see [State Machine](state-machine.md).

The Validate stage ships with `wait_for_ci: true` enabled by default.

#### Enabling the Gate

Add `wait_for_ci: true` to the relevant stage YAML:

```yaml
name: Validate
order: 5
wait_for_ci: true
...
```

CI checks are only evaluated on the **PR head SHA** — Fabrik makes two REST calls per poll: one to get the head SHA via the linked PR, and one to fetch the check runs. If no check runs exist (the repo has no CI), the gate clears immediately.

#### Label Lifecycle

When a `wait_for_ci: true` stage emits `FABRIK_STAGE_COMPLETE`, Fabrik immediately adds the `fabrik:awaiting-ci` label to the issue — it means "CI gate active" and covers both pending and failed CI states. `stage:X:complete` is withheld until `checkCIGate` confirms all checks pass (conjunctive gate, ADR 032). This label:

- Makes the CI-blocked state visible on the project board; present whether checks are still running or have failed
- Is cleared automatically when all CI checks pass; or when the CI wait timeout elapses (removed before pausing with `fabrik:awaiting-input`)
- Triggers the `itemMayNeedWork` cache bypass — CI results change independently of the issue's GitHub `updatedAt`, so items with `fabrik:awaiting-ci` bypass the staleness cache and are re-evaluated on every poll
- **R5 post-push guard**: within the current engine run, if the PR has previously had check runs, empty check run results after a push are treated as a post-push registration delay (GitHub takes a few seconds to register new runs) rather than "no CI configured" — the gate does not prematurely clear

#### The CI-Fix Report

When CI fails and Fabrik re-invokes the stage agent, it passes a synthetic CI failure comment with the following structure:

```
🏭 **Fabrik — CI Fix Required**

CI checks failed on the PR head. Fix **NEW REGRESSION** failures before proceeding.

### Failed checks

| Check | Status | Classification |
|-------|--------|----------------|
| Go tests | failure | **NEW REGRESSION** |
| Go vet | failure | pre-existing (also failing on base branch) |

### Base branch CI status (for comparison)

| Check | Status |
|-------|--------|
| Go tests | success |
| Go vet | failure |
```

- **NEW REGRESSION** — the check passes on the base branch but fails on this PR. The agent should fix these.
- **pre-existing** — the check also fails on the base branch. The agent should not attempt to fix these.

The agent should fix NEW REGRESSION failures, commit, push, and **not** emit `FABRIK_STAGE_COMPLETE`. Fabrik re-evaluates CI on the next poll and advances the issue once all checks pass.

#### Two-Prong CI Gate

The CI gate operates on two different paths depending on whether the item is being auto-merged or just being polled:

**Merge guard (Validate+yolo auto-merge path):**
- Checks CI before attempting the merge in `attemptMergeOnValidate()`
- **`mergeable_state` shortcut (v0.0.52):** `attemptMergeOnValidate` queries GitHub's `mergeable_state` first; if `clean` or `unstable`, the gate clears immediately and proceeds to merge — per-check classification is skipped entirely
- While CI is pending: returns an error (no label applied — avoids churn)
- On CI failure: adds `fabrik:awaiting-ci`, returns error (skips merge)
- On timeout (pending too long): pauses with `fabrik:paused` + `fabrik:awaiting-input`

**Catch-up loop (all other paths):**
- Evaluates CI on every poll for items with `fabrik:awaiting-ci` on stages with `wait_for_ci: true`
- **`mergeable_state` shortcut (v0.0.52):** `checkCIGate` queries GitHub's `mergeable_state` first; if `clean` or `unstable`, `addCompleteLabelAndRemoveCI` runs immediately — stale `fabrik:awaiting-ci` is cleared, `stage:<X>:complete` is added, and per-check classification is skipped entirely
- On pending: `fabrik:awaiting-ci` is already present (applied at FABRIK_STAGE_COMPLETE); no additional label applied, re-evaluates next poll
- On failure: `fabrik:awaiting-ci` applied idempotently; dispatches CI-fix re-invocation
- On timeout: pauses with `fabrik:paused` + `fabrik:awaiting-input`; timeout measured from when `fabrik:awaiting-ci` was first applied (durable across restarts)

> **Rationale for the `mergeable_state` shortcut:** GitHub's `mergeable_state` reflects branch protection rules — it already aggregates required check status, reviewer approvals, and protection constraints. Non-required check_run failures (e.g., cleanup workflow jobs, notification steps) do not block merges per branch protection, so Fabrik's gate must not block on them either. Consulting `mergeable_state` first avoids over-aggressive blocking caused by raw per-check classification that cannot distinguish required from non-required checks.

#### Merge-Conflict Gate

In addition to checking CI status, the catch-up loop also checks the `mergeable` field of the linked PR. When GitHub reports `mergeable: false` (confirmed — not null or pending) while the item is in the CI-await window, Fabrik applies the `fabrik:rebase-needed` label and dispatches a rebase re-invocation. Claude is instructed to `git fetch`, rebase the branch onto the base branch, resolve any conflicts, and force-push. The label clears automatically on the next poll when GitHub flips `mergeable` back to `true`.

The rebase cycle cap is controlled by `--max-rebase-cycles` / `FABRIK_MAX_REBASE_CYCLES` (default 3). When the cap is exceeded, Fabrik posts a comment, keeps `fabrik:rebase-needed` applied so the reason for the pause remains visible, and pauses the issue with `fabrik:paused` + `fabrik:awaiting-input`. The pause comment instructs the operator to resolve the conflict manually, then remove both `fabrik:paused` and `fabrik:rebase-needed` to resume. The default of 3 is intentionally lower than the CI-fix and review defaults (both 5): a rebase either succeeds in one automated pass or it has a semantic conflict that requires human judgment. More than a handful of automated attempts without success almost always means a human needs to look at it.

#### CI-Fix Skill

By default, CI-fix re-invocations use the stage's `comment_skill`. To use a dedicated skill for CI fixes, set `ci_fix_skill`:

```yaml
name: Validate
order: 5
wait_for_ci: true
ci_fix_skill: fabrik-validate-ci-fix   # Custom skill for CI-fix re-invocations
comment_skill: fabrik-validate-comment  # Used for regular comment processing
...
```

When `ci_fix_skill` is not set, `comment_skill` is used. When neither is set, the stage's primary skill is used.

#### Cycle Limit

To prevent infinite loops (e.g., a non-deterministic flaky test that keeps failing), Fabrik caps the number of CI-fix re-invocation cycles per issue per stage per engine session:

```bash
FABRIK_MAX_CI_FIX_CYCLES=3  # Limit to 3 CI-fix cycles per issue per stage (default: 5)
# or equivalently:
fabrik --max-ci-fix-cycles=3
```

The cycle count resets on engine restart, and is also reset by `clearFailedStage()` when the user manually unpauses a failed issue.

#### Timeout Configuration

`FABRIK_CI_WAIT_TIMEOUT` sets the timeout in minutes for the CI gate (catch-up loop path). If `fabrik:awaiting-ci` has been present for longer than this duration, Fabrik pauses the issue with `fabrik:awaiting-input` rather than continuing to re-invoke.

```bash
FABRIK_CI_WAIT_TIMEOUT=60  # Wait up to 60 minutes for CI to pass (default: 30)
# or equivalently:
fabrik --ci-wait-timeout=60
```

The CI timeout is measured from when `fabrik:awaiting-ci` was first applied. Because the label is applied immediately at FABRIK_STAGE_COMPLETE, the timeout covers both pending and failed CI states — it starts from when the stage finishes, not from when CI first fails.

#### Restart Persistence

The CI timeout is measured from the `fabrik:awaiting-ci` label's creation timestamp (fetched via the GitHub API), making it durable across engine restarts. The cycle count is in-memory and resets on restart.

---

## 4. Stage Reference

### Default Pipeline

| Stage | Order | Purpose |
|-------|-------|---------|
| **Backlog** | -- | Parking lot. No stage config needed. |
| **Specify** | 0 | Refine rough issues into clear, unambiguous specs. |
| **Research** | 1 | Explore codebase, surface technical findings and questions. |
| **Plan** | 2 | Design implementation approach with task checklist. |
| **Implement** | 3 | Write code and tests, commit frequently, push to branch. |
| **Review** | 4 | Rebase, review, fix issues, push. Posts output on PR. |
| **Validate** | 5 | Run tests, verify requirements, confirm PR is ready. |
| **Done** | 99 | Terminal state. Cleanup stage removes worktree. Item remains on the board. |

#### Done Stage and Archiving

When an issue reaches Done, Fabrik:
1. **Removes the worktree** — frees disk space from the issue's working copy
2. **Adds `stage:Done:complete`** — marks cleanup as finished

**Note:** Auto-archive is currently disabled. It was removing items from the board before users could see them, and is being reworked to track actual Done stage completion time. Items will remain in the Done column until auto-archive is re-enabled in a future release. When re-enabled, archived items will not be deleted — they will remain accessible via the project board's "Archive" view in GitHub.

**Closed-issue auto-advance:** In auto-advance mode (`--yolo`, `fabrik:yolo`, or `fabrik:cruise`), if an issue is closed externally (for example, a PR merge closes it) while it is still in an intermediate stage column with a `stage:<name>:complete` label already applied, Fabrik automatically advances it to Done on the next poll. No manual board move is required — Fabrik detects the closed state and runs the normal catch-up path. This typically happens when a PR merge closes an issue sitting in the Validate column. Without auto-advance, closed issues with a stage-complete label are not moved automatically; you can drag the card to Done manually.

### Customizing Stages

```bash
# Initialize with defaults
./fabrik init

# Edit stage configs and skills
vim .fabrik/stages/research.yaml
vim .fabrik/plugin/skills/fabrik-research/SKILL.md

# Or point to a custom stages directory
./fabrik --stages ./my-custom-stages ...
```

You can add, remove, or reorder stages. Stages must have `name`, `order`, and either
`skill` or `prompt`. The `name` must match a board column and `order` values define
the sequence.

---

## 5. Plugin & Skills

### How Skills Work

Each stage references a **skill** -- a markdown file that contains detailed methodology
for how Claude should approach the stage. Skills are packaged as a Claude Code plugin
in `.fabrik/plugin/` and loaded via `--plugin-dir` on each Claude invocation.

The default skills are:

| Skill | Purpose |
|-------|---------|
| `fabrik-specify` | Requirements clarification, consistency checks, prior art research; preserves the Problem section verbatim (never compressed) and places it first in the spec |
| `fabrik-research` | Codebase exploration, technical analysis, constraint discovery; includes a Documentation Impact section for user-facing features |
| `fabrik-plan` | Implementation design, task checklist, decision documentation; includes explicit doc update tasks in the checklist for user-facing features |
| `fabrik-implement` | Code writing, testing, committing, pushing; updates `USER_GUIDE.md` and/or `README.md` in the same PR for user-facing features — never defers documentation to a follow-up issue |
| `fabrik-review` | Code review, fix issues, rebase, prepare PR |
| `fabrik-validate` | Final verification, test suite, requirements check |

### Customizing Skills

Skills live in `.fabrik/plugin/skills/<skill-name>/SKILL.md`. Edit them to change
how Claude approaches each stage. Changes take effect on the next Claude invocation.

The stage YAML references the skill by name:
```yaml
skill: fabrik-research    # loads .fabrik/plugin/skills/fabrik-research/SKILL.md
```

### Skill vs Prompt

- **`skill:`** references a plugin skill file (recommended). The skill contains rich
  methodology, quality checklists, and scope boundaries.
- **`prompt:`** is an inline prompt string in the YAML (legacy, still supported).
  Useful for quick customization but harder to maintain for complex stages.

When `skill` is set, Fabrik sends a minimal directive:
```
You are operating as the Fabrik Research agent for issue #42.
Follow the instructions in the fabrik-research skill exactly.
```

The skill is auto-loaded by Claude Code via the plugin system.

### Built-in Skill: `/cut-release`

The `/cut-release` skill automates the Fabrik release process — pre-flight checks, release notes, commit/tag/push, and documentation issue filing — in a single invocation.

#### Invocation

```
/cut-release              # Auto-suggests next patch bump; confirms version with you
/cut-release v0.0.12      # Uses the explicit version; no confirmation prompt
```

#### Pre-flight checks

Before doing anything, `/cut-release`:

1. Verifies the working tree is clean (`git status --porcelain`). Stops if dirty.
2. Pulls latest main (`git pull origin main`).
3. Runs `go build ./...` and `go test -race ./...`. Stops if either fails.

The build/test gate is mandatory — broken releases are worse than delayed ones.

#### Version determination

- Reads the latest tag via `git describe --tags --abbrev=0`.
- If you provided an explicit version, validates it: must be valid semver with a `v` prefix and greater than the current tag.
- If no version is provided, auto-suggests the next patch bump (e.g. `v0.0.11` → `v0.0.12`) and confirms with you before proceeding.

#### Change gathering

Runs `git log <last-tag>..HEAD --oneline` and groups commits into four categories:

| Category | What goes here |
|----------|---------------|
| **Features** | New user-facing capabilities |
| **Fixes** | Bug fixes |
| **Improvements** | Enhancements to existing features |
| **Internal** | Refactoring, test improvements, CI — summarized briefly, not enumerated |

Merge commits and `Co-Authored-By` lines are ignored.

#### Release notes format

Writes `release-notes.md` in the repo root:

```markdown
# Fabrik <version>

## Features
- Description of feature (#issue)

## Fixes
- Description of fix (#issue)

## Improvements
- Description of improvement (#issue)

## Internal
- Summary of internal changes

## Upgrading

\```bash
# Auto-upgrade from a running Fabrik instance
# Fabrik checks for new releases each poll cycle and upgrades automatically with --auto-upgrade

# Or download directly
gh release download --repo shadoworg/fabrik --pattern '*<os>_<arch>*' -O - | tar xz
\```
```

Empty category sections are omitted. The `release-notes.md` file **must be committed before the tag push** because the GitHub Actions release workflow (`release.yml`) uses it to populate the GitHub Release body.

#### Commit, tag, and push

Once the release notes are written, `/cut-release` proceeds without a confirmation prompt (pre-flight already passed):

1. `git add release-notes.md`
2. Commits with message: `Release notes for <version>`
3. Creates the tag: `git tag <version>`
4. Pushes branch and tag together: `git push origin main <version>`
5. Reports the triggered GitHub Actions run URL.

**Never force-push tags.** If a tag already exists at that version, `/cut-release` stops and tells you.

#### Documentation issue

After a successful push, `/cut-release` automatically files a GitHub issue:

- **Title**: `Update docs for v<version>`
- **Labels**: `documentation`, `fabrik:yolo`
- **Board position**: Added to the Fabrik PM project board and moved to the **Specify** column

The `fabrik:yolo` label means Fabrik will pick it up automatically and run it through the pipeline. You don't need to do anything else for documentation updates after cutting a release.

### Built-in Skill: `/audit-documentation`

The `/audit-documentation` skill compares recently shipped features against `USER_GUIDE.md`, `README.md`, and `docs/index.md`, then files GitHub issues for gaps and closes existing documentation issues whose gaps are now covered.

#### Invocation

```
/audit-documentation                  # Scan issues closed in the last 30 days
/audit-documentation --since v0.0.20  # Scan issues referenced in commits since a tag
```

#### What it does

1. **Discovers source issues** — finds recently closed issues using the selected mode (see below).
2. **Filters** — excludes issues labeled `documentation` and issues that are infrastructure/tooling-only with no user-facing behavior change.
3. **Reads the docs** — reads `USER_GUIDE.md`, `README.md`, and `docs/index.md` in full.
4. **Analyzes gaps** — for each filtered issue, checks whether the feature is adequately described in the docs. Groups related issues into single gap entries. Errs on the side of filing — false positives are refined by the Specify stage; false negatives cause documentation drift.
5. **Clears the deck** — for each existing open `documentation` issue whose gap is now clearly and completely covered, closes it with a comment.
6. **Files new gap issues** — for each newly identified gap, creates a GitHub issue labeled `documentation` and `fabrik:yolo`, and places it in the **Specify** column on the PM board.

#### Modes

**Date-based mode (default):** Fetches closed issues via `gh issue list --state closed --search "closed:>$CUTOFF_DATE" --limit 200`.

**Tag-based mode (`--since <tag>`):** Two passes — extracts `#NNN` references from commit messages since the tag, then scans merged PRs for `Closes #NNN` references.

#### Output

After running, `/audit-documentation` prints a structured summary:

```
## Audit Documentation — Summary

**Period**: <date range or tag range scanned>

**Issues scanned**: <N>
**Issues excluded**: <N>
  - <issue#>: <title> — <reason>

**Documentation gaps found**: <N>

**New gap issues filed** (<N>):
  - #<number>: <title>

**Existing documentation issues closed** (<N>):
  - #<number>: <title>

**Existing documentation issues left open** (<N>):
  - #<number>: <title> — <reason left open>
```

If no gaps are found, it reports: "No documentation gaps found — docs appear current."

#### Known limitations

- **Rate limits**: May apply for repos with hundreds of closed issues.
- **Tag-based mode coverage**: Misses issues whose PRs used `Fixes` or bare URLs instead of `Closes #NNN` in the PR body.

### Plugin Development

For developing the plugin itself, use `--plugin-dir` to point at your working copy:
```bash
./fabrik --plugin-dir ./plugin/fabrik-plugin ...
```

---

## 6. Labels Reference

> For the authoritative label lifecycle spec — when each label is added, cleared, and what transitions it guards — see [State Machine](state-machine.md).

### Fabrik-Managed Labels

> Fabrik auto-seeds all managed labels on the configured repository at startup — creating them with their descriptions and colors if they do not already exist, so the GitHub UI shows meaningful hover text.

| Label | Purpose |
|-------|---------|
| `fabrik:locked:<user>` | Issue being processed by this user's instance |
| `fabrik:editing` | Issue body being updated (comment processing) |
| `fabrik:paused` | Processing paused (max retries exceeded or manual) |
| `fabrik:awaiting-input` | Stage paused waiting for user input; auto-clears on a new comment from the configured user |
| `fabrik:awaiting-review` | Set when a `wait_for_reviews: true` stage completes with outstanding reviewer requests; cleared when no requested reviewers are outstanding **and** at least one review has been submitted (then re-invocation fires unconditionally), or when the `FABRIK_REVIEW_WAIT_TIMEOUT` elapses (then issue is paused with `fabrik:awaiting-input`) |
| `fabrik:awaiting-ci` | Applied immediately when a `wait_for_ci: true` stage emits `FABRIK_STAGE_COMPLETE`; means "CI gate active" and covers both pending and failed CI states; `stage:X:complete` is applied only when CI passes — not when this label is cleared by timeout (conjunctive gate, ADR 032). Triggers `itemMayNeedWork` cache bypass so CI results are re-evaluated on every poll. Cleared when all checks pass or the CI wait timeout elapses (then issue is paused with `fabrik:awaiting-input`). See [§3 CI Gate](USER_GUIDE.md#ci-gate-and-ci-fix-workflow). |
| `fabrik:rebase-needed` | Set when GitHub reports the linked PR as `mergeable: false` on a `wait_for_ci: true` stage — typically because another PR merged into the base branch during the CI-await window. The engine dispatches a rebase re-invocation instructing Claude to `git fetch && git rebase origin/<base>`, resolve conflicts conservatively (watching for semantic collisions like duplicated ADR numbers), and force-push. The label clears when GitHub flips `mergeable` back to `true`. Triggers `itemMayNeedWork` cache bypass because base-branch advances don't bump the item's `updatedAt`. |
| `fabrik:blocked` | Issue is waiting for one or more blocking issues to close; added and removed automatically by the engine (Fabrik creates this label on first use — no pre-creation needed) |
| `stage:<name>:in_progress` | Stage actively running |
| `stage:<name>:complete` | Stage completed successfully |
| `stage:<name>:failed` | Stage hit max retries |

### User-Set Labels

| Label | Effect |
|-------|--------|
| `model:opus` | Override Claude model to Opus for this issue |
| `model:sonnet` | Override Claude model to Sonnet for this issue |
| `effort:low` | Override thinking effort to low for this issue |
| `effort:medium` | Override thinking effort to medium for this issue |
| `effort:high` | Override thinking effort to high for this issue |
| `effort:max` | Override thinking effort to max for this issue |
| `fabrik:paused` | Manually pause processing (add to pause, remove to resume) |
| `fabrik:yolo` | Force auto-advance for this issue even when `auto_advance: false`; also triggers auto-merge of the linked PR when Validate completes. If the linked PR was already manually merged when auto-merge runs, Fabrik treats it as a success and advances the issue to Done normally. |
| `fabrik:cruise` | Auto-advances through all stages like `fabrik:yolo` but stops at Validate — no auto-merge, no move to Done. If both `fabrik:cruise` and `fabrik:yolo` are present, `fabrik:yolo` takes precedence. |
| `fabrik:unrestricted` | Pass `--dangerously-skip-permissions` instead of `--permission-mode dontAsk` for this issue; bypasses the default tool allowlist entirely. Use only when a stage needs tools outside the default set or when the default posture prevents required work. **Caution:** removes all tool restrictions. |
| `fabrik:extend-turns` | Pre-grant 2× `max_turns` for the first invocation of the next stage. Auto-extends to 3× when actual progress is detected during that invocation (Implement: new git commit (HEAD SHA changed) OR (baseline was clean AND working tree is now dirty — uncommitted file edits by Claude); Review: new git commit or resolved reviewer thread count; Validate: new comment; other stages: no progress signal). Auto-removed by the engine on successful stage completion. No-op when `max_turns` is 0 (unlimited). The displayed turn counter denominator always reflects the effective budget. |
| `base:<branch>` | Override the base branch for this issue (e.g. `base:liminis`). Fabrik will fork from, rebase onto, and target PRs at `<branch>` instead of the repository default. Apply before Research; adding mid-pipeline is unsupported and may produce unexpected results. Branch names containing `/` are supported (e.g. `base:release/1.x`). If the named branch does not exist on the remote, Fabrik falls back to the default branch and posts a comment on the issue. |

Model label precedence: `model:<name>` label > stage YAML `model` field > default.

Effort label precedence: `max > high > medium > low`. If multiple `effort:` labels are present, the highest-ranked value wins and a warning is logged.

### Advanced: `base:<branch>` timing

The `base:<branch>` label is read on every stage invocation, so a change takes effect from the next stage onward. However, Fabrik cannot retroactively re-create past work — the issue branch is forked once, commits accumulate on it, and the PR is opened once. The later you apply or change the label, the more of that history is already locked to the original base.

| When applied | Effect |
|---|---|
| **Before any stage runs** | Clean. The issue branch is forked from the specified base, all commits stack on the right base, the PR targets the right base. This is the intended path. |
| **After Specify, before Research** | Fairly clean. The worktree exists but no commits yet. The next stage's `updateWorktreeFromMain` rebases onto the new base — usually no conflict. |
| **After Research, before Implement** | Similar — Research is read-only, so no commits. Rebase onto the new base works. |
| **After Implement (commits exist, no PR yet)** | Messy. The issue branch has commits forked from the old base. Rebase onto the new base may produce conflicts that Claude must resolve. The PR, when created, will target the new base correctly. |
| **After PR creation** | Fabrik automatically updates the PR's base branch when the `base:<branch>` label changes mid-pipeline. No manual intervention required. |

**Natural language in the issue body is NOT parsed.** Writing "use the `develop` branch" in the Problem section has no effect on Fabrik's base-branch selection. The `base:<branch>` label is the only structured signal.

If the named branch does not exist on the remote, Fabrik falls back to the repository's default branch and posts a comment on the issue explaining the fallback. Fabrik keeps `refs/remotes/origin/HEAD` in sync with the remote's current default branch on every fetch, so a default-branch change on the remote (after Fabrik initially cloned the repo) is detected automatically on the next stage invocation.

---

## 7. Permissions

Fabrik passes `--permission-mode dontAsk` to every Claude Code invocation. In this mode Claude does not prompt for tool permissions — tools not in the allowed set are silently denied rather than triggering an interactive prompt. This ensures Fabrik works correctly in headless mode and shared environments regardless of the user's `~/.claude/settings.json`. Fabrik does not require users to pre-configure permissions in their Claude Code settings.

**Default allowed-tool set** (used when a stage does not specify `allowed_tools`):

| Category | Tools |
|----------|-------|
| File operations | `Read`, `Edit`, `Write`, `Glob`, `Grep` |
| Task management | `TodoWrite`, `Skill`, `Task` |
| Git & GitHub | `Bash(git:*)`, `Bash(gh:*)` |
| Build systems | `Bash(go:*)`, `Bash(npm:*)`, `Bash(npx:*)`, `Bash(yarn:*)`, `Bash(pnpm:*)`, `Bash(make:*)`, `Bash(cargo:*)` |
| Python | `Bash(python:*)`, `Bash(pip:*)`, `Bash(uv:*)`, `Bash(pytest:*)` |
| Shell utilities | `Bash(ls:*)`, `Bash(cat:*)`, `Bash(rm:*)`, `Bash(cp:*)`, `Bash(mv:*)`, `Bash(mkdir:*)`, `Bash(find:*)` |

**`allowed_tools` replaces the defaults — it is not additive.** When a stage sets `allowed_tools`, only those tools are permitted; the default list above is not merged in. This is intentional: Research, Plan, and Specify stages set `allowed_tools` to a read-only subset to prevent Claude from writing files during those stages.

**`fabrik:unrestricted` bypasses everything** — passes `--dangerously-skip-permissions` instead, granting Claude full tool access. Use this label only when a stage requires tools outside the default set (e.g. `deno`, `bun`, or other non-standard toolchains).

**Note on personal `~/.claude/settings.json`:** Under `dontAsk` mode, tools the user has explicitly allowed in their own `permissions.allow` remain allowed in addition to Fabrik's list — this does not break the isolation goal, it just means personal extras carry through. If your organization deploys managed settings (MDM/enterprise level), a `deny` rule in managed settings cannot be overridden by `--allowedTools` — that is outside Fabrik's control.

---

## 8. TUI Dashboard

The interactive terminal dashboard is enabled by default when running in a real terminal. To disable it, use `--notui`:

```bash
./fabrik --notui --owner your-org --repo your-repo --project 1 --user you
```

### Layout

The TUI shows a compact header with poll status, an In Progress pane showing active
Claude sessions, a scrollable History pane with completed jobs, and a status bar
(footer) displaying REST and GraphQL rate limit stats.

### Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Tab` | Switch focus between In Progress and History panes |
| `Up/Down` or `j/k` | Navigate items within the focused pane |
| `l` | Open `fabrik watch` for selected issue (live Claude output, stage tabs, PR/CI status) |
| `enter` | Toggle inline detail panel for the selected item |
| `r` | Resume Claude session for selected history item (history pane only, item must not be active) |
| `Escape` | Close open dialogs; with no dialog open, triggers quit confirmation |
| `n` / `N` | Cancel quit or clear-all confirmation dialogs |
| `c` | Delete selected history entry |
| `C` | Clear all history (with confirmation) |
| `w` | Wake: reset idle backoff and poll immediately |
| `?` | Toggle help panel (keybindings and labels reference) |
| `q` | Quit |

#### Help Panel (`?`)

Pressing `?` opens an overlay that displays all keybindings and a labels reference. The labels reference covers every label Fabrik uses:

| Label | Meaning |
|-------|---------|
| `fabrik:yolo` | Auto-advance through all stages and auto-merge PR on Validate completion |
| `fabrik:cruise` | Auto-advance through all stages without auto-merging the PR |
| `fabrik:paused` | Issue processing is paused |
| `fabrik:awaiting-input` | Stage is blocked waiting for user input |
| `fabrik:awaiting-review` | Stage completed; waiting for outstanding PR reviewer requests; clears when no requested reviewers are outstanding and at least one review has been submitted, then re-invokes the stage agent to address feedback |
| `fabrik:locked:<user>` | Issue is locked for editing by the specified user |
| `stage:<name>:in_progress` | Named stage is currently running |
| `stage:<name>:complete` | Named stage completed successfully |
| `stage:<name>:failed` | Named stage failed (hit max retries) |
| `model:<name>` | Override the model for this issue (e.g. `model:opus`) |
| `effort:<level>` | Override thinking effort for this issue (`low`, `medium`, `high`, `max`) |
| `fabrik:unrestricted` | Bypass default permission posture; passes `--dangerously-skip-permissions` instead |
| `fabrik:extend-turns` | Pre-grant 2× max turns budget; auto-extends to 3× when progress is detected (see the Labels Reference / state-machine semantics for the full definition); auto-removed on success; the turn counter denominator reflects the effective budget |
| `base:<branch>` | Override base branch for this issue (e.g. `base:liminis`); Fabrik forks from, rebases onto, and targets PRs at this branch |

Dismiss the panel by pressing `?` again or `Esc`.

In terminals that support OSC 8 hyperlinks (Ghostty, iTerm2, WezTerm, Kitty), issue numbers (`#NNN`) in the In Progress and History panes are hyperlinks. **Cmd+click** (macOS) or **Ctrl+click** (Linux) on a `#NNN` to open the corresponding GitHub issue in your browser; **Cmd+click** / **Ctrl+click** the board title in the footer to open the project board. Use keyboard navigation for selection and scrolling.

### What's Displayed

**In Progress**: Issue number, stage name, elapsed time, a live turn counter (`[N/M turns]` or compact `[N/M]` when terminal width is tight), issue title, and latest status message. The turn counter updates in real time as Claude advances through turns; the denominator reflects the effective per-invocation budget. With `fabrik:extend-turns`, the first invocation may use a denominator of `2× stage.MaxTurns`, while subsequent extension invocations revert to `stage.MaxTurns` each. Review-reinvoke jobs appear here alongside regular stage jobs — they emit `JobStarted`/`JobCompleted` events and display with the issue number, stage name, and elapsed time, just like a normal stage invocation.

Pressing `Enter` on an active item opens the inline detail panel, which shows Issue, Stage, Elapsed, and a live `Turns: N/M` counter for in-progress stages.

**History**: Issue number, stage name, success/fail icon, duration, timestamp, turns used,
cost, and issue title. Status icons:

| Icon | Meaning |
|------|---------|
| `✓` | Stage completed successfully |
| `✗` | Stage failed (hit max retries) |
| `?` | Stage blocked / awaiting user input |
| `↻` | Stage retrying |
| `💬` | Issue has unprocessed comments |

### History Persistence

Job history is saved to `.fabrik/history.json` (project-local, alongside `.fabrik/config.yaml`) and restored on restart.

---

## 9. Observability

### `fabrik watch` — Per-Issue TUI

The recommended way to monitor a running issue is `fabrik watch`:

```bash
fabrik watch 42
# or with explicit credentials:
fabrik watch 42 --owner myorg --repo myrepo --token ghp_...
```

This opens a real-time terminal UI that shows:
- Issue title, labels, and current stage
- **Live Claude output** — streams from the `.log` file as Claude writes it (no polling)
- **Live turn counter** — `turn N/M` shown inline in the PR/CI row while a stage is active; updates in real time as Claude advances through turns (`turn N` when the stage has no turn limit)
- **Stage history** — completed stages with duration and cost
- **PR status** — linked PR number, open/draft/merged state
- **CI check results** — compact pass/fail/pending summary
- **Comment count** — updates on each GitHub poll

#### Keyboard shortcuts

| Key | Action |
|-----|--------|
| `↑` / `↓` or `k` / `j` | Scroll live output |
| `g` / `G` | Jump to top / bottom of output |
| `left` / `h` | Switch to previous stage tab |
| `right` / `l` | Switch to next stage tab |
| `i` | Open interactive Claude session in the issue's worktree (resumes current stage) |
| `esc` | Quit (same as `q`) |
| `q` / `ctrl+c` | Quit |

The session ID for the active Claude session is shown in the header when available.

#### Credentials

`fabrik watch` reads owner, repo, and poll interval from `.fabrik/config.yaml` (see §2). Without a GitHub token, it shows local log output and stage history but skips GitHub API calls (PR/CI/comments).

### Log Files

Session logs are saved to `.fabrik/logs/<owner>-<repo>/issue-<N>/` (relative to the project working directory) as NDJSON (one JSON event per line). Each stage produces a `.log` file written continuously during execution — `fabrik watch` follows this file live. To view a log manually:

```bash
ls -lt .fabrik/logs/<owner>-<repo>/issue-42/

# Render a completed log as human-readable text
cat .fabrik/logs/<owner>-<repo>/issue-42/<stage>-output-<timestamp>.json | fabrik stream-filter | less -R
```

Logs are namespaced by repository: `.fabrik/logs/<owner>-<repo>/issue-<N>/`.

`fabrik stream-filter` reads NDJSON or JSON array Claude output from stdin and renders thinking blocks, text responses, and tool calls as human-readable text.

### Poll Log

Fabrik writes engine-level output to `.fabrik/fabrik.log` in the project working directory. This file is **truncated on each startup** and contains only the current run.

The poll log captures:
- Deep-fetch decisions (which issues were shallow-skipped vs. fully fetched)
- Dispatch events (which issues were dispatched to workers and why)
- Upgrade checks and idle-upgrade transitions
- GitHub API rate limit stats per poll cycle
- `[#N extend-turns]` verdict lines — emitted on every `detectProgress` call (pass or fail) without `--debug-output`; useful for diagnosing why a stage did or didn't receive a turn extension

The poll log is written in both TUI and non-TUI modes. It is most useful for post-mortem debugging of engine-level behavior — for example, diagnosing why an issue was not picked up during a poll cycle.

### Auto-migration from `~/.fabrik/`

On the first startup after upgrading from a pre-v0.0.32 release, Fabrik automatically migrates session files and stage logs from `~/.fabrik/sessions/` and `~/.fabrik/logs/` to `<cwd>/.fabrik/sessions/` and `<cwd>/.fabrik/logs/`. The migration is idempotent (files whose destination already exists are skipped) and handles cross-device filesystems. Empty source directories are removed after migration.

### Debug Output

Enable `--debug-output` to save Claude's raw output to `.fabrik/debug/` in the
working directory. Useful for diagnosing prompt issues or unexpected behavior.

### Subprocess Environment

Claude Code is invoked with a clean, isolated environment — environment variables
from the parent process do not leak into Claude subprocesses. If you previously
relied on ambient env vars being available inside Claude (e.g., API keys or tool
paths set in your shell), pass them explicitly via your stage YAML or a shell
wrapper script.

### Rate Limit Monitoring

Fabrik reports GitHub API rate limit stats in each poll cycle (visible in the TUI
status bar (footer) or plain-text log output):
- REST API: requests remaining / limit
- GraphQL API: points remaining / limit

The GraphQL query uses a two-phase fetch to minimize rate limit consumption:

1. **Shallow board scan** — fetches all items with lightweight fields (`updatedAt`,
   labels, status). Items whose `updatedAt` hasn't changed since the last poll are
   skipped entirely.
2. **Targeted detail fetch** — runs only for items that passed the shallow filter.
   `fabrik:blocked` items do not receive a forced deep-fetch on every poll. Instead,
   they are re-evaluated via the standard `processedSet` cooldown (every
   `10 × poll interval`, typically ~5 minutes at the default 30s base) — when the
   cooldown fires, Fabrik deep-fetches the blocked item to check whether its
   dependencies have closed. If GitHub bumps the blocked item's `updatedAt` when a
   dependency closes, unblocking may also be detected sooner — on the next poll cycle.

   Items with `fabrik:awaiting-input` or `fabrik:awaiting-review` do **not** force a
   deep-fetch. New user comments bump the issue's `updatedAt`; PR review submissions
   bump the linked PR's `updatedAt` — both are captured by the shallow scan, so the
   targeted detail fetch fires naturally when there is actually something new to see.

Typical cost is ~5–30 points per poll depending on active items, well within the
5,000 points/hour limit.

---

## 10. Webhook Mode

Webhook mode is an optional feature that dramatically reduces GraphQL usage and cuts event latency from up to 30 seconds to near-zero. When enabled, Fabrik receives GitHub events within seconds of them occurring instead of waiting for the next poll tick.

### How It Works

Fabrik spawns `gh webhook forward` as a background subprocess. This tool connects outbound to GitHub's SSE stream and forwards every matching event as an HTTP POST to a local listener on `127.0.0.1:<port>`. No public endpoint, no ngrok, no infrastructure changes. Each event payload is authenticated with HMAC-SHA256 before Fabrik acts on it.

Events are translated into immediate poll wakeups — the existing board-fetch and filter logic handles the rest. The poll loop continues running as a safety net (up to once every 60 minutes when the webhook stream is healthy) so missed or delayed events never strand work. Board-column changes that aren't delivered as events are caught by a lightweight status sweep that runs every 10 minutes by default (see `--status-poll`).

### Prerequisites

- **`cli/gh-webhook` extension** — `gh webhook forward` is **not** a built-in `gh` subcommand; it ships as a separate extension that must be installed once:
  ```bash
  gh extension install cli/gh-webhook
  ```
  Verify installation: `gh webhook --help` (should print usage, not `unknown command "webhook"`). No specific minimum version is pinned; any recent `gh` with extension support works.
- **`gh auth login`** with a token that has `admin:repo_hook` write scope (classic PAT) or equivalent repo admin access. Fine-grained tokens may lack this scope.
- **Repo admin access** on each repository on your board, or org-admin access for org-level subscription.

If any prerequisite is missing, Fabrik logs a clear warning and falls back to polling-only mode. Webhook mode failure is always non-fatal.

### Enabling Webhook Mode

Three equivalent ways to enable:

**CLI flag:**
```bash
fabrik --webhooks
```

**Environment variable:**
```bash
FABRIK_WEBHOOKS=true fabrik
```

**`.fabrik/config.yaml`:**
```yaml
webhooks: true
```

### Configuration Options

| Option | Flag | Env var | Config YAML | Default |
|--------|------|---------|-------------|---------|
| Enable webhooks | `--webhooks` | `FABRIK_WEBHOOKS` | `webhooks: true` | off |
| Listener port | `--webhook-port=<N>` | `FABRIK_WEBHOOK_PORT` | `webhook_port: <N>` | 0 (OS-assigned) |
| Event filter | `--webhook-events=<csv>` | `FABRIK_WEBHOOK_EVENTS` | `webhook_events: [...]` | all supported events |
| Board cache | `--board-cache=<mode>` | `FABRIK_BOARD_CACHE` | `board_cache: <mode>` | `in-memory` when `--webhooks`, else `none` |

By default, Fabrik subscribes to: `issue_comment`, `issues`, `pull_request`, `pull_request_review`, `pull_request_review_comment`, `check_run`, `check_suite`, and `projects_v2_item` (board column changes — see note below).

### Health States and TUI Indicator

The TUI footer shows a webhook health indicator when webhook mode is enabled:

| Indicator | State | Meaning |
|-----------|-------|---------|
| `● webhook (N)` (green) | Healthy | At least one event received in the last 10 minutes. Full-board idle cap: 60 min; status sweep: 10 min. |
| `○ webhook (N)` (blue) | Starting Up | Subprocess launched; waiting for first event (up to 30s grace). Full-board idle cap: 60 min; status sweep: 10 min. |
| `◌ webhook (N)` (yellow) | Unhealthy | No event received in 10+ minutes or startup grace expired. Idle cap: 5 min (same as polling-only). |

`N` is the total number of webhook events received since startup. You can also verify the stream by checking `fabrik.log` for `[webhook]` tag lines.

### Expected GraphQL Load Reduction

On an active board that would poll every 30 seconds without webhooks, enabling webhook mode reduces steady-state GraphQL polling to at most once every 60 minutes — roughly a 120× reduction in GraphQL points consumed while idle. During active periods, polls are triggered immediately by webhook events instead of waiting for the tick.

### In-Memory Board Cache (`--board-cache`)

When webhook mode is active, Fabrik maintains an in-memory board cache (`--board-cache=in-memory`, the default). Incoming webhook payloads are applied as typed state deltas to the cache before the poll loop wakes, so most reads are served from memory rather than GitHub's API.

**Cache modes:**

| Mode | When | Behavior |
|------|------|----------|
| `in-memory` | `--webhooks` enabled (default) | Cache populated at startup; deltas from webhooks; reconciles every 60 min; falls back to GitHub when stream is unhealthy |
| `none` | `--webhooks` disabled (default), or explicit override | All reads go directly to GitHub API (original behavior) |

**To disable the cache while keeping webhooks:**
```bash
fabrik --webhooks --board-cache=none
```

**Stream-health failover:** If the webhook stream goes unhealthy (no events for 10 minutes), the cache pauses and all reads fall back to the live GitHub API. When the stream recovers, the cache reconciles from GitHub before resuming. This ensures correctness is never compromised by a degraded webhook stream.

**Reconciliation:** Every 60 minutes (matching the idle-cap for healthy webhook streams), Fabrik fetches a fresh board snapshot from GitHub and updates the cache. This heals any events the webhook stream may have missed. Reconciliation also runs inline when the stream recovers from unhealthy.

> **Note:** `--board-cache=in-memory` requires `--webhooks`. Without a webhook stream, there is no delta source and the cache would only reconcile every 60 minutes — worse than the default polling behavior.

### `projects_v2_item` (Board Column Changes)

Fabrik attempts to subscribe to `projects_v2_item` events, which fire when an issue is moved between board columns. If `gh webhook forward` does not support this event type for your installation, Fabrik logs a warning and continues without it. In this case, board-column changes are caught by the safety-net poll within 60 minutes (or 5 minutes if the stream is unhealthy). All other event types continue to work normally.

### Security

- The HTTP listener binds exclusively to `127.0.0.1` and is never reachable from outside your machine.
- Every incoming payload is verified with HMAC-SHA256 before Fabrik acts on it. A malicious local process cannot spoof events without the secret.
- The webhook secret is generated from `crypto/rand` (32 bytes) at startup and is never written to disk or logged.
- If HMAC verification fails repeatedly (5 consecutive failures within 2 minutes), Fabrik automatically rotates the secret and restarts the subprocess. After 2 failed rotation cycles, webhook mode is disabled for the session with a clear log message.

### Multi-Repo Boards

On multi-repo boards, Fabrik uses a single `gh webhook forward` subprocess:

1. If your token has org-admin access, Fabrik uses `--org=<org>` to subscribe to all repos in the org with one subscription — no restarts needed when new repos appear.
2. Otherwise, Fabrik subscribes per-repo. When a new repo appears on the board, the subprocess is briefly restarted with the updated repo list (the safety-net poll covers any events missed during the restart).

> **Startup delay on multi-repo boards**: Webhook subscriptions are established after the first board poll completes (up to one poll interval, typically 30 seconds). During this window the TUI shows the blue "starting up" indicator and the safety-net poll is in effect. This is expected — the webhook manager waits until the repo list is known before subscribing.

### Troubleshooting Webhook Mode

**"prerequisite check failed"** — `gh` is missing or the `cli/gh-webhook` extension is not installed. Install `gh` at <https://cli.github.com>, then run `gh extension install cli/gh-webhook`.

**Stream shows yellow "unhealthy"** — The subprocess may have exited or GitHub isn't delivering events. Check `fabrik.log` for `[webhook]` error lines. Run `gh auth status` to verify your token has `admin:repo_hook` scope.

**HMAC failures logged** — Usually caused by a `gh auth refresh` that invalidated the webhook secret. Fabrik auto-rotates; if failures persist after 2 cycles, restart Fabrik.

**`projects_v2_item` warning** — Board column changes fall back to the 60-minute safety-net poll. Not a problem for most workflows.

---

## 11. Troubleshooting

### GitHub API Returns 401 or "Fine-Grained Token" Warning

Fabrik requires a **classic** personal access token (`ghp_...`). Fine-grained tokens (`github_pat_...`) are **not supported** because GitHub Projects v2 GraphQL — which Fabrik uses for status updates and board queries — is not available to fine-grained PATs.

**Symptoms:**
- Startup warning: `[warn] Fine-grained personal access tokens (github_pat_...) do not support GitHub Projects v2 GraphQL...`
- Error on first API call: `GitHub API returned 401: ... If you used a fine-grained access token...`

**Fix:**
1. Go to https://github.com/settings/tokens and select **"Tokens (classic)"**
2. Generate a new token with scopes: `repo`, `project`, `workflow`
3. Update `FABRIK_TOKEN` in your `.env` file with the new `ghp_...` token

### Startup Board Validation Failure

On every startup, Fabrik fetches the project board and compares stage names in `.fabrik/stages/*.yaml` to the column names on the board. If any non-cleanup stage is missing from the board, Fabrik exits with an error listing the mismatched names.

To fix: ensure stage YAML `name` fields match board column names exactly (case-sensitive). If you renamed a column on the board, update the matching stage YAML. Extra board columns (with no matching stage) produce a warning but don't block startup.

### Stage YAML Drift Warning

At startup, Fabrik prints a warning when a `.fabrik/stages/*.yaml` file is missing top-level keys that exist in the embedded defaults for the same stage name:

```
[startup] warning: .fabrik/stages/validate.yaml is missing fields present in v0.0.53 defaults: wait_for_ci, wait_for_reviews. Run `fabrik refresh-stages --apply` to add the missing keys.
```

This is informational — the engine continues running. It means your stage file predates behavioral options added in a newer binary.

To resolve: run `fabrik refresh-stages --apply` to add the missing keys to your stage YAMLs, then `git diff` and `git commit` to record the update. See [§1 Stage YAML Drift Warning](#stage-yaml-drift-warning) for a full explanation and list of common keys.

### Issue Not Being Picked Up

- Confirm the board column name exactly matches a stage YAML `name` (case-sensitive).
- Confirm the issue is on the project board.
- Check for `fabrik:locked:<other-user>` label (another instance has it).
- Check for stuck `fabrik:editing` label (remove manually if stale).
- Check the poll log -- the issue may be filtered by `updatedAt` caching. Restart Fabrik
  to force a fresh scan.

### Stage Keeps Retrying

A stage that never outputs `FABRIK_STAGE_COMPLETE` retries after each cooldown. Causes:

- **Claude needs user input** -- answer the questions in the issue comments.
- **Missing context** -- add detail to the issue body.
- **Bug in the skill** -- check the SKILL.md in `.fabrik/plugin/skills/`.

After `--max-retries` failures, the issue is paused with `fabrik:paused`. Remove the
label to resume.

### Stale Worktrees

Worktrees are at `.fabrik/worktrees/<owner>-<repo>/issue-N/` on branch `fabrik/issue-N`. On each stage
invocation, Fabrik rebases onto the latest base branch (or the branch specified by a
`base:<branch>` label), unless it's a retry, which preserves the worktree as-is to
maintain Claude's context. If the rebase conflicts, it's silently aborted and Claude
works from the current base.

To manually clean up:
```bash
git worktree remove --force .fabrik/worktrees/<owner>-<repo>/issue-N
git branch -D fabrik/issue-N
```

### Killed or Interrupted Sessions

Claude sessions are stored at `.fabrik/sessions/issue-N/<stage>.session` (relative to the project working directory). On retry, Fabrik resumes from the session file. The Implement stage commits frequently to minimize lost work.

To force a fresh session:
1. Remove: `rm .fabrik/sessions/issue-N/<stage>.session`
2. Remove `stage:<name>:complete` label if incorrectly applied
3. Fabrik starts a fresh session on next poll

### Comment Reprocessed After Restart

The rocket reaction is the durable "processed" marker. If a comment gets reprocessed
after restart, it means the rocket was not applied before shutdown (killed mid-flight).
This is expected for in-flight comments.

### Issue Body Not Updated

If you see `FABRIK_ISSUE_UPDATE_BEGIN/END` markers in stage comments, the markers
are being processed correctly -- the issue body is updated and the markers are stripped
from the posted comment. If the markers appear verbatim, update Fabrik to the latest
version.

### Post-to-PR Output Missing

For stages with `post_to_pr: true`:
- Confirm the issue has a linked PR (via "Development" section or `Closes #N` in PR body)
- If no PR is found, output falls back to the issue

### Raw JSON in Comments

If you see raw JSON dumped in issue/PR comments, the output was too large for the JSON
parser or the format changed. Fabrik now posts an error message instead of raw JSON.
Check `.fabrik/logs/<owner>-<repo>/issue-N/` for the full output.

### Multi-User and Multi-Instance Operation

Multiple users and multiple Fabrik instances can safely share a project board.

**Comment processing**: Fabrik processes comments from *any* author, not just the
configured `--user`. Human colleagues, code-review bots (Copilot, Gemini), and
other Fabrik instances all trigger comment processing. Only two categories are
skipped: comments that start with `🏭 **Fabrik` (Fabrik's own output) and comments
that already carry a 🚀 rocket reaction (already processed).

**Multi-instance locking**: The `fabrik:locked:<user>` label is still the
concurrency guard, but acquisition now uses a **lock-then-verify** protocol:

1. An instance adds its `fabrik:locked:<username>` label.
2. It waits 2 seconds to let a competing instance place its own lock.
3. It re-fetches the issue's labels.
4. If no other `fabrik:locked:*` label is present → proceed.
5. If a competing lock exists → **lexicographic tie-breaking**: the instance
   whose username sorts *lower* alphabetically keeps its lock and proceeds;
   the instance whose username sorts *higher* removes its lock and skips this
   issue for this poll cycle. On the next poll, the winner's lock is already
   visible, so the loser skips cleanly without retrying.

This guarantees that exactly one instance processes each issue at a time, even
when multiple instances poll simultaneously.

### Duplicate Comments on Issues

If a single stage run produces multiple comments on the issue (each with the
`🏭 Fabrik — stage:` header), the cause is almost always globally-installed
Claude Code plugins interfering with Fabrik's headless sessions.

The `superpowers` plugin is a known offender — its SessionStart hook causes
Claude to spawn parallel Agent subagents, each producing separate output that
Fabrik posts as individual comments.

**Fix:**
- Remove interfering global plugins: `rm -rf ~/.claude/plugins/cache/claude-plugins-official/superpowers`
- Check for other global plugins: `ls ~/.claude/plugins/cache/claude-plugins-official/`
- Fabrik's behavior should be governed only by the repo's CLAUDE.md and the Fabrik plugin — not personal global plugins

**Prevention:** Avoid installing global Claude Code plugins on machines that run Fabrik.

### Multiple Fabrik Instances

Running two Fabrik processes against the same project board causes duplicate
dispatches, wasted API credits, and conflicting comments. Each instance has its
own in-memory deduplication, so they can't detect each other.

Starting with v0.0.30, Fabrik acquires an exclusive file lock on
`.fabrik/fabrik.lock` at startup. If another instance is already running for
the same project, it exits with a clear error. The lock is automatically
released if the process crashes.

To check for stale instances: `pgrep -la fabrik`

### `unknown command "webhook" for "gh"` in Webhook Mode

**Symptom:** Fabrik logs lines like:

```
[webhook] [gh] unknown command "webhook" for "gh"
[webhook] gh webhook forward exited: exit status 1 — restarting in 1s
```

**Cause:** `gh webhook forward` is not a built-in `gh` subcommand — it is the [`cli/gh-webhook`](https://github.com/cli/gh-webhook) extension, which must be installed separately.

**Fix:**
```bash
gh extension install cli/gh-webhook
gh webhook --help   # verify: should print usage
```

Then restart Fabrik. The extension is a one-time install and persists across `gh` upgrades.

### Plugin Not Loading

If Claude doesn't seem to follow the skill instructions:
- Verify `.fabrik/plugin/` exists (run `fabrik init` if not)
- Check that the stage YAML has `skill:` set (not `prompt:`)
- For development, use `--plugin-dir` to point at your working copy
- Verify Claude Code version supports `--plugin-dir`
