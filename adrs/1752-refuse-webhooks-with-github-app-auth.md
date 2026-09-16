# ADR 1752: Refuse `--webhooks` + GitHub App Auth; Correct the Permission Key

**Date**: 2026-09-16
**Status**: Accepted
**Issue**: #1752 — App auth is incompatible with `--webhooks` (silent no-op), and the webhooks permission key is wrong

## Context

#1713 added GitHub App installation auth as a co-equal alternative to PAT auth. It shipped two defects around `--webhooks`:

1. `--webhooks` delivers events via `gh webhook forward` (`engine/webhook.go`'s `webhookManager`), which is feature-gated to user tokens by the GitHub CLI itself. An installation token gets `Error: you do not have access to this feature` — measured directly against a real App installation, same repo, same command, only the credential differing. No App permission grant fixes this; it is not a permission problem, it is a CLI-side auth-type gate. Nothing refused this combination, so `--webhooks` + App auth silently degraded to `--reconcile-interval` polling with no event stream at all and no indication anything was wrong.
2. `RequiredGitHubAppPermissions(webhooksEnabled)` requested `perms["webhooks"] = "write"` when webhooks were enabled. `webhooks` is not a real GitHub App permission key — GitHub's actual key for repo-level webhook management is `repository_hooks` (`organization_hooks` for org-level). Had the combination above ever been reached, the startup grant check would have compared a live installation's permissions against a key that can never appear in that response, producing an unsatisfiable shortfall no operator could resolve by granting anything.

`RefuseGHESWithGitHubApp` (`engine/github_app_auth.go`) is the established precedent for refusing an incompatible App-auth config combination loudly at startup rather than attempting it and failing (or silently degrading) later.

## Decisions

### 1. New `RefuseWebhooksWithGitHubApp`, structurally identical to `RefuseGHESWithGitHubApp`

`RefuseWebhooksWithGitHubApp(webhooksEnabled bool) error` takes a plain `bool`, not a `Config`, matching `RefuseGHESWithGitHubApp(ghesHost string)`'s convention exactly: `nil` for the compatible case (`false`), otherwise an error naming both `--webhooks`/`FABRIK_WEBHOOKS` and GitHub App auth, explaining `gh webhook forward`'s feature-gating to user tokens, and stating both ways out (drop `--webhooks` to use App auth with polling, or drop the App config to use `--webhooks` with a PAT). Wired into `resolveGitHubAppAuth` immediately after the existing `RefuseGHESWithGitHubApp(cfg.GHESHost)` call — both are pure, no-network config-shape refusals with no shared state, so their relative order doesn't affect correctness, only which message an operator sees first if both are misconfigured simultaneously.

### 2. Correct the permission key to `repository_hooks`

`RequiredGitHubAppPermissions` now requests `perms["repository_hooks"] = "write"` instead of `perms["webhooks"] = "write"`. Verification evidence, in two stages: first, five real, recorded GitHub App `permissions` objects in `github/testdata/recordings/fetch_check_runs.json` (a scrubbed recording from `handarbeit/fabrik#1505`) each carry `"repository_hooks": "write"` as a genuine App's own granted-permissions payload — real, recorded, non-documentation evidence in the same permission-key namespace, but not literally `GET /app/installations/{id}` against this repo's own `fabrik` App installation per #770's exact methodology, since no such installation was reachable during implementation. Second, during Validate review (2026-09-16), a live `GET /orgs/handarbeit/installations` response for the `claude` App's installation on `handarbeit` was measured directly and carries `"repository_hooks": "write"` in its granted `perms`:

```json
{"app": "claude", "perms": {
  "actions": "write", "checks": "write", "contents": "write",
  "discussions": "write", "issues": "write", "members": "read",
  "metadata": "read", "pull_requests": "write",
  "repository_hooks": "write", "statuses": "read", "workflows": "write"
}}
```

This is #770's exact methodology — a live installation's granted-permissions payload — just via a different App (`claude`, not `fabrik`) on the same org, since no `fabrik` App installation was reachable either time. `repository_hooks` is therefore confirmed, not merely corroborated.

### 3. Keep the `webhooksEnabled` parameter and branch on `RequiredGitHubAppPermissions`

Even though Decisions 1 and 5 together mean neither caller's CLI-facing entry point (`resolveGitHubAppAuth` at engine startup, or `runInit` at setup time) can ever again reach `RequiredGitHubAppPermissions(true)` — both refusals fire first — the parameter is not removed. `RequiredGitHubAppPermissions` is a general-purpose, hand-maintained correspondence shared by two callers (the engine and `cmd/init_github_app.go`'s `runGitHubAppSetup`), and removing the parameter would be scope creep beyond what R1/R4 ask for. The key still needs to be correct in principle for both callers and for any future one — `runGitHubAppSetup` itself still accepts and acts on `Webhooks: true` when called directly (as its own unit test does), even though neither shipped CLI path can reach it with that value anymore.

### 4. Document the resulting dead-code relationship in `DeleteForwardingHooks`, not by removing anything

`DeleteForwardingHooks` (`github/hooks.go`) exists solely to tidy up the per-repo hook `gh webhook forward` leaves behind; its only call site is `engine/poll.go`'s `cleanupFn`, gated on `e.cfg.Webhooks`, independent of auth mode. Once Decision 1 lands, App auth and `--webhooks` can never be configured together at the engine's own startup, so this call site is structurally unreachable under App auth — but it remains correct and reachable under PAT mode, the only mode it now actually runs in. This is documented in the function's own doc comment (per the issue's explicit request) rather than left implicit or "fixed" with a functional code change — no functional change is warranted; PAT mode still needs this function to work exactly as before.

### 5. `cmd/init.go`'s `--github-app --webhooks` setup flag is also refused, mirroring the GHES precedent

`fabrik init --github-app --webhooks` is a second, separate `--webhooks`-shaped flag (`githubAppSetupOptions.Webhooks`) from the engine's own runtime `--webhooks`. This issue originally scoped that combination as an out-of-scope gap — the engine's own refusal (Decision 1) would still make it a non-silent, if confusing, two-step discovery rather than D1's original silent no-op. A bot review flagged the asymmetry against `cmd/init.go`'s existing `RefuseGHESWithGitHubApp(ghesHost)` call at its `--github-app --ghes-host` refusal point (line ~498): that call already guards against exactly this "setup succeeds, engine refuses on next startup" shape for GHES, but no equivalent guard existed for `--webhooks`. `cmd/init.go` now calls `engine.RefuseWebhooksWithGitHubApp(*webhooksFlag)` immediately after the GHES check, in the same `if *githubApp` block, before any network call — `fabrik init --github-app --webhooks` is refused up front instead of writing a config the engine can never start with. The `--webhooks` flag's help text is updated to state the refusal directly rather than implying the combination works. `TestRunInit_GitHubApp_RefusesWebhooks` (`cmd/init_github_app_test.go`) mirrors `TestRunInit_GitHubApp_RefusesGHESHost` exactly. `TestRunGitHubAppSetup_Webhooks_ExpandsRequiredPermissions` still exercises `runGitHubAppSetup` (and hence `RequiredGitHubAppPermissions`'s `true` branch) directly, bypassing `cmd/init.go`'s flag-parsing layer entirely — this is deliberate, unit-level coverage of `runGitHubAppSetup` itself, not evidence the CLI path is reachable with `Webhooks: true` (it no longer is).

### 6. Documentation corrected, not supplemented

`docs/USER_GUIDE.md`'s "Setting up App auth" `--webhooks` callout and "Startup permission verification" permission list both described combining App auth with `--webhooks` as supported. Both are corrected in place (not left as-is with a caveat appended) to state the combination is refused, matching the "Not combinable with GHES" precedent already in the Known limitations section with a mirrored "Not combinable with `--webhooks`" bullet, and a fourth bullet added to the GitHub App Authentication Startup Failures troubleshooting section.

## Consequences

- `--webhooks` + GitHub App auth now fails fast and loud at startup with an actionable message, instead of silently running in poll-only mode with no indication the requested feature never activated.
- `RequiredGitHubAppPermissions`'s webhook-related key is now spelled correctly for any caller that does reach the `true` branch (currently only `runGitHubAppSetup`'s own unit-level test coverage — no shipped CLI path reaches it with `Webhooks: true` anymore).
- Real App-owned webhook delivery (one URL/secret on the App itself, covering every granted repo, via a public relay) remains the way to actually *solve* rather than *refuse* this incompatibility — Pruefer's `event_source: hookdeck` mode (ADR-1254, ADR-1563) is a working precedent for that shape in a different binary/module, and adopting it for the engine's own webhook path is explicitly out of scope here. This ADR's refusal must remain in place until that (or an equivalent) replacement actually works for the engine.
- Both `--webhooks` + GitHub-App-auth entry points — the engine's own startup (`resolveGitHubAppAuth`, Decision 1) and `fabrik init --github-app --webhooks` (`cmd/init.go`, Decision 5) — now refuse the combination loudly, before any network call. Neither can silently produce a config the other side can't run.

## Prior Art

- **ADR-1713** (`1713-engine-github-app-auth.md`) — the originating ADR for App auth in the engine. Decision 8 is the direct precedent this ADR's Decision 1 mirrors (GHES refusal: "loud config-time refusal, not a client that fails obscurely later"). Decision 10 recorded the now-corrected `webhooks:write` permission entry; this ADR supersedes that specific entry rather than editing ADR-1713's historical decision text in place, per this repo's ADR-numbering convention (new ADRs are numbered after their originating issue, not retroactive edits to prior ADRs).
- **ADR-1709** (`1709-github-app-grant-verification.md`) — origin of the `VerifyGrants`/`checkGrantedPermissions` machinery that actually compares `RequiredGitHubAppPermissions`'s output against a live installation; Decision 2 here flows through that machinery unmodified.
- **#770** — established the "read a live installation's granted-permissions response, don't trust documentation or a plausible-looking name" methodology this ADR's Decision 2 follows as closely as available access allowed.
- **ADR-1254** (`1254-event-driven-hookdeck-ingestion.md`) / **ADR-1563** (`1563-hookdeck-drop-accounting-and-signature-drift-escalation.md`) — Pruefer's `event_source: hookdeck` mode, the existing precedent for the "actually solve D1" shape referenced in Consequences, explicitly out of scope for this issue.
