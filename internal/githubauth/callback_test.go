package githubauth

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

var manifestActionRe = regexp.MustCompile(`action="([^"]+)"`)

// fetchStartAndExtractState fetches the /start page and extracts the state
// query parameter from its auto-submit form's action URL — exactly what a
// real browser would send on to GitHub, letting the test complete the round
// trip through the loopback server's own CSRF check without needing to know
// the internally-generated state ahead of time.
func fetchStartAndExtractState(t *testing.T, startURL string) string {
	t.Helper()
	resp, err := http.Get(startURL)
	if err != nil {
		t.Fatalf("GET %s: %v", startURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading start page body: %v", err)
	}
	m := manifestActionRe.FindStringSubmatch(string(body))
	if m == nil {
		t.Fatalf("could not find form action in start page: %s", body)
	}
	actionURL, err := url.Parse(m[1])
	if err != nil {
		t.Fatalf("parsing form action URL %q: %v", m[1], err)
	}
	state := actionURL.Query().Get("state")
	if state == "" {
		t.Fatalf("form action URL %q has no state parameter", m[1])
	}
	return state
}

func testManifestBuilder(redirectURL string) map[string]interface{} {
	return map[string]interface{}{"name": "test-app", "redirect_url": redirectURL}
}

func TestRunManifestCallbackServer_HappyPath(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=testcode123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status = %d, want 200", resp.StatusCode)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("unexpected callback error: %v", res.err)
		}
		if res.code != "testcode123" {
			t.Errorf("code = %q, want testcode123", res.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result")
	}
}

// TestRunManifestCallbackServer_StateMismatchDoesNotConsumeResult is the
// regression test for a review finding: a /callback hit with a
// wrong-but-non-empty state (e.g. a different local process trying to
// complete the flow with its own code, or any other stray hit on this
// ephemeral port) must not consume the single-buffered result channel. The
// state check still rejects it (a wrong state can never be accepted as a
// completion of this flow), but treating it as terminal would silently drop
// the genuine GitHub redirect — carrying the correct state — if it arrives
// afterward, hanging the flow until manifestCallbackTimeout with no
// indication of the real cause.
func TestRunManifestCallbackServer_StateMismatchDoesNotConsumeResult(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	// Deliberately ignore the real state and supply a wrong one, simulating
	// a different local process trying to complete the flow with its own
	// code — the CSRF check must reject this, not silently accept it, but
	// must also not block the real redirect that follows.
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=wrong-state&code=testcode123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("state-mismatch callback status = %d, want 404", resp.StatusCode)
	}

	state := fetchStartAndExtractState(t, startURL)
	realCallbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=realcode123"
	resp2, err := http.Get(realCallbackURL)
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("real callback status = %d, want 200", resp2.StatusCode)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("unexpected callback error: %v", res.err)
		}
		if res.code != "realcode123" {
			t.Errorf("code = %q, want realcode123 (the genuine redirect must not have been dropped)", res.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result — the genuine redirect after a state-mismatch hit was silently dropped")
	}
}

func TestRunManifestCallbackServer_MissingCodeRejected(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()

	select {
	case res := <-results:
		if res.err == nil {
			t.Fatal("expected a missing-code error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result")
	}
}

// TestRunManifestCallbackServer_SpuriousBareHitDoesNotConsumeResult is the
// regression test for a review finding: a /callback hit carrying neither a
// state nor a code parameter (e.g. a stray local probe hitting this
// ephemeral port — browser prefetch, an extension, antivirus scanning) must
// not consume the single-buffered result channel. Since results is only
// ever read once, treating a spurious bare hit as terminal would silently
// drop the genuine GitHub redirect if it arrives afterward, hanging the
// flow until manifestCallbackTimeout with no indication of the real cause.
func TestRunManifestCallbackServer_SpuriousBareHitDoesNotConsumeResult(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	bareURL := strings.TrimSuffix(startURL, "/start") + "/callback"
	resp, err := http.Get(bareURL)
	if err != nil {
		t.Fatalf("GET bare callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bare callback status = %d, want 404", resp.StatusCode)
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=realcode123"
	resp2, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("real callback status = %d, want 200", resp2.StatusCode)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("unexpected callback error: %v", res.err)
		}
		if res.code != "realcode123" {
			t.Errorf("code = %q, want realcode123 (the genuine redirect must not have been dropped)", res.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result — the genuine redirect after a spurious bare hit was silently dropped")
	}
}

// TestRunManifestCallbackServer_CodeOnlyHitDoesNotConsumeResult is the
// regression test for a review finding: the earlier "state and code both
// absent" bare-hit guard let a hit carrying only ?code= (no state at all)
// fall through to the state-mismatch check, which permanently consumed the
// single-buffered result channel — silently dropping the genuine GitHub
// redirect if it arrived afterward. A stray hit with a code but no state is
// not a plausible completion attempt (GitHub always echoes state back) and
// must be ignored, not treated as terminal.
func TestRunManifestCallbackServer_CodeOnlyHitDoesNotConsumeResult(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	codeOnlyURL := strings.TrimSuffix(startURL, "/start") + "/callback?code=attackercode"
	resp, err := http.Get(codeOnlyURL)
	if err != nil {
		t.Fatalf("GET code-only callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("code-only callback status = %d, want 404", resp.StatusCode)
	}

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=realcode123"
	resp2, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("real callback status = %d, want 200", resp2.StatusCode)
	}

	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("unexpected callback error: %v", res.err)
		}
		if res.code != "realcode123" {
			t.Errorf("code = %q, want realcode123 (the genuine redirect must not have been dropped)", res.code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result — the genuine redirect after a code-only hit was silently dropped")
	}
}

func TestWaitForCallback_TimeoutRestoresCleanly(t *testing.T) {
	old := manifestCallbackTimeout
	manifestCallbackTimeout = 30 * time.Millisecond
	defer func() { manifestCallbackTimeout = old }()

	results := make(chan callbackResult) // never sent to
	_, err := waitForCallback(context.Background(), results)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want it to mention timeout", err)
	}
}

func TestWaitForCallback_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan callbackResult)
	if _, err := waitForCallback(ctx, results); err == nil {
		t.Fatal("expected an error when context is already cancelled")
	}
}

func TestRenderManifestForm_EscapesManifestContent(t *testing.T) {
	html, err := renderManifestForm(map[string]interface{}{"name": `"><script>alert(1)</script>`}, "somestate")
	if err != nil {
		t.Fatalf("renderManifestForm: %v", err)
	}
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Errorf("expected manifest content to be escaped, got: %s", html)
	}
}

func TestRunManifestCallbackServer_PortAssignedDynamically(t *testing.T) {
	startURL, _, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()
	if !strings.HasPrefix(startURL, "http://127.0.0.1:") {
		t.Errorf("startURL = %q, want a 127.0.0.1 loopback URL", startURL)
	}
	if !strings.HasSuffix(startURL, "/start") {
		t.Errorf("startURL = %q, want it to end in /start", startURL)
	}
}

func TestRunManifestCallbackServer_ManifestReceivesAssignedRedirectURL(t *testing.T) {
	var gotRedirectURL string
	startURL, _, shutdown, err := runManifestCallbackServer(func(redirectURL string) map[string]interface{} {
		gotRedirectURL = redirectURL
		return map[string]interface{}{"redirect_url": redirectURL}
	})
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	if !strings.Contains(gotRedirectURL, "/callback") {
		t.Errorf("redirectURL = %q, want it to point at /callback", gotRedirectURL)
	}
	if !strings.HasPrefix(gotRedirectURL, "http://127.0.0.1:") {
		t.Errorf("redirectURL = %q, want a loopback URL", gotRedirectURL)
	}
	// The port in redirectURL must match the one actually served.
	wantRedirectURL := strings.TrimSuffix(startURL, "/start") + "/callback"
	if gotRedirectURL != wantRedirectURL {
		t.Errorf("redirectURL = %q, want %q (same port as startURL %q)", gotRedirectURL, wantRedirectURL, startURL)
	}
}

// TestRunManifestCallbackServer_DuplicateStateMatchingHitRespondsExplicitly
// is the regression test for a review finding: once the first state-matching
// /callback hit has been delivered on the single-buffered result channel,
// once.Do makes any further state-matching hit (a browser retry/prefetch, or
// a user double-clicking the redirect) a no-op — previously that fell
// through to an unadorned empty 200 rather than an explicit response,
// which could look like a hang to whoever's driving the browser.
func TestRunManifestCallbackServer_DuplicateStateMatchingHitRespondsExplicitly(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	state := fetchStartAndExtractState(t, startURL)
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=" + state + "&code=testcode123"

	resp1, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback (first): %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("first callback status = %d, want 200", resp1.StatusCode)
	}

	select {
	case res := <-results:
		if res.err != nil || res.code != "testcode123" {
			t.Fatalf("unexpected first result: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first callback result")
	}

	resp2, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback (duplicate): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("duplicate callback status = %d, want 200", resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("reading duplicate callback body: %v", err)
	}
	if !strings.Contains(string(body), "already completed") {
		t.Errorf("duplicate callback body = %q, want it to explicitly say the flow already completed", body)
	}
}
