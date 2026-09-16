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

`RequiredGitHubAppPermissions` now requests `perms["repository_hooks"] = "write"` instead of `perms["webhooks"] = "write"`. Verification evidence: five real, recorded GitHub App `permissions` objects in `github/testdata/recordings/fetch_check_runs.json` (a scrubbed recording from `handarbeit/fabrik#1505`) each carry `"repository_hooks": "write"` as a genuine App's own granted-permissions payload. This is not literally `GET /app/installations/{id}` against this repo's own `fabrik` App installation per #770's exact methodology — no such installation was reachable during implementation — but it is real, recorded, non-documentation evidence in the same permission-key namespace both endpoints draw from, and is treated as sufficient corroboration rather than a blocking dependency.

### 3. Keep the `webhooksEnabled` parameter and branch on `RequiredGitHubAppPermissions`

Even though Decision 1 means the engine's own runtime path (`resolveGitHubAppAuth` → `setUpGitHubAppAuth` → `RequiredGitHubAppPermissions(cfg.Webhooks)`) can never again call this with `true` — the refusal fires first — the parameter is not removed. `RequiredGitHubAppPermissions` is a general-purpose, hand-maintained correspondence shared by two callers: the engine and `cmd/init_github_app.go`'s independent `--webhooks` setup flag (`fabrik init --github-app --webhooks`), which still exercises the `true` branch today and is out of scope for this issue (Decision 5). The key still needs to be correct in principle for that caller and for any future one.

### 4. Document the resulting dead-code relationship in `DeleteForwardingHooks`, not by removing anything

`DeleteForwardingHooks` (`github/hooks.go`) exists solely to tidy up the per-repo hook `gh webhook forward` leaves behind; its only call site is `engine/poll.go`'s `cleanupFn`, gated on `e.cfg.Webhooks`, independent of auth mode. Once Decision 1 lands, App auth and `--webhooks` can never be configured together at the engine's own startup, so this call site is structurally unreachable under App auth — but it remains correct and reachable under PAT mode, the only mode it now actually runs in. This is documented in the function's own doc comment (per the issue's explicit request) rather than left implicit or "fixed" with a functional code change — no functional change is warranted; PAT mode still needs this function to work exactly as before.

### 5. `cmd/init_github_app.go`'s own `--webhooks` setup flag is left unrefused, deliberately out of scope

`fabrik init --github-app --webhooks` is a second, separate `--webhooks`-shaped flag (`githubAppSetupOptions.Webhooks`) from the engine's own runtime `--webhooks`, and is not gated by Decision 1's refusal (that refusal lives in `engine/github_app_auth.go`'s `resolveGitHubAppAuth`, which `cmd/init_github_app.go` does not call). A successful `fabrik init --github-app --webhooks` run can still write a `github_app_*` + `webhooks: true` config that the engine then refuses to start with on its very next invocation. This is a real but non-silent gap — the engine's own refusal names both flags and both ways out the moment the engine actually starts, so the failure mode is "confusing two-step discovery," not D1's original "silent no-op." This issue's explicit scope names only `engine/github_app_auth.go`, its wiring, the `DeleteForwardingHooks` audit, docs, and tests — not `cmd/init.go`/`cmd/init_github_app.go`. Closing this gap (mirroring this ADR's Decision 1 at `cmd/init.go`'s existing `--github-app --ghes-host` refusal point) is a natural, low-risk follow-up issue, not folded in here.

### 6. Documentation corrected, not supplemented

`docs/USER_GUIDE.md`'s "Setting up App auth" `--webhooks` callout and "Startup permission verification" permission list both described combining App auth with `--webhooks` as supported. Both are corrected in place (not left as-is with a caveat appended) to state the combination is refused, matching the "Not combinable with GHES" precedent already in the Known limitations section with a mirrored "Not combinable with `--webhooks`" bullet, and a fourth bullet added to the GitHub App Authentication Startup Failures troubleshooting section.

## Consequences

- `--webhooks` + GitHub App auth now fails fast and loud at startup with an actionable message, instead of silently running in poll-only mode with no indication the requested feature never activated.
- `RequiredGitHubAppPermissions`'s webhook-related key is now spelled correctly for any caller that does reach the `true` branch (currently only `cmd/init_github_app.go`'s setup flow).
- Real App-owned webhook delivery (one URL/secret on the App itself, covering every granted repo, via a public relay) remains the way to actually *solve* rather than *refuse* this incompatibility — Pruefer's `event_source: hookdeck` mode (ADR-1254, ADR-1563) is a working precedent for that shape in a different binary/module, and adopting it for the engine's own webhook path is explicitly out of scope here. This ADR's refusal must remain in place until that (or an equivalent) replacement actually works for the engine.
- `cmd/init_github_app.go --webhooks` staying unrefused (Decision 5) is a known, accepted gap pending a follow-up issue.

## Prior Art

- **ADR-1713** (`1713-engine-github-app-auth.md`) — the originating ADR for App auth in the engine. Decision 8 is the direct precedent this ADR's Decision 1 mirrors (GHES refusal: "loud config-time refusal, not a client that fails obscurely later"). Decision 10 recorded the now-corrected `webhooks:write` permission entry; this ADR supersedes that specific entry rather than editing ADR-1713's historical decision text in place, per this repo's ADR-numbering convention (new ADRs are numbered after their originating issue, not retroactive edits to prior ADRs).
- **ADR-1709** (`1709-github-app-grant-verification.md`) — origin of the `VerifyGrants`/`checkGrantedPermissions` machinery that actually compares `RequiredGitHubAppPermissions`'s output against a live installation; Decision 2 here flows through that machinery unmodified.
- **#770** — established the "read a live installation's granted-permissions response, don't trust documentation or a plausible-looking name" methodology this ADR's Decision 2 follows as closely as available access allowed.
- **ADR-1254** (`1254-event-driven-hookdeck-ingestion.md`) / **ADR-1563** (`1563-hookdeck-drop-accounting-and-signature-drift-escalation.md`) — Pruefer's `event_source: hookdeck` mode, the existing precedent for the "actually solve D1" shape referenced in Consequences, explicitly out of scope for this issue.
