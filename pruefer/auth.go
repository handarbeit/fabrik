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

// Auth holds the bootstrapped GitHub App identity Pruefer needs: the App's
// own bot login (for self-review skip and GitHub-derived review-state
// checks) and the machinery to keep the installation token on the wrapped
// *github.Client fresh for the lifetime of the daemon.
type Auth struct {
	AppID          int64
	InstallationID int64
	// BotLogin is the App's own identity as it appears as a PR/review
	// author, e.g. "pruefer-bot[bot]". Always slug + "[bot]" for a GitHub
	// App — this is what makes the "review identity != PR author" guarantee
	// structural rather than operator-discipline-dependent (see ADR-1113).
	BotLogin string

	privateKey *rsa.PrivateKey
	baseURL    string
	client     *gh.Client

	mu        sync.Mutex
	expiresAt time.Time
}

// Bootstrap performs the full GitHub App auth flow: parses the private key,
// builds a JWT, resolves the App's own bot login, resolves (or validates)
// the installation to act as, and mints the first installation token onto
// client. baseURL selects GitHub's API host; pass "" for production or an
// httptest server URL in tests.
func Bootstrap(cfg Config, client *gh.Client, baseURL string) (*Auth, error) {
	keyPEM, err := os.ReadFile(cfg.AppPrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("reading GitHub App private key %s: %w", cfg.AppPrivateKeyPath, err)
	}
	privateKey, err := gh.ParseAppPrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parsing GitHub App private key %s: %w", cfg.AppPrivateKeyPath, err)
	}

	a := &Auth{
		AppID:          cfg.AppID,
		InstallationID: cfg.AppInstallationID,
		privateKey:     privateKey,
		baseURL:        baseURL,
		client:         client,
	}

	jwt, err := gh.BuildAppJWT(a.AppID, a.privateKey)
	if err != nil {
		return nil, fmt.Errorf("building app JWT: %w", err)
	}

	slug, err := gh.FetchAppSlug(baseURL, jwt)
	if err != nil {
		return nil, fmt.Errorf("resolving app identity: %w", err)
	}
	a.BotLogin = slug + "[bot]"

	if a.InstallationID == 0 {
		installations, err := gh.FetchAppInstallations(baseURL, jwt)
		if err != nil {
			return nil, fmt.Errorf("discovering app installations: %w", err)
		}
		switch len(installations) {
		case 0:
			return nil, fmt.Errorf("github app has no installations — install it on at least one account/repo first")
		case 1:
			a.InstallationID = installations[0].ID
		default:
			// V1 requires an explicit installation ID once more than one
			// exists — silently picking one could watch/act on the wrong
			// org. Auto-discovery still removes per-repo config churn
			// within a single installation's repo set.
			return nil, fmt.Errorf("github app has %d installations; set github_app_installation_id to select one", len(installations))
		}
	}

	if err := a.refresh(); err != nil {
		return nil, fmt.Errorf("minting initial installation token: %w", err)
	}
	return a, nil
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
// stays valid until its actual (not margin-adjusted) expiry.
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
