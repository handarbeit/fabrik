# ADR 1253: Per-User GitHub App Auth Reconciler (Manifest Flow, No Shared Key)

**Date**: 2026-07-29
**Status**: Accepted
**Issue**: #1253 — self-hostable GitHub App auth with no shared credential

## Context

ADR-1113 §1 established Pruefer's identity model: authenticate as **one shared GitHub App** (`handarbeit-pruefer`, id 4408765), set to "Any account" (public) so it can be installed on multiple orgs, with the operator holding the App's private key and manually installing it via `cmd/pruefer/README.md`'s Setup steps. #1233 (ADR-1233) extended this with owner-derived multi-installation routing (`AuthSet`/`BootstrapMulti`), so one daemon can cover every account the App is installed on.

That model works for its original operator but does not distribute: shipping Pruefer to a second user means either handing them the App's private key — a credential that authenticates *as the App* for every installation everywhere it's ever installed — or walking them through hand-registering their own App and manually copying an App ID and PEM path into config. The shared-App model also means anyone can attempt to install the public App; only key secrecy protects it from being used to impersonate Pruefer's review identity elsewhere.

This ADR amends ADR-1113 §1: it supersedes "single shared App, pin `installation_id` / any-account" as the **default** model with "per-user manifest-created App + an idempotent reconciler," while keeping the shared/pinned-installation model fully supported as a compat mode — the existing `handarbeit-pruefer` deployment (or any operator with valid `github_app_id` + `app-private-key.pem` + `watched_repos`) is required to see zero behavior change. ADR-1113 §2–§9 (review-state derivation, comment-only reviews, invocation-failure handling) are unaffected.

## Decisions

### 1. `internal/githubauth` owns the entire auth lifecycle, independent of `pruefer`

A new package, `internal/githubauth`, absorbs `pruefer/auth.go`'s `Auth`/`AuthSet`/`BootstrapMulti`/`checkSelectedRepos` machinery (moved, not duplicated — `pruefer/auth.go` is deleted) and adds manifest bootstrap, credential storage, installation discovery/guidance, and a 9-step reconciliation state machine on top. This mirrors the `internal/selfupgrade` precedent (ADR-1196): the package must never import `pruefer`; callers supply their own paths (private key path, app-state path), watched-repo list, and a log function via `githubauth.Options`, and the package hardcodes nothing project-specific. This keeps the door open for a future second self-hosted daemon to reuse the same reconciler without inheriting Pruefer's own concerns.

The rest of Pruefer depends only on the narrow `GitHubAuth` interface:

```go
type GitHubAuth interface {
    ClientForRepo(ctx context.Context, owner, repo string) (*github.Client, error)
    BotLogin() string
}
```

`pruefer/execute.go` becomes the sole call site: it calls `githubauth.Reconcile(ctx, opts)` in place of `BootstrapMulti`, then calls `ClientForRepo` once per distinct watched-repo owner at startup to build exactly the same `map[string]GitHubLister` shape `BootstrapMulti` used to produce. `Daemon.Clients` and `daemon.go`'s poll loop are untouched — `ClientForRepo` satisfies the issue's literal interface requirement without threading live reconciliation into the poll cycle or rewriting `daemon_test.go`'s existing `Daemon{Clients: ...}` call sites, for a live-reload capability Pruefer doesn't have today (see Decision 5).

### 2. Manifest flow: loopback callback, browser-driven, no Pruefer-hosted backend

First-run bootstrap (`RunManifestFlow`, `internal/githubauth/bootstrap.go`) starts a temporary loopback HTTP server (`127.0.0.1:0`, OS-assigned port), builds a GitHub App manifest scoped to exactly Pruefer's permissions (see Decision 3), and serves an auto-submitting HTML form at `/start` that POSTs the manifest to `https://github.com/settings/apps/new?state=<random>`. GitHub redirects the browser back to the loopback server's `/callback` with a one-time code; the CSRF `state` GitHub echoes back is checked as a **hard requirement**, not best-effort — a state mismatch is treated as a possible CSRF attempt and rejected, since the loopback listener is new local attack surface (anyone on the same host could otherwise complete the flow with their own code). The code is exchanged via the unauthenticated, single-use `POST /app-manifests/{code}/conversions` for the created App's ID, PEM, webhook secret, client ID/secret, and slug.

`openBrowser` (`internal/githubauth/browser.go`) is a hand-rolled per-OS `exec.Command` (no new dependency, matching this module's dependency-minimization convention), exposed as a package var so tests can replace it. If it fails (headless/SSH session) or `NoBrowser` is set, the URL is always printed via the caller's log function regardless — the browser-open attempt is pure convenience, never load-bearing.

Nothing is persisted until the exchange fully succeeds: an abandoned flow, an expired code (GitHub's fixed ~1-hour window), or any error along the way leaves prior valid local config completely untouched, and the caller can simply call `RunManifestFlow` again — satisfying the issue's abandonment/expiry-handling requirements without any special-case cleanup logic.

No Pruefer-hosted backend is involved at any point — every network call in the flow is either the user's own browser talking to `github.com`, or Pruefer's own process talking directly to GitHub's REST API.

### 3. Manifest permissions match the existing manual-setup list exactly; no active webhook

The generated manifest (`buildManifest`, `internal/githubauth/manifest.go`) requests exactly `metadata: read`, `pull_requests: write`, `contents: read`, `issues: write` — matching `cmd/pruefer/README.md`'s manual Setup section, no more. `issues` is `write`, not `read`: GitHub's Issue Comments API requires `issues: write` to create a reaction (the eyes/rocket acknowledgment `AcknowledgeForceReview`/`MarkForceReviewsProcessed` leave on a `/pruefer review` comment), not just to read comments — a gap found and fixed during Validate after a manifest-flow-bootstrapped App 403'd on every acknowledgment under `issues: read`. `hook_attributes.active` is always `false` and no `default_events` are requested: Pruefer V1 is polling-only (ADR-1113 §1, ADR-032), and webhook ingestion is a separate, explicitly out-of-scope future issue this manifest must never enable.

### 4. Credential model: PEM at the existing default path, everything else in a new reconciler-owned state file

A freshly manifest-created App's PEM is written to the **same** default path the manual/compat flow has always used (`AppPrivateKeyPath`, default `.pruefer/app-private-key.pem`). This is the single decision that makes "no separate setup command" and "skip straight to install/token verification on subsequent runs" fall out for free: after one successful run, a manifest-bootstrapped setup and a manually-registered one are byte-for-byte indistinguishable to the reconciler — both are just "a readable PEM at the configured path, plus an App ID it can validate against GitHub."

Everything else the manifest exchange produces — App ID (when not already pinned via `config.yaml`'s `github_app_id`), slug, webhook secret, client ID/secret, and a best-effort installation→repo cache — is persisted in a new file, `AppStatePath` (default `.pruefer/app-state.json`), already covered by `.pruefer/`'s wholesale `.gitignore` entry (#1198 precedent). **`config.yaml` is never written back to by the reconciler** — this resolves the issue's own open question in favor of a separate reconciler-owned state file, so the reconciler never mutates a file the operator hand-edits. `AppID` resolution therefore checks `opts.AppID` (explicit config) first, falling back to the app-state file's own `AppID` (a prior manifest run) only when unset.

Installation access tokens are never persisted, unchanged from ADR-1113/1233: `Auth`/`RunRefreshLoop` (moved verbatim into `internal/githubauth/tokenauth.go`) still mints on demand from `app id + private key + installation id` and holds the token only in memory until shortly before its ~1-hour expiry.

### 5. Nine-step reconciliation, safe to retry at every transition, no live triggers in V1

`Reconcile` (`internal/githubauth/reconciler.go`) implements the issue's state machine as a straight-line function: load local state → bootstrap via manifest flow if no App ID is known anywhere → validate identity against GitHub (`FetchAppSlug`) → load desired repos (`WatchedRepos`) → discover installations (or use the pinned-`AppInstallationID` compat path, skipping discovery entirely — Decision 6) → map each watched owner to an installation → for an uncovered owner, log a guided install URL and continue → for a `selected`-mode installation, verify per-repo access and log any exclusion's grant URL, without failing the owner's other repos → return a ready `*Reconciler`.

Every step is a plain function call with no partial persisted state of its own (the only persistence — `RunManifestFlow`'s credential writes — is all-or-nothing per Decision 2), so a failure partway through leaves nothing to clean up; the caller retries by calling `Reconcile` again. Per the issue's own scoping ("config change"/"adding repos later" reconciliation triggers "degrade to next process start" is acceptable given Pruefer has no live config reload today), `pruefer/execute.go` calls `Reconcile` exactly once, at daemon startup — there is no file watcher, timer, or webhook-driven re-reconciliation in V1. A restart re-runs the full 9 steps; once credentials are valid this costs only a handful of GitHub calls, which is not a meaningfully worse UX than a live trigger for this version.

### 6. Three-way credential-problem branching, not two

The issue's failure-handling requirements distinguish three distinct "something's wrong with local credentials" cases, and `Reconcile`/`loadOrBootstrapCredentials` keep them separate rather than collapsing to a single "bootstrap or fail" branch:

- **Missing entirely** (no `AppID` in config, and no prior manifest run's App ID in the state file) → run `RunManifestFlow` immediately; this is the ordinary first-run path.
- **Repair-needed** (an `AppID` is known from either source, but the PEM at `AppPrivateKeyPath` is missing or fails to parse) → an explicit, hard error naming the repair action ("restore it from the App's settings page") — **never** auto-regenerated. A corrupt-but-recoverable local file must never be silently treated as "the App is gone," which would otherwise risk minting a second App nobody asked for.
- **App deleted externally** (`AppID` and PEM both load fine, but `FetchAppSlug` fails against GitHub) → behavior forks on where `AppID` came from:
  - If resolved from the reconciler-owned state file (a prior manifest run, `opts.AppID == 0`), `Reconcile` logs why, then re-enters the manifest flow itself, preserving whatever non-secret local config existed. This is safe to self-heal because the next restart resolves the freshly-created App's ID from `AppStatePath` with no stale config value in the way.
  - If `AppID` is pinned via explicit config (`opts.AppID != 0` — the compat-mode path), `Reconcile` instead returns an explicit repair error naming `github_app_id` and never invokes the manifest flow. Auto-recreating here would be unsafe: `config.yaml` is never written back to (Decision 4), so `loadOrBootstrapCredentials` would resolve the same stale, now-deleted `AppID` again on the very next restart, fail identity validation again, and silently mint another orphan App — every restart, forever. An external review caught this exact loop in an earlier revision of this change; `TestReconcile_AppDeletedExternally_PinnedAppID_ReturnsRepairErrorNeverLoops` guards against it.

The compat-mode `github_app_installation_id` pin (ADR-1233 Decision 4) is preserved exactly: when set, `Reconcile` skips discovery (and the per-repo `selected`-mode check, which only applies on the discovery path) and mints one shared token for every watched owner, byte-for-byte matching pre-reconciler behavior.

### 7. `checkSelectedRepos` becomes a soft per-repo status, discovery-path only

ADR-1233 Decision 5's `checkSelectedRepos` hard-errored bootstrap entirely when a `repository_selection: selected` installation excluded a watched repo. `verifyRepoAccess` (`internal/githubauth/installations.go`) generalizes this into a per-repo `RepoStatus{Authorized, Reason, GrantURL}`, logged (`! <repo> is not authorized (...) → https://github.com/settings/installations/<id>`) rather than returned as an error — matching the issue's "mark it unauthorized, surface the grant URL" requirement. The owner's other, already-authorized repos keep working; only the excluded repo is affected. This is a deliberate behavior change from ADR-1233, scoped strictly to the discovery path — the pinned-installation compat path never performed this check and still doesn't.

### 8. No TUI integration for reconciliation progress

The reconciler reports progress via a plain `func(format string, args ...any)` callback (`Options.Logf`), matching every other package's `logf` convention in this codebase, which `pruefer/execute.go` wires to Pruefer's existing console/structured logging. Piping progress into `pruefer/tui`'s `ptui.Event` channel instead would require `internal/githubauth` to import `pruefer/tui`, inverting the intended dependency direction (Decision 1) for a one-time startup sequence the TUI doesn't otherwise model. The issue's own example output (`✓ handarbeit/fabrik authorized` / `! shadoworg has no installation → ...`) is console-shaped, not a TUI mockup — flagged as a reasonable follow-up, not required here.

## Consequences

**Positive:**
- Pruefer is now genuinely self-hostable by anyone with zero shared credential: each user's first run creates and owns their own dedicated App, with no private key ever leaving their machine and no Pruefer-hosted service involved.
- The existing shared-App deployment (`handarbeit-pruefer`) keeps working with **zero code or config change** — the reconciler detects its valid local credentials and skips the manifest flow entirely, verified by a backward-compatibility regression test suite (`TestReconcile_BackwardCompat_*` in `internal/githubauth/reconciler_test.go`) asserting zero manifest-flow invocations.
- `Auth`/`RunRefreshLoop`'s existing refresh-timing/retry behavior and tests carry over unchanged (moved, not reimplemented), avoiding the "two parallel token-refresh implementations" risk Research flagged.
- A repo excluded from a `selected`-mode installation, or an owner with no installation at all, no longer takes down the whole daemon at startup — only that owner/repo is affected, and the operator gets an actionable URL instead of a bootstrap crash.

**Negative / Trade-offs:**
- "Adding a repo later" and "config change" reconciliation both require a process restart in V1, not a live trigger — acceptable given Pruefer has no config hot-reload today, but a real limitation the issue's own Research flagged as appropriate V1 scope rather than a full solution.
- A fully non-interactive first run (no human ever available to click through the manifest/install flow, e.g. an unattended background service with no prior setup) cannot be automated — inherent to GitHub's manifest/installation flow requiring a human, not a gap specific to this implementation.
- The reconciler-owned `.pruefer/app-state.json` is a new on-disk file operators must understand exists (documented in `cmd/pruefer/README.md`), alongside the existing `config.yaml` and `app-private-key.pem` — a small increase in on-disk surface area in exchange for never mutating the operator's own config file.

## Related Work

- ADR-1113 (`adrs/1113-pruefer-v1-architecture.md`) §1 — the single shared-App bootstrap model this ADR supersedes as the *default* (kept as a fully supported compat mode). §2–§9 are unaffected.
- ADR-1233 (`adrs/1233-pruefer-multi-installation-auth.md`) — the owner-derived multi-installation routing (`AuthSet`/`BootstrapMulti`) this ADR's reconciler subsumes; Decisions 4 (pinned-installation compat) and 5 (`selected`-mode check, now generalized to a soft status per Decision 7 above) both carry forward.
- ADR-1196 (`adrs/1196-extract-self-upgrade-package.md`) — the `internal/selfupgrade` engine-independence precedent `internal/githubauth`'s package boundary mirrors.
- `cmd/pruefer/README.md` — Setup section rewritten in the same change to document the manifest flow as the default first-run path, with the manual registration flow kept as documented compat mode, plus new config reference rows for `github_app_state_path`/`no_browser`.

**References:** [cmd/pruefer/README.md](../cmd/pruefer/README.md)
