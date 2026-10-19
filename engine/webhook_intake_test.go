package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/events/hookdeck"
)

// The gh-forward handler hands the delivery to the engine sink and acknowledges
// at once: the 200 is written before any of the apply work (#2351 R2).
func TestHandleWebhook_AcksBeforeApplyWork(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.secret = secret
	wm.mu.Unlock()

	release := make(chan struct{})
	applied := make(chan struct{})
	in := newIntake(newDeliveryLedger("", time.Now(), nil), func(string, []byte) {
		<-release // the apply work "takes longer than GitHub's 10 s timeout"
		close(applied)
	}, nil, time.Now)
	t.Cleanup(func() { close(release); in.Drain() })
	wm.SetSink(func(d delivery) { in.Submit(d) })

	body := []byte(`{"action":"created","repository":{"full_name":"myorg/myrepo"},"issue":{"number":3}}`)
	req := signedRequest(t, body, secret)
	req.Header.Set("X-GitHub-Event", "issue_comment")
	req.Header.Set("X-GitHub-Delivery", "d-1")
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() { wm.handleWebhook(rr, req); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleWebhook did not return while the apply work was still blocked: the ack waits for the apply")
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	select {
	case <-applied:
		t.Fatal("apply ran before it was released")
	default:
	}
}

func TestHandleWebhook_PassesDeliveryIDToSink(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	secret, _ := generateSecret()
	wm.mu.Lock()
	wm.secret = secret
	wm.mu.Unlock()

	var got []delivery
	wm.SetSink(func(d delivery) { got = append(got, d) })

	body := []byte(`{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":1}}`)
	req := signedRequest(t, body, secret)
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	wm.handleWebhook(httptest.NewRecorder(), req)

	// No header: still delivered, with an empty ID (applied undeduplicated).
	req2 := signedRequest(t, body, secret)
	req2.Header.Set("X-GitHub-Event", "issues")
	wm.handleWebhook(httptest.NewRecorder(), req2)

	if len(got) != 2 {
		t.Fatalf("sink saw %d deliveries, want 2", len(got))
	}
	if got[0].ID != "72d3162e-cc78-11e3-81ab-4c9367dc0958" || got[0].EventType != "issues" || got[0].Transport != "webhook" || string(got[0].Payload) != string(body) {
		t.Errorf("first delivery = %+v", got[0])
	}
	if got[1].ID != "" {
		t.Errorf("a delivery with no header must carry an empty ID, got %q", got[1].ID)
	}
}

// The listener accepts a body of GitHub's documented maximum and rejects one byte
// more with 413 — never silently truncating it into an HMAC failure (R6).
func TestHandleWebhook_BodyCap(t *testing.T) {
	if webhookMaxBodyBytes != 25<<20 {
		t.Fatalf("webhookMaxBodyBytes = %d, want 25 MiB", webhookMaxBodyBytes)
	}
	prefix := `{"action":"created","repository":{"full_name":"o/r"},"pad":"`
	suffix := `"}`
	build := func(total int) []byte {
		pad := total - len(prefix) - len(suffix)
		return []byte(prefix + strings.Repeat("x", pad) + suffix)
	}

	t.Run("exactly the cap is accepted", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		secret, _ := generateSecret()
		wm.mu.Lock()
		wm.secret = secret
		wm.mu.Unlock()
		var n atomic.Int32
		wm.SetSink(func(delivery) { n.Add(1) })
		body := build(webhookMaxBodyBytes)
		req := signedRequest(t, body, secret)
		req.Header.Set("X-GitHub-Event", "issue_comment")
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)
		if rr.Code != http.StatusOK || n.Load() != 1 {
			t.Fatalf("status=%d delivered=%d, want 200 and 1", rr.Code, n.Load())
		}
	})

	t.Run("one byte over is rejected with 413 and does not count as an HMAC failure", func(t *testing.T) {
		wm, _ := newTestWebhookManager(t)
		secret, _ := generateSecret()
		wm.mu.Lock()
		wm.secret = secret
		wm.mu.Unlock()
		var n atomic.Int32
		wm.SetSink(func(delivery) { n.Add(1) })
		body := build(webhookMaxBodyBytes + 1)
		req := signedRequest(t, body, secret)
		req.Header.Set("X-GitHub-Event", "issue_comment")
		rr := httptest.NewRecorder()
		wm.handleWebhook(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rr.Code)
		}
		wm.mu.Lock()
		failures := wm.consecutiveFailures
		wm.mu.Unlock()
		if n.Load() != 0 || failures != 0 {
			t.Fatalf("delivered=%d consecutiveFailures=%d; an oversized body must be neither applied nor an HMAC failure", n.Load(), failures)
		}
	})
}

// The Hookdeck source's frame cap and the gh-forward body cap are the same figure.
func TestHookdeckFrameCapMatchesWebhookBodyCap(t *testing.T) {
	if got := int64(hookdeck.MaxFrameBytes); got != webhookMaxBodyBytes {
		t.Fatalf("hookdeck frame cap = %d, gh-forward body cap = %d: they must both be GitHub's 25 MiB", got, webhookMaxBodyBytes)
	}
}

// Hookdeck hands the delivery ID through to the sink (it used to drop it).
func TestHookdeckManager_HandlePassesDeliveryID(t *testing.T) {
	hm, _ := newTestHookdeckManager(t)
	var got delivery
	hm.SetSink(func(d delivery) { got = d })
	hm.Handle(context.Background(), events.GitHubEvent{DeliveryID: "hd-9", EventType: "issues", Payload: []byte(`{"a":1}`)})
	if got.ID != "hd-9" || got.EventType != "issues" || got.Transport != "hookdeck" || string(got.Payload) != `{"a":1}` {
		t.Fatalf("delivery = %+v", got)
	}
}

// setReconnectSettle overrides the fallback delay on one manager.
func setReconnectSettle(wm *webhookManager, d time.Duration) {
	wm.mu.Lock()
	wm.reconnectSettle = d
	wm.mu.Unlock()
}

// gh-forward: the first subprocess launch is not a reconnect; a later restart
// requests its catch-up reconcile only once the settle delay has elapsed with the
// subprocess still running — never at launch, before the hook is attached (R5).
func TestSupervise_RestartDefersCatchUpUntilSettle(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	setReconnectSettle(wm, 150*time.Millisecond)
	wm.probeTimeout = 30 * time.Millisecond
	wm.mu.Lock()
	wm.orgModeFailed = true
	wm.mu.Unlock()

	var mu sync.Mutex
	var launches int
	var launchAt, reconnectAt []time.Time
	var reconnectLaunch []int
	wm.SetReconnectHandler(func() {
		mu.Lock()
		reconnectAt = append(reconnectAt, time.Now())
		reconnectLaunch = append(reconnectLaunch, launches)
		mu.Unlock()
	})
	wm.startSubprocessFn = func(ctx context.Context, args []string) (*exec.Cmd, <-chan string, error) {
		mu.Lock()
		launches++
		launchAt = append(launchAt, time.Now())
		mu.Unlock()
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 0.6")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		ch := make(chan string, 1)
		ch <- ""
		return cmd, ch, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go wm.supervise(ctx)
	<-ctx.Done()

	mu.Lock()
	defer mu.Unlock()
	if launches < 2 {
		t.Fatalf("only %d launches in 4s, want >= 2", launches)
	}
	if len(reconnectAt) == 0 {
		t.Fatal("a restart never requested a catch-up reconcile")
	}
	if reconnectLaunch[0] != 2 {
		t.Fatalf("first catch-up was requested during launch %d: the first launch must not request one", reconnectLaunch[0])
	}
	if gap := reconnectAt[0].Sub(launchAt[1]); gap < 100*time.Millisecond {
		t.Fatalf("catch-up requested %v after the restart, want it deferred by the settle delay", gap)
	}
}

// A subprocess that dies before the settle delay (a crash loop) requests nothing.
func TestSupervise_CrashLoopRequestsNoCatchUp(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	setReconnectSettle(wm, 10*time.Second)
	wm.probeTimeout = 30 * time.Millisecond
	wm.mu.Lock()
	wm.orgModeFailed = true
	wm.mu.Unlock()

	var launches, reconnects atomic.Int32
	wm.SetReconnectHandler(func() { reconnects.Add(1) })
	wm.startSubprocessFn = func(ctx context.Context, args []string) (*exec.Cmd, <-chan string, error) {
		launches.Add(1)
		cmd := exec.CommandContext(ctx, "sh", "-c", "exit 1")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		ch := make(chan string, 1)
		ch <- ""
		return cmd, ch, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go wm.supervise(ctx)
	<-ctx.Done()

	if launches.Load() < 2 {
		t.Fatalf("only %d launches in 3s, want >= 2", launches.Load())
	}
	if reconnects.Load() != 0 {
		t.Fatalf("%d catch-up requests from a crash-looping subprocess, want 0", reconnects.Load())
	}
}

// The first signature-verified delivery after a restart proves the stream is up
// and fires the owed catch-up exactly once; an unsigned one does not.
func TestHandleWebhook_FirstVerifiedDeliveryFiresPendingCatchUp(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	setReconnectSettle(wm, time.Hour)
	var fired atomic.Int32
	wm.SetReconnectHandler(func() { fired.Add(1) })
	wm.SetSink(func(delivery) {})
	wm.mu.Lock()
	secret := wm.secret
	wm.mu.Unlock()

	wm.armReconnect()
	bad := signedRequest(t, []byte(`{"action":"x"}`), "not-the-secret")
	wm.handleWebhook(httptest.NewRecorder(), bad)
	if fired.Load() != 0 {
		t.Fatal("an unverified delivery fired the catch-up")
	}
	for i := 0; i < 2; i++ {
		wm.handleWebhook(httptest.NewRecorder(), signedRequest(t, []byte(`{"action":"x"}`), secret))
	}
	if fired.Load() != 1 {
		t.Fatalf("catch-up fired %d times, want exactly 1", fired.Load())
	}
}

// Echo misses for reservations made while the hook was still attaching are not
// evidence of a dead stream, even when the sweep lands after the state turned Healthy.
func TestWebhookManager_NoteEchoMisses_IgnoresStartingUpReservations(t *testing.T) {
	wm, _ := newTestWebhookManager(t)
	base := time.Now()
	wm.mu.Lock()
	wm.state = WebhookStreamHealthy
	wm.startingUpSince = base.Add(-10 * time.Second)
	wm.startingUpEnd = base.Add(-5 * time.Second)
	wm.mu.Unlock()

	during := base.Add(-8 * time.Second)
	wm.noteEchoMisses([]time.Time{during, during, during})
	wm.mu.Lock()
	state, n := wm.state, len(wm.missHistory)
	wm.mu.Unlock()
	if state != WebhookStreamHealthy || n != 0 {
		t.Fatalf("state=%s misses=%d: startup-window reservations must not count", state, n)
	}

	after := base.Add(-2 * time.Second)
	wm.noteEchoMisses([]time.Time{after, after, after})
	wm.mu.Lock()
	state = wm.state
	wm.mu.Unlock()
	if state != WebhookStreamUnhealthy {
		t.Fatalf("state = %s: reservations made after the stream was up still count", state)
	}
}
