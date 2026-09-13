# ADR 1715: `fabrik init --github-app` Setup Flow

**Date**: 2026-09-13
**Status**: Accepted
**Issue**: #1715 — feat(init): --github-app setup flow, with grant verification and org-only guard

## Context

By the time this issue was picked up, every building block it composes already existed
and was merged to `main`:

- **#1713** wired the engine onto a GitHub App installation as a second, co-equal
  authentication path — but strictly **compat mode**: it consumes an App ID, private
  key, and installation ID an operator has already created and installed by hand, and
  never runs `internal/githubauth`'s manifest/browser bootstrap flow itself.
- **#1714** gave `fabrik init --create-board`/`fabrik repair-board` the ability to
  create and repair a project board's Status columns directly from stage configs.
- **#1712/#1709/#1711** parameterized `internal/githubauth`'s manifest identity,
  added granted-permission verification (`Reconciler.VerifyGrants`), and fixed the
  manifest flow's `hook_attributes` rejection — the mechanisms this issue needed were
  already correct and tested, just never wired into a guided setup command.

`docs/USER_GUIDE.md`'s own "GitHub App Authentication" section named the gap explicitly:
*"Fabrik does not walk you through creating or installing the App itself; that guided
setup is tracked as a separate, future `fabrik init --github-app` enhancement."* This
issue closes that forward-reference.

## Decision

### Reuse `Reconcile`'s own bootstrap idempotency instead of hand-rolling one

A naive design — "call `RunManifestFlow` directly whenever not adopting an existing
App" — would create a **second**, orphaned App on any retry after a downstream failure
(installation not found, permission shortfall, config write failure). `Reconcile`'s
`loadOrBootstrapCredentials` already resolves an App ID from `AppStatePath` before ever
running the manifest flow, so routing every path (create, adopt, and any retry) through
`Reconcile` gets this retry-safety for free instead of re-deriving it. `cmd/init_github_app.go`'s
`runGitHubAppSetup` never calls `RunManifestFlow` or `FetchAppInstallations` directly —
only `githubauth.Reconcile`, `Reconciler.LastDerived`, `Reconciler.ClientForRepo`,
`Reconciler.VerifyGrants`, and the new `Reconciler.AppID` getter.

### Two `Reconcile` calls only when installation discovery is needed

`VerifyGrants` only works on a **pinned** Reconciler (`Options.AppInstallationID != 0`),
and pinning requires an installation ID, which — absent an explicit
`--github-app-installation-id` — is only known after a non-pinned discovery pass. Rather
than reinvent discovery via a raw `FetchAppInstallations` call, the first (non-pinned)
`Reconcile` call's own `LastDerived().Installations` already carries the per-owner
`InstallationID` from the Derive it already performed internally. A second call then
pins that discovered ID. When `--github-app-installation-id` is given explicitly up
front, the first call is already pinned and the second never happens — the shape then
matches the engine's own `setUpGitHubAppAuth` (#1713) exactly, one `Reconcile` call.

### Export, don't refork, the engine's R3/R4 primitives

`engine/github_app_auth.go`'s `engineRequiredGitHubAppPermissions`,
`refuseUserOwnedBoardForAppAuth`, `engineGitHubAppStatePath`, `formatPermissionShortfalls`,
`engineGitHubAppName`, and `engineGitHubAppHomepageURL` are renamed to their exported
forms (`RequiredGitHubAppPermissions`, `RefuseUserOwnedBoardForAppAuth`,
`GitHubAppStatePath`, `FormatPermissionShortfalls`, `GitHubAppName`,
`GitHubAppHomepageURL`) with no behavior change. `cmd` already imports `engine`
(`cmd/root.go`, `cmd/resume.go`), so `cmd/init_github_app.go` calls these directly rather
than writing a third independent copy — the engine's own doc comments already flagged
`cmd/board_admin.go`'s `refuseIfUserOwnedBoard` as an *existing* second copy with a
"check the other for drift" comment; adding a third would have compounded exactly the
problem that comment warns about. `cmd/board_admin.go`'s `refuseIfUserOwnedBoard` is a
deliberately separate, board-operation-specific check (different wording, different
question — "can this specific board operation proceed" vs. "can App auth work for this
owner at all") and still runs independently at the `--create-board` hand-off; the two
guards are not the same check and both apply, at different points, when both flags are
given together.

Sharing `GitHubAppStatePath(".")` between the setup flow and the engine's own startup
(`resolveGitHubAppAuth`, called with `fabrikDir = os.Getwd()`) means both point at the
identical `.fabrik/github-app-state.json` when run from the same project root, which is
the only way the setup flow's freshly-created App metadata is visible to the engine's
own next-run diagnostics cache.

### New `Reconciler.AppID()` getter

No caller needed this before this issue: every existing caller either already knew its
App ID (the pinned config path) or never needed to read it back. `runGitHubAppSetup`
does, specifically for the **create** path — the operator never supplied an App ID up
front, so the only way to learn the manifest flow's freshly-minted App ID (to persist as
`github_app_id` in `.fabrik/config.yaml`) is to read it back off the Reconciler that just
resolved it. Mirrors the existing `BotLogin()` getter's shape exactly.

### Config-writing refactor: `configValues` struct

`cmd/init.go`'s `writeConfigTemplate`/`buildConfigWithValues` took six positional string
parameters before this issue. Adding three more (`GitHubAppID int64`,
`GitHubAppPrivateKeyPath string`, `GitHubAppInstallationID int64`) as positional
parameters would have made call sites unreadable and error-prone (nine same-shaped
positional arguments, several sharing a type). Both functions now take a single
`configValues` struct instead — pure refactor, no behavior change for the six
pre-existing fields.

### Adopt requires the App-ID/key-path pair, or neither

Mirrors the engine's own runtime config rule (`validateGitHubAppConfig`, #1713):
`--github-app-id` and `--github-app-private-key-path` must be given together, or not at
all. Accepting only `--github-app-private-key-path` and treating it as "a custom path
for a fresh App" was considered and rejected — a fresh manifest bootstrap writing to an
operator-named path that might already hold an unrelated existing key would silently
overwrite it. A custom key path for a **freshly created** App is out of scope for this
issue; the default (`.fabrik/github-app-key.pem`) is used, and the file can be moved
afterward if a different location is wanted (updating `github_app_private_key_path` in
`.fabrik/config.yaml` to match).

### `--owner` is required whenever `--github-app` is given

Research's open question — whether owner resolution should depend on `--create-board`'s
own `--owner`, or need an independent flag — is resolved by requiring `--owner`
independently on the `--github-app` path itself. This keeps `fabrik init --github-app`
alone (no `--create-board`) well-defined, and the two flags' `--owner` values are the
same flag instance in `cmd/init.go` regardless of which subset is given.

### R5's approval URL: reuse the existing codebase form

The issue's own text speculated `https://github.com/settings/installations/{id}/permissions/update`;
Research could not confirm either form against a live App or GitHub's public docs. This
implementation reuses the form already established elsewhere in the codebase —
`internal/githubauth/installations.go`'s `verifyRepoAccess`'s `GrantURL` field and
`cmd/pruefer/README.md` — `https://github.com/settings/installations/{id}` (no
`/permissions/update` suffix), on the basis that GitHub's settings pages are known to
redirect from a bare installation-settings URL to the specific configuration screen
needed, and that reusing an existing, already-referenced form is preferable to
introducing a third, unverified one. Documented in `docs/USER_GUIDE.md` without claiming
it is the definitive deep-link — an org admin landing on the installation's settings
page can always navigate to the permission-approval prompt from there.

### Board hand-off uses the App's own client, not a second `--token`

When `--github-app` and `--create-board` are both given, `cmd/init.go` calls
`createBoardCore` directly with the `*gh.Client` the setup flow already minted, instead
of `runCreateBoard`'s token-loading path. The App auth this flow just verified already
carries `organization_projects:write` — exactly what board creation needs — so demanding
a second, separate `--token`/`FABRIK_TOKEN` credential in the same invocation would be
pure friction with no security benefit.

## Consequences

- `fabrik init --github-app` is now the primary, guided path to GitHub App
  authentication; manual App creation and manual `.fabrik/config.yaml` editing remain
  possible (compat mode never required the guided flow) but are no longer the only
  documented path.
- The four exported `engine` identifiers (`RequiredGitHubAppPermissions`,
  `RefuseUserOwnedBoardForAppAuth`, `GitHubAppStatePath`, `FormatPermissionShortfalls`,
  plus the two exported name/homepage constants) are now part of `engine`'s public
  surface for a second caller (`cmd`) — any future change to the engine's required
  permission set or user-owned-board refusal wording automatically applies to setup-time
  verification too, with no separate update needed.
- The manifest-flow **create** path (an operator confirming App creation in a real
  browser against real github.com) is not covered by this issue's automated tests — it
  is inherently browser-driven, exactly as `internal/githubauth`'s own tests substitute
  a package-private `runManifestFlow` var (unexported, not reachable from `cmd`) rather
  than exercising a real browser round trip. Test coverage here is therefore scoped to
  the **adopt** path (pinned and via discovery), R4's refusal, R3/R5's verification and
  shortfall reporting, and `runInit`'s flag validation — all reachable without a browser.
- A future change to `internal/githubauth.Reconcile`'s discovery/pinning contract must
  preserve the "two calls only when needed" idempotency shape this issue depends on, or
  `runGitHubAppSetup`'s retry-safety claim (no orphaned App on a downstream failure)
  regresses silently.

See also: [ADR-1713](1713-engine-github-app-auth.md) (engine compat mode),
[ADR-1714](1714-board-admin-create-and-repair.md) (board create/repair hand-off),
[ADR-1712](1712-githubauth-manifest-identity-parameterization.md) (manifest identity
parameterization), [ADR-1709](1709-github-app-grant-verification.md) (grant
verification single-source-of-truth precedent).
