# ADR 1113: Pruefer V1 Architecture

**Date**: 2026-07-27
**Status**: Accepted
**Issue**: #1113 — V1 core: self-hosted PR review daemon driving the `claude` CLI

## Context

Fabrik's `wait_for_reviews: true` gate needs a reviewer that submits a formal `pull_request_review`. Depending on a hosted bot (Gemini) took the pipeline down on 2026-07-25 when its daily quota exhausted (#1058/#1059/#1065, analysed in #1071). The interim mitigation, `claude-review.yml` (a per-repo GitHub Actions workflow using `anthropics/claude-code-action`), works but bills per-token against an API key and multiplies badly under Fabrik's auto-fix-and-push review cycles (18 reviews on PR #1078, 19 on #1091), and is duplicated across eight repos.

Pruefer is a small, self-hosted Go daemon — architecturally a sibling of Fabrik's engine, not a subordinate — that watches configured repositories, reviews pull requests by invoking the `claude` CLI (subscription-backed, not API-metered), and submits a formal comment-only review. This ADR records the structural decisions made to satisfy the issue's requirements, most of which were pinned down during Research/Plan and are recorded here as-built.

## Decisions

### 1. GitHub App identity, not a distinct PAT

Pruefer authenticates as a GitHub App rather than a second personal-access-token account. Reviews and comments are attributed to `<app-slug>[bot]` — a genuine GitHub Bot identity. This makes the "review identity distinct from PR author" requirement **structural**: a GitHub App can never literally be a PR's human or bot author, eliminating the "misconfigured PAT accidentally equals PR author" failure mode that a token-based design leaves to operator discipline.

Mechanics (`github/app.go`, `pruefer/auth.go`):
- `BuildAppJWT` hand-rolls a short-lived (9-minute) RS256 JWT (`iss=<app_id>`), signed with the App's RSA private key. No JWT library — GitHub's App-auth flow needs exactly one fixed-shape token, which stdlib `crypto/rsa` + `encoding/json` + `encoding/base64` covers directly, consistent with this module's "minimize external dependencies" convention (`.claude/rules/golang.md`).
- `FetchAppInstallations` (`GET /app/installations`, JWT-authenticated) discovers every installation of the App. If `github_app_installation_id` is unset in config and exactly one installation exists, it's used automatically; zero or multiple installations is a hard bootstrap error requiring explicit configuration — silently picking one among several could watch/act on the wrong org. Auto-discovery still removes per-repo config churn within a single installation's repo set, directly serving the issue's "adding a repo shouldn't mean N PRs" motivation.
- `MintInstallationToken` exchanges the JWT for a ~1-hour installation access token, used as an ordinary Bearer token for all subsequent REST calls.
- `FetchAppSlug` (`GET /app`) resolves the App's own login (`<slug>[bot]`) once at bootstrap, used for self-review skip and GitHub-derived review-state checks.
- `github.Client.SetToken` (mutex-guarded, mirroring the existing `SetMergeStrategy` pattern) lets `pruefer.Auth.RunRefreshLoop` swap the token in place before expiry (5-minute margin) without reconstructing the client. A failed refresh is logged and retried after a fixed delay rather than crashing the daemon — the current token stays valid until its actual (not margin-adjusted) expiry, so a transient GitHub outage doesn't take Pruefer down.

The App's private key never goes in `.env`: config takes a file path (`github_app_private_key_path`, default `.pruefer/app-private-key.pem`, gitignored) rather than an embedded PEM blob, sidestepping the multi-line-env-value encoding question entirely.

### 2. Review-state mechanism: GitHub-derived, not on-disk

"Already reviewed at SHA X" is derived by querying `FetchPRReviews` (extended with a `CommitID` field, populated from GitHub's REST `commit_id`) filtered to Pruefer's own App login, rather than a local database or file.

This is self-healing and restart-safe by construction — the acceptance criterion "review state survives a restart" is satisfied with zero persistence code, and there is no local-state-vs-GitHub-truth divergence to reconcile. The incremental cost is one `FetchPRReviews` call per PR per poll cycle, which the daemon already pays regardless (it needs the PR's review list for the eligibility check itself).

### 3. Ephemeral shallow clone, not zero-clone diff-only

Each review does a single-use `git init` + `git fetch --depth 1 <url> refs/pull/<N>/head` + `git checkout FETCH_HEAD` into a fresh temp directory (`pruefer/worktree.go`), deleted after the review completes. The clone URL embeds the installation token as HTTP basic auth (`x-access-token:<token>@github.com/...`, GitHub's documented convention for App tokens); `runGit` redacts the token from any error text before it can reach logs.

The issue's own example allowlist (`Read`, `Grep`, `Glob`, `Bash(git:*)`) presumes a checkout, and real review quality benefits from surrounding-code context, not just an isolated diff hunk. No bare-clone cache: Fabrik's worktrees persist for days across many stage runs on the same issue, but Pruefer's reviews are single-shot, so a cache would add lifecycle-management complexity for comparatively little benefit at V1 scale — flagged as a future optimization if watched-repo PR volume grows.

`FetchPRDiff` (`GET /pulls/{n}` with `Accept: application/vnd.github.v3.diff`) is used separately, purely to measure diff size and parse changed paths cheaply before paying for a clone — see Decision 4.

### 4. Diff-size guard runs before cloning, and skips rather than truncates

`ReviewPR` (`pruefer/review.go`) fetches the diff via `FetchPRDiff` and compares its byte length against `Config.MaxDiffBytes` (default 500 KB) before doing any clone or Claude invocation. An oversized diff is **skipped**, not truncated — the PR stays outstanding for `wait_for_reviews` and the skip is logged, rather than silently feeding a partial diff into the prompt and producing a review that looks complete but wasn't. The same diff fetch is reused to parse changed paths (`ParseChangedPaths`, from `diff --git a/... b/...` headers) for path-exclusion matching, so the size guard and path-exclusion check share one network call.

Cheap checks (draft, self-authored, excluded author/label, already-reviewed-at-SHA) run in `select.go`'s `Eligible` *before* the diff fetch, so a PR skipped on those grounds never costs the extra round-trip.

### 5. Claude never calls `gh` or submits the review itself

Despite the issue's illustrative allowlist example including `Bash(gh:*)`, the `gh` CLI is not read-only — `gh pr review --approve`, `gh pr merge`, `gh pr edit` are all reachable through an unscoped grant. Allowing it would make the "never `APPROVE`/`REQUEST_CHANGES`" and "never mutate the repo" guarantees prompt-level policy only — the same structural weakness `claude-review.yml` already has.

Instead: Pruefer's Go code (`github.Client.SubmitPRReview`, `pruefer/review.go`) submits the review directly, with `event: "COMMENT"` hardcoded and never a caller-supplied parameter. Claude's tool allowlist (`pruefer/claude.go`'s `reviewAllowedTools`) is narrowed to `Read`, `Grep`, `Glob`, and specific read-only git subcommands (`Bash(git diff:*)`, `Bash(git log:*)`, `Bash(git show:*)`, `Bash(git blame:*)`, `Bash(git grep:*)`, `Bash(git status:*)`) rather than blanket `Bash(git:*)` or any `gh` access at all. The App installation token is never placed in the Claude subprocess's environment — Claude has no GitHub-calling capability, so there is nothing to leak even if the allowlist were somehow bypassed. This makes "never approve" a code-level invariant, not a prompt-level one.

### 6. Process-group kill escalation is duplicated, not extracted

`engine/procattr_unix.go` / `_windows.go` implement `setCmdProcAttr` (start the process in its own process group) and `killProcGroupGraceful` (SIGINT→SIGTERM→SIGKILL escalation) — exactly what Pruefer's own Claude invocation needs, but both are unexported `package engine` functions, and the issue explicitly forbids importing `engine`.

`pruefer/procattr_unix.go` / `_windows.go` duplicate this logic (~80 lines) rather than extracting it into a new shared `internal/` package. These are small, stable OS primitives; extracting them would touch `engine`'s existing call sites for a security-relevant piece of code for the sake of a one-time copy. Duplication is the lower-risk choice for V1.

### 7. Defaults

| Setting | Default | Rationale |
|---|---|---|
| Concurrency cap | 3 | Fabrik's default is 5, scoped to one board's throughput. Pruefer's reviews are lower-urgency and share one subscription-backed `claude` capacity across N watched repos, not one board — a lower cap leaves headroom. |
| Diff-size guard | 500 KB | Large enough for real PRs, small enough to bound prompt size and review latency. Skip, not truncate (Decision 4). |
| Poll interval | 120s | No webhook integration in V1; REST cost scales with watched-repo count. 120s balances on-demand `/pruefer review` responsiveness against API budget (vs. Fabrik's 30s, which assumes webhook-backed idle detection). |
| Effort | medium | The issue explicitly frames max-effort-by-default as a cost decision, not a default. Medium is a defensible middle ground for routine review, raisable per-repo via config. |
| Default exclusions | empty | Reviewing everything by default is safer than silently skipping dependency-bot PRs, which can carry real risk. Operators opt in per-repo to excluded authors/paths/labels. |

### 8. Why V1 never approves or requests changes

`SubmitPRReview` hardcodes `event: "COMMENT"`; there is no code path that can produce `APPROVE` or `REQUEST_CHANGES`. An automated approval is a policy decision this issue does not make, and `REQUEST_CHANGES` blocks merges and would need an explicit unblock path that doesn't exist yet. A comment-only review is sufficient to satisfy Fabrik's `hasReviews` gate predicate (`engine/reviews.go:271-275`) without taking on either of those additional product decisions.

### 9. On invocation failure, post nothing

If the `claude` invocation fails (process error, timeout, unparseable output, or an `is_error` result), `ReviewPR` returns a non-nil `Err` and submits no review (`pruefer/review.go`). No review-state is written on failure — since review state is GitHub-derived (Decision 2), the PR is naturally retried on the next poll cycle with no stale-state cleanup required. This is the opposite of `claude-review.yml`'s safety-net stub pattern (posting a placeholder comment when the review step fails): Pruefer owns its own retry loop, so a stalled gate is no longer a risk that a stub comment needs to hedge against, and a stub would misrepresent "no review happened" as "a review happened."

## Consequences

**Positive:**
- The distinct-identity requirement is structural (App auth), not operator-discipline-dependent.
- Zero persistence code for review state — restart-safety comes for free from deriving state off GitHub.
- The never-approve guarantee is enforced in Go, not by prompt instruction — a compromised or confused Claude invocation cannot escalate to `APPROVE`/`REQUEST_CHANGES` or call `gh` at all.
- No shared Go imports with `engine` — Pruefer can evolve independently and carries no risk of accidentally coupling to board/stage state.

**Negative / Trade-offs:**
- Installation-token refresh is a new liveness dependency absent from a plain-PAT design: an unrefreshed token silently starts failing every call ~1 hour after mint if the refresh loop breaks. Mitigated by a dedicated refresh-loop test asserting refresh fires before expiry, but this is a genuinely new failure class worth operator awareness (see README).
- Duplicated process-group kill logic (`engine/procattr_*.go` vs. `pruefer/procattr_*.go`) is two copies of security-relevant code to keep in sync if either changes.
- No bare-clone cache means every review re-fetches repo data from scratch; acceptable at V1 scale, a candidate optimization if watched-repo PR volume grows materially.
- Empty default exclusions could produce noisy reviews on high-volume dependency-bot repos until an operator configures exclusions.

## Related Work

- `adrs/005-claude-cli-invocation.md` — original rationale for shelling out to `claude` via `os/exec`; Pruefer's invocation approach is fully consistent, no conflict.
- `adrs/066-consolidated-github-request-helpers.md` — the `github/` package's REST core (`do()`/`doWithAccept()`, `restPostWithResponse`) that every new Pruefer-driven `github/` method routes through rather than hand-rolling HTTP.
- `adrs/032-webhook-event-delivery.md` — ruled out GitHub Apps for webhook *delivery* on a local-CLI/no-public-URL basis. Does not conflict with this ADR: Pruefer uses a GitHub App purely for API authentication/identity, and polls (mirroring `engine/poll.go`'s shape) rather than receiving webhooks, so ADR-032's objection doesn't apply here.
- `engine/reviews.go:271-275` (`hasReviews`) — the gate predicate a Pruefer-submitted `COMMENT` review satisfies.
- `.github/workflows/claude-review.yml` — the existing per-repo mitigation Pruefer is not replacing in V1 (removal is explicitly out of scope for this issue); both the pattern to mirror (formal-review submission) and the pattern to avoid (safety-net stub, `synchronize`-triggered re-review loops) it demonstrates.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
