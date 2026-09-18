---
name: fabrik-setup
description: Bootstrap a project to use Fabrik (the GitHub-Project-driven SDLC pipeline orchestrator that drives Claude Code workers through Specify/Research/Plan/Implement/Review/Validate stages). Use this skill when the user wants to install, set up, initialize, or get started with Fabrik for the first time — especially when they ask "how do I get started with Fabrik", "install Fabrik", "set up Fabrik in this project", or mention Fabrik in a project where no `.fabrik/` directory exists yet. Walks through prerequisites, binary install, GitHub Project board setup, `fabrik init`, secret configuration, and the first run.
---

# Bootstrapping Fabrik

This skill guides a user through installing Fabrik and standing it up for the first time in their project. Don't run any of these steps yourself unsolicited — walk the user through them, confirm their environment, and let them execute. Authoritative docs live at https://fabrik.handarbeit.io — link the user there rather than reproducing details that may drift.

## Before you start

Confirm the user has — or help them obtain — each of these. Don't proceed past step 0 until prerequisites are in place.

1. **Claude Code CLI installed and authenticated.** They almost certainly have this if they're talking to you, but check with `claude --version` if uncertain.
2. **A GitHub repo and a GitHub Project (v2) board.** The board is non-optional — Fabrik *is* a board orchestrator. `fabrik init --create-board --owner <org> --repo <repo>` (organization-owned repos) creates one directly from the stage configs, with columns guaranteed to match — prefer this over hand-building one in GitHub's UI. See Step 4.
3. **A GitHub token with `repo`, `project`, and `workflow` scopes.** The real constraint is narrower than "classic only": fine-grained tokens (`github_pat_...`) don't work with GitHub Projects v2 GraphQL, but both classic PATs (`ghp_...`) and `gh` CLI OAuth tokens (`gho_...`) do. Since `gh auth login` is already a prerequisite (next item), the shortest path is usually:
   ```bash
   FABRIK_TOKEN=$(gh auth token)
   ```
   Note this value rotates whenever the user re-runs `gh auth login` or `gh auth refresh` — a `.env` pinned to a stale copy will start failing after a re-auth. A classic PAT (create at https://github.com/settings/tokens, select "Tokens (classic)") is the alternative if they want a token independent of their `gh` CLI session.

   **Identity-mismatch trap:** if they're reusing a token copied from another Fabrik project's `.env`, it may belong to a *different* GitHub account than the one the Project board is under — a classic, correctly-scoped token, so nothing about it looks wrong. It fails as `NOT_FOUND: Could not resolve to a ProjectV2`, which reads like a wrong project number, not a wrong identity. Have them check: `gh api graphql -f query='{ viewer { login } }'` should match `user:` in `config.yaml`.
4. **`gh` CLI authenticated** (`gh auth status`) — needed for the binary download path, for `FABRIK_TOKEN=$(gh auth token)` above, and convenient for many Fabrik workflows.
5. **Go 1.26.1+** — only required if they want to build from source instead of downloading a release binary.

If they're missing prerequisites, get them sorted first. Don't paper over a missing token with vague instructions; the real constraint (fine-grained tokens don't work) is a common trap even when the user believes they've followed "use a classic PAT" advice from elsewhere.

## Step 1 — install the binary

Two paths. **Strongly prefer the release binary** unless the user has a reason to build from source.

**Option A — release binary (recommended):**

```bash
cd ~/bin  # or any directory on PATH
gh release download --repo handarbeit/fabrik \
  --pattern "fabrik_*_$(uname -s | tr A-Z a-z)_$(uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/').tar.gz" \
  -O - | tar xz
```

**Option B — build from source:**

```bash
git clone https://github.com/handarbeit/fabrik.git
cd fabrik
go build -o fabrik .
```

Verify with `fabrik --version`.

## Step 2 — initialize the project

**Run `fabrik init` from the main project checkout** — `cd` into the repo you're setting up Fabrik for, and run `fabrik init` there. This is the recommended default for a first-time, single-repo setup. Fabrik bare-clones each managed repo separately into `.fabrik/repos/` regardless of where `.fabrik/` itself lives, so this doesn't create any conflict with the checkout it sits inside; `fabrik init` also writes `.git/info/exclude` entries specifically to keep Fabrik's own working directories out of `git status` noise in this layout (see the gitignore note below).

A sibling directory (not inside any repo checkout) also works, and is what multi-repo setups typically use, since `.fabrik/` isn't naturally "inside" any one of several managed repos in that case. Either layout is fully supported — pick the in-checkout layout unless they already know they want multi-repo.

Two paths for `init`, depending on whether the org already has a board:

**Recommended — let Fabrik create the board too** (organization-owned repos):

```bash
fabrik init --create-board --owner <org> --repo <repo>
```

This extracts the stage configs *and* creates a fully-configured Project (v2) board from them in one step — column names are guaranteed to match, since nothing was hand-typed into GitHub's UI. It writes the new project's number/owner/owner-type into `.fabrik/config.yaml` for you. Skip to Step 3.

**Manual board / user-owned repos:**

```bash
fabrik init --user <github-username> https://github.com/orgs/<org>/projects/<N>
```

Points `fabrik init` at a Project board that already exists (see Step 4 for building one by hand if it doesn't exist yet).

Either way, `fabrik init` creates:

- `.fabrik/stages/` — stage YAML configs (commit these to git if you want them version-controlled per project)
- `.fabrik/plugin/` — the worker-side Claude Code plugin. **Not gitignored** — per-project skill customization here is a documented feature (`fabrik upgrade` detects local edits and refuses to clobber them rather than silently overwriting). Don't tell the user this directory is excluded from git tracking.
- `.fabrik/config.yaml` — project config template, populated with values from `--create-board` or the Project URL

If the user runs `fabrik init` without `--create-board` or a URL, it falls back to interactive prompts, or writes a commented template if non-interactive.

`fabrik init` also updates `.git/info/exclude` (when run inside a git checkout) with `.fabrik/repos/`, `.fabrik/worktrees/`, `.fabrik/debug/`, `.fabrik/history.json`, and `.fabrik/warnings.json` — **not** `.fabrik/plugin/`. Two things worth telling the user:

- `.git/info/exclude` is **machine-local** — it doesn't travel with the repo. A second machine or a fresh clone starts without any of it. If they want the exclusion to travel with the repo, recommend a `.gitignore` snippet instead (see below).
- A few runtime artifacts a fresh setup will produce aren't excluded by `fabrik init` at all: `.fabrik/fabrik.lock`, `.fabrik/fabrik.log`, `.fabrik/logs/`, `.fabrik/sessions/`, `.fabrik/plugin/.installed-version`.

Suggested `.gitignore` snippet for anyone who wants this to travel (note `.fabrik/plugin/` is deliberately *not* in this list — see above):

```
.fabrik/repos/
.fabrik/worktrees/
.fabrik/debug/
.fabrik/history.json
.fabrik/warnings.json
.fabrik/fabrik.lock
.fabrik/fabrik.log
.fabrik/logs/
.fabrik/sessions/
.fabrik/plugin/.installed-version
```

## Step 3 — create the .env

Add a gitignored `.env` next to `.fabrik/`, using the token from Prerequisite 3:

```
FABRIK_TOKEN=<your gh auth token or classic PAT>
```

e.g. `FABRIK_TOKEN=$(gh auth token)` captured as a literal value, or `FABRIK_TOKEN=ghp_...` for a classic PAT.

**Important:** when a `.git/` directory is present, Fabrik refuses to start unless `.env` is listed in `.gitignore`. This is a token-leak guard, not a bug.

## Step 4 — verify the Project board columns

If you used `fabrik init --create-board` in Step 2, the board already has every column correctly named — nothing to do here, skip to Step 5.

Otherwise, the Project board needs a status column for each stage. `fabrik init` always extracts all nine stage YAMLs, so the full column list, in order, is:

`Backlog → Specify → Research → Plan → Implement → Review → Validate → Queued → Done`

`Queued` is easy to miss by eye since it's a holding column, not a pipeline stage — it's only used when the merge-train feature (`merge_train: on`) is enabled, but `fabrik init` extracts its stage YAML unconditionally, so a board built without it produces a startup warning immediately, and outright fails startup if `merge_train` is later turned on without the column existing. See the User Guide's merge-train section for what it's for.

Column names must match `name:` in the stage YAML files **exactly** (case-sensitive). On startup, Fabrik validates this and refuses to start if any non-cleanup stage is missing from the board — so it's worth getting right before the first run. If the board already exists and is missing columns (e.g. it predates `Queued`), run `fabrik repair-board --apply` to add them without disturbing any item's current status, instead of editing the board by hand.

If the user wants a custom pipeline they should edit `.fabrik/stages/*.yaml` and rename columns to match. This is a deeper customization — point them at the User Guide before going down that road.

## Step 5 — first run

```bash
fabrik
```

There's no `--once`/`--validate`/dry-run flag — don't tell the user one exists. The safe way to check a fresh board without committing to a live run: startup board validation runs before any dispatch, so it's safe to run `fabrik`, watch it validate columns and log its first poll, and Ctrl-C once that looks right. Nothing gets dispatched until an issue is actually sitting in the `Specify` column.

Fabrik will poll the board every 30s, validate column names against stage YAML, and start dispatching workers when issues land in the `Specify` column. To kick off the first issue: create one in the repo, add it to the Project, set its status to `Specify`, and watch.

Useful flags for the first run:

- `fabrik --debug-output` — saves raw Claude output to `.fabrik/debug/` for troubleshooting a stage.
- `fabrik --notui` — disables the TUI dashboard (plain log output instead).
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
- **What labels do** — https://fabrik.handarbeit.io/USER_GUIDE#6-labels-reference
- **State machine** (engine behaviour) — https://fabrik.handarbeit.io/state-machine
- **Troubleshooting** — https://fabrik.handarbeit.io/troubleshooting

## What this skill is not

- **Not a runbook for Fabrik in production** — auth setup, multi-repo, SSH cloning, observability all live in the User Guide. Point users there when they ask.
- **Not for users who already have `.fabrik/`** — if `.fabrik/` exists, Fabrik is already initialized; the user wants the supervisor skill, not setup. If they're trying to *re-init* or *upgrade*, point them at `fabrik upgrade` rather than re-running `fabrik init`.
