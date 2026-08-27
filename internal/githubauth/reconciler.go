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

// identityValidationSleepFunc is identityValidationRetryDelays' delay
// function signature — a package var so tests can replace it with a
// near-instant stand-in instead of sleeping for real. It still takes ctx
// (and returns ctx.Err() on cancellation) so a test substitute can exercise
// the cancellation path too, not just skip the delay.
type identityValidationSleepFunc func(ctx context.Context, d time.Duration) error

// identityValidationSleep is identityValidationRetryDelays' delay function.
// The default selects on ctx.Done() vs. a timer so a canceled context (e.g.
// shutdown) aborts the wait immediately instead of always sleeping out the
// full delay — this only shortens the *pause between* retry attempts; the
// gh.FetchAppSlug call each attempt makes is not itself ctx-aware yet (a
// separate, larger gap — see the Reconcile-wide ctx-threading note in
// Reconcile's doc comment).
var identityValidationSleep identityValidationSleepFunc = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryIdentityCheckAfterCreation re-attempts an identity check that just
// failed immediately after this exact Reconcile call created or recreated
// the App (the justBootstrapped and post-self-heal call sites below) —
// pausing identityValidationRetryDelays between attempts before giving up.
// Shared so the two call sites can't drift out of sync on the backoff
// policy; each keeps its own error message, since "just created" and "just
// recreated" warrant different wording.
func retryIdentityCheckAfterCreation(ctx context.Context, baseURL string, jwt string) (slug string, err error) {
	// Seeded non-nil so an empty identityValidationRetryDelays (impossible
	// today — the package-level slice always has two entries — but not
	// guaranteed by the type system, and one accidental edit or test
	// override away) makes the for-range below execute zero times and
	// fall through to this return, rather than the zero-value nil a bare
	// `var err error` would leave: both call sites treat a nil error here
	// as "identity now resolves" and proceed to build a bot identity from
	// the (also zero-value) slug — a silent, bogus success instead of the
	// surfaced failure this is supposed to guarantee.
	err = errors.New("no retry attempts configured (identityValidationRetryDelays is empty)")
	for _, delay := range identityValidationRetryDelays {
		if sleepErr := identityValidationSleep(ctx, delay); sleepErr != nil {
			return "", fmt.Errorf("waiting to retry identity check: %w", sleepErr)
		}
		if slug, err = gh.FetchAppSlug(baseURL, jwt); err == nil {
			return slug, nil
		}
	}
	return "", err
}

// identityRetryFailureClause describes why retryIdentityCheckAfterCreation
// gave up, for interpolation into the three "just created/recreated App"
// error messages below. A canceled context (identityValidationSleep bailing
// out of its backoff wait, possibly before a single retry attempt even ran)
// is a materially different situation from GitHub's identity check itself
// failing on every configured retry — "still doesn't resolve on GitHub
// after N retries" is misleading if what actually happened is Reconcile's
// caller cancelling the wait, not GitHub repeatedly rejecting the identity
// check.
func identityRetryFailureClause(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Deliberately doesn't restate "context canceled"/"deadline
		// exceeded" here — every call site interpolates this clause
		// immediately followed by "(%w)", which already surfaces that
		// detail from err itself; repeating it here would just say the
		// same thing twice in one sentence.
		return "identity validation was interrupted before it could be confirmed"
	}
	return fmt.Sprintf("its identity still doesn't resolve on GitHub after %d retries", len(identityValidationRetryDelays))
}

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
	// WatchedRepos is, since this issue (handarbeit/fabrik#1641), no longer
	// the primary input to discovery — it is an optional intersection
	// filter (R3) applied on top of whatever the App's installations
	// actually grant. Absent/empty means "everything the installations
	// grant"; present narrows to the intersection, and a named repo the
	// installation grant doesn't cover is reported via
	// DerivedRepoSet.FilteredOut rather than silently included or excluded
	// with no explanation. See Derive.
	WatchedRepos []string
	// MaxDerivedRepos bounds the derived set's size (R5) — applied last,
	// after the installation-grant union and any WatchedRepos filter. <= 0
	// means no cap.
	MaxDerivedRepos int
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

	// mintErrors records, per lower-cased owner, why that owner has no
	// entry in clients despite having an installation — i.e. mintAuth
	// failed during Reconcile (see the discovery loop below). Distinct
	// from an owner simply having no installation at all: ClientForRepo
	// consults this to avoid misattributing a transient mint failure as
	// "not installed" (which would send an operator down a pointless
	// re-install detour for a problem a Reconcile retry would likely
	// resolve on its own).
	mintErrors map[string]error

	// appID, privateKey, and baseURL are the App-level identity Reconcile
	// resolves once; MintOwnerAuth reuses them to mint a token for a single
	// additional owner a caller discovers after Reconcile has already run
	// (e.g. Pruefer's SIGHUP config-reload handler, ADR-1640), without
	// re-deriving or re-reading the private key file. This is safe
	// specifically because a caller's AppID/AppPrivateKeyPath/
	// AppInstallationID are always restart-only in its own reload
	// classification (see pruefer/config.go's `reload:"restart"` tags) —
	// nothing can change the App identity these fields capture without a
	// full process restart, which re-runs Reconcile from scratch anyway.
	appID      int64
	privateKey *rsa.PrivateKey
	baseURL    string
	// pinnedInstallationID mirrors Options.AppInstallationID: non-zero means
	// every owner — including one newly added after Reconcile via
	// MintOwnerAuth — shares the App's single pinned installation rather
	// than being resolved via per-owner discovery. Fixed for the
	// Reconciler's lifetime (restart-only, see above), so this is set once
	// in Reconcile and never itself changes.
	pinnedInstallationID int64

	// lastDerived is the result of the most recent Derive call (including
	// the one Reconcile itself performs) — see LastDerived and Derive's own
	// doc comment.
	lastDerived DerivedRepoSet
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
		// An installation existed for owner but minting a token from it
		// failed during Reconcile (transient network/API error, not a
		// missing installation) — say so, rather than the generic
		// "no installation" message below, which would send an operator
		// toward re-installing the App when the actual fix is retrying
		// Reconcile (e.g. restarting Pruefer, or waiting for the next
		// reconciliation trigger).
		if mintErr, ok := r.mintErrors[strings.ToLower(owner)]; ok {
			return nil, fmt.Errorf("owner %q has an App installation, but minting a token for it failed during the last reconciliation: %w — this is not a missing-installation problem; retry Reconcile (e.g. restart Pruefer)", owner, mintErr)
		}
		return nil, fmt.Errorf("no authorized GitHub App installation for owner %q — if it's already in watched_repos, install the App on %q (Reconcile logs the install URL at startup); otherwise add it to watched_repos and restart to trigger reconciliation", owner, owner)
	}
	return client, nil
}

// RunRefreshLoops starts one token-refresh goroutine per distinct
// installation this Reconciler minted — see Auth.RunRefreshLoop. logf is
// called once per Auth, synchronously, to build that installation's own log
// function before its goroutine is spawned.
//
// Returns a wait function the caller MUST call, and block on, before tearing
// down anything the per-installation logf closures depend on (e.g.
// pruefer's package-level Logf hook and the log file it writes to) — these
// goroutines are unmanaged by any daemon/poll-loop lifecycle, so nothing
// else guarantees they've stopped calling logf by the time ctx is
// cancelled. Each goroutine exits promptly once ctx.Done() fires (they
// select on it both while waiting for the next refresh and after a failed
// refresh's retry delay), but "promptly" is not "synchronously with ctx
// cancellation" — a goroutine can still be mid-refresh or mid-logf-call
// when the caller's shutdown path proceeds, racing an unsynchronized write
// to that same logf infrastructure. wait() blocks until every goroutine has
// actually returned, closing that window.
func (r *Reconciler) RunRefreshLoops(ctx context.Context, logf func(installationID int64) func(format string, args ...any)) (wait func()) {
	r.mu.Lock()
	auths := append([]*Auth(nil), r.auths...)
	r.mu.Unlock()
	var wg sync.WaitGroup
	for _, a := range auths {
		installLogf := logf(a.InstallationID)
		wg.Add(1)
		a.startRefreshLoop(ctx, installLogf, wg.Done)
	}
	return wg.Wait
}

// MintOwnerAuth mints a fresh installation token for owner without
// registering it in the Reconciler — the "mint" half of a two-phase
// mint-then-commit split a caller uses to add one or more owners to an
// already-Reconciled set atomically (e.g. Pruefer's SIGHUP config-reload
// handler, ADR-1640): the caller must call CommitOwnerAuth for every
// returned *Auth only after every owner in the same batch has minted
// successfully here, so a later owner's mint failing leaves an earlier
// one exactly as unregistered as if MintOwnerAuth had never been called
// for it.
//
// watchedRepos is the caller's full, post-change watched-repo list (not
// just owner's own repos) — passed through unchanged to verifyRepoAccess
// exactly as Reconcile's own discovery loop does, for a "selected"-mode
// installation.
//
// mintedFresh is false when the Reconciler is in pinned-installation mode
// (pinnedInstallationID != 0): owner shares the App's single already-minted
// Auth rather than getting one of its own, so CommitOwnerAuth must register
// the client mapping without starting a second refresh loop for it. In
// non-pinned mode mintedFresh is always true.
func (r *Reconciler) MintOwnerAuth(owner string, watchedRepos []string, logf func(format string, args ...any)) (a *Auth, mintedFresh bool, err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r.mu.Lock()
	pinnedID := r.pinnedInstallationID
	appID := r.appID
	privateKey := r.privateKey
	baseURL := r.baseURL
	botLogin := r.botLogin
	var pinnedAuth *Auth
	if pinnedID != 0 && len(r.auths) > 0 {
		pinnedAuth = r.auths[0]
	}
	r.mu.Unlock()

	if pinnedID != 0 {
		if pinnedAuth == nil {
			return nil, false, fmt.Errorf("no pinned installation auth available for owner %q", owner)
		}
		return pinnedAuth, false, nil
	}

	jwt, err := gh.BuildAppJWT(appID, privateKey)
	if err != nil {
		return nil, false, fmt.Errorf("building app JWT: %w", err)
	}
	installations, installationsTruncated, err := gh.FetchAppInstallations(baseURL, jwt)
	if err != nil {
		return nil, false, fmt.Errorf("discovering app installations: %w", err)
	}
	// Mirrors Reconcile's own discovery loop: a truncated result means
	// owner's actual installation could be sitting past the pagination
	// ceiling rather than genuinely missing — a newly-watched owner on a
	// very large App must not be misreported (and the whole reload aborted
	// on) a pagination gap it never had.
	var inst *gh.AppInstallation
	for i := range installations {
		if strings.EqualFold(installations[i].Account, owner) {
			inst = &installations[i]
			break
		}
	}
	if inst == nil {
		if installationsTruncated {
			return nil, false, fmt.Errorf("no GitHub App installation found for owner %q in the first 100 installations returned (newly watched in watched_repos) — the App may have ≥100 installations, so this could be a pagination gap rather than a genuinely missing installation; retry the reload to confirm before installing the app on %q", owner, owner)
		}
		return nil, false, fmt.Errorf("no GitHub App installation found for owner %q (newly watched in watched_repos) — install the app on %q", owner, owner)
	}

	newAuth, err := mintAuth(appID, inst.ID, botLogin, privateKey, baseURL)
	if err != nil {
		return nil, false, fmt.Errorf("minting installation token for owner %q (installation %d): %w", owner, inst.ID, err)
	}

	if inst.RepositorySelection == "selected" {
		// Soft, logged-only verification — matching Reconcile's own
		// discovery-path convention (see its repoVerifyFailed handling)
		// rather than the old pruefer/auth.go AuthSet's hard-fail
		// checkSelectedRepos: a newly-watched owner whose repo-access check
		// merely fails to verify (transient network error) should not abort
		// an otherwise-successful reload the way a genuinely missing
		// installation does.
		if _, err := verifyRepoAccess(baseURL, newAuth.client.Token(), inst.ID, owner, watchedRepos); err != nil {
			logf("! verifying repo access for newly-watched owner %q failed: %v", owner, err)
		}
	}

	return newAuth, true, nil
}

// CommitOwnerAuth registers an already-minted *Auth (from MintOwnerAuth)
// for owner — the "commit" half of the two-phase split. mintedFresh must be
// exactly the value MintOwnerAuth returned alongside a. When mintedFresh is
// true, this also starts a's refresh loop under ctx (a pinned-mode Auth
// already has one running, started by Reconcile/RunRefreshLoops, and must
// not get a second). Returns the *gh.Client now resolvable via
// ClientForRepo for owner.
func (r *Reconciler) CommitOwnerAuth(ctx context.Context, owner string, a *Auth, mintedFresh bool, logf func(format string, args ...any)) *gh.Client {
	r.mu.Lock()
	key := strings.ToLower(owner)
	r.clients[key] = a.client
	delete(r.mintErrors, key)
	if mintedFresh {
		r.auths = append(r.auths, a)
	}
	r.mu.Unlock()

	if mintedFresh {
		a.startRefreshLoop(ctx, logf, nil)
	}
	return a.client
}

// DetachedAuth pairs a removed owner with the *Auth its token was minted
// for. Returned by RemoveOwners instead of being stopped there directly —
// see that method's doc comment for why.
type DetachedAuth struct {
	Owner string
	Auth  *Auth
}

// RemoveOwners removes every owner in removed from the Reconciler's client
// map and, in non-pinned mode, detaches the *Auth that owner's installation
// was minted for — but deliberately does NOT stop its refresh loop here,
// returning it as a DetachedAuth instead. An in-flight review dispatched
// before the removal may still hold a *gh.Client backed by this exact
// Auth (see Pruefer's Daemon.executeReview snapshot-at-dispatch-time
// design); stopping its refresh loop immediately risks that review's
// remaining GitHub calls failing with 401 once the token's actual expiry
// passes, with no recovery short of a restart. The caller is responsible
// for only calling DetachedAuth.Auth.Stop() once it has separately
// confirmed nothing still depends on it — see Pruefer's
// Daemon.drainThenStopAuth (ADR-1640).
//
// Pinned-installation mode is deliberately exempt from detachment: every
// owner there shares the single App-level Auth (r.auths[0]) regardless of
// watched_repos, so it never accumulates per-owner Auths the way non-pinned
// mode does, and detaching it on the last pinned owner's removal would
// strand MintOwnerAuth's pinned-mode short-circuit for the next owner added
// under the same pin.
func (r *Reconciler) RemoveOwners(removed []string) []DetachedAuth {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pinnedInstallationID != 0 {
		for _, o := range removed {
			delete(r.clients, strings.ToLower(o))
		}
		return nil
	}
	var detached []DetachedAuth
	for _, o := range removed {
		key := strings.ToLower(o)
		// Delete mintErrors unconditionally, before the clients lookup
		// below: an owner whose installation was found but whose token
		// mint failed (Reconcile's discovery loop, or a prior reload's
		// MintOwnerAuth) never gets a clients entry, so gating this
		// deletion on the clients ok-check would leave that stale error
		// behind forever once the owner is removed from watched_repos —
		// silently mis-explaining a later ClientForRepo miss for a
		// completely different, since-removed owner if the lower-cased
		// key were ever reused.
		delete(r.mintErrors, key)
		c, ok := r.clients[key]
		if !ok {
			continue
		}
		delete(r.clients, key)
		for i, a := range r.auths {
			if a.client != c {
				continue
			}
			detached = append(detached, DetachedAuth{Owner: o, Auth: a})
			r.auths = append(r.auths[:i], r.auths[i+1:]...)
			break
		}
	}
	return detached
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
		// GitHub's public API reference does not document a status code
		// that distinguishes "deleted" from "suspended by GitHub" (e.g.
		// for a ToS violation) for this call — both are treated
		// identically here, and the messages below say so explicitly
		// rather than asserting deletion as fact, so an operator whose App
		// keeps getting recreated (rather than genuinely deleted just
		// once) has a concrete next place to look.
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
			// The pinned path never runs loadOrBootstrapCredentials'
			// fingerprint-mismatch check (that check only fires for the
			// non-pinned, AppStatePath-resolved AppID case), so a 401/403
			// here is genuinely ambiguous between "the App is
			// deleted/suspended" and "AppPrivateKeyPath holds the wrong or
			// a stale, rotated private key for this still-existing App" —
			// GitHub's App JWT auth returns the same rejection either way.
			// Name both possibilities so an operator doesn't jump straight
			// to recreating a perfectly fine App over a swapped key file.
			//
			// If github_app_installation_id is also pinned, name that too:
			// installation IDs are App-specific, so if the App really was
			// deleted/recreated, that pin is now stale as well — an
			// operator who only clears github_app_id would hit a second,
			// more confusing failure from the now-mismatched installation
			// ID once a new App is created.
			installationNote := ""
			if opts.AppInstallationID != 0 {
				installationNote = fmt.Sprintf("; if the App was deleted or suspended (not a key mismatch), github_app_installation_id %d is also now stale — installation IDs are App-specific — and must be updated or cleared too, not just github_app_id", opts.AppInstallationID)
			}
			return nil, fmt.Errorf("github_app_id %d is configured but no longer resolves on GitHub (%w) — either the App was deleted or suspended by GitHub (check https://github.com/settings/apps), or the private key at github_app_private_key_path no longer matches this App (e.g. a stale/rotated key); if the App still exists on GitHub, restore the correct key instead of recreating it — otherwise update or remove github_app_id in config to let first-run setup create a new App (repair required, not auto-recreated)%s", opts.AppID, err, installationNote)
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
			slug, err = retryIdentityCheckAfterCreation(ctx, opts.BaseURL, jwt)
			if err != nil {
				clause := identityRetryFailureClause(err)
				if opts.AppInstallationID != 0 {
					return nil, fmt.Errorf("just created App ID %d but %s (%w) — not treated as deletion since the App was only just created in this run; note github_app_installation_id %d is a leftover pin from before this run — it cannot be an installation of this brand-new App, and will need to be updated (or cleared to let discovery run) once the App's identity resolves; retry Reconcile", appID, clause, err, opts.AppInstallationID)
				}
				return nil, fmt.Errorf("just created App ID %d but %s (%w) — not treated as deletion since the App was only just created in this run; retry Reconcile", appID, clause, err)
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
			return nil, fmt.Errorf("github_app_installation_id %d is configured but App ID %d no longer resolves on GitHub (%w) — it may have been deleted, or suspended by GitHub (check https://github.com/settings/apps), either of which also invalidates the pinned installation ID (installation IDs are App-specific); update or remove github_app_id/github_app_installation_id in config to let first-run setup create a new App and installation (repair required, not auto-recreated)", opts.AppInstallationID, appID, err)
		} else {
			// AppID was resolved from the reconciler-owned state file (a
			// prior manifest run), not pinned by config — safe to
			// self-heal: the next restart resolves the freshly-created
			// App's ID from AppStatePath with no stale config value in the
			// way. Preserve existing non-secret config (RunManifestFlow
			// only overwrites on full success) and re-enter the manifest
			// flow rather than silently giving up — see the issue's "App
			// deleted externally" failure handling requirement.
			logf("! app identity validation failed for App ID %d (%v) — it may have been deleted, or suspended by GitHub; if this keeps recurring after a fresh App is created, check https://github.com/settings/apps for a suspended App rather than assuming repeated deletion; starting App creation again", appID, err)
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
			// Same propagation-lag reasoning as the justBootstrapped branch
			// above: this App was also just created (by the self-heal path
			// above, seconds ago), so an immediate 401/403/404 here is more
			// plausibly lag than a second, near-instant deletion. Without
			// this retry, a transient failure here would return a hard
			// error while AppStatePath/PEM have already been persisted for
			// the new App — the next Reconcile call would then load this
			// same new AppID from state, fail identity validation again,
			// and self-heal a *second* time, creating an orphan App.
			//
			// This narrows that window to "propagation lag outlasting
			// identityValidationRetryDelays across two consecutive Reconcile
			// calls," not zero: a persisted "just self-heal-created, extend
			// leniency" fact (e.g. a timestamp in Credentials) could close
			// it further, but Reconcile's documented triggers are startup,
			// config change, and explicit drift signals — not a tight
			// automatic retry loop — so two such calls landing within the
			// same ~1s propagation-lag window is not a realistic sequence in
			// practice. Left as a known, accepted residual risk rather than
			// adding persisted-timestamp state for it.
			slug, err = gh.FetchAppSlug(opts.BaseURL, jwt)
			if err != nil {
				slug, err = retryIdentityCheckAfterCreation(ctx, opts.BaseURL, jwt)
			}
			if err != nil {
				return nil, fmt.Errorf("re-created App ID %d but %s (%w) — not treated as a second deletion since the App was only just created in this run; retry Reconcile", appID, identityRetryFailureClause(err), err)
			}
		}
	}
	botLogin := slug + "[bot]"
	logf("✓ authenticated as %s", botLogin)

	r := &Reconciler{
		botLogin:             botLogin,
		clients:              map[string]*gh.Client{},
		mintErrors:           map[string]error{},
		appID:                appID,
		privateKey:           privateKey,
		baseURL:              opts.BaseURL,
		pinnedInstallationID: opts.AppInstallationID,
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
		// A single pass both validates and enumerates — see
		// distinctOwnersLogging's own doc comment for why this replaced two
		// separate passes (one purely to log malformed entries, one via
		// distinctOwners to actually use them) that re-ran the identical
		// splitOwnerRepo check over the same slice.
		owners := distinctOwnersLogging(opts.WatchedRepos, logf)
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
		r.lastDerived = derivedSetForPinned(opts.WatchedRepos, opts.AppInstallationID)
		saveInstallationRepoCache(opts.AppStatePath, opts.AppPrivateKeyPath, appID, slug, repoCache, logf)
		// Unlike the discovery path below (verifyRepoAccess), minting a
		// token here is the only check this compat path performs — it
		// never confirms the pinned installation actually grants access to
		// every individual watched repo (see the trust-model comment
		// above). Surface that explicitly so an operator who first sets
		// this pin doesn't mistake "✓" for "every repo was verified."
		logf("✓ using pinned installation %d for all watched repos (owner-level access not individually verified — see github_app_installation_id in the README)", opts.AppInstallationID)
		return r, nil
	}

	// Non-pinned: installations are the desired state (R1) — Derive
	// discovers every installation of this App and enumerates its
	// accessible repos; opts.WatchedRepos (if any) is only an optional
	// intersection filter (R3), never the primary input. See Derive's doc
	// comment — this is the core inversion handarbeit/fabrik#1641 makes.
	set, _, err := r.Derive(ctx, opts.WatchedRepos, opts.MaxDerivedRepos, logf)
	if err != nil {
		return nil, fmt.Errorf("deriving repo set from app installations: %w", err)
	}
	logDerivedSet(set, logf)
	guideMissingInstallations(opts, slug, set, logf)

	// repoCache only gets an entry for an installation Derive got a
	// definitive answer for this round — one whose FetchInstallationRepositories
	// call actually succeeded (RepoListError == ""). An installation whose
	// listing failed this round (a transient error) contributes no entry at
	// all, mirroring the pre-Derive discovery loop's repoVerifyFailed
	// handling: saveInstallationRepoCache's per-owner merge then leaves that
	// owner's last-known-good cache entry untouched instead of wiping it to
	// empty over a transient hiccup.
	repoCache := make(map[string][]string)
	for _, dr := range set.Repos {
		key := strings.ToLower(dr.Owner)
		repoCache[key] = append(repoCache[key], dr.Repo)
	}
	for _, inst := range set.Installations {
		if inst.RepoListError != "" {
			continue
		}
		key := strings.ToLower(inst.Account)
		if _, ok := repoCache[key]; !ok {
			repoCache[key] = []string{}
		}
	}
	saveInstallationRepoCache(opts.AppStatePath, opts.AppPrivateKeyPath, appID, slug, repoCache, logf)

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
// known good" diagnostic. A *definitive* zero-repos-authorized answer is
// not a transient failure, though, and does contribute an entry (an empty,
// non-nil slice) precisely so it overwrites — rather than is
// indistinguishable from omitting — a stale "authorized" entry left over
// from before access was revoked.
//
// If the AppID being reconciled differs from whatever AppID this state
// file was last saved under, the existing entry belongs to a *different*
// App entirely (e.g. an operator switched github_app_id to point at a new
// App while reusing the same AppStatePath, or the self-heal path in
// Reconcile just recreated the App) — its WebhookSecret/ClientID/
// ClientSecret and InstallationRepoCache must not be carried forward under
// the new AppID, or they'd sit alongside a mismatched identity, e.g. once a
// future webhook-transport consumer starts reading them keyed on AppID.
//
// This discard is sound for any AppID mismatch, not just the self-heal
// case, because every writer of this file sets Credentials.AppID to the
// exact identity its secrets belong to at write time (saveCredentials
// callers: RunManifestFlow sets it from the manifest exchange response, and
// this function sets it to the appID Reconcile just live-validated) — the
// two are never written independently. So existing.AppID != appID can only
// mean "whatever secrets are in existing.WebhookSecret/ClientID/
// ClientSecret belong to a AppID other than the one just authenticated as,"
// never "these are appID's own secrets, just filed under a stale label."
// Discarding them therefore never loses the *active* App's own secrets —
// only ever a previous, now-inactive identity's, which is correct to drop
// regardless of how the mismatch arose (self-heal, manual config change, or
// a restored/stale AppStatePath backup).
//
// PrivateKeyFingerprint is handled separately from the wholesale-discard
// above, and deliberately recomputed on every call (mismatch or not) from
// privateKeyPath's current on-disk contents, rather than either carried
// forward unconditionally or wiped to "" alongside the other fields: the
// old value belongs to whichever App's PEM was active last time this ran,
// which is wrong for a mismatch (would fail loadOrBootstrapCredentials'
// fingerprint check against the *new* App's own already-authenticated
// PEM) and stale-but-usually-still-correct otherwise — recomputing fresh
// here keeps the crash-window consistency check loadOrBootstrapCredentials
// performs (comparing this field against the PEM at reconciler-restart
// time) accurate for whichever App is actually active, instead of either
// wrong or (an earlier version of this function) silently disabled by
// leaving it blank. If the PEM can't be read here at all (a rare
// double-failure — this same PEM just authenticated successfully earlier
// in the same Reconcile call), there is no trustworthy value to fall back
// to once a mismatch has already discarded the prior record, so the whole
// save is skipped for that round rather than persisting a record with a
// blank or stale fingerprint.
func saveInstallationRepoCache(statePath, privateKeyPath string, appID int64, slug string, repoCache map[string][]string, logf func(string, ...any)) {
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
	pemBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		// Reconcile already successfully authenticated with this PEM
		// earlier in this same call, so a read failure here would be
		// surprising (e.g. removed mid-run). Unlike the AppID-match case,
		// "leave PrivateKeyFingerprint as whatever loadCredentials
		// returned above" is not a safe fallback when the AppID-mismatch
		// branch above just reset existing to a zero-value Credentials{}:
		// "whatever loadCredentials returned" is already gone, so that
		// path would silently persist a blank fingerprint — exactly the
		// crash-window-consistency-check-disabling bug this function's
		// AppID-mismatch handling exists to fix. Skip the save entirely
		// rather than persist a record we can't compute a trustworthy
		// fingerprint for; the InstallationRepoCache update this round is
		// diagnostics-only and can wait for the next Reconcile.
		logf("could not recompute private key fingerprint for the installation-repo cache (skipping save): %v", err)
		return
	}
	existing.PrivateKeyFingerprint = privateKeyFingerprint(pemBytes)
	if err := saveCredentials(statePath, existing); err != nil {
		logf("could not persist installation-repo cache: %v", err)
	}
}

// loadOrBootstrapCredentials implements state-machine steps 1-2. Three
// credential-problem cases are distinguished per the issue's
// failure-handling requirements:
//   - no AppID anywhere (opts.AppID, nor a prior manifest run's
//     app-state.json) → nothing to load; run the manifest flow ("missing").
//   - an AppID is known (either way) but the private key file is missing,
//     unparseable, or (when AppID came from opts.AppStatePath rather than a
//     pinned opts.AppID) doesn't match that state file's recorded
//     PrivateKeyFingerprint → an explicit repair error, never
//     auto-regenerated ("repair-needed"). A corrupt-but-recoverable local
//     file, or a stale key left over from an interrupted App re-creation,
//     must never be mistaken for "the App is gone."
//   - both are present, readable, and (when checked) fingerprint-consistent
//     → return them; Reconcile's later identity-validation call is what
//     detects an externally-deleted App ("app-deleted-externally") — that
//     case is not this function's job.
//
// The fingerprint check is scoped to the non-pinned path (opts.AppID == 0,
// appID resolved from opts.AppStatePath): a pinned opts.AppID already gets
// an explicit repair error straight from Reconcile's identity-validation
// failure if its private key doesn't match, without ever attempting
// self-heal — see the comment there.
//
// The bool return reports whether this call itself just ran the manifest
// flow — Reconcile uses it to avoid mistaking the freshly-created App's own
// first identity check for a sign of external deletion (see the retry loop
// around FetchAppSlug in Reconcile).
func loadOrBootstrapCredentials(ctx context.Context, opts Options, logf func(string, ...any)) (int64, *rsa.PrivateKey, bool, error) {
	// appIDPinned distinguishes appID coming from an explicit
	// github_app_id config value from appID resolved out of the
	// reconciler-owned state file below — the two error-message sets
	// further down name the source accurately rather than always
	// claiming "github_app_id is configured," which is false in the
	// latter case (a fresh manifest-flow bootstrap never sets it).
	appIDPinned := opts.AppID != 0
	appID := opts.AppID
	var expectedFingerprint string
	if appID == 0 {
		state, err := loadCredentials(opts.AppStatePath)
		if err != nil {
			return 0, nil, false, fmt.Errorf("app state file %s exists but is corrupt — repair or remove it: %w", opts.AppStatePath, err)
		}
		appID = state.AppID
		expectedFingerprint = state.PrivateKeyFingerprint
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

	// appIDSource names where appID actually came from, for the three
	// error messages below — appIDPinned means an explicit github_app_id
	// config value; justBootstrapped means it was minted by the manifest
	// flow a few lines above in this same call; anything else means it
	// was resolved from the reconciler-owned AppStatePath. Saying
	// "github_app_id N is configured" when no such config value exists
	// (the state-file and just-bootstrapped cases) sends an operator
	// looking for a config entry that was never there.
	appIDSource := fmt.Sprintf("App ID %d (resolved from %s)", appID, opts.AppStatePath)
	switch {
	case appIDPinned:
		appIDSource = fmt.Sprintf("github_app_id %d is configured", appID)
	case justBootstrapped:
		appIDSource = fmt.Sprintf("App ID %d (just created by first-run setup)", appID)
	}

	if _, err := os.Stat(opts.AppPrivateKeyPath); os.IsNotExist(err) {
		return 0, nil, false, fmt.Errorf("%s but its private key %s is missing — restore it from the App's settings page (repair required, not auto-regenerated)", appIDSource, opts.AppPrivateKeyPath)
	}
	pemBytes, err := os.ReadFile(opts.AppPrivateKeyPath)
	if err != nil {
		return 0, nil, false, fmt.Errorf("%s but its private key %s is unreadable/corrupt — restore it from the App's settings page (repair required, not auto-regenerated): %w", appIDSource, opts.AppPrivateKeyPath, err)
	}
	privateKey, err := gh.ParseAppPrivateKey(pemBytes)
	if err != nil {
		return 0, nil, false, fmt.Errorf("%s but its private key %s is unreadable/corrupt — restore it from the App's settings page (repair required, not auto-regenerated): %w", appIDSource, opts.AppPrivateKeyPath, err)
	}

	// justBootstrapped means we just wrote both files ourselves a few
	// lines above — trivially consistent, no need to re-check. Otherwise,
	// only check when the state file actually recorded a fingerprint
	// (empty means either a pre-fingerprint state file, or AppID came
	// from a pinned opts.AppID and this variable was never populated).
	if !justBootstrapped && expectedFingerprint != "" {
		if got := privateKeyFingerprint(pemBytes); got != expectedFingerprint {
			return 0, nil, false, fmt.Errorf("app ID %d's private key at %s does not match the fingerprint recorded in %s — likely stale from an interrupted App re-creation (a crash between persisting the new App's state and its private key); restore the correct private key for App ID %d, or remove %s and %s together to trigger fresh setup (repair required, not auto-recreated)", appID, opts.AppPrivateKeyPath, opts.AppStatePath, appID, opts.AppStatePath, opts.AppPrivateKeyPath)
		}
	}

	return appID, privateKey, justBootstrapped, nil
}
