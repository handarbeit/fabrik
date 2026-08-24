# Pruefer

Pruefer is a self-hosted PR review daemon. It watches configured GitHub repositories, reviews open pull requests by invoking the `claude` CLI (subscription-backed, not API-metered), and submits a formal `pull_request_review` — a non-blocking comment by default, or a blocking `REQUEST_CHANGES` review when a finding's severity meets a configured threshold (see [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes) below).

It exists to satisfy Fabrik's `wait_for_reviews: true` gate (and any repo that wants a review bot) without depending on a hosted third-party reviewer's quota, and without per-token API billing. See [adrs/1113-pruefer-v1-architecture.md](../../adrs/1113-pruefer-v1-architecture.md) for the full architectural rationale and [adrs/1251-pruefer-severity-gated-request-changes.md](../../adrs/1251-pruefer-severity-gated-request-changes.md) for the `REQUEST_CHANGES` amendment.

**Pruefer never approves a PR — ever, under any configuration.** Approval is an accountability decision that must not rest on a bot rubber-stamping itself; this is permanent, not scoped to any toggle. `REQUEST_CHANGES` is opt-in (default off) and computed deterministically in Go from parsed finding severities — never from anything Claude writes in prose, so prompt injection cannot escalate or suppress it.

## How it works

Every `poll_interval_seconds`, Pruefer lists open, non-draft PRs on each watched repo and, for each one, checks:

- Is the PR authored by Pruefer's own bot identity? Skip (GitHub rejects self-review anyway).
- Does an excluded author or label match? Skip.
- Has Pruefer already reviewed this exact head SHA? Skip — **unless** an unprocessed `/pruefer review` comment is on the PR, which forces a fresh review of the current head.
- Is *every* touched path excluded by `excluded_paths`? Skip — this is the only whole-PR path exclusion; see below for the per-file case.
- Once the diff is fetched, `excluded_paths` is applied **per file**, before the diff is ever measured against `max_diff_bytes` — so a file matching an exclusion glob can never count toward the size verdict. If the diff is still over `max_diff_bytes` after exclusion, Pruefer trims further (largest files first) rather than skipping outright, and reviews the remainder. Whatever was dropped — by exclusion or by trimming — is disclosed to the reviewing model (so it doesn't silently assume it saw the whole change) and named in a PR comment if the raw diff was actually oversized. Only when nothing reviewable survives exclusion and trimming does Pruefer skip the PR, for the same reason a partial review that presents as a complete one would be worse than no review at all. See [adrs/1462-pruefer-per-file-diff-exclusion-and-trim.md](../../adrs/1462-pruefer-per-file-diff-exclusion-and-trim.md).
- Does GitHub refuse to render the diff at all (a 406 `too_large` response — the diff exceeds GitHub's own 20,000-line ceiling on the `.diff` media type, independent of `max_diff_bytes`)? This is a deterministic size verdict, not a transient error, so Pruefer does not hot-retry it every poll. Instead it falls back to the paginated changed-files API (no line-count ceiling) to reconstruct the list of touched paths, and reviews the PR normally against the local clone — with inline-comment anchoring naturally unavailable, so findings land in the review body instead of as line comments. Only if that fallback also fails does Pruefer skip the PR (same disposition as the `max_diff_bytes` skip above) and post a single PR comment explaining why, so the decision is visible to a human instead of silently repeating in the log. This path's `excluded_paths` check is still whole-PR (no diff text is ever obtained to filter per file). See [adrs/1427-pruefer-diff-too-large-degrade-not-block.md](../../adrs/1427-pruefer-diff-too-large-degrade-not-block.md).

Otherwise, Pruefer clones the PR's head commit into a temporary directory, invokes `claude` with a read-only tool allowlist to produce a prose summary plus structured findings (each classified with a severity tier), and submits it as a formal `pull_request_review` pinned to that head SHA — `event: COMMENT` by default, or `event: REQUEST_CHANGES` if `request_changes_threshold` is set and a finding meets it (see below). Findings that map to a changed line in the diff are posted as line-anchored inline comments in the same request; any finding that can't be anchored (a line the diff doesn't touch) is demoted into the summary body instead of dropping it or failing the whole review — but still counts toward the severity threshold either way. Inline comments are what let Fabrik's review-reinvoke path pick up Pruefer's findings and act on them automatically — see [adrs/1189-pruefer-inline-review-comments.md](../../adrs/1189-pruefer-inline-review-comments.md). On any failure — clone, invocation, or submission — Pruefer posts nothing and logs the failure; the PR is naturally retried on the next poll.

Review state ("already reviewed at SHA X") is derived from GitHub itself (existing reviews authored by Pruefer's bot identity), not stored locally — a restart never causes a review storm.

## Setup

### 1. Create a GitHub App

Pruefer authenticates as a GitHub App so its reviews are attributed to a genuine bot identity (`<app-slug>[bot]`) — this is what makes "review identity distinct from PR author" structural rather than a setup mistake waiting to happen.

1. Go to your org or personal account's **Settings → Developer settings → GitHub Apps → New GitHub App**.
2. Fill in:
   - **GitHub App name**: anything unique (e.g. `your-org-pruefer`). This becomes the `<slug>` in the App's `<slug>[bot]` identity.
   - **Homepage URL**: any placeholder URL — not used.
   - **Webhook**: **uncheck "Active"**. Pruefer polls by default; it does not receive webhooks (see ADR-1113 for why this doesn't conflict with ADR-032's webhook-delivery ruling). If you plan to use `event_source: hookdeck` (see [Event-driven mode (Hookdeck)](#event-driven-mode-hookdeck) below), leave this unchecked for now — you'll come back and point it at Hookdeck once a source is provisioned.
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
# excluded_paths: ["testdata/schema/**"]  # a vendored schema or generated fixture directory — filtered per file, applied before max_diff_bytes (see above)
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

## Version & self-upgrade

`pruefer --version` (or `-version`) prints the running build's version and exits: a stamped release tag (e.g. `v0.0.76`) for a binary downloaded from GitHub Releases, or `dev(<short-sha>)` for a binary built from source. This distinction is what the self-upgrade logic below uses to pick its upgrade path — Pruefer ships from the same `.goreleaser.yaml`/tag as Fabrik itself, not an independent release train (see [adrs/1197-pruefer-self-upgrade.md](../../adrs/1197-pruefer-self-upgrade.md) for the rationale).

Self-upgrade is **off by default** (`--auto-upgrade` / `PRUEFER_AUTO_UPGRADE` / `auto_upgrade: true`) — an operator opts in deliberately, mirroring Fabrik's own `-auto-upgrade` default. Given that a stale Pruefer has no board or issues to make its staleness visible (see the top of this README's motivation), **enabling `--auto-upgrade` is recommended** for any long-running deployment. When enabled, Pruefer checks for an upgrade at the poll boundary — right after a poll cycle's reviews have all completed (`Daemon.poll` joins every in-flight review before returning) and before the next poll begins — so an upgrade never interrupts an in-flight review's ephemeral clone or `claude` subprocess. The check itself is throttled to roughly every 30 minutes, independent of `poll_interval_seconds`, to bound `git fetch`/GitHub Releases API chatter.

Which upgrade path runs depends on how the binary was built:

- **Dev mode** (running version is `dev(<sha>)`): Pruefer must be run from the Fabrik source checkout, invoked such that `.pruefer/pruefer.lock` (and thus Pruefer's working directory) sits at the checkout root — the same convention Fabrik's own dev-rebuild path uses. On finding new commits on `origin/main` (or unpushed local commits matching neither), Pruefer runs `git pull --ff-only` and rebuilds itself with `go build` from `cmd/pruefer` before re-exec'ing. This is the mode you get by building and running `pruefer` directly inside a Fabrik checkout — no extra configuration needed beyond `--auto-upgrade`.
- **Release mode** (running version is a stamped tag): Pruefer checks `handarbeit/fabrik`'s GitHub Releases for a newer tag using a dedicated, unauthenticated GitHub API client (decoupled from the per-owner App installation tokens used for reviews — those aren't guaranteed to cover `handarbeit/fabrik`), downloads the matching platform archive, atomically replaces the running binary, and re-execs. This is the mode for a deployment running from a distinct directory (e.g. `~/dev/pruefer`) with a binary downloaded from a release — the actual "usage" deployment shape this feature targets.

On macOS arm64, a release-mode upgrade re-signs the replacement binary ad-hoc after download (the same step Fabrik's own upgrade path uses) so the swapped-in binary isn't rejected by Gatekeeper/AMFI.

## Event-driven mode (Hookdeck)

By default (`event_source: poll`, unset), Pruefer's behavior is exactly what's described above — poll every `poll_interval_seconds`, list open PRs, review the eligible ones. `event_source: hookdeck` is an opt-in alternative: GitHub webhooks are forwarded through [Hookdeck](https://hookdeck.com) into Pruefer, which reviews a PR within moments of `opened`/`reopened`/`synchronize`/`ready_for_review` instead of waiting up to a full poll interval. Polling is never removed in this mode — it's demoted to a low-frequency reconciliation safety net (`reconciliation.fallback_interval`, default `2m`), covering startup, a Hookdeck outage, and the moment right after reconnecting, so a missed or dropped event is never fatal.

Pruefer is the deliberate test bed for this pattern before it's ported to Fabrik's own webhook infrastructure — see [adrs/1254-event-driven-hookdeck-ingestion.md](../../adrs/1254-event-driven-hookdeck-ingestion.md).

### Provisioning a Hookdeck source

1. Sign up at [hookdeck.com](https://hookdeck.com) and create a **Connection** (a Source + Destination pair is Hookdeck's terminology, but Pruefer only needs the Source side — see "no local HTTP hop" below).
2. From your Hookdeck dashboard, copy an **API key** — this authenticates Pruefer's CLI-session connection to Hookdeck, not GitHub.
3. Set the API key in your environment under whatever variable name you configure via `hookdeck.api_key_env` (default `HOOKDECK_API_KEY`) — e.g. in `.env`:
   ```
   HOOKDECK_API_KEY=hd_...
   ```
4. Go back to your GitHub App's settings (**Settings → Developer settings → GitHub Apps → your app → General**) and:
   - Check **"Active"** under Webhook.
   - Set the **Webhook URL** to the URL Hookdeck's dashboard shows for your source (Hookdeck forwards from there to Pruefer over a persistent, replay-capable session — no public endpoint needs to run on Pruefer's own host).
   - **Content type must be `application/json`.** GitHub Apps also offer `application/x-www-form-urlencoded`, which wraps the payload as `payload=<url-encoded JSON>` instead of sending raw JSON — Pruefer's normalizer expects raw JSON and would fail to parse every single delivery if this is set wrong, permanently and silently falling back to poll-only (each failure is logged, but rate-limited to once per 30s, so it's easy to miss). This field defaults to `application/json`, so it only needs attention if something has changed it.
   - Set a **Webhook secret** — generate one yourself (e.g. `openssl rand -hex 32`), enter it here, and set the same value in your environment under whatever variable name you configure via `hookdeck.webhook_secret_env` (default `PRUEFER_GITHUB_WEBHOOK_SECRET`):
     ```
     PRUEFER_GITHUB_WEBHOOK_SECRET=<the same secret you entered in the GitHub App settings>
     ```
5. Set `event_source: hookdeck` in `.pruefer/config.yaml`:
   ```yaml
   event_source: hookdeck
   hookdeck:
     api_key_env: HOOKDECK_API_KEY
     webhook_secret_env: PRUEFER_GITHUB_WEBHOOK_SECRET
   reconciliation:
     startup: true
     fallback_interval: 2m
   ```
6. Run Pruefer as usual (`./pruefer`). It authenticates to Hookdeck's CLI-session protocol, verifies every forwarded delivery's GitHub signature, dedupes by delivery ID, and dispatches reviews the moment a relevant event arrives — falling back to poll on any Hookdeck outage, with no crash at any point (startup, mid-run, or reconnect failure).

### Why signature verification still matters

Hookdeck's own transport auth (your API key) proves a request came from *your Hookdeck source*, not from GitHub. A webhook secret and its HMAC-SHA256 signature (`X-Hub-Signature-256`) is what proves the payload's *contents* actually originated at GitHub and weren't tampered with in transit or fabricated by anyone who obtained your Hookdeck forwarding URL. Pruefer verifies this signature on every received event regardless of transport; an unverified or invalid signature is dropped before it's ever normalized or acted on.

Signature verification depends on Hookdeck forwarding the request body byte-identically to what GitHub signed — a property of Hookdeck's own wire format, not a documented guarantee (see [adrs/1254-event-driven-hookdeck-ingestion.md](../../adrs/1254-event-driven-hookdeck-ingestion.md)). If that ever changes, or if the webhook secret is wrong, **every** delivery fails signature verification — see the next section for how Pruefer surfaces that.

### Detecting a total protocol break

A dropped event is individually harmless — GitHub's at-least-once redelivery and the reconciliation-fallback poll both cover it. But if signature verification starts failing on *every* delivery (a misconfigured `hookdeck.webhook_secret_env`, or a break in Hookdeck's wire format), Pruefer degrades to poll-only silently unless you're watching for it — the ack Hookdeck receives is unaffected either way (see [ADR-1254](../../adrs/1254-event-driven-hookdeck-ingestion.md)'s accepted trade-offs), and the drop itself only used to produce a rate-limited log line.

The TUI's footer now shows a running breakdown of dropped deliveries by category — `dropped: N (sig S · unwatched U · dedupe D · other O)` — so an occasional dedupe hit or an event for a repo outside `watched_repos` (expected, benign) never looks the same as a signature failure (actionable). If signature verification fails on 20 consecutive deliveries with no success in between, Pruefer escalates: a loud warning is logged, and the footer shows a `⚠ SIGNATURE DRIFT — check webhook secret` banner until the next delivery verifies successfully. Polling keeps running throughout — nothing about this escalation disables the reconciliation fallback; it only makes the fact that you're currently relying on it more than usual explicit and visible. See [adrs/1563-hookdeck-drop-accounting-and-signature-drift-escalation.md](../../adrs/1563-hookdeck-drop-accounting-and-signature-drift-escalation.md) for the design.

If you see the drift banner: check that `hookdeck.webhook_secret_env` (and the environment variable it points at) matches the secret configured in your GitHub App's webhook settings; if it does, Hookdeck's forwarding format may have changed upstream and is worth reporting.

## Terminal UI

When run with a real terminal attached (both stdin and stdout), Pruefer launches an interactive TUI by default — the same `bubbletea`/`bubbles`/`lipgloss` stack and model/update/view structure as Fabrik's own `tui/` package, so the two feel like the same family of tool. It shows:

- Watched repositories, each with its last poll time, PR count found, and last error (if any).
- PRs currently under review, with elapsed time since the review started.
- Recently completed reviews (last 200, in-memory — not persisted across restarts): repo, PR, outcome (reviewed / skipped / errored), turns, cost, and duration.
- Skipped PRs with their reason, covering every skip category Pruefer tracks (draft, self-authored, excluded author/label/path, already reviewed at this head SHA, diff too large).
- Errors, plus GitHub REST API rate-limit state and a running session-total cost/turn count.
- In `event_source: hookdeck` mode: a per-category breakdown of dropped deliveries (signature failures, unwatched-repo/owner, dedupe hits, other), and a signature-drift banner when signature verification has been failing on every delivery for a sustained stretch — see [Detecting a total protocol break](#detecting-a-total-protocol-break) below.

Keyboard: `q` or `ctrl+c` to quit, `tab` to switch panes, `↑`/`↓` or `j`/`k` to scroll and select an entry, `enter` to view its detail.

The TUI is purely observational — it never changes which PRs get reviewed, when, or how. Running with `-notui` disables it entirely and falls back to Pruefer's existing structured `logf`-based console output, with **identical review behavior** either way. Use `-notui` (or `PRUEFER_TUI=0` / `tui: false` in the YAML config) when running under systemd, tmux with no attached TTY, or any other non-interactive environment.

## Logging

Pruefer writes every daemon log line — poll cycles, review outcomes, warnings, auth events — to a timestamped, mutex-serialized log file at `.pruefer/pruefer.log` by default, so an incident is diagnosable from the daemon's own durable record instead of requiring reconstruction from the GitHub API. Set `log_file` (or `--log-file` / `PRUEFER_LOG_FILE`) to write elsewhere, or to an empty value to disable file logging entirely.

Each line looks like:

```
2026-08-08T18:12:03Z [pr#103 warn] listing open PRs for verveguy/zusammen: context deadline exceeded — skipping this repo this cycle
```

- **With the TUI running** (the default on a real terminal), stderr output would corrupt the bubbletea display, so the log file is the sole destination. If `log_file` is disabled (or the log file can't be opened) while the TUI is running, log lines are discarded rather than written raw to stderr — the same corruption concern, just with no file to fall back to.
- **In plain daemon mode** (`-notui`, or no TTY attached), logging is additive: every line is written to both stderr and the log file, so an operator watching the terminal keeps seeing exactly what they see today. With `log_file` disabled in plain mode, output stays on stderr only, matching Pruefer's behavior before this feature existed.

The log file is append-only across restarts — it is never truncated on daemon startup, unlike Fabrik engine's own `fabrik.log` — so a restart doesn't erase the history an incident investigation needs. Growth is bounded by size-triggered rotation: once the file reaches 10 MB it's renamed to `pruefer.log.1` (existing numbered backups shift up, `pruefer.log.3` is dropped), and a fresh file is opened. Up to 3 rotated backups are retained.

A `warn`-tagged line fires when a review's summary doesn't follow the `PRUEFER_SUMMARY_BEGIN`/`PRUEFER_SUMMARY_END` delimiter contract: either the markers were missing (or malformed) entirely, or a well-formed pair was found but some preamble text ahead of the opening marker had to be discarded. Neither is fatal — the review still submits — but either is a sign the model drifted from the prompt's output contract and is worth a look.

## Config reload (SIGHUP)

Send `SIGHUP` to a running daemon (`kill -HUP <pid>`) to reload `.pruefer/config.yaml` without a restart. Pruefer re-resolves the full flag > environment variable > YAML config file > default precedence chain (the same one `LoadConfig` uses at startup — see [Configuration reference](#configuration-reference)), validates it in full, and applies whichever fields are safe to change on a live daemon. A running review is never cancelled, the Hookdeck WebSocket session (in `event_source: hookdeck` mode) is never dropped, and the dedupe ring is never reset by a reload.

> **This is not the same as Fabrik's own engine `SIGHUP`.** Fabrik's engine treats `SIGHUP` as a request to gracefully restart itself in place (drain in-flight workers, then re-exec) — it picks up new code as well as new config. Pruefer's `SIGHUP` never restarts or re-execs the process; it only ever mutates the running daemon's config in place. If you operate both daemons, don't assume the same signal means the same thing for each.

Every field is classified as either **live** (applied immediately) or **restart-only** (reported in the log, but the running daemon keeps its old value until the next restart):

| YAML key | Reload |
|---|---|
| `watched_repos` | Live. Added repos are polled starting the next cycle; removed repos stop being polled starting the next cycle. A review already in flight for a repo removed mid-reload is allowed to finish — it is never cancelled, and its owner's installation token keeps refreshing until it does, so its remaining GitHub API calls never fail with a stale token either. |
| `poll_interval_seconds` | Live, effective starting the next cycle. |
| `model` | Live. |
| `effort` | Live. |
| `concurrency_cap` | Live. Growing or shrinking the semaphore never disturbs a review already holding a slot — a shrink only reduces future admission, so concurrency can transiently run above the new cap (bounded by the old cap) until the existing holders drain. See [adrs/1640-pruefer-config-reload.md](../../adrs/1640-pruefer-config-reload.md). |
| `max_diff_bytes` | Live. |
| `max_wall_time_seconds` | Live. |
| `excluded_authors` | Live. |
| `excluded_paths` | Live. |
| `excluded_labels` | Live. |
| `request_changes_threshold` | Live. |
| `auto_upgrade` | Live, effective starting the next poll boundary. |
| `reconciliation.fallback_interval` | Live (event-driven mode only). |
| `github_app_id` | Restart-only. |
| `github_app_private_key_path` | Restart-only. |
| `github_app_installation_id` | Restart-only. |
| `tui` | Restart-only — the interactive dashboard is started once, before the daemon begins running. |
| `log_file` | Restart-only. |
| `event_source` | Restart-only — switching between `poll` and `hookdeck` means constructing or tearing down a whole event source, including its WebSocket session and dedupe ring. |
| `hookdeck.api_key_env` | Restart-only. |
| `hookdeck.webhook_secret_env` | Restart-only. |
| `reconciliation.startup` | Restart-only — consulted once, before the event-driven run loop starts. |

A restart-only field that changed is always named in the reload's log summary — never silently ignored. A malformed config file leaves the previously running config completely untouched (not partially applied) and logs the parse error; fix the file and send `SIGHUP` again.

**Adding a repo under a new owner** mints that owner's GitHub App installation token as part of the reload. If the owner has no matching installation, the *entire* reload fails with a clear error (not just that one repo) — the running config, including any other change bundled in the same edit, is left untouched. Install the app on the new owner first, then retry the reload.

Every reload — successful or not — logs a diff-style summary to `.pruefer/pruefer.log` (see [Logging](#logging) above): every repo added/removed, every changed field's old → new value, and every restart-only field that was reported but not applied.

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

Precedence, highest to lowest: **flag > environment variable > YAML config file > default.** Most of these fields can also be changed on a running daemon via `SIGHUP` — see [Config reload (SIGHUP)](#config-reload-sighup) for which ones, and what happens to the rest.

| Flag | Env var | YAML key | Default | Notes |
|---|---|---|---|---|
| `--repos` | `PRUEFER_REPOS` | `watched_repos` | (none — required) | Comma-separated `owner/repo` list |
| `--poll-interval` | `PRUEFER_POLL_INTERVAL` | `poll_interval_seconds` | `120` | Seconds |
| `--model` | `PRUEFER_MODEL` | `model` | `sonnet` | Claude model |
| `--effort` | `PRUEFER_EFFORT` | `effort` | `medium` | `low`, `medium`, `high`, or `max` |
| `--concurrency` | `PRUEFER_CONCURRENCY` | `concurrency_cap` | `3` | Max simultaneous `claude` invocations |
| `--max-diff-bytes` | `PRUEFER_MAX_DIFF_BYTES` | `max_diff_bytes` | `500000` | Compared against the diff **after** `excluded_paths` filtering; if still over, Pruefer trims further (largest files first) and reviews the remainder rather than skipping, disclosing what was dropped to both the model and a PR comment. Skips only if nothing reviewable survives. |
| `--max-wall-time` | `PRUEFER_MAX_WALL_TIME` | `max_wall_time_seconds` | `0` (no cap) | Seconds; caps a single `claude` review invocation's wall-clock duration on top of the fixed 15-minute inactivity watchdog |
| `--excluded-authors` | `PRUEFER_EXCLUDED_AUTHORS` | `excluded_authors` | (none) | Comma-separated logins |
| `--excluded-labels` | `PRUEFER_EXCLUDED_LABELS` | `excluded_labels` | (none) | Skip if any label matches |
| `--excluded-paths` | `PRUEFER_EXCLUDED_PATHS` | `excluded_paths` | (none) | Glob patterns, filtered **per file** and applied before `max_diff_bytes` is measured; a PR is skipped whole only if **every** touched path matches |
| `--request-changes-threshold` | `PRUEFER_REQUEST_CHANGES_THRESHOLD` | `request_changes_threshold` | (none — disabled) | `low`, `medium`, `high`, or `critical`; submits `REQUEST_CHANGES` when a finding's severity meets or exceeds this tier. See [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes). |
| `--github-app-id` | `PRUEFER_GITHUB_APP_ID` | `github_app_id` | (none — required) | |
| `--github-app-private-key-path` | `PRUEFER_GITHUB_APP_PRIVATE_KEY_PATH` | `github_app_private_key_path` | `.pruefer/app-private-key.pem` | |
| `--github-app-installation-id` | `PRUEFER_GITHUB_APP_INSTALLATION_ID` | `github_app_installation_id` | `0` (derive from `watched_repos`) | Legacy pin: set to force every watched repo through one specific installation, regardless of owner |
| `--config` | `PRUEFER_CONFIG` | — | `.pruefer/config.yaml` | Path to the YAML config file itself |
| `-notui` | `PRUEFER_TUI` | `tui` | `true` | Set `-notui` / `PRUEFER_TUI=0` / `tui: false` to disable the interactive TUI and fall back to console logging. The TUI is further gated on a real terminal being detected on both stdin and stdout, regardless of this setting. |
| `--log-file` | `PRUEFER_LOG_FILE` | `log_file` | `.pruefer/pruefer.log` | Path daemon log lines are written to, resolved against the process's working directory (same convention as `github_app_private_key_path`). An explicitly empty value (`--log-file ""`, `PRUEFER_LOG_FILE=`, or `log_file: ""`) disables file logging entirely. See [Logging](#logging). |
| `--auto-upgrade` | `PRUEFER_AUTO_UPGRADE` | `auto_upgrade` | `false` | Check for a newer version at the poll boundary and self-upgrade (dev-rebuild or release-download, depending on the running build — see [Version & self-upgrade](#version--self-upgrade)). Recommended for long-running deployments. |
| `--version` | — | — | — | Print the running version (a stamped release tag, or `dev(<sha>)`) and exit. |
| `--event-source` | `PRUEFER_EVENT_SOURCE` | `event_source` | `poll` | `poll` (default, unchanged behavior) or `hookdeck` (event-driven — see [Event-driven mode (Hookdeck)](#event-driven-mode-hookdeck)) |
| `--hookdeck-api-key-env` | `PRUEFER_HOOKDECK_API_KEY_ENV` | `hookdeck.api_key_env` | `HOOKDECK_API_KEY` | Name of the env var holding the Hookdeck API key. Only consulted when `event_source: hookdeck` |
| `--hookdeck-webhook-secret-env` | `PRUEFER_HOOKDECK_WEBHOOK_SECRET_ENV` | `hookdeck.webhook_secret_env` | `PRUEFER_GITHUB_WEBHOOK_SECRET` | Name of the env var holding the GitHub App's webhook secret. Only consulted when `event_source: hookdeck` |
| `--reconciliation-startup` | `PRUEFER_RECONCILIATION_STARTUP` | `reconciliation.startup` | `true` | Run a full poll reconciliation pass at startup before event delivery begins. Only consulted when `event_source: hookdeck` |
| `--reconciliation-fallback-interval` | `PRUEFER_RECONCILIATION_FALLBACK_INTERVAL` | `reconciliation.fallback_interval` | `2m` | Go duration (e.g. `2m`, `90s`); the low-frequency poll interval used as a safety net in event-driven mode. Separate from `poll_interval_seconds`, which only applies in poll-only mode. Only consulted when `event_source: hookdeck` |

Draft PRs are always skipped — there is no configuration flag to include them in V1.

## Out of scope

- `APPROVE` verdicts — permanently out of scope, under any configuration (see [Severity-gated REQUEST_CHANGES](#severity-gated-request_changes)). `REQUEST_CHANGES` is now in scope, gated behind `request_changes_threshold` (default off).
- Self-dismissing Pruefer's own `REQUEST_CHANGES` review as a fallback for repos without "dismiss stale reviews on push" enabled — documented as a future option, not implemented.
- Per-repo severity thresholds — `request_changes_threshold` is global to the daemon instance, like every other Pruefer config field.
- Risk scoring/rubric (deciding which PRs/repos need what tier of human review) — a distinct, separate concept from per-finding severity.
- Multi-line (`start_line`) inline comment ranges — single-line anchors only.
- Non-GitHub forges.
- ~~Removing `.github/workflows/claude-review.yml` from any repo — that stays until Pruefer is proven in practice.~~ **Done, 2026-08-13.** Pruefer is proven in practice and the workflow is deleted from `handarbeit/fabrik`, `fabrik-test-alpha`, and `fabrik-test-beta`. It was the middle step of Gemini (flaky) → Claude bot → Pruefer, and it outlived the handover by weeks: on the two test repos it was still reviewing every PR, while Pruefer's `watched_repos` did not include them at all. The bar this line set — *proven in practice* — was met by adding the repos to `watched_repos` and then observing a real Pruefer review land (`fabrik-test-alpha#4731`, 2026-08-13T12:56:11Z) **before** deleting anything. Removing it also removed the last consumer of the `ANTHROPIC_API_KEY` secret in all three repos: Pruefer runs as a local daemon under the operator's logged-in profile (`CLAUDE_CONFIG_DIR`), so PR review is no longer billed against a metered API key.
