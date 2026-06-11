---
name: fabrik-setup
description: Bootstrap a project to use Fabrik (the GitHub-Project-driven SDLC pipeline orchestrator that drives Claude Code workers through Specify/Research/Plan/Implement/Review/Validate stages). Use this skill when the user wants to install, set up, initialize, or get started with Fabrik for the first time — especially when they ask "how do I get started with Fabrik", "install Fabrik", "set up Fabrik in this project", or mention Fabrik in a project where no `.fabrik/` directory exists yet. Walks through prerequisites, binary install, GitHub Project board setup, `fabrik init`, secret configuration, and the first run.
---

# Bootstrapping Fabrik

This skill guides a user through installing Fabrik and standing it up for the first time in their project. Don't run any of these steps yourself unsolicited — walk the user through them, confirm their environment, and let them execute. Authoritative docs live at https://fabrik.handarbeit.io — link the user there rather than reproducing details that may drift.

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

## The `.fabrik/verify.yaml` contract

`.fabrik/verify.yaml` is an **optional**, per-project, language-agnostic contract that declares how to behaviorally verify a change — "does it actually work end-to-end?", beyond compile + unit tests. The Implement and Validate workers read and run it. It exists because **CI-green ≠ behavior-correct**: a change can pass every unit test while leaving the feature broken, when the tests bypassed the real production seam. This file gives the workers a concrete, project-authored behavioral gate to run.

It is opt-in. If the file is absent, there is no project-declared behavioral gate — the workers' real-entry-point doctrine still applies, but nothing here hard-blocks.

### Schema

```yaml
# Behavioral verification for this project — "does it actually work end-to-end?",
# beyond compile + unit tests. Read and run by the Fabrik Implement/Validate workers.
command: <shell command; exits non-zero on wrong behavior>   # the default behavioral gate
needs: []                 # env var names required to run `command`; if any is unset → UNVERIFIED
description: <one line: what this proves>
fallback_prose: |         # optional: human-interpretable steps when no runnable command exists
  <steps + expected observables>
checks:                   # optional named recipes a spec/issue can reference by name
  <name>:
    command: <shell command>
    needs: [<ENV>, ...]
    description: <one line>
```

### Rules

- **Behavioral changes** — anything altering runtime behavior, as opposed to pure docs or refactors already covered by existing tests — MUST run the default `command`, plus any `checks.<name>` the issue's acceptance criteria reference by name.
- **Non-zero exit = failing verification.** Treat it like a failing test, not a warning.
- **Any `needs:` var unset** → the command cannot run → take the honest-unverified path: report it explicitly and do not fabricate a pass.
- **`command` absent but `fallback_prose` present** → execute the prose steps with available tools; if that's impossible, honest-unverified.
- **Absent file** → no project-declared behavioral gate; the real-artifact-testing doctrine still applies.

Keep the default `command` **CI-runnable** wherever possible, so it guards every PR automatically. Checks that need live secrets or external services belong under `checks.<name>` with their `needs:` declared — they run only when those secrets are present, and degrade to honest-unverified otherwise.

### Example

```yaml
command: npm run test:verify
needs: []
description: Drives discoverSidecars on real sidecar fixtures and asserts the
  ESVCLP form data reaches the generated workbook end-to-end.
fallback_prose: |
  Run the report generator against the ESVCLP sample sidecar. Open the
  generated workbook and confirm the portfolio-company cells are populated
  (not blank) — a blank cell means the sidecar was silently dropped.
checks:
  cir-live:
    command: npm run test:cir-live
    needs: [CIR_API_TOKEN, CIR_BASE_URL]
    description: Exercises the live CIR endpoint end-to-end against a real token.
```

Here `command` is CI-runnable (no `needs:`), so it guards every PR. The `cir-live` check needs live secrets, so it lives under `checks.<name>` and runs only when `CIR_API_TOKEN` and `CIR_BASE_URL` are set — otherwise the worker reports it unverified rather than claiming a pass.

## Where to point the user next

Once they're up:

- **Authoring an issue Fabrik can act on** — switch to the `fabrik:fabrik` supervisor skill (ambient, will load itself once `.fabrik/` exists).
- **What labels do** — https://fabrik.handarbeit.io/USER_GUIDE#6-labels-reference
- **State machine** (engine behaviour) — https://fabrik.handarbeit.io/state-machine
- **Troubleshooting** — https://fabrik.handarbeit.io/troubleshooting

## What this skill is not

- **Not a runbook for Fabrik in production** — auth setup, multi-repo, SSH cloning, observability all live in the User Guide. Point users there when they ask.
- **Not for users who already have `.fabrik/`** — if `.fabrik/` exists, Fabrik is already initialized; the user wants the supervisor skill, not setup. If they're trying to *re-init* or *upgrade*, point them at `fabrik upgrade` rather than re-running `fabrik init`.
