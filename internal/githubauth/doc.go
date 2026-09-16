// Package githubauth implements a self-hostable GitHub App authentication
// reconciler: given a desired set of watched repos, it ensures a GitHub App
// exists, its credentials are stored locally, every watched repo is covered
// by an installation, and valid installation tokens can be minted on demand
// — driving that state via GitHub's App Manifest flow (loopback callback,
// browser handoff, code exchange) when no usable local credentials exist.
//
// This package must never import pruefer. The boundary it draws mirrors
// internal/selfupgrade's: callers supply their own paths/config (private
// key path, app-state path, watched repos, a log function), and the rest of
// the caller's code depends only on the narrow GitHubAuth interface
// (ClientForRepo, BotLogin) — it never sees PEMs, JWTs, installation IDs,
// browser flows, or refresh loops.
//
// As of #1712, this package is caller-agnostic in the same sense as
// internal/selfupgrade: manifest.go's defaultAppName ("pruefer"),
// defaultAppHomepageURL (github.com/handarbeit/fabrik), and the permission
// set buildManifest requests are all just Pruefer's own defaults now —
// Options.AppName, Options.AppHomepageURL and Options.RequiredPermissions
// (threaded through ManifestFlowOptions) let a second caller (e.g. the
// engine, #770) override all three and get its own App name, homepage, and
// permission set instead. Options.RequiredPermissions doubles as both "what
// a fresh App's manifest requests" and "what an existing installation is
// verified against" (#1709) — one field per caller, not two that could
// silently drift. Wiring a second caller's actual identity/permissions in
// is separate follow-up work; this package only makes the mechanism
// pluggable.
//
// As of #1722, Options.ServedAccounts is caller-agnostic the same way:
// whichever caller constructs Options decides which accounts its own
// deployment is authorized to serve, and Derive's account-recognition gate
// (R4/R5) is driven entirely by that field plus the pre-existing
// Options.WatchedRepos — this package never hardcodes Pruefer's or any other
// caller's account list.
//
// As of #1763, Options.OpenBrowser lets a caller outside this package (e.g.
// the engine's own tests) override guideMissingInstallations' browser-open
// call in preference to the package-level openBrowser var, without needing
// to know that Options.NoBrowser's zero value (false) is otherwise unsafe —
// restoring the "testable by external users with zero side effects"
// guarantee this doc comment already promises, for the one call site
// (Reconcile's non-pinned discovery branch) external callers concretely
// reach. It does not cover RunManifestFlow's own browser-open call, which
// remains out of scope for #1763 (legitimately interactive, user-initiated
// first-run setup) — see ADR-1763.
//
// See adrs/1253-github-app-manifest-auth-reconciler.md for the design
// rationale, including why a manifest-created App supersedes (while still
// supporting as a compat mode) the single shared public App described in
// adrs/1113-pruefer-v1-architecture.md §1.
package githubauth
