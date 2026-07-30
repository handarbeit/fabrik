// Package githubauth implements a self-hostable GitHub App authentication
// reconciler: given a desired set of watched repos, it ensures a GitHub App
// exists, its credentials are stored locally, every watched repo is covered
// by an installation, and valid installation tokens can be minted on demand
// — driving that state via GitHub's App Manifest flow (loopback callback,
// browser handoff, code exchange) when no usable local credentials exist.
//
// This package must never import pruefer. It exists so that Pruefer (and
// any future self-hosted daemon) can obtain a dedicated, per-user GitHub App
// identity with no shared credential and no Pruefer-hosted backend — the
// boundary this package draws mirrors internal/selfupgrade's: callers supply
// their own paths/config (private key path, app-state path, watched repos,
// a log function), and nothing here hardcodes "pruefer". The rest of the
// caller's code depends only on the narrow GitHubAuth interface
// (ClientForRepo, BotLogin) — it never sees PEMs, JWTs, installation IDs,
// browser flows, or refresh loops.
//
// See adrs/1253-github-app-manifest-auth-reconciler.md for the design
// rationale, including why a manifest-created App supersedes (while still
// supporting as a compat mode) the single shared public App described in
// adrs/1113-pruefer-v1-architecture.md §1.
package githubauth
