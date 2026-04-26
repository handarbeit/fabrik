package engine

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		name    string
		body    []byte
		sig     string
		secret  string
		want    bool
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
			name:   "empty body valid sig",
			body:   []byte{},
			sig:    func() string {
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
		ver           string
		major, minor, patch int
		want          bool
	}{
		{"2.40.1", 2, 32, 0, true},
		{"2.32.0", 2, 32, 0, true},
		{"2.31.9", 2, 32, 0, false},
		{"3.0.0", 2, 32, 0, true},
		{"1.99.99", 2, 32, 0, false},
		{"v2.40.1", 2, 32, 0, true},  // leading v
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
	wm.mu.Unlock()

	if !disabled {
		t.Error("webhookManager should be disabled after max rotation cycles")
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
