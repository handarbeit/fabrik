package githubauth

import (
	"context"
	"crypto/rsa"
	"fmt"
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

// Auth holds one minted GitHub App installation token and the machinery to
// keep it fresh on the wrapped *github.Client for the lifetime of the
// daemon. A Reconciler holds one Auth per distinct installation it actually
// needs — see Reconciler.RunRefreshLoops.
type Auth struct {
	AppID          int64
	InstallationID int64
	// BotLogin is the App's own identity as it appears as a PR/review
	// author, e.g. "pruefer-bot[bot]". Always slug + "[bot]" for a GitHub
	// App — this is what makes the "review identity != PR author" guarantee
	// structural rather than operator-discipline-dependent (see ADR-1113).
	// Invariant across every installation of the same App, so every Auth a
	// Reconciler holds carries the same value.
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
// GitHub outage shouldn't take the daemon down, since the *current* token
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
