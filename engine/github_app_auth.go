package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/githubauth"
)

// This file wires the engine onto a GitHub App installation as a second,
// co-equal authentication path alongside the existing PAT (Config.Token) —
// see #1713. It is compat-mode only: it consumes an App ID + private key +
// installation ID an operator has already created and installed, by
// whatever means (manually or via Pruefer's manifest flow) — it never runs
// internal/githubauth's manifest/browser bootstrap flow itself. PAT mode is
// permanent and co-equal (R2), not deprecated: a user-owned board can never
// use App auth at all (see RefuseUserOwnedBoardForAppAuth below), so PAT
// remains the only option for that case. See adrs/1713-engine-github-app-
// auth.md.

// GitHubAppName and GitHubAppHomepageURL are forwarded to
// Reconcile's ManifestFlowOptions identity fields. In the engine's own
// compat-mode config (AppID + AppInstallationID both pinned), Reconcile's
// manifest/browser bootstrap path is structurally unreachable — a pinned
// AppID always takes the explicit-repair-error branch instead of
// self-healing via manifest — so these are never actually used there to
// create anything; setting them anyway is cheap, correct defensive hygiene,
// not load-bearing. `fabrik init --github-app` (#1715) is the first code
// path that actually exercises App creation with this identity — see
// cmd/init_github_app.go.
const (
	GitHubAppName        = "fabrik"
	GitHubAppHomepageURL = "https://github.com/handarbeit/fabrik"
)

// GitHubAppStatePath is the fixed, non-configurable path Reconcile uses
// for reconciler-owned diagnostics state (its installation-repo cache) —
// and, for a fresh App the manifest flow creates, the App ID/slug/secrets
// themselves. In the engine's own compat-mode usage the manifest flow never
// runs and nothing meaningful is ever read back from this file — it exists
// solely because Reconcile's internal saveInstallationRepoCache needs some
// path to write to. Exported (not itself a config key) so `fabrik init
// --github-app` (#1715) points its own Reconcile calls at the exact same
// path the engine will read at startup — see cmd/init_github_app.go.
func GitHubAppStatePath(fabrikDir string) string {
	return filepath.Join(fabrikDir, ".fabrik", "github-app-state.json")
}

// RequiredGitHubAppPermissions returns the GitHub App permission set
// the engine's own GitHubClient code paths require (R3) — the set an
// installation's actually-granted permissions are compared against at
// startup. webhooksEnabled adds the repo-webhook-management permission only
// when cfg.Webhooks is on (DeleteForwardingHooks manages repo hooks; the
// engine never touches that API otherwise) — though as of #1752,
// RefuseWebhooksWithGitHubApp means the engine's own runtime path
// (resolveGitHubAppAuth) never calls this with webhooksEnabled true anymore;
// it stays reachable with true only via cmd/init_github_app.go's own
// --webhooks setup flag. See that function's doc comment and
// DeleteForwardingHooks' for the full unreachable-under-App-auth chain.
//
// This is a hand-maintained correspondence with the engine's actual API
// usage (mirroring internal/githubauth's own requiredPermissions doc
// comment), derived from engine.GitHubClient's method set rather than
// confirmed against a real installation's granted-permissions JSON per
// #770's methodology — that verification is a tracked follow-up, not yet
// done (see #1713's PR description) for every key below *except*
// "repository_hooks" and "contents", both confirmed by direct measurement
// rather than inference:
//
// "repository_hooks": #1752 confirmed that spelling (GitHub has no
// "webhooks" permission key — the pre-#1752 value here was wrong) against
// five real, recorded GitHub App permissions objects in
// github/testdata/recordings/fetch_check_runs.json (a different App's own
// granted-permissions payload, not literally a GET /app/installations/{id}
// response for this repo's own App, but real and non-documentation,
// drawing from the same permission-key namespace).
//
// "contents": "read" was confirmed when FetchCommitsBehind's compare
// endpoint (GET /repos/.../compare/{base}...{head}) was found to 403 under
// App auth without it (2026-09-16, bed installation 162085522, see #1755).
// "contents: write" (needed, if at all, for PR merge) remains unmeasured
// and is deliberately not added here — see #1755's scope notes.
//
// If the engine starts using a new GitHub API needing a permission not yet
// listed here, this check will pass while a genuinely new gap goes
// undetected until that feature's first use — the same is true if any of
// the other keys below turns out to be wrong.
func RequiredGitHubAppPermissions(webhooksEnabled bool) map[string]string {
	perms := map[string]string{
		"metadata":              "read",
		"organization_projects": "write",
		"issues":                "write",
		"pull_requests":         "write",
		"checks":                "read",
		"statuses":              "read",
		"contents":              "read",
	}
	if webhooksEnabled {
		perms["repository_hooks"] = "write"
	}
	return perms
}

// gitHubAppFieldsSet reports which of the three GitHub App compat-mode
// config fields are non-empty, for the all-or-nothing check below (R5).
func gitHubAppFieldsSet(cfg Config) (id, key, inst bool) {
	return cfg.GitHubAppID != 0, cfg.GitHubAppPrivateKeyPath != "", cfg.GitHubAppInstallationID != 0
}

// gitHubAppAuthConfigured reports whether the engine should authenticate as
// a GitHub App installation rather than a PAT — true only when all three
// compat-mode fields are set (see validateGitHubAppConfig for the partial
// case).
func gitHubAppAuthConfigured(cfg Config) bool {
	id, key, inst := gitHubAppFieldsSet(cfg)
	return id && key && inst
}

// validateGitHubAppConfig enforces R5: partial App configuration (e.g. a
// Client ID with no private key) must fail at startup with a clear message,
// never silently fall back to PAT. Compat-mode-only usage means the engine
// must never let a partially-configured set reach githubauth.Reconcile,
// which would otherwise treat a missing AppID as "run the manifest/browser
// bootstrap flow" — entirely wrong for a headless daemon and not what R5
// (or this issue's explicitly-out-of-scope bootstrap UX) wants.
//
// Returns nil for both the fully-unconfigured case (PAT mode, unchanged
// behavior — R1/AC2) and the fully-configured case (App-auth mode).
func validateGitHubAppConfig(cfg Config) error {
	id, key, inst := gitHubAppFieldsSet(cfg)
	if !id && !key && !inst {
		return nil
	}
	if id && key && inst {
		return nil
	}
	var missing []string
	if !id {
		missing = append(missing, "github_app_id")
	}
	if !key {
		missing = append(missing, "github_app_private_key_path")
	}
	if !inst {
		missing = append(missing, "github_app_installation_id")
	}
	return fmt.Errorf("GitHub App authentication is partially configured — missing %s; "+
		"github_app_id, github_app_private_key_path, and github_app_installation_id must all be set "+
		"together to authenticate as a GitHub App installation, or none of them at all to use a "+
		"personal access token (FABRIK_TOKEN) instead. See docs/USER_GUIDE.md",
		strings.Join(missing, ", "))
}

// RefuseGHESWithGitHubApp refuses the GHES-host + GitHub-App-auth
// combination outright rather than silently attempting it: internal/
// githubauth's client construction (mintAuth) unconditionally builds a
// gh.NewClientWithBaseURL client, which is documented as deriving an
// incorrect GraphQL endpoint for a GHES host (NewClientForHost exists
// specifically because GHES needs independently-derived REST/GraphQL
// paths). This is a pre-existing gap in internal/githubauth this issue does
// not fix — the safe move is a loud config-time refusal, not a client that
// fails obscurely later against the wrong endpoint.
//
// Exported (review finding, PR #1731) alongside RequiredGitHubAppPermissions/
// RefuseUserOwnedBoardForAppAuth so `fabrik init --github-app` (#1715) can
// refuse this combination at setup time too — without it, setup would
// register/adopt an App against github.com even when --ghes-host is set,
// then write a github_app_*/ghes_host combination the engine refuses
// unconditionally on its very next startup. Takes the resolved ghesHost
// string directly, not a Config, since cmd/init.go has no engine.Config to
// hand it — resolveGitHubAppAuth below passes cfg.GHESHost.
func RefuseGHESWithGitHubApp(ghesHost string) error {
	if ghesHost == "" {
		return nil
	}
	return fmt.Errorf("GitHub App authentication cannot be combined with a GitHub Enterprise Server host "+
		"(ghes_host %q) — internal/githubauth's client construction does not yet derive the correct GHES "+
		"endpoints; remove ghes_host to use GitHub App auth against github.com, or remove the GitHub App "+
		"config to use a personal access token against this GHES instance instead", ghesHost)
}

// RefuseHTTPSWorkerGitUnderAppAuth refuses App auth + default HTTPS worker
// git loudly at startup, mirroring RefuseGHESWithGitHubApp's shape (#1756,
// R2). Under App auth, buildClaudeEnv (engine/claude.go) injects the
// installation token as GH_TOKEN/GITHUB_TOKEN into every stage worker's
// environment. RequiredGitHubAppPermissions grants `contents:read` (added by
// #1755, for the engine's own compare/merge calls) but not `contents:write`
// — so a worker `git fetch` over the bare clone's default HTTPS remote
// (buildCloneURL, engine/worktree.go) would likely succeed, but
// `git push`/`git push --force-with-lease` would still resolve credentials
// through a `gh auth setup-git`-style helper straight to that
// write-ungranted token and 403. Two configurations mask this entirely:
// gitSSH (the clone/push protocol is SSH, so no HTTPS credential helper is
// ever consulted) and hasSSHRewrite (a global
// url.git@github.com:.insteadOf = https://github.com/ rewrite transparently
// redirects the HTTPS remote to SSH before git ever asks a credential
// helper for anything). Outside those two cases, silently depending on host
// git config is exactly the failure mode this function exists to eliminate
// — see ADR-1756.
//
// Deliberately does not attempt to distinguish "no credential helper
// configured" (already covered, advisory-only, by checkHTTPSCredentials)
// from "a credential helper is configured and will hand the worker's git a
// bad token": under App auth, the latter is the default outcome on any
// machine where `gh auth setup-git` (or an equivalent helper) has ever been
// run, which is common enough that a hard refusal is the safer default
// rather than a best-effort probe. It also does not attempt to distinguish
// "worker only ever fetches" from "worker also needs to push" — every
// managed stage commits and pushes its own work (see CLAUDE.md's "Commit
// frequently" convention), so the push failure is reachable from every
// stage, not a corner case worth probing around.
func RefuseHTTPSWorkerGitUnderAppAuth(gitSSH, hasSSHRewrite bool) error {
	if gitSSH || hasSSHRewrite {
		return nil
	}
	return fmt.Errorf("GitHub App authentication is configured with default HTTPS git cloning — under App auth, " +
		"stage workers authenticate gh/git via the installation token (see RequiredGitHubAppPermissions), which " +
		"is granted `contents:read` but not `contents:write`, so a worker's `git push` over the default HTTPS " +
		"remote would 403 as soon as any git credential helper (e.g. one registered by `gh auth setup-git`) " +
		"resolves credentials from the GH_TOKEN/GITHUB_TOKEN environment (fetch alone would likely succeed, but " +
		"every managed stage also commits and pushes). Fix by either setting git_ssh: true (or --ssh) in " +
		".fabrik/config.yaml so worktrees clone over SSH instead, or configuring a global " +
		"url.git@github.com:.insteadOf = https://github.com/ rewrite so HTTPS remotes are transparently sent " +
		"over SSH. See ADR-1756 and docs/USER_GUIDE.md")
}

// RefuseWebhooksWithGitHubApp refuses the --webhooks + GitHub-App-auth
// combination outright rather than letting it silently degrade to polling
// (#1752): `gh webhook forward` (engine/webhook.go's webhookManager) is
// feature-gated to user tokens by GitHub CLI itself — an installation token
// gets "you do not have access to this feature", measured directly against
// a real App installation, not a documented claim. No App permission grant
// fixes this; it is not a permission problem. Mirrors RefuseGHESWithGitHubApp
// above exactly: nil for the compatible case, a descriptive error otherwise
// naming both settings and both ways out.
func RefuseWebhooksWithGitHubApp(webhooksEnabled bool) error {
	if !webhooksEnabled {
		return nil
	}
	return fmt.Errorf("GitHub App authentication cannot be combined with --webhooks (FABRIK_WEBHOOKS) — " +
		"gh webhook forward is feature-gated to user tokens and refuses a GitHub App installation token " +
		"outright (\"you do not have access to this feature\"); no App permission grant fixes this. " +
		"Remove --webhooks to use GitHub App auth with --reconcile-interval polling instead, or remove the " +
		"GitHub App config to use --webhooks with a personal access token (FABRIK_TOKEN)")
}

// FormatPermissionShortfalls renders R3's "name each missing permission"
// requirement as one human-readable, deterministically-ordered string —
// checkGrantedPermissions already sorts shortfalls by permission name.
func FormatPermissionShortfalls(shortfalls []githubauth.RequiredPermissionShortfall) string {
	parts := make([]string, len(shortfalls))
	for i, s := range shortfalls {
		granted := s.Granted
		if granted == "" {
			granted = "none"
		}
		parts[i] = fmt.Sprintf("%s (required %q, granted %q)", s.Permission, s.Required, granted)
	}
	return strings.Join(parts, "; ")
}

// RefuseUserOwnedBoardForAppAuth is R4: a user-owned board must be refused
// explicitly under App auth, not left to fail silently. Without this check,
// GitHub simply strips organization-scoped permissions (including Projects
// v2 access) from a user-account installation, the board fetch then returns
// empty, and startup board validation reports a confusing "stage names
// missing from board" — sending an operator chasing a config error that
// does not exist. ownerType is resolved live via (*gh.Client).ResolveOwner
// (ADR-1714's primitive), not ProjectBoard.OwnerType, which is merely an
// echo of a caller-supplied argument in the common case and would only
// catch a mismatch that happened to already be reflected in config.
//
// Mirrors cmd/board_admin.go's refuseIfUserOwnedBoard wording — duplicated
// rather than shared, since engine cannot import cmd (cmd imports engine);
// a future edit to one should check the other for drift.
func RefuseUserOwnedBoardForAppAuth(client *gh.Client, owner string) error {
	_, ownerType, err := client.ResolveOwner(owner)
	if err != nil {
		return fmt.Errorf("resolving owner %q to check organization/user type: %w", owner, err)
	}
	if ownerType != "user" {
		return nil
	}
	return fmt.Errorf("refusing: GitHub App authentication requires an organization-owned project board — "+
		"%q is a user account, and GitHub strips organization-scoped permissions (including Projects v2 "+
		"access) from user-account installations (see #770); use a personal access token (FABRIK_TOKEN) "+
		"for a user-owned board instead — App auth is not available for this case, permanently", owner)
}

// setUpGitHubAppAuth performs R1/R3/R4's App-auth construction: reconciling
// the pinned installation (skipping discovery/manifest entirely — see
// Reconcile's compat-mode pin), obtaining a single owner-scoped client (the
// engine holds one client, not Pruefer's per-owner map — see the engine's
// own single e.client field), refusing a user-owned board (R4), and failing
// hard on any granted-permission shortfall (R3). Returns the minted client
// and the Reconciler whose refresh loop the caller must run for the
// engine's lifetime (see poll.go's Run()).
//
// cfg.Owner + "/*" is passed as Options.WatchedRepos: the pinned branch of
// Reconcile populates its client map from the *owners* named there — the
// repo half of each entry is never consulted for that purpose (only for a
// diagnostics-only repo cache) — and the engine has no watched-repo concept
// of its own to supply instead. cfg.Owner is always non-empty by the time
// New() runs (cmd/root.go requires it), and it is always the login that
// owns the project board — including in multi-repo mode, where cfg.Repo may
// be empty but cfg.Owner names the board's own organization.
func setUpGitHubAppAuth(ctx context.Context, cfg Config, fabrikDir, baseURL string) (*gh.Client, *githubauth.Reconciler, error) {
	required := RequiredGitHubAppPermissions(cfg.Webhooks)

	// Options.RequiredPermissions is deliberately left unset here (rather
	// than passed required): Reconcile's own verifyPinnedGrants would only
	// use it for ADR-1709's soft, log-only check — a second
	// GET /app/installations/{id} round trip whose entire effect (logging a
	// shortfall) is strictly subsumed by VerifyGrants' fail-hard check
	// below, which runs moments later against the same required set and
	// fails startup outright instead of merely logging. Skipping it here
	// avoids minting a redundant JWT and making a duplicate API call on
	// every startup for no additional coverage.
	reconciler, err := githubauth.Reconcile(ctx, githubauth.Options{
		AppID:             cfg.GitHubAppID,
		AppInstallationID: cfg.GitHubAppInstallationID,
		AppPrivateKeyPath: cfg.GitHubAppPrivateKeyPath,
		AppStatePath:      GitHubAppStatePath(fabrikDir),
		WatchedRepos:      []string{cfg.Owner + "/*"},
		BaseURL:           baseURL, // "" in production (github.com); tests point this at an httptest server
		AppName:           GitHubAppName,
		AppHomepageURL:    GitHubAppHomepageURL,
		Logf:              func(format string, args ...any) { fmt.Printf("[startup] github-app: "+format+"\n", args...) },
	})
	if err != nil {
		return nil, nil, fmt.Errorf("reconciling GitHub App auth: %w", err)
	}

	client, err := reconciler.ClientForRepo(ctx, cfg.Owner, cfg.Repo)
	if err != nil {
		return nil, nil, fmt.Errorf("obtaining GitHub App installation client for owner %q: %w", cfg.Owner, err)
	}

	// R4 before R3 (deliberately): GitHub never grants organization_projects
	// to a user-account installation, so a user-owned board would also fail
	// the R3 grant check below — but with a confusing "missing
	// organization_projects" message instead of naming the actual, more
	// fundamental cause. Checking ownership first gives the clearer,
	// more actionable error.
	if err := RefuseUserOwnedBoardForAppAuth(client, cfg.Owner); err != nil {
		return nil, nil, err
	}

	shortfalls, err := reconciler.VerifyGrants(required)
	if err != nil {
		return nil, nil, fmt.Errorf("verifying GitHub App installation's granted permissions: %w", err)
	}
	if len(shortfalls) > 0 {
		return nil, nil, fmt.Errorf("GitHub App installation %d is missing required permissions: %s — "+
			"grant these permissions to the installation (App settings → Install App → Configure) and "+
			"restart Fabrik", cfg.GitHubAppInstallationID, FormatPermissionShortfalls(shortfalls))
	}

	fmt.Printf("[startup] authenticated as %s (GitHub App installation, organization %q)\n", reconciler.BotLogin(), cfg.Owner)
	return client, reconciler, nil
}

// selfLogin returns Fabrik's own GitHub-facing identity: the login every
// comment/review the engine itself posts actually carries as author on the
// wire. Under App auth this is the installation's bot login
// (e.ghAppAuth.BotLogin(), "<app-slug>[bot]"); under PAT mode it is the
// operator's login (e.cfg.User) — true by construction, since PAT mode
// posts under the operator's own account.
//
// Every "is this comment/review mine?" comparison and every cache
// author write-through MUST route through this accessor rather than
// testing e.cfg.User directly — e.cfg.User only coincides with Fabrik's
// actual posting identity in PAT mode. See issue #1754 (S1-S3): four call
// sites tested e.cfg.User directly and silently misbehaved under App auth
// (durable review-suppression went permanently inert, the blocked-
// dependencies comment was never updated, and cached comments read as
// "human" pre-refetch but "bot" post-refetch).
//
// Exception: fabrik:locked:<user> and itemstate.LocalLockAcquired are
// deliberately about which *operator* holds a local advisory lock, not
// about Fabrik's own posting identity — they must keep using e.cfg.User
// directly and must never be routed through this accessor.
func (e *Engine) selfLogin() string {
	if e.ghAppAuth != nil {
		return e.ghAppAuth.BotLogin()
	}
	return e.cfg.User
}

// resolveGitHubAppAuth is New()'s single entry point for everything in this
// file: validates config (R5), refuses an unsupported GHES combination, and
// — only when App auth is actually configured — performs the full R1/R3/R4
// construction. Returns (nil, nil, nil) for PAT mode (the unconfigured
// case), which New() interprets as "build the client from cfg.Token exactly
// as before" (R1/AC2). baseURL is "" in production (github.com); tests pass
// an httptest server URL to exercise this without a real GitHub App.
func resolveGitHubAppAuth(ctx context.Context, cfg Config, fabrikDir, baseURL string) (*gh.Client, *githubauth.Reconciler, error) {
	if err := validateGitHubAppConfig(cfg); err != nil {
		return nil, nil, err
	}
	if !gitHubAppAuthConfigured(cfg) {
		return nil, nil, nil
	}
	if err := RefuseGHESWithGitHubApp(cfg.GHESHost); err != nil {
		return nil, nil, err
	}
	if err := RefuseWebhooksWithGitHubApp(cfg.Webhooks); err != nil {
		return nil, nil, err
	}
	// Both a PAT and a full GitHub App config can be present at once (e.g.
	// an operator migrating from one to the other without yet clearing
	// FABRIK_TOKEN). App auth always wins in that case — cfg.Token is never
	// referenced again for the engine's own GitHub client or worker gh auth
	// (releaseUpgradeToken's use of it is a separate, unrelated concern).
	// This is a deliberate precedence, not an ambiguous config R5 should
	// refuse (unlike a partial App config, "both fully set" is unambiguous
	// — App auth is the more specific, more recently configured intent) —
	// but silently ignoring a still-valid credential with no trace in the
	// log is exactly the kind of surprise the "co-equal, not a silent
	// fallback" framing elsewhere in this file wants to avoid. So it's
	// logged, not silent.
	if cfg.Token != "" {
		fmt.Printf("[startup] github-app: GitHub App authentication is configured and takes precedence over the " +
			"configured personal access token (FABRIK_TOKEN) for GitHub API calls and worker gh CLI auth — but " +
			"git clone/fetch/push (engine's own and, unless git_ssh/an SSH rewrite is configured, workers' too) " +
			"may still depend on the PAT via ambient credentials; do not revoke it until git_ssh or an SSH " +
			"rewrite is confirmed in place (see ADR-1756)\n")
	}
	return setUpGitHubAppAuth(ctx, cfg, fabrikDir, baseURL)
}
