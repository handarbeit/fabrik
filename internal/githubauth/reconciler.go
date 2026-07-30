package githubauth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"sync"

	gh "github.com/handarbeit/fabrik/github"
)

// Options configures Reconcile. It mirrors the subset of the caller's own
// config the reconciler needs — this package must never import pruefer (see
// doc.go), so callers pass these fields explicitly rather than a
// pruefer.Config.
type Options struct {
	// AppID and AppInstallationID mirror pruefer.Config's compat-mode
	// fields: an explicit AppID (from config.yaml/env/flag) always wins
	// over anything discovered by a prior manifest run; AppInstallationID,
	// when non-zero, pins every watched repo to one installation and skips
	// discovery entirely — see ADR-1233 Decision 4, preserved here
	// unchanged.
	AppID             int64
	AppInstallationID int64
	// AppPrivateKeyPath is where the App's PEM lives — both the compat path
	// (an operator-registered App) and a freshly manifest-created App's key
	// are read from and written to this same path, which is what makes the
	// two indistinguishable to Reconcile after one successful run.
	AppPrivateKeyPath string
	// AppStatePath is where reconciler-owned, non-key metadata (App ID once
	// manifest-created, slug, webhook secret, client ID/secret) is
	// persisted. Never config.yaml — see the Plan's "config.yaml is never
	// written back to" decision.
	AppStatePath string
	WatchedRepos []string
	// NoBrowser is forwarded to RunManifestFlow unchanged.
	NoBrowser bool
	// BaseURL selects GitHub's API host. "" = production; tests point it at
	// an httptest server.
	BaseURL string
	Logf    func(format string, args ...any)
}

// GitHubAuth is the narrow interface the rest of a caller (e.g. Pruefer's
// daemon) depends on: a way to get a token-scoped *github.Client for a
// given repo, and the App's own bot identity. Callers never see PEMs, JWTs,
// installation IDs, browser flows, or refresh loops — this is the
// token-provider boundary the issue requires, generalizing
// pruefer/auth.go's former AuthSet.Clients map lookup into a
// repo-granular, error-returning call.
type GitHubAuth interface {
	ClientForRepo(ctx context.Context, owner, repo string) (*gh.Client, error)
	BotLogin() string
}

// Reconciler is the concrete GitHubAuth Reconcile returns. It holds one
// *gh.Client (and its own token-refresh Auth) per distinct owner covered by
// WatchedRepos — the same security property pruefer/auth.go's AuthSet
// established (see ADR-1233): only owners actually named in WatchedRepos are
// ever tokenized or contacted.
type Reconciler struct {
	botLogin string

	mu      sync.Mutex
	clients map[string]*gh.Client
	auths   []*Auth
}

// BotLogin returns the App's own identity as it appears as a PR/review
// author (always "<slug>[bot]").
func (r *Reconciler) BotLogin() string { return r.botLogin }

// ClientForRepo returns the *github.Client scoped to owner's installation.
// repo is accepted (per the issue's literal interface requirement) but
// unused today — every client is owner-scoped, not repo-scoped, matching
// how GitHub App installation tokens actually work; a future
// selected-mode-aware routing could use it without changing callers.
func (r *Reconciler) ClientForRepo(ctx context.Context, owner, repo string) (*gh.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	client, ok := r.clients[owner]
	if !ok {
		return nil, fmt.Errorf("no authorized GitHub App installation for owner %q — add it to watched_repos and restart to trigger reconciliation, or install the App on %q if it's already watched", owner, owner)
	}
	return client, nil
}

// RunRefreshLoops starts one token-refresh goroutine per distinct
// installation this Reconciler minted — see Auth.RunRefreshLoop. logf is
// called once per Auth, synchronously, to build that installation's own log
// function before its goroutine is spawned.
func (r *Reconciler) RunRefreshLoops(ctx context.Context, logf func(installationID int64) func(format string, args ...any)) {
	r.mu.Lock()
	auths := append([]*Auth(nil), r.auths...)
	r.mu.Unlock()
	for _, a := range auths {
		go a.RunRefreshLoop(ctx, logf(a.InstallationID))
	}
}

// InstallationCount returns the number of distinct installations minted —
// used for startup logging.
func (r *Reconciler) InstallationCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.auths)
}

// runManifestFlow is a package var (not a direct call to RunManifestFlow)
// so tests can assert it is never invoked on the backward-compat path —
// existing valid local credentials must skip the manifest flow entirely,
// with zero prompts and zero behavior change (see the issue's backward
// compatibility requirement).
var runManifestFlow = RunManifestFlow

// Reconcile drives Pruefer's (or any caller's) GitHub App auth to the
// desired state described in the issue:
//
//  1. Load local auth state.
//  2. If credentials are missing entirely, run the manifest bootstrap.
//  3. Validate app identity against GitHub.
//  4. Load desired repos (opts.WatchedRepos).
//  5. Discover the app's installations.
//  6. Map each watched repo to an installation.
//  7. For any owner not covered, guide the user through installation.
//  8. Verify access to every watched repo (soft, selected-mode only).
//  9. Reach READY — return the built Reconciler.
//
// Every step is safe to retry: a failure partway through leaves no
// half-written state (RunManifestFlow only persists on full success) and
// callers can simply call Reconcile again.
func Reconcile(ctx context.Context, opts Options) (*Reconciler, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	appID, privateKey, err := loadOrBootstrapCredentials(ctx, opts, logf)
	if err != nil {
		return nil, err
	}

	jwt, err := gh.BuildAppJWT(appID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("building app JWT: %w", err)
	}
	slug, err := gh.FetchAppSlug(opts.BaseURL, jwt)
	if err != nil {
		// Only a JWT the App API actively rejected (401/403) or an App ID it
		// reports as gone (404) is treated as a sign the App may have been
		// deleted. Any other error — a transport failure, timeout, or a 5xx
		// — is transient: appRequest doesn't distinguish those from a real
		// rejection by string alone, and treating them identically would
		// mean an ordinary network hiccup at startup (the steady-state
		// restart path for every self-hosted operator, not just first-run)
		// kicks off a full re-bootstrap or "repair required" error instead
		// of a retry. Bubble the transient error up unchanged so the caller
		// (e.g. Pruefer's daemon) can retry Reconcile on its own terms.
		if !errors.Is(err, gh.ErrAppUnauthorized) && !errors.Is(err, gh.ErrNotFound) {
			return nil, fmt.Errorf("validating app identity for App ID %d: %w (transient failure, not treated as app deletion — retry)", appID, err)
		}
		// App-identity validation was actively rejected against a
		// locally-known AppID: the App may have been deleted externally.
		//
		// If AppID is pinned via opts.AppID (explicit github_app_id config —
		// the compat-mode path), auto-recreating here would be unsafe: the
		// manifest flow persists the new App's ID only to AppStatePath, but
		// loadOrBootstrapCredentials always prefers a non-zero opts.AppID
		// first (config.yaml is never written back to — see Credentials'
		// doc). The next restart would resolve the same stale, now-deleted
		// AppID again, fail identity validation again, and silently create
		// yet another orphan App — every restart, forever. Surface an
		// explicit repair error instead, naming the fix (update or clear
		// github_app_id), rather than looping.
		if opts.AppID != 0 {
			return nil, fmt.Errorf("github_app_id %d is configured but no longer resolves on GitHub (%w) — it may have been deleted; update or remove github_app_id in config to let first-run setup create a new App (repair required, not auto-recreated)", opts.AppID, err)
		}
		// AppID was resolved from the reconciler-owned state file (a prior
		// manifest run), not pinned by config — safe to self-heal: the next
		// restart resolves the freshly-created App's ID from AppStatePath
		// with no stale config value in the way. Preserve existing
		// non-secret config (RunManifestFlow only overwrites on full
		// success) and re-enter the manifest flow rather than silently
		// giving up — see the issue's "App deleted externally" failure
		// handling requirement.
		logf("! app identity validation failed for App ID %d (%v) — it may have been deleted; starting App creation again", appID, err)
		creds, bootErr := runManifestFlow(ctx, ManifestFlowOptions{
			BaseURL: opts.BaseURL, NoBrowser: opts.NoBrowser,
			PrivateKeyPath: opts.AppPrivateKeyPath, AppStatePath: opts.AppStatePath, Logf: logf,
		})
		if bootErr != nil {
			return nil, fmt.Errorf("app identity validation failed (%w) and re-creating the App also failed: %v", err, bootErr)
		}
		appID = creds.AppID
		privateKey, err = loadPrivateKey(opts.AppPrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("reading freshly-created App's private key: %w", err)
		}
		jwt, err = gh.BuildAppJWT(appID, privateKey)
		if err != nil {
			return nil, fmt.Errorf("building app JWT after re-creating the App: %w", err)
		}
		slug, err = gh.FetchAppSlug(opts.BaseURL, jwt)
		if err != nil {
			return nil, fmt.Errorf("validating freshly-created App's identity: %w", err)
		}
	}
	botLogin := slug + "[bot]"
	logf("✓ authenticated as %s", botLogin)

	r := &Reconciler{botLogin: botLogin, clients: map[string]*gh.Client{}}

	for _, spec := range opts.WatchedRepos {
		if _, _, ok := splitOwnerRepo(spec); !ok {
			logf("! %q is not a valid \"owner/repo\" watched-repo entry — skipping", spec)
		}
	}

	owners := distinctOwners(opts.WatchedRepos)
	if len(owners) == 0 {
		logf("no watched repos configured — reconciliation has nothing to authorize")
		return r, nil
	}

	// Compat path: a pinned installation ID skips discovery entirely,
	// preserving pre-reconciler behavior byte-for-byte — see ADR-1233
	// Decision 4.
	if opts.AppInstallationID != 0 {
		a, err := mintAuth(appID, opts.AppInstallationID, botLogin, privateKey, opts.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("minting pinned installation token: %w", err)
		}
		for _, owner := range owners {
			r.clients[owner] = a.client
		}
		r.auths = []*Auth{a}
		logf("✓ using pinned installation %d for all watched repos", opts.AppInstallationID)
		return r, nil
	}

	installations, err := gh.FetchAppInstallations(opts.BaseURL, jwt)
	if err != nil {
		return nil, fmt.Errorf("discovering app installations: %w", err)
	}
	byAccount := make(map[string]gh.AppInstallation, len(installations))
	for _, inst := range installations {
		byAccount[inst.Account] = inst
	}

	// repoCache accumulates the "which of this owner's watched repos does
	// the installation actually grant access to" diagnostic the issue's
	// credential-model requirement asks the reconciler to persist
	// (Credentials.InstallationRepoCache) — populated here, from data this
	// loop already fetches, and saved once discovery completes. Purely a
	// human-readable cache: Reconcile always re-verifies live against
	// GitHub on the next run rather than trusting it (see that field's own
	// doc comment).
	repoCache := make(map[string][]string)

	// openedInstallBrowser bounds guided-installation browser-opening to at
	// most once per Reconcile call: WatchedRepos can plausibly span several
	// owners with no installation at all (e.g. a first run watching repos
	// under three different orgs), and popping open a separate browser
	// tab/window per missing owner is surprising, unlike the single-flow
	// manifest bootstrap. Every missing owner still gets its install URL
	// logged; only the first one also gets an actual browser-open attempt.
	openedInstallBrowser := false
	for _, owner := range owners {
		inst, ok := byAccount[owner]
		if !ok {
			installURL := fmt.Sprintf("https://github.com/apps/%s/installations/new", slug)
			if !opts.NoBrowser && !openedInstallBrowser {
				logf("! %s has no installation → opening %s …", owner, installURL)
				if err := openBrowser(installURL); err != nil {
					logf("could not open browser automatically (%v) — visit the URL above manually", err)
				}
				openedInstallBrowser = true
			} else {
				logf("! %s has no installation → %s", owner, installURL)
			}
			continue
		}

		a, err := mintAuth(appID, inst.ID, botLogin, privateKey, opts.BaseURL)
		if err != nil {
			logf("! minting token for owner %q (installation %d) failed: %v", owner, inst.ID, err)
			continue
		}

		if inst.RepositorySelection == "selected" {
			statuses, err := verifyRepoAccess(opts.BaseURL, a.client.Token(), inst.ID, owner, opts.WatchedRepos)
			if err != nil {
				logf("! verifying repo access for owner %q failed: %v", owner, err)
			}
			for _, st := range statuses {
				if st.Authorized {
					repoCache[owner] = append(repoCache[owner], st.Repo)
				} else {
					logf("! %s is not authorized (%s) → %s", st.Repo, st.Reason, st.GrantURL)
				}
			}
		} else {
			// An "all" installation grants access to every current and
			// future repo on the account, so every one of owner's watched
			// repos is authorized — no live per-repo check to make, unlike
			// the "selected" branch above.
			for _, spec := range opts.WatchedRepos {
				if o, _, ok := splitOwnerRepo(spec); ok && o == owner {
					repoCache[owner] = append(repoCache[owner], spec)
				}
			}
		}

		r.clients[owner] = a.client
		r.auths = append(r.auths, a)
		logf("✓ %s authorized", owner)
	}

	saveInstallationRepoCache(opts.AppStatePath, appID, slug, repoCache, logf)

	return r, nil
}

// saveInstallationRepoCache persists repoCache into AppStatePath's
// Credentials.InstallationRepoCache, refreshing AppID/Slug to the identity
// Reconcile just validated live while preserving every other already-stored
// field (webhook secret, client ID/secret) untouched. Best-effort: this
// cache is diagnostics-only (see Credentials.InstallationRepoCache's doc
// comment) and a write failure here must never fail reconciliation itself.
func saveInstallationRepoCache(statePath string, appID int64, slug string, repoCache map[string][]string, logf func(string, ...any)) {
	existing, err := loadCredentials(statePath)
	if err != nil {
		// loadOrBootstrapCredentials already validated AppStatePath is
		// either absent or parseable earlier in this same Reconcile call,
		// so a failure here would be surprising — log and move on rather
		// than failing reconciliation over a diagnostics-only cache.
		logf("could not update installation-repo cache: %v", err)
		return
	}
	existing.AppID = appID
	existing.Slug = slug
	existing.InstallationRepoCache = repoCache
	if err := saveCredentials(statePath, existing); err != nil {
		logf("could not persist installation-repo cache: %v", err)
	}
}

// loadOrBootstrapCredentials implements state-machine steps 1-2. Three
// credential-problem cases are distinguished per the issue's
// failure-handling requirements:
//   - no AppID anywhere (opts.AppID, nor a prior manifest run's
//     app-state.json) → nothing to load; run the manifest flow ("missing").
//   - an AppID is known (either way) but the private key file is missing or
//     unparseable → an explicit repair error, never auto-regenerated
//     ("repair-needed"). A corrupt-but-recoverable local file must never be
//     mistaken for "the App is gone."
//   - both are present and readable → return them; Reconcile's later
//     identity-validation call is what detects an externally-deleted App
//     ("app-deleted-externally") — that case is not this function's job.
func loadOrBootstrapCredentials(ctx context.Context, opts Options, logf func(string, ...any)) (int64, *rsa.PrivateKey, error) {
	appID := opts.AppID
	if appID == 0 {
		state, err := loadCredentials(opts.AppStatePath)
		if err != nil {
			return 0, nil, fmt.Errorf("app state file %s exists but is corrupt — repair or remove it: %w", opts.AppStatePath, err)
		}
		appID = state.AppID
	}

	if appID == 0 {
		logf("no usable local GitHub App credentials found — starting first-run setup")
		creds, err := runManifestFlow(ctx, ManifestFlowOptions{
			BaseURL: opts.BaseURL, NoBrowser: opts.NoBrowser,
			PrivateKeyPath: opts.AppPrivateKeyPath, AppStatePath: opts.AppStatePath, Logf: logf,
		})
		if err != nil {
			return 0, nil, fmt.Errorf("first-run GitHub App setup: %w", err)
		}
		appID = creds.AppID
	}

	if _, err := os.Stat(opts.AppPrivateKeyPath); os.IsNotExist(err) {
		return 0, nil, fmt.Errorf("github_app_id %d is configured but its private key %s is missing — restore it from the App's settings page (repair required, not auto-regenerated)", appID, opts.AppPrivateKeyPath)
	}
	privateKey, err := loadPrivateKey(opts.AppPrivateKeyPath)
	if err != nil {
		return 0, nil, fmt.Errorf("github_app_id %d is configured but its private key %s is unreadable/corrupt — restore it from the App's settings page (repair required, not auto-regenerated): %w", appID, opts.AppPrivateKeyPath, err)
	}

	return appID, privateKey, nil
}
