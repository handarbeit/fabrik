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

// manifestShutdownGraceTimeout bounds how long shutdownFn (below) waits for
// http.Server.Shutdown to drain in-flight connections gracefully before
// force-closing instead. Var so tests can shrink it.
var manifestShutdownGraceTimeout = 5 * time.Second

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
// called (even after a successful result) to stop the listener. logf may be
// nil (tests that don't care about the rare unexpected-Serve-error log
// line); RunManifestFlow always passes its own logf through.
func runManifestCallbackServer(buildManifestFn func(redirectURL string) map[string]interface{}, logf func(string, ...any)) (startURL string, results <-chan callbackResult, shutdown func(), err error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
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
	// firstDeliveryErr records whether the one result actually delivered on
	// resultCh (see once.Do below) was a success or a failure, so a later
	// state-matching hit (see the !delivered branch below) can report the
	// truth instead of unconditionally claiming success — sync.Once's Do
	// guarantees this write, made inside the first call's f, happens-before
	// any later call to Do returns, so reading it after once.Do here is
	// race-free without its own lock.
	var firstDeliveryErr error
	mux := http.NewServeMux()
	// /start (and therefore state, embedded in formHTML's form-action URL)
	// is served to any requester that can reach this loopback port, not just
	// the browser this flow opened — so state here is not secret from a
	// co-resident local process the way it is from a remote/cross-site
	// attacker. During this brief first-run window (before RunManifestFlow
	// has written anything to AppPrivateKeyPath/AppStatePath), a co-resident
	// attacker who races /start then completes App creation as their own
	// GitHub identity before the real user does could get this flow to
	// adopt an App *they* control — a materially different, and not yet
	// equally available, outcome versus "read the PEM directly" (there is
	// no PEM yet). Accepted risk anyway, not an oversight: the bar for it is
	// merely reading state off this process's own stdout/logs (or a process
	// listing showing the /start URL) and issuing a plain HTTP request with
	// it — not "arbitrary code execution" in the stronger sense that phrase
	// usually implies, just local, unprivileged access to this machine. A
	// threat model this flow's CSRF `state` check was never meant to cover
	// (that defends against remote/cross-site redirection, not a
	// co-resident process reading local output), and one under which every
	// other part of Pruefer's trust boundary (config files, the
	// browser-open command, the process itself) is already unenforceable.
	// Narrowing the window further (e.g. serving /start at most once) would
	// not close it, only shrink it, so it's left undone rather than adding
	// complexity for a race that stays exploitable either way under this
	// threat model.
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, formHTML)
	})
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		// GitHub's own redirect is always a GET. Rejecting other methods is
		// a cheap hardening step, but — per the /start comment above — it
		// does not close the actual co-resident-process risk: state is
		// carried in the URL (query string), so a GET with the right state
		// works exactly as well as any other method for whoever already has
		// it.
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
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
				firstDeliveryErr = cbErr
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
			// look like a hang to whoever's driving the browser — but say
			// what actually happened: the first delivery may have been a
			// failure (e.g. missing code), and "already completed" would
			// contradict that outcome for the same user.
			if firstDeliveryErr != nil {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintln(w, "Pruefer setup already failed: "+firstDeliveryErr.Error())
			} else {
				fmt.Fprintln(w, "Pruefer setup already completed — you can close this tab.")
			}
		}
	})

	// ReadHeaderTimeout guards against a slow-loris-style stray/slow local
	// connection tying up a server goroutine indefinitely during the
	// manifest-flow window — cheap insurance even though this listener is
	// 127.0.0.1-only and short-lived. ReadTimeout/WriteTimeout cover the
	// phase ReadHeaderTimeout doesn't: a connection that finishes sending
	// headers within 5s but then stalls (or a client that never closes a
	// completed response) could otherwise still tie up a goroutine for the
	// life of the process.
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			// http.ErrServerClosed is the expected return once shutdownFn
			// calls Shutdown/Close; anything else is unexpected and would
			// otherwise leave the manifest flow hanging until
			// manifestCallbackTimeout with no diagnostic pointing at the
			// actual cause.
			logf("manifest callback server: unexpected error: %v", err)
		}
	}()

	shutdownFn := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), manifestShutdownGraceTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Graceful shutdown didn't complete within the deadline — most
			// plausibly a stalled local connection ReadHeaderTimeout hasn't
			// caught yet. Force-close so the srv.Serve goroutine above is
			// guaranteed to exit rather than leaking for the rest of the
			// process's life; the manifest flow is already over by the
			// time shutdownFn runs; nothing more this listener could do
			// waiting.
			srv.Close()
		}
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
