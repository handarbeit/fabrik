package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
)

// gatedHookAPI wraps a fakeHookAPI and holds ListAppHookDeliveries until gate
// is closed, so a test can act between startup and the startup pass.
type gatedHookAPI struct {
	*fakeHookAPI
	gate chan struct{}
}

func (g *gatedHookAPI) ListAppHookDeliveries(cursor string, perPage int) ([]gh.AppHookDelivery, string, error) {
	<-g.gate
	return g.fakeHookAPI.ListAppHookDeliveries(cursor, perPage)
}

func (f *fakeHookAPI) listCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

func (f *fakeHookAPI) redeliveredIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.redelivered...)
}

// startDirectEngineReplay is startDirectEngine with the replay switch and an
// injected delivery-log API.
func startDirectEngineReplay(t *testing.T, enabled bool, api hookDeliveryAPI) (*Engine, func()) {
	t.Helper()
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceDirect
	eng.cfg.DirectListen = "127.0.0.1:0"
	eng.cfg.AppWebhookPath = directTestPath
	eng.cfg.ReplayFailedDeliveries = enabled
	if api != nil {
		eng.SetReplayAPIForTest(api)
	}
	// No real timers: the follow-up delay (3 min) would otherwise fire in a later
	// test of the package. It blocks until the replayer's context ends.
	eng.SetReplayWaitForTest(func(ctx context.Context, d time.Duration) bool {
		<-ctx.Done()
		return false
	})
	cache, _ := eng.UseBoardCacheForTest()
	cleanup := startIngestion(t, eng, cache)
	return eng, cleanup
}

func issueBody(n int, label string) []byte {
	return []byte(fmt.Sprintf(`{"action":"opened","repository":{"full_name":"o/r"},"issue":{"number":%d,"node_id":"I_%d","title":"t","labels":[{"name":%q}],"assignees":[],"milestone":null,"updated_at":"2026-10-22T03:00:00Z"}}`, n, n, label))
}

func replayWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// AC1/AC2/AC6, R3 and SC-007 through the real listener, intake and ledger: the
// startup pass requests exactly the failed delivery the ledger has not seen,
// GitHub's redelivery re-enters through the normal handler and is applied once,
// and the transport's health row reports the pass.
func TestDirectReplay_StartupPassEndToEnd(t *testing.T) {
	oldPace := replayPace
	replayPace = 0
	t.Cleanup(func() { replayPace = oldPace })

	now := time.Now()
	recent := func(id int64, guid string, code int, ago time.Duration) gh.AppHookDelivery {
		return gh.AppHookDelivery{ID: id, GUID: guid, StatusCode: code, DeliveredAt: now.Add(-ago), Event: "issues"}
	}
	fake := &fakeHookAPI{pages: [][]gh.AppHookDelivery{{
		recent(21, "d-lost", 502, time.Minute),
		recent(20, "d-seen", 500, 2*time.Minute),
		recent(19, "d-ok", 200, 3*time.Minute),
	}}}
	api := &gatedHookAPI{fakeHookAPI: fake, gate: make(chan struct{})}

	eng, cleanup := startDirectEngineReplay(t, true, api)
	defer cleanup()
	tr := directFromEngine(t, eng)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}

	// Before the pass: the row already reports replay as enabled, all unknown.
	if r := tr.HealthRow().Replay; r == nil || r.LastPassAt.Valid {
		t.Fatalf("pre-pass replay health = %+v", r)
	}

	// The engine received d-seen while GitHub still lists it as failed.
	seenBody := issueBody(5, "seen")
	if r := h.post(t, seenBody, signDirect(seenBody), "issues", "d-seen"); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	eng.intake.Drain()

	// GitHub's redelivery of d-lost: the same X-GitHub-Delivery, to the listener.
	lostBody := issueBody(6, "lost")
	var postWG sync.WaitGroup
	fake.redeliverFn = func(id int64) error {
		postWG.Add(1)
		defer postWG.Done()
		if id != 21 {
			t.Errorf("redelivery requested for unexpected delivery %d", id)
			return nil
		}
		req, _ := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(lostBody))
		req.Header.Set("X-Hub-Signature-256", signDirect(lostBody))
		req.Header.Set("X-GitHub-Event", "issues")
		req.Header.Set("X-GitHub-Delivery", "d-lost")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	close(api.gate)
	replayWaitFor(t, "startup pass summary", func() bool {
		r := tr.HealthRow().Replay
		return r != nil && r.LastPassAt.Valid
	})
	postWG.Wait()
	eng.intake.Drain()

	if got := fake.redeliveredIDs(); fmt.Sprint(got) != "[21]" {
		t.Fatalf("redelivered %v, want [21] (failed and unseen only)", got)
	}
	snap, ok := eng.store.Peek("o/r", 6)
	if !ok || len(snap.Labels()) != 1 || snap.Labels()[0] != "lost" {
		t.Fatalf("replayed delivery not applied (ok=%v)", ok)
	}
	if n := eng.intake.ledger.Len(); n != 2 {
		t.Fatalf("ledger = %d, want 2 (d-seen, d-lost)", n)
	}
	// A second copy of the redelivery is dropped by the intake, not applied again.
	if r := h.post(t, lostBody, signDirect(lostBody), "issues", "d-lost"); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	eng.intake.Drain()
	if n := eng.intake.ledger.Len(); n != 2 {
		t.Fatalf("duplicate redelivery recorded: ledger = %d", n)
	}

	r := tr.HealthRow().Replay
	if r.FoundFailed.V != 1 || r.Requested.V != 1 || r.Capped.V != 0 || r.Failed.V != 0 || r.Outcome != "ok" {
		t.Errorf("health replay = %+v", r)
	}
}

// The transport's reconnect callback triggers a further pass (for transports
// that signal one, #2143); the direct transport itself never calls it.
func TestDirectReplay_ReconnectHandlerTriggersAnotherPass(t *testing.T) {
	oldPace := replayPace
	replayPace = 0
	t.Cleanup(func() { replayPace = oldPace })
	fake := &fakeHookAPI{}
	eng, cleanup := startDirectEngineReplay(t, true, fake)
	defer cleanup()
	tr := directFromEngine(t, eng)

	replayWaitFor(t, "startup pass", func() bool { return fake.listCount() >= 1 })
	replayWaitFor(t, "startup pass summary", func() bool {
		r := tr.HealthRow().Replay
		return r != nil && r.LastPassAt.Valid
	})
	tr.mu.Lock()
	reconnect := tr.reconnectFn
	tr.mu.Unlock()
	if reconnect == nil {
		t.Fatal("no reconnect handler installed")
	}
	reconnect()
	replayWaitFor(t, "reconnect pass", func() bool { return fake.listCount() >= 2 })
}

// AC5: with replay disabled the deliveries API is never called, on start or on
// reconnect, and the health row carries no replay object.
func TestDirectReplay_DisabledMakesNoAPICalls(t *testing.T) {
	fake := &fakeHookAPI{}
	eng, cleanup := startDirectEngineReplay(t, false, fake)
	defer cleanup()
	tr := directFromEngine(t, eng)
	if eng.replayer != nil {
		t.Fatal("replayer must not exist when replay is disabled")
	}
	tr.mu.Lock()
	reconnect := tr.reconnectFn
	tr.mu.Unlock()
	reconnect()
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 || len(fake.redeliveredIDs()) != 0 {
		t.Fatalf("API called with replay disabled: list=%d redeliver=%v", fake.listCount(), fake.redeliveredIDs())
	}
	if tr.HealthRow().Replay != nil {
		t.Fatal("health row must carry no replay object when disabled")
	}
}

// Replay only applies to a direct receiver: another source ignores the setting
// (FR-011/FR-015), and its reconnect signal still requests only the reconcile.
func TestReplay_IgnoredForOtherTransports(t *testing.T) {
	fake := &fakeHookAPI{}
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.ReplayFailedDeliveries = true
	eng.SetReplayAPIForTest(fake)
	ft := newFakeTransport("other")
	eng.extraTransports = []ingestTransport{ft}
	cleanup := eng.setUpIngestion(context.Background(), nil)
	defer cleanup()

	if eng.replayer != nil {
		t.Fatal("no replayer for a non-direct transport")
	}
	ft.mu.Lock()
	reconnect := ft.reconnect
	ft.mu.Unlock()
	if reconnect == nil {
		t.Fatal("reconnect handler not installed")
	}
	reconnect() // must not panic
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 {
		t.Fatalf("API called for a non-direct transport: %d", fake.listCount())
	}
	if ft.HealthRow().Replay != nil {
		t.Fatal("non-direct row must carry no replay object")
	}
}

// R4: an App that cannot read its deliveries degrades; startup completes and
// the row reports the degraded pass.
func TestDirectReplay_ForbiddenDegradesWithoutBlockingStartup(t *testing.T) {
	fake := &fakeHookAPI{listErr: fmt.Errorf("GitHub App API returned 403: x: %w", gh.ErrAppUnauthorized)}
	eng, cleanup := startDirectEngineReplay(t, true, fake)
	defer cleanup()
	tr := directFromEngine(t, eng)
	replayWaitFor(t, "degraded summary", func() bool {
		r := tr.HealthRow().Replay
		return r != nil && r.LastPassAt.Valid
	})
	r := tr.HealthRow().Replay
	if len(r.Outcome) < 8 || r.Outcome[:8] != "degraded" || len(fake.redeliveredIDs()) != 0 || fake.listCount() != 1 {
		t.Errorf("replay = %+v list=%d redelivered=%v", r, fake.listCount(), fake.redeliveredIDs())
	}
}
