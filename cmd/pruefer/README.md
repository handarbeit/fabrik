# Pruefer

Pruefer is a self-hosted PR review daemon. It watches configured GitHub repositories, reviews open pull requests by invoking the `claude` CLI (subscription-backed, not API-metered), and submits a formal comment-only `pull_request_review`.

It exists to satisfy Fabrik's `wait_for_reviews: true` gate (and any repo that wants a review bot) without depending on a hosted third-party reviewer's quota, and without per-token API billing. See [adrs/074-pruefer-v1-architecture.md](../../adrs/074-pruefer-v1-architecture.md) for the full architectural rationale.

**V1 scope**: comment-only reviews (`event: COMMENT`). Pruefer never approves a PR and never requests changes — see the ADR for why.

## How it works

Every `poll_interval_seconds`, Pruefer lists open, non-draft PRs on each watched repo and, for each one, checks:

- Is the PR authored by Pruefer's own bot identity? Skip (GitHub rejects self-review anyway).
- Does an excluded author/label/path match? Skip.
- Has Pruefer already reviewed this exact head SHA? Skip — **unless** an unprocessed `/pruefer review` comment is on the PR, which forces a fresh review of the current head.
- Is the diff larger than `max_diff_bytes`? Skip (logged, not truncated).

Otherwise, Pruefer clones the PR's head commit into a temporary directory, invokes `claude` with a read-only tool allowlist to produce review text, and submits it as a formal `pull_request_review` (event `COMMENT`) pinned to that head SHA. On any failure — clone, invocation, or submission — Pruefer posts nothing and logs the failure; the PR is naturally retried on the next poll.

Review state ("already reviewed at SHA X") is derived from GitHub itself (existing reviews authored by Pruefer's bot identity), not stored locally — a restart never causes a review storm.

## Setup

### 1. Create a GitHub App

Pruefer authenticates as a GitHub App so its reviews are attributed to a genuine bot identity (`<app-slug>[bot]`) — this is what makes "review identity distinct from PR author" structural rather than a setup mistake waiting to happen.

1. Go to your org or personal account's **Settings → Developer settings → GitHub Apps → New GitHub App**.
2. Fill in:
   - **GitHub App name**: anything unique (e.g. `your-org-pruefer`). This becomes the `<slug>` in the App's `<slug>[bot]` identity.
   - **Homepage URL**: any placeholder URL — not used.
   - **Webhook**: **uncheck "Active"**. Pruefer polls; it does not receive webhooks (see ADR-074 for why this doesn't conflict with ADR-032's webhook-delivery ruling).
3. **Repository permissions**, minimum required:
   - **Pull requests**: Read and write (review submission, reading PR metadata/diff)
   - **Contents**: Read (cloning the PR head commit)
   - **Metadata**: Read (mandatory baseline for every App)
   - **Issues**: Read (reading `/pruefer review` comments and their reactions — GitHub's Issue Comments API also covers PR-conversation comments)
4. **Where can this GitHub App be installed?**: your choice — "Only on this account" is simplest for a single org.
5. Click **Create GitHub App**. Note the **App ID** shown on the app's settings page.
6. Scroll to **Private keys** and click **Generate a private key**. This downloads a `.pem` file — save it somewhere outside version control (Pruefer's default config gitignores `.pruefer/*.pem`).
7. Click **Install App** (left sidebar) and install it on every repository you want Pruefer to watch.

### 2. Place the private key

Put the downloaded `.pem` file at `.pruefer/app-private-key.pem` (the default `github_app_private_key_path`), or point `github_app_private_key_path` at a different location. **Never** put the key's contents directly in `.env` or the YAML config — Pruefer only ever takes a file path.

### 3. Configure Pruefer

Create `.pruefer/config.yaml` (or use flags/env vars — see Configuration below):

```yaml
watched_repos:
  - your-org/repo-one
  - your-org/repo-two

github_app_id: 123456
# github_app_private_key_path: .pruefer/app-private-key.pem  # default shown
# github_app_installation_id: 0  # 0 = auto-discover (requires exactly one installation)

poll_interval_seconds: 120
model: sonnet
effort: medium
concurrency_cap: 3
max_diff_bytes: 500000

# excluded_authors: [dependabot]
# excluded_labels: [skip-review]
# excluded_paths: ["vendor/**", "*.generated.go"]
```

If the App is installed on more than one org/account, set `github_app_installation_id` explicitly — Pruefer refuses to guess among multiple installations.

### 4. Run it

```bash
go build -o pruefer ./cmd/pruefer
./pruefer
```

Pruefer loads `.env` (via the same `godotenv`-based loader Fabrik uses), reads `.pruefer/config.yaml`, bootstraps GitHub App auth, and polls until interrupted (SIGINT/SIGTERM). Pruefer also needs a working `claude` CLI installation with a valid Claude Code subscription on the host it runs on — a separate credential from the GitHub App.

A lock file at `.pruefer/pruefer.lock` prevents two instances from polling the same working directory concurrently.

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
| `--excluded-authors` | `PRUEFER_EXCLUDED_AUTHORS` | `excluded_authors` | (none) | Comma-separated logins |
| `--excluded-labels` | `PRUEFER_EXCLUDED_LABELS` | `excluded_labels` | (none) | Skip if any label matches |
| `--excluded-paths` | `PRUEFER_EXCLUDED_PATHS` | `excluded_paths` | (none) | Glob patterns; skip only if **every** touched path matches |
| `--github-app-id` | `PRUEFER_GITHUB_APP_ID` | `github_app_id` | (none — required) | |
| `--github-app-private-key-path` | `PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH` | `github_app_private_key_path` | `.pruefer/app-private-key.pem` | |
| `--github-app-installation-id` | `PRUEFER_GITHUB_APP_INSTALLATION_ID` | `github_app_installation_id` | `0` (auto-discover) | Required if the App has more than one installation |
| `--config` | `PRUEFER_CONFIG` | — | `.pruefer/config.yaml` | Path to the YAML config file itself |

Draft PRs are always skipped — there is no configuration flag to include them in V1.

## Out of scope for V1

- A TUI (follow-up issue).
- `APPROVE` / `REQUEST_CHANGES` verdicts.
- Inline line-level review comments (top-level comment body only).
- Non-GitHub forges.
- Removing `.github/workflows/claude-review.yml` from any repo — that stays until Pruefer is proven in practice.
