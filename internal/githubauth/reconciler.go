package githubauth

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// identityValidationRetryDelays are the pauses between retries when the App
// this exact Reconcile call just manifest-created immediately fails its own
// first identity check — see the justBootstrapped branch in Reconcile.
var identityValidationRetryDelays = []time.Duration{250 * time.Millisecond, 750 * time.Millisecond}

// identityValidationSleep is identityValidationRetryDelays' delay function —
// a package var so tests can replace it with a no-op instead of sleeping for
// real.
var identityValidationSleep = time.Sleep

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
//
// clients is keyed by lower-cased owner: GitHub org/user logins are
// case-insensitive, and callers (e.g. pruefer/execute.go, pruefer/daemon.go)
// may derive an owner string from watched_repos independently of — and with
// different casing than — whatever casing Reconcile itself first saw, so
// ClientForRepo must resolve either casing to the same client.
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
	client, ok := r.clients[strings.ToLower(owner)]
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

	appID, privateKey, justBootstrapped, err := loadOrBootstrapCredentials(ctx, opts, logf)
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
		// justBootstrapped means loadOrBootstrapCredentials manifest-created
		// this exact App a few lines above, in this same Reconcile call. An
		// immediate 401/403/404 here is far more plausibly brief
		// propagation lag on a brand-new App (or a network blip that
		// happens to land on one of those status codes) than genuine
		// external deletion seconds after creation — GitHub hasn't had a
		// chance to delete anything yet. Treating it as deletion would walk
		// the user through creating a *second* App with no indication the
		// first is a duplicate caused by this race, rather than a real
		// external deletion. Retry the identity check a few times first;
		// only self-heal (re-enter the manifest flow) below when the App
		// was NOT just created in this call.
		if justBootstrapped {
			for _, delay := range identityValidationRetryDelays {
				identityValidationSleep(delay)
				if slug, err = gh.FetchAppSlug(opts.BaseURL, jwt); err == nil {
					break
				}
			}
			if err != nil {
				return nil, fmt.Errorf("just created App ID %d but its identity still doesn't resolve on GitHub after %d retries (%w) — not treated as deletion since the App was only just created in this run; retry Reconcile", appID, len(identityValidationRetryDelays), err)
			}
		} else if opts.AppInstallationID != 0 {
			// AppID itself came from the state file (not pinned), but
			// opts.AppInstallationID *is* pinned — and installation IDs are
			// App-specific. Self-healing here (silently creating a new App)
			// would produce a *new* appID while opts.AppInstallationID still
			// names an installation of the just-deleted App; the
			// opts.AppInstallationID != 0 branch below would then mint
			// against a mismatched (appID, installationID) pair and fail
			// with an opaque GitHub error instead of a clear diagnosis.
			// Surface an explicit repair error instead, exactly like the
			// pinned-AppID case above — an operator using the
			// installation-ID pin must also decide what to do about it
			// before Reconcile creates a new App out from under it.
			return nil, fmt.Errorf("github_app_installation_id %d is configured but App ID %d no longer resolves on GitHub (%w) — it may have been deleted, which also invalidates the pinned installation ID (installation IDs are App-specific); update or remove github_app_id/github_app_installation_id in config to let first-run setup create a new App and installation (repair required, not auto-recreated)", opts.AppInstallationID, appID, err)
		} else {
			// AppID was resolved from the reconciler-owned state file (a
			// prior manifest run), not pinned by config — safe to
			// self-heal: the next restart resolves the freshly-created
			// App's ID from AppStatePath with no stale config value in the
			// way. Preserve existing non-secret config (RunManifestFlow
			// only overwrites on full success) and re-enter the manifest
			// flow rather than silently giving up — see the issue's "App
			// deleted externally" failure handling requirement.
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
	}
	botLogin := slug + "[bot]"
	logf("✓ authenticated as %s", botLogin)

	r := &Reconciler{botLogin: botLogin, clients: map[string]*gh.Client{}}

	// A single pass both validates and enumerates — see
	// distinctOwnersLogging's own doc comment for why this replaced two
	// separate passes (one purely to log malformed entries, one via
	// distinctOwners to actually use them) that re-ran the identical
	// splitOwnerRepo check over the same slice.
	owners := distinctOwnersLogging(opts.WatchedRepos, logf)
	if len(owners) == 0 {
		logf("no watched repos configured — reconciliation has nothing to authorize")
		return r, nil
	}

	// Compat path: a pinned installation ID skips discovery entirely,
	// preserving pre-reconciler behavior byte-for-byte — see ADR-1233
	// Decision 4. Every watched repo is cached as authorized under the
	// pinned installation: unlike the discovery path below, there is no
	// per-owner "is this actually covered" question here — the operator
	// has explicitly asserted the pin covers everything.
	//
	// This trust model is unchanged by the manifest flow adding a
	// first-run path: RunManifestFlow/loadOrBootstrapCredentials never set
	// AppInstallationID themselves — it is only ever populated from
	// opts.AppInstallationID, i.e. an explicit github_app_installation_id
	// in config, env, or flag (pruefer/config.go). A genuinely first-run
	// operator who has never manually set that field always goes through
	// discovery below instead. If watched_repos spans more than one owner
	// and the pin covers only some of them, requests against an uncovered
	// owner's repo will fail at actual GitHub API call time (403/404) with
	// no reconcile-time warning — accepted here because it's the exact
	// legacy behavior this compat path exists to preserve, not a
	// regression introduced by the reconciler.
	if opts.AppInstallationID != 0 {
		a, err := mintAuth(appID, opts.AppInstallationID, botLogin, privateKey, opts.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("minting pinned installation token: %w", err)
		}
		repoCache := make(map[string][]string)
		for _, owner := range owners {
			r.clients[strings.ToLower(owner)] = a.client
		}
		// Keyed by strings.ToLower(owner), matching r.clients two lines
		// above: unlike the discovery path below (which iterates the
		// already-deduped owners slice), this loop iterates every raw
		// watched-repo spec directly, so two entries naming the same
		// account with different casing (e.g. "MyOrg/repo1",
		// "myorg/repo2") must still land in one InstallationRepoCache
		// entry, not two.
		for _, spec := range opts.WatchedRepos {
			if owner, _, ok := splitOwnerRepo(spec); ok {
				repoCache[strings.ToLower(owner)] = append(repoCache[strings.ToLower(owner)], spec)
			}
		}
		r.auths = []*Auth{a}
		saveInstallationRepoCache(opts.AppStatePath, appID, slug, repoCache, logf)
		logf("✓ using pinned installation %d for all watched repos", opts.AppInstallationID)
		return r, nil
	}

	installations, err := gh.FetchAppInstallations(opts.BaseURL, jwt)
	if err != nil {
		return nil, fmt.Errorf("discovering app installations: %w", err)
	}
	// Keyed by lower-cased account login: GitHub org/user logins are
	// case-insensitive, but FetchAppInstallations returns each Account in
	// its canonical case while owner (below) comes verbatim from the
	// user's watched_repos config string — an exact-case map would treat
	// e.g. a config entry "MyOrg/repo" against a canonical "myorg"
	// installation as uninstalled, spuriously re-triggering guided-install
	// guidance every restart despite the installation already existing.
	byAccount := make(map[string]gh.AppInstallation, len(installations))
	for _, inst := range installations {
		byAccount[strings.ToLower(inst.Account)] = inst
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
		inst, ok := byAccount[strings.ToLower(owner)]
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

		// repoVerifyFailed tracks whether the "selected"-mode repo-access
		// check below errored (e.g. a transient failure listing
		// /installation/repositories) — the minted token is still usable
		// either way, but the final "✓ authorized" log line must say so
		// rather than implying repo access was actually confirmed.
		repoVerifyFailed := false
		if inst.RepositorySelection == "selected" {
			statuses, err := verifyRepoAccess(opts.BaseURL, a.client.Token(), inst.ID, owner, opts.WatchedRepos)
			if err != nil {
				logf("! verifying repo access for owner %q failed: %v", owner, err)
				repoVerifyFailed = true
			}
			for _, st := range statuses {
				if st.Authorized {
					repoCache[strings.ToLower(owner)] = append(repoCache[strings.ToLower(owner)], st.Repo)
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
				if o, _, ok := splitOwnerRepo(spec); ok && strings.EqualFold(o, owner) {
					repoCache[strings.ToLower(owner)] = append(repoCache[strings.ToLower(owner)], spec)
				}
			}
		}

		r.clients[strings.ToLower(owner)] = a.client
		r.auths = append(r.auths, a)
		if repoVerifyFailed {
			logf("✓ %s authorized (installation token minted; repo-access verification was skipped — see error above)", owner)
		} else {
			logf("✓ %s authorized", owner)
		}
	}

	saveInstallationRepoCache(opts.AppStatePath, appID, slug, repoCache, logf)

	return r, nil
}

// saveInstallationRepoCache merges repoCache into AppStatePath's
// Credentials.InstallationRepoCache (per-owner), refreshing AppID/Slug to
// the identity Reconcile just validated live. Best-effort: this cache is
// diagnostics-only (see Credentials.InstallationRepoCache's doc comment)
// and a write failure here must never fail reconciliation itself.
//
// repoCache only contains entries for owners Reconcile got a definitive
// answer for this round — an owner whose verifyRepoAccess call hit a
// transient error (e.g. a network blip listing /installation/repositories)
// contributes no entry (see the repoVerifyFailed branch in Reconcile).
// Merging by owner, rather than replacing the whole map, means that
// owner's last known entry survives the transient hiccup instead of being
// wiped to empty — a purely transient failure shouldn't regress a "last
// known good" diagnostic.
//
// If the AppID being reconciled differs from whatever AppID this state
// file was last saved under, the existing entry belongs to a *different*
// App entirely (e.g. an operator switched github_app_id to point at a new
// App while reusing the same AppStatePath) — its WebhookSecret/
// ClientID/ClientSecret and InstallationRepoCache must not be carried
// forward under the new AppID, or they'd sit alongside a mismatched
// identity, e.g. once a future webhook-transport consumer starts reading
// them keyed on AppID.
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
	if existing.AppID != 0 && existing.AppID != appID {
		logf("app-state %s was last saved for App ID %d; discarding its stale webhook/client secrets and repo cache now that App ID %d is active", statePath, existing.AppID, appID)
		existing = Credentials{}
	}
	merged := existing.InstallationRepoCache
	if merged == nil {
		merged = make(map[string][]string)
	}
	for owner, repos := range repoCache {
		merged[owner] = repos
	}
	existing.AppID = appID
	existing.Slug = slug
	existing.InstallationRepoCache = merged
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
//
// The bool return reports whether this call itself just ran the manifest
// flow — Reconcile uses it to avoid mistaking the freshly-created App's own
// first identity check for a sign of external deletion (see the retry loop
// around FetchAppSlug in Reconcile).
func loadOrBootstrapCredentials(ctx context.Context, opts Options, logf func(string, ...any)) (int64, *rsa.PrivateKey, bool, error) {
	appID := opts.AppID
	if appID == 0 {
		state, err := loadCredentials(opts.AppStatePath)
		if err != nil {
			return 0, nil, false, fmt.Errorf("app state file %s exists but is corrupt — repair or remove it: %w", opts.AppStatePath, err)
		}
		appID = state.AppID
	}

	justBootstrapped := false
	if appID == 0 {
		logf("no usable local GitHub App credentials found — starting first-run setup")
		creds, err := runManifestFlow(ctx, ManifestFlowOptions{
			BaseURL: opts.BaseURL, NoBrowser: opts.NoBrowser,
			PrivateKeyPath: opts.AppPrivateKeyPath, AppStatePath: opts.AppStatePath, Logf: logf,
		})
		if err != nil {
			return 0, nil, false, fmt.Errorf("first-run GitHub App setup: %w", err)
		}
		appID = creds.AppID
		justBootstrapped = true
	}

	if _, err := os.Stat(opts.AppPrivateKeyPath); os.IsNotExist(err) {
		return 0, nil, false, fmt.Errorf("github_app_id %d is configured but its private key %s is missing — restore it from the App's settings page (repair required, not auto-regenerated)", appID, opts.AppPrivateKeyPath)
	}
	privateKey, err := loadPrivateKey(opts.AppPrivateKeyPath)
	if err != nil {
		return 0, nil, false, fmt.Errorf("github_app_id %d is configured but its private key %s is unreadable/corrupt — restore it from the App's settings page (repair required, not auto-regenerated): %w", appID, opts.AppPrivateKeyPath, err)
	}

	return appID, privateKey, justBootstrapped, nil
}
