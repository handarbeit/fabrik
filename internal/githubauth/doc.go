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
// That said, unlike internal/selfupgrade, this package is not yet fully
// caller-agnostic in practice: manifest.go's defaultAppName ("pruefer"),
// defaultAppHomepageURL (github.com/handarbeit/fabrik), and the specific
// permission set buildManifest requests are all Pruefer-shaped constants,
// not Options/ManifestFlowOptions fields a second caller could override. A
// future second self-hosted daemon reusing this package today would get an
// App named "pruefer", homepaged at fabrik's repo, and scoped to exactly
// Pruefer's permissions — parameterizing those three is follow-up work, not
// something this package already does.
//
// See adrs/1253-github-app-manifest-auth-reconciler.md for the design
// rationale, including why a manifest-created App supersedes (while still
// supporting as a compat mode) the single shared public App described in
// adrs/1113-pruefer-v1-architecture.md §1.
package githubauth
