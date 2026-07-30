package githubauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"sync"
	"time"
)

// manifestCallbackTimeout bounds how long the loopback listener waits for
// GitHub's redirect before giving up — well under GitHub's own 1-hour
// manifest-code expiry, so a human who wanders off mid-flow gets a clear
// local timeout (and can simply restart setup) rather than a silently-hung
// process. Var so tests can shrink it.
var manifestCallbackTimeout = 10 * time.Minute

// callbackResult is what the loopback listener captures from GitHub's
// manifest-flow redirect: either a usable code, or an error describing why
// the redirect wasn't usable (state mismatch, missing code).
type callbackResult struct {
	code string
	err  error
}

// githubManifestCreateURL is GitHub's App-creation-from-manifest endpoint —
// the form served at "/start" auto-submits the manifest here. See
// https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest.
const githubManifestCreateURL = "https://github.com/settings/apps/new"

var manifestFormTemplate = template.Must(template.New("manifest-form").Parse(`<!DOCTYPE html>
<html><body onload="document.forms[0].submit()">
<form action="{{.Action}}" method="post">
<input type="hidden" name="manifest" value="{{.Manifest}}">
</form>
<p>Redirecting to GitHub to create your Pruefer GitHub App&hellip;</p>
</body></html>
`))

// renderManifestForm renders the auto-submitting HTML form GitHub's
// manifest flow expects: a POST of the manifest JSON (as a single form
// field) to githubManifestCreateURL, with state carried as a query
// parameter GitHub echoes back unchanged on its redirect — this is how the
// loopback callback later confirms the redirect really answers this flow
// and not some other request hitting the same port.
func renderManifestForm(manifest map[string]interface{}, state string) (string, error) {
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshaling manifest: %w", err)
	}
	var buf bytes.Buffer
	err = manifestFormTemplate.Execute(&buf, struct {
		Action   string
		Manifest string
	}{
		Action:   githubManifestCreateURL + "?state=" + state,
		Manifest: string(manifestJSON),
	})
	if err != nil {
		return "", fmt.Errorf("rendering manifest form: %w", err)
	}
	return buf.String(), nil
}

func randomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random state: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// runManifestCallbackServer starts a temporary loopback HTTP server serving
// two routes: "/start" (the auto-submitting form above) and "/callback"
// (GitHub's redirect target once the user creates the App). The manifest is
// built from the redirect_url this call assigns — the listener port is only
// known after Listen, so buildManifestFn is called with it rather than the
// manifest being constructed up front. Returns the URL to open in a browser
// and a channel that receives exactly one callbackResult; shutdown must be
// called (even after a successful result) to stop the listener.
func runManifestCallbackServer(buildManifestFn func(redirectURL string) map[string]interface{}) (startURL string, results <-chan callbackResult, shutdown func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, fmt.Errorf("starting loopback listener: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	state, err := randomState()
	if err != nil {
		ln.Close()
		return "", nil, nil, err
	}

	manifest := buildManifestFn(redirectURL)
	formHTML, err := renderManifestForm(manifest, state)
	if err != nil {
		ln.Close()
		return "", nil, nil, err
	}

	resultCh := make(chan callbackResult, 1)
	var once sync.Once
	mux := http.NewServeMux()
	// /start (and therefore state, embedded in formHTML's form-action URL)
	// is served to any requester that can reach this loopback port, not just
	// the browser this flow opened — so state here is not secret from a
	// co-resident local process the way it is from a remote/cross-site
	// attacker. Accepted risk, not an oversight: reading it requires local
	// code execution on the machine running Pruefer, which is already a
	// stronger precondition than this flow's threat model (cross-site/CSRF)
	// defends against — a co-resident attacker with that capability can
	// already read AppPrivateKeyPath directly, no callback race needed.
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, formHTML)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 {
			// Either no state parameter at all, or one that doesn't match
			// ours — neither is a plausible attempt at completing this flow
			// (a genuine GitHub redirect always echoes back our exact
			// state), so both are just a stray hit on this ephemeral port
			// (browser prefetch, an extension, antivirus scanning, or a
			// local process guessing/smuggling a state — see /start's
			// comment above on that residual risk). Respond and keep
			// listening rather than triggering once.Do: consuming the
			// single-buffered resultCh here would permanently drop the real
			// GitHub redirect if it arrives afterward. This also covers a
			// hit carrying only ?code= with no state, which would otherwise
			// fall through to the mismatch case below with the same effect.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// From here, state matches ours — this can only plausibly be
		// GitHub's own redirect completing the flow, so a further problem
		// (missing code) is a real, terminal failure worth surfacing rather
		// than a stray hit to ignore.
		var cbErr error
		if q.Get("code") == "" {
			cbErr = fmt.Errorf("callback missing code parameter")
		}
		delivered := false
		once.Do(func() {
			delivered = true
			if cbErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, "Pruefer setup failed: "+cbErr.Error())
				resultCh <- callbackResult{err: cbErr}
				return
			}
			fmt.Fprintln(w, "Pruefer setup received — you can close this tab.")
			resultCh <- callbackResult{code: q.Get("code")}
		})
		if !delivered {
			// A second state-matching hit (browser retry/prefetch after the
			// real redirect already completed, or a double-click) — the
			// single-buffered resultCh has already been filled, so there's
			// nothing left to deliver. Respond explicitly rather than
			// falling through to an unadorned empty 200, so this doesn't
			// look like a hang to whoever's driving the browser.
			fmt.Fprintln(w, "Pruefer setup already completed — you can close this tab.")
		}
	})

	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	shutdownFn := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/start", port), resultCh, shutdownFn, nil
}

// waitForCallback blocks until the loopback server reports a result, the
// context is cancelled, or manifestCallbackTimeout elapses — whichever comes
// first. A timeout is reported as an ordinary error so RunManifestFlow can
// leave existing valid local config untouched and let the caller restart
// the flow cleanly, per the issue's expiry-handling requirement.
func waitForCallback(ctx context.Context, results <-chan callbackResult) (string, error) {
	select {
	case res := <-results:
		if res.err != nil {
			return "", res.err
		}
		return res.code, nil
	case <-time.After(manifestCallbackTimeout):
		return "", fmt.Errorf("manifest flow timed out waiting for GitHub redirect after %s — restart setup to try again", manifestCallbackTimeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
