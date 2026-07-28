# Pruefer

Pruefer is a self-hosted PR review daemon. It watches configured GitHub repositories, reviews open pull requests by invoking the `claude` CLI (subscription-backed, not API-metered), and submits a formal comment-only `pull_request_review`.

It exists to satisfy Fabrik's `wait_for_reviews: true` gate (and any repo that wants a review bot) without depending on a hosted third-party reviewer's quota, and without per-token API billing. See [adrs/1113-pruefer-v1-architecture.md](../../adrs/1113-pruefer-v1-architecture.md) for the full architectural rationale.

**V1 scope**: comment-only reviews (`event: COMMENT`). Pruefer never approves a PR and never requests changes — see the ADR for why.

## How it works

Every `poll_interval_seconds`, Pruefer lists open, non-draft PRs on each watched repo and, for each one, checks:

- Is the PR authored by Pruefer's own bot identity? Skip (GitHub rejects self-review anyway).
- Does an excluded author/label/path match? Skip.
- Has Pruefer already reviewed this exact head SHA? Skip — **unless** an unprocessed `/pruefer review` comment is on the PR, which forces a fresh review of the current head.
- Is the diff larger than `max_diff_bytes`? Skip (logged, not truncated).

Otherwise, Pruefer clones the PR's head commit into a temporary directory, invokes `claude` with a read-only tool allowlist to produce a prose summary plus structured findings, and submits it as a formal `pull_request_review` (event `COMMENT`) pinned to that head SHA. Findings that map to a changed line in the diff are posted as line-anchored inline comments in the same request; any finding that can't be anchored (a line the diff doesn't touch) is demoted into the summary body instead of dropping it or failing the whole review. Inline comments are what let Fabrik's review-reinvoke path pick up Pruefer's findings and act on them automatically — see [adrs/1189-pruefer-inline-review-comments.md](../../adrs/1189-pruefer-inline-review-comments.md). On any failure — clone, invocation, or submission — Pruefer posts nothing and logs the failure; the PR is naturally retried on the next poll.

Review state ("already reviewed at SHA X") is derived from GitHub itself (existing reviews authored by Pruefer's bot identity), not stored locally — a restart never causes a review storm.

## Setup

### 1. Create a GitHub App

Pruefer authenticates as a GitHub App so its reviews are attributed to a genuine bot identity (`<app-slug>[bot]`) — this is what makes "review identity distinct from PR author" structural rather than a setup mistake waiting to happen.

1. Go to your org or personal account's **Settings → Developer settings → GitHub Apps → New GitHub App**.
2. Fill in:
   - **GitHub App name**: anything unique (e.g. `your-org-pruefer`). This becomes the `<slug>` in the App's `<slug>[bot]` identity.
   - **Homepage URL**: any placeholder URL — not used.
   - **Webhook**: **uncheck "Active"**. Pruefer polls; it does not receive webhooks (see ADR-1113 for why this doesn't conflict with ADR-032's webhook-delivery ruling).
3. **Repository permissions**, minimum required:
   - **Pull requests**: Read and write (review submission, reading PR metadata/diff)
   - **Contents**: Read (cloning the PR head commit)
   - **Metadata**: Read (mandatory baseline for every App)
   - **Issues**: Read (reading `/pruefer review` comments and their reactions — GitHub's Issue Comments API also covers PR-conversation comments)
4. **Where can this GitHub App be installed?**: your choice — "Only on this account" is simplest for a single org.
5. Click **Create GitHub App**. Note the **App ID** shown on the app's settings page.
6. Scroll to **Private keys** and click **Generate a private key**. This downloads a `.pem` file — save it somewhere outside version control (Pruefer's default config gitignores `.pruefer/*.pem`).
7. Click **Install App** (left sidebar) and install it on every account whose repos you list in `watched_repos` — a single App installed on multiple orgs/accounts is exactly the multi-org setup this section leads into.

### 2. Place the private key

Put the downloaded `.pem` file at `.pruefer/app-private-key.pem` (the default `github_app_private_key_path`), or point `github_app_private_key_path` at a different location. **Never** put the key's contents directly in `.env` or the YAML config — Pruefer only ever takes a file path.

### 3. Configure Pruefer

Create `.pruefer/config.yaml` (or use flags/env vars — see Configuration below):

```yaml
watched_repos:
  - your-org/repo-one
  - your-org/repo-two
  - another-org/repo-three   # a different owner works too — see below

github_app_id: 123456
# github_app_private_key_path: .pruefer/app-private-key.pem  # default shown
# github_app_installation_id: 0  # 0 = derive installations from watched_repos (see below)

poll_interval_seconds: 120
model: sonnet
effort: medium
concurrency_cap: 3
max_diff_bytes: 500000

# excluded_authors: [dependabot]
# excluded_labels: [skip-review]
# excluded_paths: ["vendor/**", "*.generated.go"]
```

### Multi-org installations and the public-App safety property

`watched_repos` may list repos across any number of distinct owners. Pruefer groups the list by owner and, for each distinct owner, resolves that owner's App installation and mints it its own token — refreshed independently on its own schedule. A single daemon can therefore cover every org/account the App is installed on, driven entirely by `watched_repos`.

**The set of installations Pruefer ever tokenizes equals exactly the set of owners appearing in `watched_repos` — nothing more.** Pruefer never enumerates "every installation of the App" and acts on all of them; it only ever mints a token for, or contacts, an owner an operator explicitly listed. This is what makes it safe to register the App as installable by anyone: a stranger who installs it on their own account is structurally untouched, because no `watched_repos` entry names them. There's no separate allowlist to maintain — "only act on my own accounts" falls directly out of what's in the config.

Two consequences:

- If an owner in `watched_repos` has **no** matching App installation, Pruefer fails fast at startup with an error naming that owner (and how to fix it — install the App there).
- `github_app_installation_id` is now a **legacy pin/escape hatch**, not a requirement. Leave it at `0` (or unset) to let Pruefer resolve one installation per watched owner automatically — this works cleanly even with several installations, as long as every owner you watch has one. Set it explicitly only to force every watched repo through one specific installation's token regardless of owner (the old single-installation behavior, preserved byte-for-byte for existing single-org deployments).

### 4. Run it

```bash
go build -o pruefer ./cmd/pruefer
./pruefer
```

Pruefer loads `.env` (via the same `godotenv`-based loader Fabrik uses), reads `.pruefer/config.yaml`, bootstraps GitHub App auth, and polls until interrupted (SIGINT/SIGTERM). Pruefer also needs a working `claude` CLI installation with a valid Claude Code subscription on the host it runs on — a separate credential from the GitHub App.

A lock file at `.pruefer/pruefer.lock` prevents two instances from polling the same working directory concurrently.

## Terminal UI

When run with a real terminal attached (both stdin and stdout), Pruefer launches an interactive TUI by default — the same `bubbletea`/`bubbles`/`lipgloss` stack and model/update/view structure as Fabrik's own `tui/` package, so the two feel like the same family of tool. It shows:

- Watched repositories, each with its last poll time, PR count found, and last error (if any).
- PRs currently under review, with elapsed time since the review started.
- Recently completed reviews (last 200, in-memory — not persisted across restarts): repo, PR, outcome (reviewed / skipped / errored), turns, cost, and duration.
- Skipped PRs with their reason, covering every skip category Pruefer tracks (draft, self-authored, excluded author/label/path, already reviewed at this head SHA, diff too large).
- Errors, plus GitHub REST API rate-limit state and a running session-total cost/turn count.

Keyboard: `q` or `ctrl+c` to quit, `tab` to switch panes, `↑`/`↓` or `j`/`k` to scroll and select an entry, `enter` to view its detail.

The TUI is purely observational — it never changes which PRs get reviewed, when, or how. Running with `-notui` disables it entirely and falls back to Pruefer's existing structured `logf`-based console output, with **identical review behavior** either way. Use `-notui` (or `PRUEFER_TUI=0` / `tui: false` in the YAML config) when running under systemd, tmux with no attached TTY, or any other non-interactive environment.

## On-demand re-review

Comment `/pruefer review` on any watched PR to force a fresh review of the current head, even if that SHA was already reviewed. Pruefer acknowledges the command with a 👀 reaction when it picks it up and a 🚀 reaction once the review has been submitted — the same idempotency convention Fabrik uses for its own comment processing.

## Configuration reference

Precedence, highest to lowest: **flag > environment variable > YAML config file > default.**

| Flag | Env var | YAML key | Default | Notes |
|---|---|---|---|---|
| `--repos` | `PRUEFER_REPOS` | `watched_repos` | (none — required) | Comma-separated `owner/repo` list |
| `--poll-interval` | `PRUEFER_POLL_INTERVAL` | `poll_interval_seconds` | `120` | Seconds |
| `--model` | `PRUEFER_MODEL` | `model` | `sonnet` | Claude model |
| `--effort` | `PRUEFER_EFFORT` | `effort` | `medium` | `low`, `medium`, `high`, or `max` |
| `--concurrency` | `PRUEFER_CONCURRENCY` | `concurrency_cap` | `3` | Max simultaneous `claude` invocations |
| `--max-diff-bytes` | `PRUEFER_MAX_DIFF_BYTES` | `max_diff_bytes` | `500000` | PRs with a larger diff are skipped, not truncated |
| `--max-wall-time` | `PRUEFER_MAX_WALL_TIME` | `max_wall_time_seconds` | `0` (no cap) | Seconds; caps a single `claude` review invocation's wall-clock duration on top of the fixed 15-minute inactivity watchdog |
| `--excluded-authors` | `PRUEFER_EXCLUDED_AUTHORS` | `excluded_authors` | (none) | Comma-separated logins |
| `--excluded-labels` | `PRUEFER_EXCLUDED_LABELS` | `excluded_labels` | (none) | Skip if any label matches |
| `--excluded-paths` | `PRUEFER_EXCLUDED_PATHS` | `excluded_paths` | (none) | Glob patterns; skip only if **every** touched path matches |
| `--github-app-id` | `PRUEFER_GITHUB_APP_ID` | `github_app_id` | (none — required) | |
| `--github-app-private-key-path` | `PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH` | `github_app_private_key_path` | `.pruefer/app-private-key.pem` | |
| `--github-app-installation-id` | `PRUEFER_GITHUB_APP_INSTALLATION_ID` | `github_app_installation_id` | `0` (derive from `watched_repos`) | Legacy pin: set to force every watched repo through one specific installation, regardless of owner |
| `--config` | `PRUEFER_CONFIG` | — | `.pruefer/config.yaml` | Path to the YAML config file itself |
| `-notui` | `PRUEFER_TUI` | `tui` | `true` | Set `-notui` / `PRUEFER_TUI=0` / `tui: false` to disable the interactive TUI and fall back to console logging. The TUI is further gated on a real terminal being detected on both stdin and stdout, regardless of this setting. |

Draft PRs are always skipped — there is no configuration flag to include them in V1.

## Out of scope for V1

- `APPROVE` / `REQUEST_CHANGES` verdicts.
- Multi-line (`start_line`) inline comment ranges — single-line anchors only.
- Non-GitHub forges.
- Removing `.github/workflows/claude-review.yml` from any repo — that stays until Pruefer is proven in practice.
