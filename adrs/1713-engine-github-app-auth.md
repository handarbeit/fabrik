# ADR 1713: Engine GitHub App Authentication (Compat Mode)

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1713 — authenticate as a GitHub App installation, with PAT mode co-equal

## Context

The engine has always authenticated every GitHub call with a personal access token
(`cfg.Token` → `gh.NewClient`). `internal/githubauth` already implements App-installation
auth with proactive token refresh, proven in production by Pruefer — but nothing in
`engine/` or `cmd/root.go` referenced it. Moving the engine onto an App installation gives
it its own rate-limit bucket (separate from any human's PAT, and larger — GitHub scales
installation limits with the organization's repo and user count) and a stable
`fabrik[bot]` identity, instead of appearing as whichever human's token happens to be
configured (see #770 for the full measured design).

This issue is **compat-mode only**: it consumes an App ID, private key, and installation
ID for an App an operator has already created and installed, by whatever means. It does
not add a manifest/browser bootstrap flow for the engine itself — that remains a
Pruefer-shaped path in `internal/githubauth` (see ADR-1712), and a future `fabrik init
--github-app` setup UX is explicitly out of scope here.

## Decisions

### 1. PAT mode is permanent and co-equal, never "legacy"

GitHub strips organization-scoped permissions — including Projects v2 access — from a
GitHub App installation on a **user** account; only an organization-owned installation
ever receives them (measured, #770). A user-owned project board can therefore never use
App auth at all, so a personal access token remains the only option for that case,
permanently. Nothing in code, logs, or docs describes PAT mode as legacy or deprecated;
`Config.Token` unset/App-auth-fields-unset (the default) is byte-identical to pre-#1713
behavior (R1/AC2).

### 2. App-auth construction happens synchronously in `New()`, not `Run()`

`New()` builds `e.readClient` via `boardcache.NewGitHubAdapter(eng.client)` before
returning — capturing whichever concrete client is live at that point. Doing App-auth
construction later (in `Run()`, which is where a real, cancellable `ctx` first exists)
would leave `e.readClient` permanently wrapping a stale PAT-based client. `New()`
therefore performs the pinned `Reconcile` call, the R4 owner-type refusal, and the R3
grant check all synchronously, using `context.Background()` — acceptable because `New()`
made zero network calls before this change, so a hang here is exactly as Ctrl-C-able as
any other synchronous startup step before `Run()`'s own signal handlers are installed.
Only the resulting `Reconciler`'s refresh-loop goroutines need a real, cancellable `ctx`
— that part is started later, from `Run()` (Decision 7).

### 3. Fail-hard grant check, not a config flip on ADR-1709's existing soft check

ADR-1709 added exactly the granted-vs-requested comparison this issue needs
(`checkGrantedPermissions`, `gh.FetchAppInstallation`, ordinal permission comparison),
but made it deliberately **soft** for Pruefer — logged loudly, never blocking
`Reconcile`'s return, because a missing permission there is non-fatal until first use.
R3/AC3 require the opposite for the engine: startup must fail, naming every missing
permission. Rather than loosening ADR-1709's check (which would regress Pruefer's own
behavior) or duplicating the ordinal-comparison logic in `engine/`, `internal/githubauth`
gains exactly one new, additive piece of exported surface:
`(*Reconciler).VerifyGrants(required) ([]RequiredPermissionShortfall, error)` — a
fail-hard sibling to the package's existing soft `verifyPinnedGrants`, reusing
`checkGrantedPermissions`/`gh.FetchAppInstallation` unchanged. Pruefer's own soft check is
untouched; only a Reconciler built from a pinned installation supports this call (a
non-pinned, discovery-mode Reconciler has no single installation to check, and returns an
explicit error rather than guessing one).

### 4. R4 (owner-type refusal) uses `ResolveOwner`, not `ProjectBoard.OwnerType` — and runs before R3

`ProjectBoard.OwnerType` is not independently GitHub-verified in the common case — it's an
echo of whatever `cfg.OwnerType` string the caller already passed in. Using it for R4
would produce a check that only works when config already happens to be right, and does
nothing to catch a stale or wrong config value — exactly the silent-failure class this
issue exists to close. `(*gh.Client).ResolveOwner(login)` (ADR-1714's primitive, a single
lightweight `repositoryOwner{__typename}` GraphQL query) is used instead: it reliably
answers "organization or user" independent of Projects v2 access at all.

R4 is checked **before** R3: GitHub never grants `organization_projects` to a user-account
installation (Decision 1, measured), so a user-owned board would also fail the grant
check — but with a confusing "missing organization_projects" message instead of naming
the actual, more fundamental cause. Checking ownership first gives the clearer, more
actionable error. The refusal message duplicates `cmd/board_admin.go`'s
`refuseIfUserOwnedBoard` wording rather than sharing it — `engine` cannot import `cmd`
(`cmd` imports `engine`), and duplicating a ~5-line static message is simpler and
lower-risk than introducing a new shared package for one string. A future edit to one
should check the other for drift.

### 5. Synthetic `WatchedRepos = []string{cfg.Owner + "/*"}`

`Reconcile`'s pinned branch (`opts.AppInstallationID != 0`) populates its client map from
the *owners* named in `Options.WatchedRepos` — the repo half of each entry is never
consulted for that purpose, only for a diagnostics-only repo cache. The engine has no
watched-repo concept of its own. Omitting this entirely would let `Reconcile` succeed
(App/JWT/installation-token minting all work) while the very first `ClientForRepo` call
fails with "no authorized GitHub App installation for owner" — a silent-failure trap this
issue's own research specifically flagged. `cfg.Owner + "/*"` satisfies the pinned
branch's owner-population loop with a value that is never itself read as a real repo
name; `cfg.Owner` is always non-empty by the time `New()` runs (`cmd/root.go` requires
it) and is always the login that owns the project board, including in multi-repo mode
(where `cfg.Repo` may be empty but `cfg.Owner` still names the board's organization).

### 6. Worker `gh` CLI auth reads the live token, not a static copy

Every built-in stage skill that shells out to `gh` directly (e.g. `fabrik-validate`'s
Pre-Completion Gate) depends on `engine/claude.go`'s `claudeGHToken`, which today is
always `cfg.Token` copied once at `New()` time. Under App-auth-only config (`cfg.Token ==
""`), that injection would silently stop firing entirely — a functional gap that would
only surface partway through a stage cycle, not at startup, looking like an unrelated
`gh`-CLI or permissions problem. This is closed by `claudeGHTokenOverrideFn func() string`
(nil in PAT mode, unchanged behavior): when set, `buildClaudeEnv` prefers it over
`claudeGHToken`, reading `(*gh.Client).Token()` live off the same client the engine's own
API calls use — riding that client's background refresh loop (proactive ~5-minute-margin
refresh) rather than adding a second minting path or goroutine.

**Residual, accepted limitation**: a single invocation whose wall time exceeds the
installation token's ~1-hour lifetime still captures a token at env-build time that can
expire mid-run, since a running child process's environment variables cannot be updated
after the fact. This is a narrow edge case (`max_wall_time` at or beyond roughly an hour,
or no cap) and is not fully solved here — consistent with R7's "deliberately deferred"
framing for git-credential injection, this is a known, documented gap (see
`docs/USER_GUIDE.md`'s "GitHub App Authentication" § "Worker `gh` CLI authentication"),
not a silently-accepted one.

### 7. Refresh-loop lifecycle lives in `Run()`, ordered against `cancel()`

`New(cfg)` has no `context.Context` parameter and returns before any `ctx` exists;
`Run()` creates `ctx` (cancelled on SIGINT/SIGTERM) and owns the shutdown-drain sequence
(ADR-1393). `Reconciler.RunRefreshLoops(ctx, logf)` is therefore called from `Run()`,
immediately after `ctx, cancel := context.WithCancel(...)`. Its `wait()` defer is
registered **before** `cancel()`'s own defer: since defers unwind LIFO, this guarantees
`cancel()` always runs first on any return path (stopping every refresh-loop goroutine),
and only then does `wait()` block until they've actually exited — before the log-file-close
defer (registered even earlier in `Run()`, so it unwinds last) runs. Getting this ordering
backwards would deadlock: a `wait()` that blocks before `ctx` is cancelled waits forever
for a goroutine that has no reason to exit yet. This mirrors `pruefer/execute.go`'s
`Reconcile → RunRefreshLoops → defer waitRefreshLoops`-before-`closeLog` ordering
established for the same reason.

### 8. GHES + App-auth is refused outright, not silently attempted

`internal/githubauth`'s `mintAuth` unconditionally builds a client via
`gh.NewClientWithBaseURL`, whose own doc comment says this derives an incorrect GraphQL
endpoint for a GHES host (`NewClientForHost` exists specifically because GHES needs
independently-derived REST/GraphQL paths). This is a pre-existing gap in
`internal/githubauth` this issue does not fix — combining `--ghes-host`/`FABRIK_GHES_HOST`
with GitHub App auth config is refused at startup with an explicit message naming the
incompatibility, rather than constructing a client that would fail obscurely later
against the wrong endpoint.

### 9. No `AppStatePath` config key is exposed

Compat-mode-only usage means the manifest/browser bootstrap flow never runs, and nothing
meaningful is ever read back from `AppStatePath` in this mode — it exists solely because
`Reconcile`'s internal `saveInstallationRepoCache` (a best-effort diagnostics write) needs
some path to write to. Hardcoded to `.fabrik/github-app-state.json`, not user-configurable.
If a future `fabrik init --github-app` bootstrap issue needs it exposed, that is its own
decision.

### 10. The engine's required-permission set is its own, engine-owned constant

`Options.RequiredPermissions` is already generic and caller-supplied — nothing in
`internal/githubauth` needs to know the engine's specific scopes. `engine/
github_app_auth.go`'s `engineRequiredGitHubAppPermissions` is a new, engine-owned constant
(`metadata:read`, `organization_projects:write`, `issues:write`, `pull_requests:write`,
`checks:read`, `statuses:read`, plus `webhooks:write` only when `--webhooks` is enabled) —
mirroring `internal/githubauth/manifest.go`'s own `PrueferRequiredPermissions` precedent
for "one hand-maintained, caller-owned required set." Like that precedent, this is an
inherently hand-maintained correspondence with the engine's actual GitHub API usage: a
future new API call needing a permission not yet listed here will pass this check while a
genuinely new gap goes undetected until that feature's first use.

## Consequences

- Cross-org spawn (ADR-1419) is unreachable under App auth: the engine holds one client
  scoped to one installation (one organization), not Pruefer's per-owner client map. A
  spawn targeting a different GitHub account/organization has no client to use under App
  auth. This mirrors a GitHub App installation's own strict account-scoping — not a
  Fabrik design choice — and is documented as a known limitation rather than silently
  broken.
- Self-upgrade's `releaseClient` becomes unauthenticated when App-auth-only (no PAT) is
  configured (`releaseUpgradeToken` already returns `""` whenever `cfg.Token == ""`). This
  is an acceptable, pre-existing fallback — public release fetching works fine
  unauthenticated at Fabrik's own low call volume — not a new gap this issue introduces.
- Git clone/push (R7) is unaffected **only under `git_ssh: true`/`--ssh`, or an active
  `url.git@github.com:.insteadOf = https://github.com/` rewrite** — in either case the
  engine's own git calls (always ambient-credentialed) and worker git/gh calls (which do
  receive the installation token as `GH_TOKEN`/`GITHUB_TOKEN`, per Decision above) never
  actually consult a credential helper for the HTTPS remote. Under the *default* HTTPS
  clone mode with neither of those in effect, this claim does not hold: a worker `git
  push` resolves credentials through a helper (e.g. one registered by `gh auth
  setup-git`) that prefers `GH_TOKEN`/`GITHUB_TOKEN` — the installation token, which is
  granted `contents:read` but not `contents:write` — and would 403 (fetch alone would
  likely succeed). This was corrected, and the gap closed with a startup-time refusal
  rather than a silent dependency on host git config, by #1756 — see ADR-1756 for the
  git-under-App-auth decision. #1846 (ADR-1846) later replaced that refusal with an
  engine-injected credential helper serving the installation token (requiring
  `contents:write`), which also closes this ADR's ~1h token-lifetime gap for git.
- A future `fabrik init --github-app` bootstrap issue would need to parameterize
  `internal/githubauth`'s manifest-bootstrap path (`defaultAppName`,
  `defaultAppHomepageURL`) for the engine's own identity, mirroring what ADR-1712 already
  did for the compat-mode fields this issue consumes.

## Prior Art

- ADR-1253 (`1253-github-app-manifest-auth-reconciler.md`) — origin of `internal/
  githubauth`. Establishes the compat-mode pin (`AppID`/`AppInstallationID` skip
  discovery) this issue relies on unchanged.
- ADR-1233 (`1233-pruefer-multi-installation-auth.md`) — the per-owner `Reconciler.clients`
  design this issue's engine usage deliberately does not fully adopt (Consequences above).
- ADR-1709 (`1709-github-app-grant-verification.md`) — the soft, log-only grant check this
  issue's `VerifyGrants` is an additive, fail-hard sibling to (Decision 3).
- ADR-1391 (`1391-ghes-support.md`) — established `NewClientForHost` and the engine's GHES
  support; this issue's Decision 8 refuses the combination rather than fixing
  `internal/githubauth`'s pre-existing GHES gap.
- ADR-1714 (`1714-board-admin-create-and-repair.md`) — source of `ResolveOwner` and
  `refuseIfUserOwnedBoard`, the direct precedent for Decision 4.
- ADR-1712 (`1712-githubauth-manifest-identity-parameterization.md`) — generalized
  `Options.AppName`/`AppHomepageURL`/`RequiredPermissions` for a second caller; this issue
  is the anticipated follow-up that actually wires the engine's own identity/permissions
  in via those already-pluggable fields.
- ADR-1393 — the shutdown-drain machinery this issue's refresh-loop lifecycle (Decision 7)
  integrates with.
