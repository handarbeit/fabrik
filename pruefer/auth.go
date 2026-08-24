package pruefer

import (
	"context"
	"crypto/rsa"
	"fmt"
	"os"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// tokenRefreshMargin is how long before an installation token's actual
// expiry the refresh loop mints a replacement. Installation tokens are valid
// ~1h; refreshing 5 minutes early tolerates clock drift and refresh-call
// latency without ever operating on an expired token. Var (not const) so
// tests can shrink it to exercise the refresh loop without waiting an hour.
var tokenRefreshMargin = 5 * time.Minute

// tokenRefreshRetryDelay is how long RunRefreshLoop waits before retrying a
// failed refresh. Var so tests aren't forced to wait a full minute.
var tokenRefreshRetryDelay = time.Minute

// Auth holds one bootstrapped GitHub App installation token and the
// machinery to keep it fresh on the wrapped *github.Client for the lifetime
// of the daemon. A multi-owner daemon holds one Auth per distinct
// installation it needs — see AuthSet/BootstrapMulti.
type Auth struct {
	AppID          int64
	InstallationID int64
	// BotLogin is the App's own identity as it appears as a PR/review
	// author, e.g. "pruefer-bot[bot]". Always slug + "[bot]" for a GitHub
	// App — this is what makes the "review identity != PR author" guarantee
	// structural rather than operator-discipline-dependent (see ADR-1113).
	// Invariant across every installation of the same App, so every Auth in
	// an AuthSet carries the same value.
	BotLogin string

	privateKey *rsa.PrivateKey
	baseURL    string
	client     *gh.Client

	mu        sync.Mutex
	expiresAt time.Time
}

// refresh mints a new installation token, applies it to the client, and
// records its expiry.
func (a *Auth) refresh() error {
	jwt, err := gh.BuildAppJWT(a.AppID, a.privateKey)
	if err != nil {
		return fmt.Errorf("building app JWT: %w", err)
	}
	token, expiresAt, err := gh.MintInstallationToken(a.baseURL, jwt, a.InstallationID)
	if err != nil {
		return fmt.Errorf("minting installation token: %w", err)
	}
	a.client.SetToken(token)
	a.mu.Lock()
	a.expiresAt = expiresAt
	a.mu.Unlock()
	return nil
}

// ExpiresAt returns the current installation token's expiry, safe for
// concurrent use alongside RunRefreshLoop.
func (a *Auth) ExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expiresAt
}

// RunRefreshLoop refreshes the installation token proactively before it
// expires, until ctx is cancelled. Meant to run in its own goroutine for the
// daemon's lifetime. A failed refresh is logged and retried after
// tokenRefreshRetryDelay rather than crashing the daemon — a transient
// GitHub outage shouldn't take Pruefer down, since the *current* token
// stays valid until its actual (not margin-adjusted) expiry. Each Auth's
// loop is fully independent: a refresh failure on one installation has no
// effect on any other Auth's loop or on the daemon as a whole.
func (a *Auth) RunRefreshLoop(ctx context.Context, logf func(format string, args ...any)) {
	for {
		wait := time.Until(a.ExpiresAt()) - tokenRefreshMargin
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := a.refresh(); err != nil {
			if logf != nil {
				logf("auth: installation token refresh failed, retrying in %s: %v", tokenRefreshRetryDelay, err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(tokenRefreshRetryDelay):
			}
			continue
		}
		if logf != nil {
			logf("auth: installation token refreshed, expires %s", a.ExpiresAt().Format(time.RFC3339))
		}
	}
}

// AuthSet holds the bootstrapped multi-installation auth state Pruefer needs
// to review PRs across every distinct owner present in watched_repos: one
// *github.Client (backed by its own *Auth) per distinct installation
// actually required, plus the App's own bot identity (invariant across
// installations — resolved once, not per owner).
//
// This is also the security boundary described in the issue: BootstrapMulti
// only ever mints a token for an owner that appears in watched_repos. An
// installation on an account nobody listed is never discovered into a
// client, never minted a token, and never contacted — see BootstrapMulti's
// doc comment for the mechanics.
type AuthSet struct {
	BotLogin string
	// Clients maps each watched-repo owner to the *github.Client whose token
	// is scoped to that owner's installation. Every owner present in
	// WatchedRepos has an entry. When AppInstallationID pins a single
	// installation, every owner maps to the same one client.
	Clients map[string]*gh.Client

	// auths holds the distinct underlying *Auth instances, deduplicated by
	// pointer identity so a pinned installation shared by several owners
	// gets exactly one refresh goroutine from RunRefreshLoops, not one per
	// owner.
	auths []*Auth

	// appID, privateKey, and baseURL are the App-level identity BootstrapMulti
	// resolves once; mintOwnerAuth (#1640, R5) reuses them to mint a token
	// for a single additional owner discovered by a config reload, without
	// re-deriving or re-reading the private key file.
	appID      int64
	privateKey *rsa.PrivateKey
	baseURL    string
	// pinnedInstallationID mirrors the Config.AppInstallationID BootstrapMulti
	// was called with: non-zero means every owner — including one newly
	// added at reload time — shares the App's single pinned installation
	// rather than being resolved via per-owner discovery. Set from
	// cfg.AppInstallationID in BootstrapMulti so mintOwnerAuth doesn't need
	// its own Config parameter for this one field.
	pinnedInstallationID int64
}

// BootstrapMulti performs the full GitHub App auth flow for every
// installation Pruefer's configured watched_repos actually require:
//   - parses the private key and resolves the App's own bot login once
//     (both are properties of the App, not of any one installation)
//   - if cfg.AppInstallationID is set, skips discovery entirely and mints
//     one token onto one client shared by every watched owner — preserving
//     pre-multi-installation behavior byte-for-byte for pinned/single-owner
//     deployments
//   - otherwise, derives the distinct owners from cfg.WatchedRepos, fetches
//     the App's installations once, and mints+owns one token per owner by
//     matching AppInstallation.Account == owner
//
// An owner with no matching installation is a hard bootstrap error naming
// the owner. An owner whose installation has repository_selection ==
// "selected" and excludes one of its listed repos is also a hard bootstrap
// error naming the excluded repo. baseURL selects GitHub's API host; pass ""
// for production or an httptest server URL in tests.
func BootstrapMulti(cfg Config, baseURL string) (*AuthSet, error) {
	keyPEM, err := os.ReadFile(cfg.AppPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading GitHub App private key %s: %w", cfg.AppPrivateKeyPath, err)
	}
	privateKey, err := gh.ParseAppPrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App private key %s: %w", cfg.AppPrivateKeyPath, err)
	}

	jwt, err := gh.BuildAppJWT(cfg.AppID, privateKey)
	if err != nil {
		return nil, fmt.Errorf("building app JWT: %w", err)
	}
	slug, err := gh.FetchAppSlug(baseURL, jwt)
	if err != nil {
		return nil, fmt.Errorf("resolving app identity: %w", err)
	}
	botLogin := slug + "[bot]"

	as := &AuthSet{
		BotLogin:             botLogin,
		Clients:              map[string]*gh.Client{},
		appID:                cfg.AppID,
		privateKey:           privateKey,
		baseURL:              baseURL,
		pinnedInstallationID: cfg.AppInstallationID,
	}
	owners := distinctOwners(cfg.WatchedRepos)

	if cfg.AppInstallationID != 0 {
		a, err := mintAuth(cfg.AppID, cfg.AppInstallationID, botLogin, privateKey, baseURL)
		if err != nil {
			return nil, fmt.Errorf("minting initial installation token: %w", err)
		}
		for _, owner := range owners {
			as.Clients[owner] = a.client
		}
		as.auths = []*Auth{a}
		return as, nil
	}

	if len(owners) == 0 {
		// No repos watched, nothing pinned: touch zero installations. This
		// is the "installations touched = owners listed" property applied
		// to its degenerate case.
		return as, nil
	}

	installations, err := gh.FetchAppInstallations(baseURL, jwt)
	if err != nil {
		return nil, fmt.Errorf("discovering app installations: %w", err)
	}
	byAccount := make(map[string]gh.AppInstallation, len(installations))
	for _, inst := range installations {
		byAccount[inst.Account] = inst
	}

	for _, owner := range owners {
		inst, ok := byAccount[owner]
		if !ok {
			return nil, fmt.Errorf("no GitHub App installation found for owner %q (watched in watched_repos) — install the app on %q", owner, owner)
		}

		a, err := mintAuth(cfg.AppID, inst.ID, botLogin, privateKey, baseURL)
		if err != nil {
			return nil, fmt.Errorf("minting initial installation token for owner %q (installation %d): %w", owner, inst.ID, err)
		}

		if inst.RepositorySelection == "selected" {
			if err := checkSelectedRepos(baseURL, a.client.Token(), owner, cfg.WatchedRepos); err != nil {
				return nil, err
			}
		}

		as.Clients[owner] = a.client
		as.auths = append(as.auths, a)
	}

	return as, nil
}

// mintAuth builds a fresh *github.Client and *Auth for one installation and
// mints its first token.
func mintAuth(appID, installationID int64, botLogin string, privateKey *rsa.PrivateKey, baseURL string) (*Auth, error) {
	client := gh.NewClientWithBaseURL("", baseURL)
	a := &Auth{
		AppID:          appID,
		InstallationID: installationID,
		BotLogin:       botLogin,
		privateKey:     privateKey,
		baseURL:        baseURL,
		client:         client,
	}
	if err := a.refresh(); err != nil {
		return nil, err
	}
	return a, nil
}

// checkSelectedRepos verifies every watchedRepos entry owned by owner is
// actually accessible to a repository_selection == "selected" installation,
// returning a hard error naming the first excluded repo found. installToken
// must be that installation's own access token — GET /installation/repositories
// is scoped to the installation identity, not the App's JWT.
func checkSelectedRepos(baseURL, installToken, owner string, watchedRepos []string) error {
	accessible, err := gh.FetchInstallationRepositories(baseURL, installToken)
	if err != nil {
		return fmt.Errorf("listing accessible repositories for owner %q's installation: %w", owner, err)
	}
	accessibleSet := make(map[string]bool, len(accessible))
	for _, r := range accessible {
		accessibleSet[r] = true
	}
	for _, repoSpec := range watchedRepos {
		o, _, ok := splitOwnerRepo(repoSpec)
		if !ok || o != owner {
			continue
		}
		if !accessibleSet[repoSpec] {
			return fmt.Errorf("watched repo %q is not accessible to its App installation (repository_selection=selected excludes it) — grant %q access in the installation's repository settings for %q, or remove it from watched_repos", repoSpec, repoSpec, owner)
		}
	}
	return nil
}

// distinctOwners returns the distinct owners of every well-formed
// "owner/repo" entry in watchedRepos, in first-seen order. Malformed entries
// are skipped here — poll() already logs and skips them independently.
func distinctOwners(watchedRepos []string) []string {
	seen := make(map[string]bool)
	var owners []string
	for _, spec := range watchedRepos {
		owner, _, ok := splitOwnerRepo(spec)
		if !ok || seen[owner] {
			continue
		}
		seen[owner] = true
		owners = append(owners, owner)
	}
	return owners
}

// RunRefreshLoops starts one RunRefreshLoop goroutine per distinct
// installation in the set — deduplicated by *Auth pointer identity, so a
// pinned installation shared by multiple owners gets exactly one goroutine,
// not one per owner — and returns immediately. logf is called once per Auth
// to build that installation's own log function, letting callers tag log
// lines with the installation ID.
func (as *AuthSet) RunRefreshLoops(ctx context.Context, logf func(installationID int64) func(format string, args ...any)) {
	for _, a := range as.auths {
		go a.RunRefreshLoop(ctx, logf(a.InstallationID))
	}
}

// InstallationCount returns the number of distinct installations minted —
// used for startup logging.
func (as *AuthSet) InstallationCount() int {
	return len(as.auths)
}

// newlyWatchedOwners returns the distinct owners present in cand's
// WatchedRepos but absent from old's, in cand's first-seen order — the set
// a config reload must mint a token for before it can be applied (R5).
// Adding a repo under an already-watched owner never appears here.
func newlyWatchedOwners(old, cand Config) []string {
	oldOwners := make(map[string]bool)
	for _, o := range distinctOwners(old.WatchedRepos) {
		oldOwners[o] = true
	}
	var added []string
	for _, o := range distinctOwners(cand.WatchedRepos) {
		if !oldOwners[o] {
			added = append(added, o)
		}
	}
	return added
}

// mintOwnerAuth mints a token for owner without mutating as — this is the
// "mint" half of R5's two-phase mint-then-commit split. Callers must call
// CommitOwner to actually register the result, and must do so only after
// every owner in the same reload batch has minted successfully (see
// handleReload in execute.go): minting owner 2-of-2 failing must leave
// owner 1 exactly as unregistered as if it had never been attempted.
//
// mintedFresh is false when as is in pinned-installation mode
// (pinnedInstallationID != 0): owner shares the App's single already-minted
// Auth rather than getting one of its own, so CommitOwner must register the
// client mapping without appending a duplicate *Auth or starting a second
// refresh loop for it. In non-pinned mode mintedFresh is always true — a
// genuinely new *Auth, discovered and minted for this owner specifically.
func (as *AuthSet) mintOwnerAuth(cfg Config, owner string) (a *Auth, mintedFresh bool, err error) {
	if as.pinnedInstallationID != 0 {
		if len(as.auths) == 0 {
			return nil, false, fmt.Errorf("no pinned installation auth available for owner %q", owner)
		}
		return as.auths[0], false, nil
	}

	jwt, err := gh.BuildAppJWT(as.appID, as.privateKey)
	if err != nil {
		return nil, false, fmt.Errorf("building app JWT: %w", err)
	}
	installations, err := gh.FetchAppInstallations(as.baseURL, jwt)
	if err != nil {
		return nil, false, fmt.Errorf("discovering app installations: %w", err)
	}
	var inst *gh.AppInstallation
	for i := range installations {
		if installations[i].Account == owner {
			inst = &installations[i]
			break
		}
	}
	if inst == nil {
		return nil, false, fmt.Errorf("no GitHub App installation found for owner %q (newly watched in watched_repos) — install the app on %q", owner, owner)
	}

	newAuth, err := mintAuth(as.appID, inst.ID, as.BotLogin, as.privateKey, as.baseURL)
	if err != nil {
		return nil, false, fmt.Errorf("minting installation token for owner %q (installation %d): %w", owner, inst.ID, err)
	}

	if inst.RepositorySelection == "selected" {
		if err := checkSelectedRepos(as.baseURL, newAuth.client.Token(), owner, cfg.WatchedRepos); err != nil {
			return nil, false, err
		}
	}

	return newAuth, true, nil
}

// CommitOwner registers an already-minted Auth (from mintOwnerAuth) for
// owner into the set: the "commit" half of R5's two-phase split. mintedFresh
// must be exactly the value mintOwnerAuth returned alongside a — true
// appends a to auths (so RunRefreshLoops/InstallationCount pick it up);
// false (the pinned-installation case) does not, since a is always
// as.auths[0], already present — several new owners sharing the pinned
// installation in the same reload batch each call CommitOwner with the same
// *Auth and mintedFresh=false, so it is never appended more than once.
func (as *AuthSet) CommitOwner(owner string, a *Auth, mintedFresh bool) {
	as.Clients[owner] = a.client
	if mintedFresh {
		as.auths = append(as.auths, a)
	}
}
