# Pruefer

Pruefer is a self-hosted PR review daemon. It watches configured GitHub repositories, reviews open pull requests by invoking the `claude` CLI (subscription-backed, not API-metered), and submits a formal `pull_request_review` — a non-blocking comment by default, or a blocking `REQUEST_CHANGES` review when a finding's severity meets a configured threshold (see [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes) below).

It exists to satisfy Fabrik's `wait_for_reviews: true` gate (and any repo that wants a review bot) without depending on a hosted third-party reviewer's quota, and without per-token API billing. See [adrs/1113-pruefer-v1-architecture.md](../../adrs/1113-pruefer-v1-architecture.md) for the full architectural rationale and [adrs/1251-pruefer-severity-gated-request-changes.md](../../adrs/1251-pruefer-severity-gated-request-changes.md) for the `REQUEST_CHANGES` amendment.

**Pruefer never approves a PR — ever, under any configuration.** Approval is an accountability decision that must not rest on a bot rubber-stamping itself; this is permanent, not scoped to any toggle. `REQUEST_CHANGES` is opt-in (default off) and computed deterministically in Go from parsed finding severities — never from anything Claude writes in prose, so prompt injection cannot escalate or suppress it.

## How it works

Every `poll_interval_seconds`, Pruefer lists open, non-draft PRs on each watched repo and, for each one, checks:

- Is the PR authored by Pruefer's own bot identity? Skip (GitHub rejects self-review anyway).
- Does an excluded author/label/path match? Skip.
- Has Pruefer already reviewed this exact head SHA? Skip — **unless** an unprocessed `/pruefer review` comment is on the PR, which forces a fresh review of the current head.
- Is the diff larger than `max_diff_bytes`? Skip (logged, not truncated).
- Does GitHub refuse to render the diff at all (a 406 `too_large` response — the diff exceeds GitHub's own 20,000-line ceiling on the `.diff` media type, independent of `max_diff_bytes`)? This is a deterministic size verdict, not a transient error, so Pruefer does not hot-retry it every poll. Instead it falls back to the paginated changed-files API (no line-count ceiling) to reconstruct the list of touched paths, and reviews the PR normally against the local clone — with inline-comment anchoring naturally unavailable, so findings land in the review body instead of as line comments. Only if that fallback also fails does Pruefer skip the PR (same disposition as the `max_diff_bytes` skip above) and post a single PR comment explaining why, so the decision is visible to a human instead of silently repeating in the log. See [adrs/1427-pruefer-diff-too-large-degrade-not-block.md](../../adrs/1427-pruefer-diff-too-large-degrade-not-block.md).

Otherwise, Pruefer clones the PR's head commit into a temporary directory, invokes `claude` with a read-only tool allowlist to produce a prose summary plus structured findings (each classified with a severity tier), and submits it as a formal `pull_request_review` pinned to that head SHA — `event: COMMENT` by default, or `event: REQUEST_CHANGES` if `request_changes_threshold` is set and a finding meets it (see below). Findings that map to a changed line in the diff are posted as line-anchored inline comments in the same request; any finding that can't be anchored (a line the diff doesn't touch) is demoted into the summary body instead of dropping it or failing the whole review — but still counts toward the severity threshold either way. Inline comments are what let Fabrik's review-reinvoke path pick up Pruefer's findings and act on them automatically — see [adrs/1189-pruefer-inline-review-comments.md](../../adrs/1189-pruefer-inline-review-comments.md). On any failure — clone, invocation, or submission — Pruefer posts nothing and logs the failure; the PR is naturally retried on the next poll.

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
# request_changes_threshold: high  # low, medium, high, or critical — see below
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

## Logging

Pruefer writes every daemon log line — poll cycles, review outcomes, warnings, auth events — to a timestamped, mutex-serialized log file at `.pruefer/pruefer.log` by default, so an incident is diagnosable from the daemon's own durable record instead of requiring reconstruction from the GitHub API. Set `log_file` (or `--log-file` / `PRUEFER_LOG_FILE`) to write elsewhere, or to an empty value to disable file logging entirely and fall back to stderr-only, matching Pruefer's behavior before this feature existed.

Each line looks like:

```
2026-08-08T18:12:03Z [pr#103 warn] listing open PRs for verveguy/zusammen: context deadline exceeded — skipping this repo this cycle
```

- **With the TUI running** (the default on a real terminal), stderr output would corrupt the bubbletea display, so the log file is the sole destination.
- **In plain daemon mode** (`-notui`, or no TTY attached), logging is additive: every line is written to both stderr and the log file, so an operator watching the terminal keeps seeing exactly what they see today.

The log file is append-only across restarts — it is never truncated on daemon startup, unlike Fabrik engine's own `fabrik.log` — so a restart doesn't erase the history an incident investigation needs. Growth is bounded by size-triggered rotation: once the file reaches 10 MB it's renamed to `pruefer.log.1` (existing numbered backups shift up, `pruefer.log.3` is dropped), and a fresh file is opened. Up to 3 rotated backups are retained.

## On-demand re-review

Comment `/pruefer review` on any watched PR to force a fresh review of the current head, even if that SHA was already reviewed. Pruefer acknowledges the command with a 👀 reaction when it picks it up and a 🚀 reaction once the review has been submitted — the same idempotency convention Fabrik uses for its own comment processing.

## Severity-gated REQUEST_CHANGES

By default, `request_changes_threshold` is unset and every review submits `event: COMMENT` — byte-for-byte the original behavior. Setting it to one of four ordinal tiers turns on blocking reviews: if any finding's severity ranks at or above the threshold, Pruefer submits `event: REQUEST_CHANGES` instead. This is the setting `review_authority: authoritative` (a per-stage Fabrik YAML field — see [adrs/1250-review-authority-orthogonal-to-autonomy.md](../../adrs/1250-review-authority-orthogonal-to-autonomy.md)) is designed to honor once Pruefer is a repo's sole reviewer.

Severity tiers, in ascending order:

| Tier | Meaning |
|---|---|
| `low` | Style, a minor nit, or a suggestion — not a defect. |
| `medium` | A real defect, but scoped and low-impact. |
| `high` | A bug or design issue likely to cause incorrect behavior. |
| `critical` | A security vulnerability, data loss, or severe correctness bug. |

The event is computed **only** from each finding's parsed `severity` field — never from Claude's prose summary or a finding's own `body` text, and never a value Claude (or a malicious PR) can pass as a raw string to the GitHub API. This holds even if the reviewed PR's content tries to inject text like "APPROVE" or "REQUEST_CHANGES" into what Claude reads. `APPROVE` remains structurally unreachable: `github.SubmitPRReview`'s event parameter is a type whose only two constructible values are "submit a comment" and "submit REQUEST_CHANGES" — there is no third construction path, in this package or any other.

An unrecognized or missing severity value on a single finding (e.g. Claude fails to follow the schema) is treated as **below every threshold** — it fails closed toward `COMMENT`, never toward the more disruptive `REQUEST_CHANGES`. By contrast, an unrecognized `request_changes_threshold` **config** value is rejected at startup with an error naming the bad value — a typo'd config setting is a one-time operator mistake worth catching immediately, not a value that should silently match every finding.

### Unblocking a REQUEST_CHANGES review

A `REQUEST_CHANGES` review blocks merges in repos with branch protection requiring resolved reviews. Unblocking it relies on GitHub's own **"Dismiss stale pull request approvals when new commits are pushed"** branch-protection setting — when Fabrik (or a human) pushes a fix, GitHub auto-dismisses the stale review, and Pruefer's next poll re-reviews the new head SHA (clean → `COMMENT`; still bad → `REQUEST_CHANGES` again, independently — Pruefer's decision carries no memory across SHAs).

**This is why resolving inline review threads is not enough on its own**: thread resolution and review dismissal are two different GitHub states. Fabrik's own auto-merge gate already reacts to unresolved threads (#1207), but a `CHANGES_REQUESTED` review's block persists at the branch-protection level regardless of thread state. Enable "dismiss stale reviews on push" in each watched repo's branch protection rule if you use `request_changes_threshold`.

**Not implemented**: Pruefer does not (yet) self-dismiss its own prior `REQUEST_CHANGES` review as a fallback for repos without stale-review dismissal enabled. If a future need for this arises: dismissing a PR review via `PUT /repos/{owner}/{repo}/pulls/{pull_number}/reviews/{review_id}/dismissals` requires the GitHub App installation to have `pull_requests: write`, **and**, on a protected branch, the App must additionally be included in that branch protection rule's explicit list of actors allowed to dismiss reviews — a separate, per-repo, manually-configured GitHub setting beyond the write permission itself. Self-dismissal is scoped to only ever dismiss Pruefer's own prior review, never another reviewer's.

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
| `--request-changes-threshold` | `PRUEFER_REQUEST_CHANGES_THRESHOLD` | `request_changes_threshold` | (none — disabled) | `low`, `medium`, `high`, or `critical`; submits `REQUEST_CHANGES` when a finding's severity meets or exceeds this tier. See [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes). |
| `--github-app-id` | `PRUEFER_GITHUB_APP_ID` | `github_app_id` | (none — required) | |
| `--github-app-private-key-path` | `PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH` | `github_app_private_key_path` | `.pruefer/app-private-key.pem` | |
| `--github-app-installation-id` | `PRUEFER_GITHUB_APP_INSTALLATION_ID` | `github_app_installation_id` | `0` (derive from `watched_repos`) | Legacy pin: set to force every watched repo through one specific installation, regardless of owner |
| `--config` | `PRUEFER_CONFIG` | — | `.pruefer/config.yaml` | Path to the YAML config file itself |
| `-notui` | `PRUEFER_TUI` | `tui` | `true` | Set `-notui` / `PRUEFER_TUI=0` / `tui: false` to disable the interactive TUI and fall back to console logging. The TUI is further gated on a real terminal being detected on both stdin and stdout, regardless of this setting. |
| `--log-file` | `PRUEFER_LOG_FILE` | `log_file` | `.pruefer/pruefer.log` | Path daemon log lines are written to, resolved against the process's working directory (same convention as `github_app_private_key_path`). An explicitly empty value (`--log-file ""`, `PRUEFER_LOG_FILE=`, or `log_file: ""`) disables file logging entirely. See [Logging](#logging). |

Draft PRs are always skipped — there is no configuration flag to include them in V1.

## Out of scope

- `APPROVE` verdicts — permanently out of scope, under any configuration (see [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes)). `REQUEST_CHANGES` is now in scope, gated behind `request_changes_threshold` (default off).
- Self-dismissing Pruefer's own `REQUEST_CHANGES` review as a fallback for repos without "dismiss stale reviews on push" enabled — documented as a future option, not implemented.
- Per-repo severity thresholds — `request_changes_threshold` is global to the daemon instance, like every other Pruefer config field.
- Risk scoring/rubric (deciding which PRs/repos need what tier of human review) — a distinct, separate concept from per-finding severity.
- Multi-line (`start_line`) inline comment ranges — single-line anchors only.
- Non-GitHub forges.
- Removing `.github/workflows/claude-review.yml` from any repo — that stays until Pruefer is proven in practice.
