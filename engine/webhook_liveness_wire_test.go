package engine

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/localapi"
)

// startLivenessDirectEngine starts a direct engine whose route-liveness
// controller talks to api and reads hookURL as the App's webhook URL. The
// controller's own sleep blocks, so only the start-up check and explicit
// triggers evaluate. neutraliseDedupe turns the intake's dedupe off (the
// non-vacuity switch).
func startLivenessDirectEngine(t *testing.T, api hookDeliveryAPI, neutraliseDedupe bool) (*Engine, *appWebhookTransport, func()) {
	t.Helper()
	isolateWarnings(t)
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceDirect
	eng.cfg.DirectListen = "127.0.0.1:0"
	eng.cfg.AppWebhookPath = directTestPath
	eng.cfg.ReplayFailedDeliveries = false
	eng.SetReplayAPIForTest(api)
	eng.SetLivenessWaitForTest(func(ctx context.Context, d time.Duration) bool {
		<-ctx.Done()
		return false
	})
	if neutraliseDedupe {
		eng.faults, _ = parseTestFaults(FaultNoDedupe)
	}
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) {
		return gh.AppHookConfig{URL: livenessHookURL, ContentType: "json"}, nil
	}
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
	cache, _ := eng.UseBoardCacheForTest()
	cleanup := startIngestion(t, eng, cache)
	return eng, directFromEngine(t, eng), cleanup
}

func routeOf(tr *appWebhookTransport) *localapi.RouteLiveness { return tr.HealthRow().Route }

// runProbeScenario drives the active check end to end through the real listener,
// intake, ledger and store: a delivery is applied once, the log holds no recent
// 2xx, the controller requests a redelivery, and GitHub's redelivered copy (a
// STALE body under the same delivery id, so applying it would be visible) comes
// back through the listener. It reports whether the stale copy changed the board.
func runProbeScenario(t *testing.T, neutraliseDedupe bool) (applied bool, tr *appWebhookTransport, eng *Engine, lastBefore, lastAfter time.Time, dupes int64) {
	t.Helper()
	fake := &fakeLivenessAPI{urls: map[int64]string{}}
	e, transport, cleanup := startLivenessDirectEngine(t, fake, neutraliseDedupe)
	t.Cleanup(cleanup)
	h := &directHarness{tr: transport, url: "http://" + transport.Addr() + directTestPath, logs: &syncLog{}}

	// The start-up check found nothing eligible (empty ledger): still unknown.
	replayWaitFor(t, "start-up evaluation", func() bool { r := routeOf(transport); return r != nil && !r.EvaluatedAt.Valid })
	if r := routeOf(transport); r.Liveness != localapi.RouteUnknown {
		t.Fatalf("start state = %+v", r)
	}

	// A real delivery the engine applies and the ledger records.
	orig := issueBody(9, "original")
	if r := h.post(t, orig, signDirect(orig), "issues", "g-probe"); r.StatusCode != http.StatusOK {
		t.Fatalf("status %d", r.StatusCode)
	}
	e.intake.Drain()
	if snap, ok := e.store.Peek("o/r", 9); !ok || len(snap.Labels()) != 1 || snap.Labels()[0] != "original" {
		t.Fatalf("original delivery not applied (ok=%v)", ok)
	}
	// That real delivery already proved the route; reset to isolate the probe.
	transport.SetRouteLiveness(localapi.RouteLiveness{Liveness: localapi.RouteUnknown})
	e.liveness.mu.Lock()
	e.liveness.state, e.liveness.evidenceAt = localapi.RouteUnknown, time.Time{}
	e.liveness.mu.Unlock()
	lastBefore = transport.HealthRow().LastDeliveryAt.V

	// The log shows that delivery only as a stale success (no recent 2xx).
	fake.add(gh.AppHookDelivery{ID: 55, GUID: "g-probe", StatusCode: 200, Status: "OK", Event: "issues", DeliveredAt: time.Now().Add(-time.Hour)})
	stale := issueBody(9, "STALE")
	fake.onRedeliver = func(id int64) {
		if id != 55 {
			t.Errorf("redelivery requested for %d", id)
		}
		req, _ := http.NewRequest(http.MethodPost, h.url, bytes.NewReader(stale))
		req.Header.Set("X-Hub-Signature-256", signDirect(stale))
		req.Header.Set("X-GitHub-Event", "issues")
		req.Header.Set("X-GitHub-Delivery", "g-probe")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}

	// A reconnect triggers the check.
	transport.FireReconnect()
	replayWaitFor(t, "probe verdict", func() bool { r := routeOf(transport); return r != nil && r.Liveness == localapi.RouteReachable })
	e.intake.Drain()

	snap, _ := e.store.Peek("o/r", 9)
	for _, l := range snap.Labels() {
		if l == "STALE" {
			applied = true
		}
	}
	return applied, transport, e, lastBefore, transport.HealthRow().LastDeliveryAt.V, e.intake.stats.snapshot().Duplicates
}

// FR-005/SC-003: a probe redelivery changes no board state, is counted as a
// duplicate and leaves the transport's last-delivery time alone; and the very
// same assertions FAIL when the intake dedupe is neutralised, so the test
// depends on the dedupe.
func TestLivenessProbe_RedeliveryIsNeverApplied(t *testing.T) {
	applied, tr, eng, before, after, dupes := runProbeScenario(t, false)
	if applied {
		t.Fatal("the probe's stale redelivery was applied to the board")
	}
	if dupes != 1 {
		t.Fatalf("duplicates = %d, want 1", dupes)
	}
	if !after.Equal(before) {
		t.Fatalf("a probe copy moved the transport's last delivery: %s → %s", before, after)
	}
	if r := routeOf(tr); r.Liveness != localapi.RouteReachable || !r.EvaluatedAt.Valid {
		t.Fatalf("route = %+v", r)
	}
	if got := eng.liveness; got == nil {
		t.Fatal("no controller")
	}
}

func TestLivenessProbe_NonVacuity_NeutralisedDedupeApplies(t *testing.T) {
	applied, _, _, _, _, dupes := runProbeScenario(t, true)
	if !applied || dupes != 0 {
		t.Fatalf("with the dedupe neutralised the stale copy must be applied (applied=%v dupes=%d): the test would prove nothing", applied, dupes)
	}
}

// A healthy log reports reachable at start with no warning, even though the
// authoritative DNS walk is gone entirely (SC-001/SC-004), and the row carries it.
func TestLivenessWire_HealthyLogIsReachableAtStart(t *testing.T) {
	fake := &fakeLivenessAPI{urls: map[int64]string{}}
	fake.log = []gh.AppHookDelivery{{ID: 1, GUID: "g1", StatusCode: 200, Status: "OK", Event: "issues", DeliveredAt: time.Now().Add(-time.Minute)}}
	_, tr, cleanup := startLivenessDirectEngine(t, fake, false)
	defer cleanup()
	replayWaitFor(t, "reachable", func() bool { r := routeOf(tr); return r != nil && r.Liveness == localapi.RouteReachable })
	if !tr.IsHealthyOrStartingUp() || tr.HealthRow().State != "listening" {
		t.Fatalf("row = %+v", tr.HealthRow())
	}
}

// A broken route makes the transport unhealthy (webhook mode must not claim
// healthy) with GitHub's reason on the row; recovery restores it.
func TestLivenessWire_UnreachableMakesTheTransportUnhealthy(t *testing.T) {
	fake := &fakeLivenessAPI{urls: map[int64]string{}}
	fake.log = []gh.AppHookDelivery{
		{ID: 2, GUID: "g2", StatusCode: 502, Status: "failed to connect to host", Event: "issues", DeliveredAt: time.Now().Add(-time.Minute)},
		{ID: 1, GUID: "g1", StatusCode: 502, Status: "failed to connect to host", Event: "issues", DeliveredAt: time.Now().Add(-2 * time.Minute)},
	}
	eng, tr, cleanup := startLivenessDirectEngine(t, fake, false)
	defer cleanup()
	// The daemon has to have been up for the failures to count. Run stamps its
	// start once, when it begins, so the back-dated start is re-applied on every
	// poll of the wait: whichever order the two land in, a later reconnect
	// evaluates against it.
	replayWaitFor(t, "unreachable", func() bool {
		eng.liveness.mu.Lock()
		eng.liveness.started = time.Now().Add(-time.Hour)
		eng.liveness.mu.Unlock()
		tr.FireReconnect()
		r := routeOf(tr)
		return r != nil && r.Liveness == localapi.RouteUnreachable
	})
	row := tr.HealthRow()
	if row.State != "listening (route unreachable)" || row.Route.Reason != "failed to connect to host" || tr.IsHealthyOrStartingUp() {
		t.Fatalf("row = %+v healthy=%v", row, tr.IsHealthyOrStartingUp())
	}
	// A real delivery arriving proves the route again.
	b := issueBody(3, "x")
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath, logs: &syncLog{}}
	h.post(t, b, signDirect(b), "issues", "g-live")
	replayWaitFor(t, "reachable again", func() bool { return routeOf(tr).Liveness == localapi.RouteReachable })
	if !tr.IsHealthyOrStartingUp() {
		t.Fatal("not healthy after recovery")
	}
}
