---
name: fabrik-setup
description: Bootstrap a project to use Fabrik (the GitHub-Project-driven SDLC pipeline orchestrator that drives Claude Code workers through Specify/Research/Plan/Implement/Review/Validate stages). Use this skill when the user wants to install, set up, initialize, or get started with Fabrik for the first time — especially when they ask "how do I get started with Fabrik", "install Fabrik", "set up Fabrik in this project", or mention Fabrik in a project where no `.fabrik/` directory exists yet. Walks through prerequisites, binary install, GitHub Project board setup, `fabrik init`, secret configuration, and the first run.
---

# Bootstrapping Fabrik

This skill guides a user through installing Fabrik and standing it up for the first time in their project. Don't run any of these steps yourself unsolicited — walk the user through them, confirm their environment, and let them execute. Authoritative docs live at https://fabrik.shadoworg.dev — link the user there rather than reproducing details that may drift.

## Before you start

Confirm the user has — or help them obtain — each of these. Don't proceed past step 0 until prerequisites are in place.

1. **Claude Code CLI installed and authenticated.** They almost certainly have this if they're talking to you, but check with `claude --version` if uncertain.
2. **A GitHub repo and a GitHub Project (v2) board.** The board is non-optional — Fabrik *is* a board orchestrator. If they don't have a Project yet, point them at https://github.com/orgs/<org>/projects/new (or the personal equivalent). The board's status column names will need to match the Fabrik stage names later.
3. **A GitHub classic personal access token** (`ghp_...`) with `repo`, `project`, and `workflow` scopes. Fine-grained tokens (`github_pat_...`) are **not supported** — GitHub Projects v2 GraphQL requires a classic PAT. Create one at https://github.com/settings/tokens (select "Tokens (classic)").
4. **`gh` CLI authenticated** (`gh auth status`) — needed for the binary download path and convenient for many Fabrik workflows.
5. **Go 1.26.1+** — only required if they want to build from source instead of downloading a release binary.

If they're missing prerequisites, get them sorted first. Don't paper over a missing PAT with vague instructions; the token type matters and getting it wrong is a common failure mode.

## Step 1 — install the binary

Two paths. **Strongly prefer the release binary** unless the user has a reason to build from source.

**Option A — release binary (recommended):**

```bash
cd ~/bin  # or any directory on PATH
gh release download --repo shadoworg/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

**Option B — build from source:**

```bash
git clone https://github.com/shadoworg/fabrik.git
cd fabrik
go build -o fabrik .
```

Verify with `fabrik --version`.

## Step 2 — initialize the project

From the **directory where you want Fabrik to run** (typically a sibling to your repo checkouts, *not* inside one — Fabrik bare-clones repos into `.fabrik/repos/` and creates worktrees as siblings):

```bash
mkdir ~/my-fabrik-dir && cd ~/my-fabrik-dir
fabrik init --user <github-username> https://github.com/orgs/<org>/projects/<N>
```

This creates:

- `.fabrik/stages/` — stage YAML configs (commit these to git if you want them version-controlled per project)
- `.fabrik/plugin/` — the worker-side Claude Code plugin (gitignored; refreshed by `fabrik upgrade`)
- `.fabrik/config.yaml` — project config template, populated with values from the Project URL

If the user runs `fabrik init` without a URL it falls back to interactive prompts, or writes a commented template if non-interactive.

## Step 3 — create the .env

Add a gitignored `.env` next to `.fabrik/`:

```
FABRIK_TOKEN=ghp_...
```

**Important:** when a `.git/` directory is present, Fabrik refuses to start unless `.env` is listed in `.gitignore`. This is a token-leak guard, not a bug.

## Step 4 — set up the Project board columns

The Project board needs a status column for each stage. The default pipeline expects, in order:

`Backlog → Specify → Research → Plan → Implement → Review → Validate → Done`

Column names must match `name:` in the stage YAML files **exactly** (case-sensitive). On startup, Fabrik validates this and refuses to start if any non-cleanup stage is missing from the board — so it's worth getting right before the first run.

If the user wants a custom pipeline they should edit `.fabrik/stages/*.yaml` and rename columns to match. This is a deeper customization — point them at the User Guide before going down that road.

## Step 5 — first run

```bash
cd ~/my-fabrik-dir
fabrik
```

Fabrik will poll the board every 30s, validate column names against stage YAML, and start dispatching workers when issues land in the `Specify` column. To kick off the first issue: create one in the repo, add it to the Project, set its status to `Specify`, and watch.

Useful flags for the first run:

- `fabrik --once` — single poll cycle, useful for debugging.
- `fabrik --verbose` — more log detail.
- The TUI dashboard is the default UI; press `?` for keybinds.

## Compatibility gotcha

**Global Claude Code plugins can interfere with worker sessions.** The `superpowers` plugin in particular causes duplicate comments. Check with:

```bash
ls ~/.claude/plugins/cache/claude-plugins-official/
```

If `superpowers` appears, recommend removing it. This *only* matters on machines that run Fabrik as a worker host — installing other Claude Code plugins (like this `fabrik` plugin) for the *user's* interactive session is fine.

## Where to point the user next

Once they're up:

- **Authoring an issue Fabrik can act on** — switch to the `fabrik:fabrik` supervisor skill (ambient, will load itself once `.fabrik/` exists).
- **What labels do** — https://fabrik.shadoworg.dev/USER_GUIDE#6-labels-reference
- **State machine** (engine behaviour) — https://fabrik.shadoworg.dev/state-machine
- **Troubleshooting** — https://fabrik.shadoworg.dev/troubleshooting

## What this skill is not

- **Not a runbook for Fabrik in production** — auth setup, multi-repo, SSH cloning, observability all live in the User Guide. Point users there when they ask.
- **Not for users who already have `.fabrik/`** — if `.fabrik/` exists, Fabrik is already initialized; the user wants the supervisor skill, not setup. If they're trying to *re-init* or *upgrade*, point them at `fabrik upgrade` rather than re-running `fabrik init`.
