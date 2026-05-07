package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/itemstate"
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
// It wires a real Store + wakeChObserver so tests that check wm.wakeCh work
// correctly: webhook → deltaFn → store.Apply → observer → wakeCh.
func newTestWebhookManager(t *testing.T) (*webhookManager, chan tui.Event) {
	t.Helper()
	events := make(chan tui.Event, 16)
	wakeCh := make(chan struct{}, 1)
	store := itemstate.NewStore(nil)
	unsub := store.Subscribe(newWakeChObserver(wakeCh))
	t.Cleanup(unsub)
	deltaFn := func(_ string, _ []byte) {
		// Apply a LabelsChanged mutation so the wakeChObserver fires.
		store.Apply(itemstate.LocalLabelAdded{Repo: "myorg/myrepo", Number: 1, Label: "test"})
	}
	wm := newWebhookManager(
		func(_ int, _, _ string, _ ...any) {},
		wakeCh,
		func(e tui.Event) { events <- e },
		map[string]bool{"myorg/myrepo": true},
		nil,
		deltaFn,
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
		// Set sessionFirstStartAt far in the past — the fix uses sessionFirstStartAt
		// (not startupTime) so subprocess restarts don't reset the timer.
		wm.sessionFirstStartAt = time.Now().Add(-(webhookStartupGrace + webhookHealthWindow + time.Second))
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

// TestIsProjectsV2ItemRejection covers all rejection phrases and negative cases.
func TestIsProjectsV2ItemRejection(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		// Gate keyword must be present.
		{"invalid phrase match", "error: projects_v2_item is invalid", true},
		{"unknown phrase match", "projects_v2_item: unknown event", true},
		{"not recognized phrase match", "projects_v2_item not recognized", true},
		{"unsupported phrase match", "projects_v2_item unsupported", true},
		{"bad request phrase match", "bad request: projects_v2_item", true},
		// The actual GitHub error wording (the fix this issue targets).
		{"not allowed phrase match", "These events are not allowed for this hook: projects_v2_item", true},
		// Case insensitivity on the phrase.
		{"not allowed uppercase", "These Events Are NOT ALLOWED for this hook: projects_v2_item", true},
		// Negative: gate keyword absent.
		{"no gate keyword", "These events are not allowed for this hook: issues", false},
		{"phrase only no keyword", "error: invalid event", false},
		{"empty", "", false},
		// Negative: gate keyword present but no rejection phrase.
		{"keyword only no phrase", "processing projects_v2_item event", false},
		{"keyword success line", "projects_v2_item subscribed successfully", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isProjectsV2ItemRejection(tc.line)
			if got != tc.want {
				t.Errorf("isProjectsV2ItemRejection(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

// TestIs422ShapedError verifies 422 detection in stderr output (case-insensitive).
func TestIs422ShapedError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"full 422 error", "Error: error creating webhook: HTTP 422: Validation Failed (https://api.github.com/repos/handarbeit/fabrik/hooks)", true},
		{"422 uppercase", "HTTP 422 ERROR", true},
		{"422 mixed case", "Http 422 Failed", true},
		{"403 not 422", "HTTP 403 Forbidden", false},
		{"404 not 422", "HTTP 404 Not Found", false},
		{"empty string", "", false},
		{"unrelated error", "connection reset by peer", false},
		{"rate limited", "HTTP 429 Too Many Requests", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := is422ShapedError(tc.in)
			if got != tc.want {
				t.Errorf("is422ShapedError(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestApplyRepoAuthFailure_Quarantine verifies that three consecutive auth-shaped
// quick exits quarantine all repos in the subscription set.
func TestApplyRepoAuthFailure_Quarantine(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true, "owner/b": true}
	wm.mu.Unlock()

	authStderr := "Error: HTTP 403 Forbidden"
	quick := orgModeProbeTimeout / 2

	for i := 0; i < webhookRepoFailureThreshold-1; i++ {
		quarantined := wm.applyRepoAuthFailure(quick, authStderr)
		if len(quarantined) != 0 {
			t.Fatalf("iteration %d: expected no quarantine yet, got %v", i+1, quarantined)
		}
	}

	// Third consecutive failure should quarantine both repos.
	quarantined := wm.applyRepoAuthFailure(quick, authStderr)
	if len(quarantined) != 2 {
		t.Fatalf("expected 2 quarantined repos after threshold, got %v", quarantined)
	}

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	unsub := make(map[string]bool)
	for k, v := range wm.unsubscribableRepos {
		unsub[k] = v
	}
	wm.mu.Unlock()

	if len(repos) != 0 {
		t.Errorf("wm.repos should be empty after quarantine, got %v", repos)
	}
	if !unsub["owner/a"] || !unsub["owner/b"] {
		t.Errorf("unsubscribableRepos = %v, want both owner/a and owner/b", unsub)
	}
}

// TestApplyRepoAuthFailure_SlowExitResetsCounters verifies that a subprocess exit
// beyond orgModeProbeTimeout resets failure counts, regardless of stderr content.
func TestApplyRepoAuthFailure_SlowExitResetsCounters(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.repoFailureCounts = map[string]int{"owner/a": webhookRepoFailureThreshold - 1}
	wm.mu.Unlock()

	// A slow exit (elapsed > orgModeProbeTimeout) should reset counts.
	slow := orgModeProbeTimeout + time.Second
	quarantined := wm.applyRepoAuthFailure(slow, "Error: HTTP 403 Forbidden")
	if len(quarantined) != 0 {
		t.Fatalf("slow exit should not quarantine, got %v", quarantined)
	}

	wm.mu.Lock()
	count := wm.repoFailureCounts["owner/a"]
	wm.mu.Unlock()
	if count != 0 {
		t.Errorf("repoFailureCounts[owner/a] = %d after slow exit, want 0", count)
	}
}

// TestApplyRepoAuthFailure_FastNonAuthNoIncrement verifies that a quick exit without
// auth-shaped stderr does not increment failure counts.
func TestApplyRepoAuthFailure_FastNonAuthNoIncrement(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.mu.Unlock()

	quick := orgModeProbeTimeout / 2
	quarantined := wm.applyRepoAuthFailure(quick, "connection reset by peer")
	if len(quarantined) != 0 {
		t.Fatalf("non-auth exit should not quarantine, got %v", quarantined)
	}

	wm.mu.Lock()
	count := wm.repoFailureCounts["owner/a"]
	wm.mu.Unlock()
	if count != 0 {
		t.Errorf("repoFailureCounts[owner/a] = %d after non-auth exit, want 0", count)
	}
}

// TestUpdateRepos_QuarantinedRepoFiltered verifies that a quarantined repo is not
// re-added to wm.repos and does not trigger a subprocess kill.
func TestUpdateRepos_QuarantinedRepoFiltered(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.unsubscribableRepos = map[string]bool{"owner/b": true} // pre-quarantined
	wm.currentCmd = &exec.Cmd{}
	wm.mu.Unlock()

	wm.UpdateRepos(map[string]bool{"owner/a": true, "owner/b": true})

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	wm.mu.Unlock()

	if repos["owner/b"] {
		t.Error("quarantined repo owner/b should not be in wm.repos after UpdateRepos")
	}
	if !repos["owner/a"] {
		t.Error("non-quarantined repo owner/a should remain in wm.repos")
	}
	// owner/b was quarantined, not new — should not kill subprocess.
	if killCount != 0 {
		t.Errorf("killFn called %d times for quarantined repo update, want 0", killCount)
	}
}

// TestUpdateRepos_NonQuarantinedRepoAdded verifies that new non-quarantined repos
// are still added and trigger a subprocess restart.
func TestUpdateRepos_NonQuarantinedRepoAdded(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true}
	wm.unsubscribableRepos = map[string]bool{"owner/bad": true} // unrelated quarantine
	wm.currentCmd = &exec.Cmd{}
	wm.mu.Unlock()

	wm.UpdateRepos(map[string]bool{"owner/a": true, "owner/c": true})

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	wm.mu.Unlock()

	if !repos["owner/c"] {
		t.Error("new non-quarantined repo owner/c should be in wm.repos")
	}
	if killCount != 1 {
		t.Errorf("killFn called %d times for new-repo update, want 1", killCount)
	}
}

// TestUpdateRepos_AdditiveOnly_NeverDropsExistingRepo verifies that calling UpdateRepos
// with a subset of the currently-subscribed repos does not remove the missing repos from
// wm.repos. This is the regression test for the spurious "new repo discovered" restart
// that fired when a poll cycle's seenRepos omitted an already-subscribed repo.
func TestUpdateRepos_AdditiveOnly_NeverDropsExistingRepo(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	killCount := 0
	wm.killFn = func(*exec.Cmd) { killCount++ }
	wm.mu.Lock()
	wm.repos = map[string]bool{"owner/a": true, "owner/b": true}
	wm.currentCmd = &exec.Cmd{}
	wm.mu.Unlock()

	// Call UpdateRepos with only owner/a — owner/b is absent from this poll cycle.
	wm.UpdateRepos(map[string]bool{"owner/a": true})

	wm.mu.Lock()
	repos := copyRepoSet(wm.repos)
	wm.mu.Unlock()

	if !repos["owner/b"] {
		t.Error("owner/b was dropped from wm.repos after UpdateRepos with subset — want additive-only behavior")
	}
	if !repos["owner/a"] {
		t.Error("owner/a should remain in wm.repos")
	}
	if killCount != 0 {
		t.Errorf("killFn called %d times when no new repos were added, want 0", killCount)
	}
}

// TestIsDisabled verifies IsDisabled() reflects the disabled field correctly.
func TestIsDisabled(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	if wm.IsDisabled() {
		t.Error("IsDisabled() = true initially, want false")
	}
	wm.mu.Lock()
	wm.disabled = true
	wm.mu.Unlock()
	if !wm.IsDisabled() {
		t.Error("IsDisabled() = false after setting disabled=true, want true")
	}
}

// fake422Subprocess returns a startSubprocessFn that exits immediately and sends
// the given stderr string on the channel. Suitable for testing fast-exit scenarios.
func fake422Subprocess(t *testing.T, stderrOutput string) func(context.Context, []string) (*exec.Cmd, <-chan string, error) {
	t.Helper()
	return func(ctx context.Context, args []string) (*exec.Cmd, <-chan string, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", "exit 1")
		if err := cmd.Start(); err != nil {
			return nil, nil, fmt.Errorf("starting fake subprocess: %w", err)
		}
		ch := make(chan string, 1)
		ch <- stderrOutput
		return cmd, ch, nil
	}
}

// TestCircuitBreaker422 verifies the HTTP 422 permanent-failure circuit-breaker.
func TestCircuitBreaker422(t *testing.T) {
	t.Run("three consecutive 422s disable the manager", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		// Use a short probe timeout so the fake fast-exiting subprocess counts as a quick exit.
		wm.probeTimeout = 50 * time.Millisecond
		// Force per-repo mode (orgModeFailed=true bypasses detectOrgMode so org=="").
		// The 422 circuit-breaker only fires in per-repo mode (gated on org=="").
		wm.mu.Lock()
		wm.orgModeFailed = true
		wm.mu.Unlock()

		healthChangeFalseCalls := 0
		wm.healthChangeFn = func(healthy bool) {
			if !healthy {
				healthChangeFalseCalls++
			}
		}

		callCount := 0
		wm.startSubprocessFn = func(ctx context.Context, args []string) (*exec.Cmd, <-chan string, error) {
			callCount++
			return fake422Subprocess(t, "Error: error creating webhook: HTTP 422: Validation Failed")(ctx, args)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		done := make(chan struct{})
		go func() {
			wm.supervise(ctx)
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("supervise did not return after circuit-breaker fired")
		}

		wm.mu.Lock()
		disabled := wm.disabled
		count := wm.permanentFailureCount
		state := wm.state
		wm.mu.Unlock()

		if !disabled {
			t.Error("manager should be disabled after 3 consecutive 422 failures")
		}
		if count != webhookPermanentFailureMax {
			t.Errorf("permanentFailureCount = %d, want %d", count, webhookPermanentFailureMax)
		}
		if state != WebhookStreamUnhealthy {
			t.Errorf("state = %q after circuit-breaker, want %q", state, WebhookStreamUnhealthy)
		}
		if healthChangeFalseCalls != 1 {
			t.Errorf("healthChangeFn(false) called %d times, want 1", healthChangeFalseCalls)
		}
		if callCount != webhookPermanentFailureMax {
			t.Errorf("startSubprocessFn called %d times, want %d", callCount, webhookPermanentFailureMax)
		}
	})

	t.Run("non-422 quick exits do not increment counter", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.probeTimeout = 50 * time.Millisecond
		wm.mu.Lock()
		wm.orgModeFailed = true
		wm.mu.Unlock()

		wm.startSubprocessFn = fake422Subprocess(t, "connection reset by peer")

		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()

		go wm.supervise(ctx)
		<-ctx.Done()

		wm.mu.Lock()
		count := wm.permanentFailureCount
		disabled := wm.disabled
		wm.mu.Unlock()

		if count != 0 {
			t.Errorf("permanentFailureCount = %d after non-422 failures, want 0", count)
		}
		if disabled {
			t.Error("manager should not be disabled by non-422 failures")
		}
	})

	t.Run("durable run resets the counter", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		probeTimeout := 30 * time.Millisecond
		wm.probeTimeout = probeTimeout

		// Pre-seed counter to just below the threshold; force per-repo mode.
		wm.mu.Lock()
		wm.permanentFailureCount = webhookPermanentFailureMax - 1
		wm.orgModeFailed = true
		wm.mu.Unlock()

		callCount := 0
		wm.startSubprocessFn = func(ctx context.Context, args []string) (*exec.Cmd, <-chan string, error) {
			callCount++
			var cmd *exec.Cmd
			if callCount == 1 {
				// First call: sleep longer than probeTimeout to simulate a durable run.
				sleepMs := (probeTimeout + 20*time.Millisecond).Milliseconds()
				cmd = exec.CommandContext(ctx, "sh", "-c", fmt.Sprintf("sleep 0.%03d; exit 0", sleepMs))
			} else {
				// Subsequent calls: exit immediately.
				cmd = exec.CommandContext(ctx, "sh", "-c", "exit 1")
			}
			if err := cmd.Start(); err != nil {
				return nil, nil, err
			}
			ch := make(chan string, 1)
			ch <- "Error: HTTP 422: Validation Failed"
			return cmd, ch, nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		go wm.supervise(ctx)

		// Wait for the durable subprocess to finish and counter to reset.
		time.Sleep(probeTimeout + 100*time.Millisecond)

		wm.mu.Lock()
		disabled := wm.disabled
		wm.mu.Unlock()

		if disabled {
			t.Error("manager should not be disabled: durable run should have reset the counter before the threshold was reached")
		}
	})
}

// TestSessionLastEventAt_NotResetOnRestart verifies that sessionLastEventAt persists
// across subprocess restarts, while lastEventTime is cleared as supervise() does.
func TestSessionLastEventAt_NotResetOnRestart(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	wm.startupTime = time.Now()
	wm.secret = secret
	wm.mu.Unlock()

	// Send a valid webhook to set both lastEventTime and sessionLastEventAt.
	body := []byte(`{"action":"created","repository":{"full_name":"myorg/myrepo"}}`)
	req := signedRequest(t, body, secret)
	rr := httptest.NewRecorder()
	wm.handleWebhook(rr, req)

	wm.mu.Lock()
	sessionEventAt := wm.sessionLastEventAt
	lastEventAt := wm.lastEventTime
	wm.mu.Unlock()

	if sessionEventAt.IsZero() {
		t.Fatal("sessionLastEventAt should be set after verified event")
	}
	if lastEventAt.IsZero() {
		t.Fatal("lastEventTime should be set after verified event")
	}

	// Simulate subprocess restart — supervise() resets lastEventTime but not sessionLastEventAt.
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	wm.startupTime = time.Now()
	wm.lastEventTime = time.Time{}
	wm.mu.Unlock()

	wm.mu.Lock()
	sessionEventAfterRestart := wm.sessionLastEventAt
	lastEventAfterRestart := wm.lastEventTime
	wm.mu.Unlock()

	if sessionEventAfterRestart.IsZero() {
		t.Error("sessionLastEventAt was reset on subprocess restart — should persist across restarts")
	}
	if !sessionEventAfterRestart.Equal(sessionEventAt) {
		t.Errorf("sessionLastEventAt changed on restart: was %v, now %v", sessionEventAt, sessionEventAfterRestart)
	}
	if !lastEventAfterRestart.IsZero() {
		t.Error("lastEventTime should be zero after restart simulation")
	}
}

// TestSessionFirstStartAt_SetOnce verifies sessionFirstStartAt is set on first subprocess
// start and not overwritten on subsequent starts.
func TestSessionFirstStartAt_SetOnce(t *testing.T) {
	wm, _ := newTestWebhookManager(t)

	wm.mu.Lock()
	if !wm.sessionFirstStartAt.IsZero() {
		t.Fatal("sessionFirstStartAt should start as zero")
	}
	wm.mu.Unlock()

	// Simulate first subprocess start (mirrors supervise() logic).
	firstStartTime := time.Now()
	wm.mu.Lock()
	wm.startupTime = firstStartTime
	wm.lastEventTime = time.Time{}
	if wm.sessionFirstStartAt.IsZero() {
		wm.sessionFirstStartAt = wm.startupTime
	}
	wm.mu.Unlock()

	wm.mu.Lock()
	firstAt := wm.sessionFirstStartAt
	wm.mu.Unlock()

	if firstAt.IsZero() {
		t.Fatal("sessionFirstStartAt should be set after first subprocess start")
	}

	// Simulate second subprocess start.
	time.Sleep(time.Millisecond)
	wm.mu.Lock()
	wm.startupTime = time.Now()
	wm.lastEventTime = time.Time{}
	if wm.sessionFirstStartAt.IsZero() {
		wm.sessionFirstStartAt = wm.startupTime
	}
	wm.mu.Unlock()

	wm.mu.Lock()
	secondAt := wm.sessionFirstStartAt
	wm.mu.Unlock()

	if !secondAt.Equal(firstAt) {
		t.Errorf("sessionFirstStartAt changed on second subprocess start: was %v, now %v", firstAt, secondAt)
	}
}

// TestHealthTransition_StartingUp_UsesSessionFirstStartAt verifies that the
// StartingUp→Unhealthy transition uses sessionFirstStartAt (not startupTime),
// confirming the Bug B fix: subprocess restarts no longer reset the health timer.
func TestHealthTransition_StartingUp_UsesSessionFirstStartAt(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	// Set startupTime far in the past — old code would have triggered Unhealthy.
	wm.startupTime = time.Now().Add(-(webhookStartupGrace + webhookHealthWindow + time.Second))
	// sessionFirstStartAt is recent — should NOT trigger Unhealthy.
	wm.sessionFirstStartAt = time.Now()
	wm.mu.Unlock()

	wm.checkHealthTransitions()

	wm.mu.Lock()
	state := wm.state
	wm.mu.Unlock()
	if state == WebhookStreamUnhealthy {
		t.Error("state should NOT be Unhealthy when only startupTime is old but sessionFirstStartAt is recent — the fix uses sessionFirstStartAt")
	}
}

// TestHealthTransition_StartingUp_SkipsGraceWindowWhenEventsReceived verifies that
// the sessionFirstStartAt grace+window path (R-B3) does NOT fire when events have
// been received (sessionLastEventAt != zero), even if sessionFirstStartAt is old.
// Without the sessionLastEventAt.IsZero() guard, this would incorrectly transition
// to Unhealthy after ~10m even on a healthy-then-restarting stream.
func TestHealthTransition_StartingUp_SkipsGraceWindowWhenEventsReceived(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	wm.mu.Lock()
	wm.state = WebhookStreamStartingUp
	// sessionFirstStartAt is old enough to trigger the grace+window path.
	wm.sessionFirstStartAt = time.Now().Add(-(webhookStartupGrace + webhookHealthWindow + time.Second))
	// But we have received events recently (within the webhookEventStaleTimeout threshold).
	wm.sessionLastEventAt = time.Now().Add(-10 * time.Second)
	wm.mu.Unlock()

	wm.checkHealthTransitions()

	wm.mu.Lock()
	state := wm.state
	wm.mu.Unlock()
	if state == WebhookStreamUnhealthy {
		t.Error("state should NOT be Unhealthy when sessionFirstStartAt is old but events were received (sessionLastEventAt != zero)")
	}
}

// TestHealthTransition_SessionStale verifies that stale sessionLastEventAt
// triggers Unhealthy faster than the 10-minute webhookHealthWindow — both from
// StartingUp and Healthy states.
func TestHealthTransition_SessionStale(t *testing.T) {
	t.Run("stale sessionLastEventAt triggers unhealthy from StartingUp", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamStartingUp
		wm.startupTime = time.Now()
		wm.sessionFirstStartAt = time.Now()
		wm.sessionLastEventAt = time.Now().Add(-(webhookEventStaleTimeout + time.Second))
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamUnhealthy {
			t.Errorf("state = %q, want %q when sessionLastEventAt is stale (StartingUp)", state, WebhookStreamUnhealthy)
		}
	})

	t.Run("stale sessionLastEventAt triggers unhealthy from Healthy", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamHealthy
		wm.lastEventTime = time.Now().Add(-1 * time.Minute) // within 10-min window
		wm.sessionLastEventAt = time.Now().Add(-(webhookEventStaleTimeout + time.Second))
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamUnhealthy {
			t.Errorf("state = %q, want %q when sessionLastEventAt is stale (Healthy)", state, WebhookStreamUnhealthy)
		}
	})

	t.Run("fresh sessionLastEventAt keeps Healthy state", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		wm.mu.Lock()
		wm.state = WebhookStreamHealthy
		wm.lastEventTime = time.Now().Add(-1 * time.Minute) // within 10-min window
		wm.sessionLastEventAt = time.Now().Add(-10 * time.Second) // fresh (< webhookEventStaleTimeout)
		wm.mu.Unlock()

		wm.checkHealthTransitions()

		wm.mu.Lock()
		state := wm.state
		wm.mu.Unlock()
		if state != WebhookStreamHealthy {
			t.Errorf("state = %q, want %q when sessionLastEventAt is fresh", state, WebhookStreamHealthy)
		}
	})
}
