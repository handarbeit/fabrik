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

func TestRunManifestCallbackServer_StateMismatchRejected(t *testing.T) {
	startURL, results, shutdown, err := runManifestCallbackServer(testManifestBuilder)
	if err != nil {
		t.Fatalf("runManifestCallbackServer: %v", err)
	}
	defer shutdown()

	// Deliberately ignore the real state and supply a wrong one, simulating
	// a different local process trying to complete the flow with its own
	// code — the CSRF check must reject this, not silently accept it.
	callbackURL := strings.TrimSuffix(startURL, "/start") + "/callback?state=wrong-state&code=testcode123"
	resp, err := http.Get(callbackURL)
	if err != nil {
		t.Fatalf("GET callback: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("callback status = %d, want 400", resp.StatusCode)
	}

	select {
	case res := <-results:
		if res.err == nil {
			t.Fatal("expected a state-mismatch error, got nil")
		}
		if !strings.Contains(res.err.Error(), "state mismatch") {
			t.Errorf("error = %v, want it to mention state mismatch", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback result")
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
