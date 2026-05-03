package engine

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/tui"
)

// TestVerifySignature validates HMAC-SHA256 webhook signature checking.
func TestVerifySignature(t *testing.T) {
	secret := "testsecret"
	body := []byte(`{"action":"created"}`)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name   string
		body   []byte
		sig    string
		secret string
		want   bool
	}{
		{
			name:   "valid signature",
			body:   body,
			sig:    validSig,
			secret: secret,
			want:   true,
		},
		{
			name:   "wrong secret",
			body:   body,
			sig:    validSig,
			secret: "wrongsecret",
			want:   false,
		},
		{
			name:   "malformed header — no prefix",
			body:   body,
			sig:    hex.EncodeToString(mac.Sum(nil)),
			secret: secret,
			want:   false,
		},
		{
			name:   "malformed header — invalid hex",
			body:   body,
			sig:    "sha256=zzz",
			secret: secret,
			want:   false,
		},
		{
			name:   "empty signature",
			body:   body,
			sig:    "",
			secret: secret,
			want:   false,
		},
		{
			name: "empty body valid sig",
			body: []byte{},
			sig: func() string {
				m := hmac.New(sha256.New, []byte(secret))
				return "sha256=" + hex.EncodeToString(m.Sum(nil))
			}(),
			secret: secret,
			want:   true,
		},
		{
			name:   "body mismatch",
			body:   []byte(`{"action":"deleted"}`),
			sig:    validSig,
			secret: secret,
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := verifySignature(tc.body, tc.sig, tc.secret)
			if got != tc.want {
				t.Errorf("verifySignature() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSemverAtLeast covers version comparison edge cases.
func TestSemverAtLeast(t *testing.T) {
	tests := []struct {
		ver                 string
		major, minor, patch int
		want                bool
	}{
		{"2.40.1", 2, 32, 0, true},
		{"2.32.0", 2, 32, 0, true},
		{"2.31.9", 2, 32, 0, false},
		{"3.0.0", 2, 32, 0, true},
		{"1.99.99", 2, 32, 0, false},
		{"v2.40.1", 2, 32, 0, true},    // leading v
		{"2.32.0-rc1", 2, 32, 0, true}, // pre-release suffix
	}
	for _, tc := range tests {
		t.Run(fmt.Sprintf("%s>=%d.%d.%d", tc.ver, tc.major, tc.minor, tc.patch), func(t *testing.T) {
			got := semverAtLeast(tc.ver, tc.major, tc.minor, tc.patch)
			if got != tc.want {
				t.Errorf("semverAtLeast(%q,%d,%d,%d) = %v, want %v",
					tc.ver, tc.major, tc.minor, tc.patch, got, tc.want)
			}
		})
	}
}

// TestIsAuthShapedError covers stderr-shape detection for org/repo permission denials.
// GitHub returns 404 (not 403) for permission-denied access to org/repo resources to
// avoid leaking existence — the matcher must recognize that shape on hooks endpoints
// so the supervise loop falls back to per-repo mode instead of looping on transient
// retries.
func TestIsAuthShapedError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		// Classic auth-shaped messages.
		{"403", "Error: HTTP 403", true},
		{"forbidden", "Forbidden: insufficient scopes", true},
		{"permission denied", "permission denied for hook", true},
		{"requires admin", "this action requires admin access", true},
		{"not allowed", "operation not allowed for this user", true},
		// 404 on org hooks endpoint — the bug this test guards against.
		{"404 on /orgs/.../hooks", "Error: error creating webhook: HTTP 404: Not Found (https://api.github.com/orgs/handarbeit/hooks)", true},
		// 404 on repo hooks endpoint — symmetric per-repo case.
		{"404 on /repos/.../hooks", "Error: error creating webhook: HTTP 404: Not Found (https://api.github.com/repos/handarbeit/fabrik/hooks)", true},
		// Negative cases — bare 404s on other paths must not be treated as auth-shaped
		// (false positives would cause permanent fallback on legitimate not-found errors).
		{"404 unrelated path", "Error: HTTP 404: Not Found (https://api.github.com/users/missing)", false},
		{"404 issues endpoint", "Error: HTTP 404: Not Found (https://api.github.com/repos/x/y/issues/9999)", false},
		{"empty", "", false},
		{"unrelated noise", "connection reset by peer", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isAuthShapedError(tc.in)
			if got != tc.want {
				t.Errorf("isAuthShapedError(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestDetectOrgMode covers org detection from a repo set.
func TestDetectOrgMode(t *testing.T) {
	sameOrg := map[string]bool{"myorg/repo1": true, "myorg/repo2": true}
	org, ok := detectOrgMode(sameOrg)
	if !ok || org != "myorg" {
		t.Errorf("detectOrgMode(sameOrg) = %q,%v; want %q,true", org, ok, "myorg")
	}

	mixedOrg := map[string]bool{"myorg/repo1": true, "other/repo2": true}
	org, ok = detectOrgMode(mixedOrg)
	if ok || org != "" {
		t.Errorf("detectOrgMode(mixedOrg) = %q,%v; want %q,false", org, ok, "")
	}

	empty := map[string]bool{}
	_, ok = detectOrgMode(empty)
	if ok {
		t.Error("detectOrgMode(empty) should return false")
	}
}

// newTestWebhookManager creates a webhookManager wired for unit testing.
func newTestWebhookManager(t *testing.T) (*webhookManager, chan tui.Event) {
	t.Helper()
	events := make(chan tui.Event, 16)
	wm := newWebhookManager(
		func(_ int, _, _ string, _ ...any) {},
		make(chan struct{}, 1),
		func(e tui.Event) { events <- e },
		map[string]bool{"myorg/myrepo": true},
		nil,
		nil,
		nil,
	)
	return wm, events
}

// TestHealthStateTransitions exercises the time-based health state machine.
func TestHealthStateTransitions(t *testing.T) {
	t.Run("startup grace → healthy on first verified event", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamStartingUp
		wm.startupTime = time.Now()
		secret, _ := generateSecret()
		wm.secret = secret
		wm.mu.Unlock()

		// Simulate a valid webhook POST.
		body := []byte(`{"action":"created","repository":{"full_name":"myorg/myrepo"}}`)
		req := signedRequest(t, body, secret)
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rr.Code)
		}
		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamHealthy {
			t.Errorf("state = %q, want %q", state, WebhookStreamHealthy)
		}
	})

	t.Run("starting up → unhealthy after grace + health window", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamStartingUp
		// Set startup time far in the past so grace + health window have both elapsed.
		wm.startupTime = time.Now().Add(-(webhookStartupGrace + webhookHealthWindow + time.Second))
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamUnhealthy {
			t.Errorf("state = %q, want %q", state, WebhookStreamUnhealthy)
		}
	})

	t.Run("healthy → unhealthy after health window with no events", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamHealthy
		wm.lastEventTime = time.Now().Add(-(webhookHealthWindow + time.Second))
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamUnhealthy {
			t.Errorf("state = %q, want %q", state, WebhookStreamUnhealthy)
		}
	})

	t.Run("healthy stays healthy within health window", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamHealthy
		wm.lastEventTime = time.Now().Add(-1 * time.Minute) // within 10 min window
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamHealthy {
			t.Errorf("state = %q, want %q (should stay healthy)", state, WebhookStreamHealthy)
		}
	})
}

// TestSecretRotationTrigger verifies that 5 consecutive HMAC failures within 2 min
// trigger a secret rotation (rotateCycleCount increments).
func TestSecretRotationTrigger(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	secret, _ := generateSecret()
	wm.secret = secret
	wm.mu.Unlock()

	// Send webhookRotationFailures POST requests with wrong HMAC.
	wrongSecret := "wrongsecret"
	body := []byte(`{"action":"created"}`)
	for i := 0; i < webhookRotationFailures; i++ {
		req := signedRequest(t, body, wrongSecret)
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: expected 401, got %d", i+1, rr.Code)
		}
	}

	// rotateSecret kills the subprocess (nil here) and increments rotateCycleCount.
	wm.mu.Lock()
	cycles := wm.rotateCycleCount
	failures := wm.consecutiveFailures
	wm.mu.Unlock()

	if cycles != 1 {
		t.Errorf("rotateCycleCount = %d, want 1 after threshold breach", cycles)
	}
	if failures != 0 {
		t.Errorf("consecutiveFailures = %d after rotation, want 0", failures)
	}
}

// TestFailureWindowReset verifies that consecutive-failure tracking resets when
// a new failure arrives after the rotation window has elapsed.
func TestFailureWindowReset(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.secret = secret
	// Simulate 3 failures that happened beyond the rotation window.
	wm.consecutiveFailures = 3
	wm.firstFailureAt = time.Now().Add(-(webhookRotationWindow + time.Second))
	wm.mu.Unlock()

	// A new failure should reset the window; consecutiveFailures becomes 1.
	body := []byte(`{"action":"created"}`)
	req := signedRequest(t, body, "wrongsecret")
	rr := httptest.NewRecorder()
	wm.handleWebhook(rr, req)

	wm.mu.Lock()
	failures := wm.consecutiveFailures
	wm.mu.Unlock()
	if failures != 1 {
		t.Errorf("consecutiveFailures = %d after window reset, want 1", failures)
	}
}

// TestRotationFallback verifies that when rotateCycleCount already equals the max,
// the next threshold breach triggers disabled=true instead of another rotation.
func TestRotationFallback(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.secret = secret
	// Pre-seed rotateCycleCount at the limit so the next threshold breach disables.
	wm.rotateCycleCount = webhookRotationMaxCycles
	wm.mu.Unlock()

	body := []byte(`{"action":"created"}`)
	wrongSecret := "wrongsecret"

	// Trigger another rotation by sending enough consecutive failures.
	for i := 0; i < webhookRotationFailures; i++ {
		req := signedRequest(t, body, wrongSecret)
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)
	}

	wm.mu.Lock()
	disabled := wm.disabled
	state := wm.state
	wm.mu.Unlock()

	if !disabled {
		t.Error("webhookManager should be disabled after max rotation cycles")
	}
	if state != WebhookStreamUnhealthy {
		t.Errorf("state = %q after disable, want %q", state, WebhookStreamUnhealthy)
	}
}

// TestHandleWebhookIncrementsCounters verifies per-type event counting and wakeCh.
func TestHandleWebhookIncrementsCounters(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	wm.startupTime = time.Now()
	wm.secret = secret
	wm.mu.Unlock()

	body := []byte(`{"action":"created","repository":{"full_name":"myorg/myrepo"}}`)
	req := signedRequest(t, body, secret)
	req.Header.Set("X-GitHub-Event", "issue_comment")
	rr := httptest.NewRecorder()
	wm.handleWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	wm.mu.Lock()
	count := wm.eventCounts["issue_comment"]
	wm.mu.Unlock()
	if count != 1 {
		t.Errorf("eventCounts[issue_comment] = %d, want 1", count)
	}

	// wakeCh should have received a signal.
	select {
	case <-wm.wakeCh:
		// good
	default:
		t.Error("wakeCh should have been signaled on valid event")
	}
}

// TestBuildGhArgs covers argument construction for org-mode and per-repo mode.
func TestBuildGhArgs(t *testing.T) {
	events := []string{"issues", "pull_request"}

	// org mode
	args := buildGhArgs("myorg", nil, 9876, "mysecret", events)
	assertContains(t, args, "--org=myorg")
	assertContains(t, args, "--url=http://127.0.0.1:9876/")
	assertContains(t, args, "--secret=mysecret")
	assertContains(t, args, "--events=issues,pull_request")

	// per-repo mode
	args = buildGhArgs("", []string{"myorg/repo1", "myorg/repo2"}, 9876, "mysecret", events)
	assertContains(t, args, "--repo=myorg/repo1")
	assertContains(t, args, "--repo=myorg/repo2")
}

// TestIsHealthyOrStartingUp covers the backoff-coupling helper.
func TestIsHealthyOrStartingUp(t *testing.T) {
	wm, _ := newTestWebhookManager(t)

	for _, s := range []WebhookHealthState{WebhookStreamStartingUp, WebhookStreamHealthy} {
		wm.mu.Lock()
		wm.state = s
		wm.mu.Unlock()
		if !wm.IsHealthyOrStartingUp() {
			t.Errorf("state %q: IsHealthyOrStartingUp() = false, want true", s)
		}
	}

	wm.mu.Lock()
	wm.state = WebhookStreamUnhealthy
	wm.mu.Unlock()
	if wm.IsHealthyOrStartingUp() {
		t.Error("state unhealthy: IsHealthyOrStartingUp() = true, want false")
	}
}

// TestUpdateRepos_NewRepoTriggersRestart verifies that discovering a new repo updates
// wm.repos and kills the current subprocess.
func TestUpdateRepos_NewRepoTriggersRestart(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	// Override killFn with a counter; no real subprocess needed.
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	// Seed with a single repo and point currentCmd to a non-nil sentinel.
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.currentCmd = &exec.Cmd{} // non-nil so killFn is invoked
	wm.mu.Unlock()

	wm.UpdateRepos(map[string]bool{"owner/a": true, "owner/b": true})

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	wm.mu.Unlock()

	if !repos["owner/a"] || !repos["owner/b"] {
		t.Errorf("repos = %v, want {owner/a, owner/b}", repos)
	}
	if killCount != 1 {
		t.Errorf("killFn called %d times, want 1 on new-repo discovery", killCount)
	}
}

// TestUpdateRepos_NoNewRepos_NoRestart verifies that calling UpdateRepos with an
// unchanged repo set does not kill the subprocess.
func TestUpdateRepos_NoNewRepos_NoRestart(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.currentCmd = &exec.Cmd{}
	wm.mu.Unlock()

	wm.UpdateRepos(map[string]bool{"owner/a": true})

	if killCount != 0 {
		t.Errorf("killFn called %d times on no-change update, want 0", killCount)
	}
}

// TestUpdateRepos_DisabledManager_NoOp verifies that a disabled manager ignores
// UpdateRepos calls entirely.
func TestUpdateRepos_DisabledManager_NoOp(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	wm.mu.Lock()
	wm.disabled = true
	wm.repos = map[string]bool{"owner/a": true}
	wm.currentCmd = &exec.Cmd{}
	wm.mu.Unlock()

	wm.UpdateRepos(map[string]bool{"owner/a": true, "owner/b": true})

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	wm.mu.Unlock()

	if repos["owner/b"] {
		t.Error("disabled manager should not update repos")
	}
	if killCount != 0 {
		t.Error("disabled manager should not call killFn")
	}
}

// TestHandleWebhookBurstCoalescence verifies that N rapid webhook events result in
// at most 1 item queued on the buffered wakeCh (single-slot channel collapses bursts).
func TestHandleWebhookBurstCoalescence(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	wm.startupTime = time.Now()
	wm.secret = secret
	wm.mu.Unlock()

	body := []byte(`{"action":"created","repository":{"full_name":"myorg/myrepo"}}`)

	const burst = 5
	for i := 0; i < burst; i++ {
		req := signedRequest(t, body, secret)
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i, rr.Code)
		}
	}

	// The buffered channel (cap 1) must hold exactly 1 pending wake.
	count := 0
	for {
		select {
		case <-wm.wakeCh:
			count++
		default:
			if count != 1 {
				t.Errorf("wakeCh queued %d items after burst of %d, want 1", count, burst)
			}
			return
		}
	}
}

// signedRequest builds an HTTP POST with a valid HMAC-SHA256 signature.
func signedRequest(t *testing.T, body []byte, secret string) *http.Request {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("Content-Type", "application/json")
	return req
}

func assertContains(t *testing.T, slice []string, val string) {
	t.Helper()
	for _, s := range slice {
		if s == val {
			return
		}
	}
	t.Errorf("expected %q in %v", val, slice)
}
