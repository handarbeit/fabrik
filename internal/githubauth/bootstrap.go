package githubauth

import (
	"context"
	"fmt"
)

// ManifestFlowOptions configures RunManifestFlow.
type ManifestFlowOptions struct {
	// BaseURL selects GitHub's API host for the code exchange. "" means
	// production; tests point it at an httptest server. The manifest
	// create page itself (githubManifestCreateURL) is always the real
	// github.com — that part of the flow is inherently browser-driven and
	// cannot be mocked.
	BaseURL string
	// NoBrowser skips the openBrowser call — the URL is always printed via
	// Logf regardless, so this only controls whether RunManifestFlow also
	// tries to launch a local browser process (irrelevant/unwanted in a
	// headless environment).
	NoBrowser      bool
	PrivateKeyPath string
	AppStatePath   string
	Logf           func(format string, args ...any)
}

// RunManifestFlow drives GitHub's App Manifest flow end to end: starts a
// loopback callback listener, generates a manifest scoped to Pruefer's
// actual permissions, opens (or prints, if NoBrowser or the open fails) the
// GitHub manifest-create URL, waits for GitHub's redirect, exchanges the
// resulting code for the created App's credentials, and persists them (PEM
// at opts.PrivateKeyPath, everything else at opts.AppStatePath).
//
// Nothing is written to disk until the exchange has fully succeeded, so an
// abandoned flow, an expired code, or any error along the way leaves prior
// valid local config completely untouched — the caller can simply retry by
// calling RunManifestFlow again, per the issue's expiry/abandonment
// handling requirements. The two post-exchange writes (app-state, then PEM)
// are not atomic as a pair, but are deliberately ordered so a failure
// between them always lands in the same explicit "repair needed" state
// loadOrBootstrapCredentials already handles for a lost/corrupt key — never
// a silently duplicated App (see the comment at the writes below).
func RunManifestFlow(ctx context.Context, opts ManifestFlowOptions) (Credentials, error) {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	startURL, results, shutdown, err := runManifestCallbackServer(buildManifest)
	if err != nil {
		return Credentials{}, fmt.Errorf("starting manifest callback listener: %w", err)
	}
	defer shutdown()

	logf("opening GitHub App creation page in your browser — if it doesn't open, visit:\n  %s", startURL)
	if !opts.NoBrowser {
		if err := openBrowser(startURL); err != nil {
			logf("could not open browser automatically (%v) — visit the URL above manually", err)
		}
	}

	code, err := waitForCallback(ctx, results)
	if err != nil {
		return Credentials{}, fmt.Errorf("waiting for GitHub App manifest callback: %w", err)
	}

	mc, err := exchangeManifestCode(opts.BaseURL, code)
	if err != nil {
		return Credentials{}, fmt.Errorf("exchanging manifest code: %w", err)
	}

	// Persist app-state (which carries AppID) before the PEM, not after: if
	// the PEM write below then fails, the next Reconcile finds a non-zero
	// AppID in AppStatePath but no private key at PrivateKeyPath, which
	// loadOrBootstrapCredentials already treats as an explicit "repair
	// needed" error — never silently re-running the manifest flow and
	// creating a second, orphaned App. Persisting in the other order would
	// leave a PEM on disk with no corresponding state file if this step
	// failed, so the next run would see AppID == 0 and quietly mint a
	// duplicate App while overwriting the orphaned PEM — exactly the
	// "silently create a duplicate app" failure mode the issue rules out.
	creds := Credentials{
		AppID:                 mc.AppID,
		Slug:                  mc.Slug,
		WebhookSecret:         mc.WebhookSecret,
		ClientID:              mc.ClientID,
		ClientSecret:          mc.ClientSecret,
		PrivateKeyFingerprint: privateKeyFingerprint([]byte(mc.PEM)),
	}
	if err := saveCredentials(opts.AppStatePath, creds); err != nil {
		return Credentials{}, fmt.Errorf("persisting new App's credentials: %w", err)
	}
	if err := savePrivateKey(opts.PrivateKeyPath, []byte(mc.PEM)); err != nil {
		return Credentials{}, fmt.Errorf("persisting new App's private key: %w", err)
	}

	logf("✓ created GitHub App %q (id %d) — install it on each account you watch: https://github.com/apps/%s/installations/new", mc.Slug, mc.AppID, mc.Slug)
	return creds, nil
}
