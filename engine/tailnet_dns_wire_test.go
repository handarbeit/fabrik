package engine

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/tui"
)

// --- transport level (R2) ---

func TestDNSProbe_DirectTransportIsUnchanged(t *testing.T) {
	h := startDirect(t)
	row := h.tr.HealthRow()
	if row.State != "listening" || row.Funnel.Valid || row.Tailnet.Valid {
		t.Fatalf("direct row = %#v", row)
	}
	if !h.tr.IsHealthyOrStartingUp() {
		t.Fatal("direct transport is not healthy")
	}
}

// R2/AC1 at the seam: pending is a distinct, unhealthy state from the first
// moment (no healthy flash on Start), reports funnel pending-dns, survives
// tsnet's own overwrites of the funnel field, and clears to tsnet's value.
func TestDNSProbe_TransportHealthDimension(t *testing.T) {
	var mu sync.Mutex
	var emitted []string
	tr := newAppWebhookTransport("tailscale", directTestPath,
		func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		directTestSecret, func(int, string, string, ...any) {},
		func(ev tui.Event) {
			if s, ok := ev.(tui.WebhookStatusEvent); ok {
				mu.Lock()
				emitted = append(emitted, s.State)
				mu.Unlock()
			}
		})
	tr.EnableDNSProbe()
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Stop)
	if tr.IsHealthyOrStartingUp() {
		t.Fatal("healthy right after Start although the name is unresolved")
	}
	mu.Lock()
	for _, s := range emitted {
		if s == string(WebhookStreamHealthy) {
			t.Fatalf("a healthy state was emitted while pending: %v", emitted)
		}
	}
	mu.Unlock()
	row := tr.HealthRow()
	if row.State != tailnetStateNotResolvable || row.Funnel.V != tailnetFunnelPendingDNS {
		t.Fatalf("pending row = %#v", row)
	}

	// tsnet's first state must not clobber the marker.
	tr.SetNetworkCondition(true, "Running", tailnetFunnelOK)
	row = tr.HealthRow()
	if row.State != tailnetStateNotResolvable || row.Funnel.V != tailnetFunnelPendingDNS || row.Tailnet.V != "Running" {
		t.Fatalf("after tsnet state: %#v", row)
	}
	if tr.IsHealthyOrStartingUp() {
		t.Fatal("healthy while pending")
	}

	// tsnet's "unavailable" is the more actionable value and wins.
	tr.SetNetworkCondition(true, "Running", tailnetFunnelUnavailable)
	if row = tr.HealthRow(); row.Funnel.V != tailnetFunnelUnavailable || row.State != "funnel unavailable" {
		t.Fatalf("unavailable did not win: %#v", row)
	}
	tr.SetNetworkCondition(true, "Running", tailnetFunnelOK)

	// Resolved clears to tsnet's own value, not a hard-coded ok.
	tr.SetDNSProbeState(dnsProbeResolved)
	if row = tr.HealthRow(); row.State != "listening" || row.Funnel.V != tailnetFunnelOK || !tr.IsHealthyOrStartingUp() {
		t.Fatalf("resolved row = %#v", row)
	}
	tr.SetNetworkCondition(true, "Running", tailnetFunnelUnknown)
	if row = tr.HealthRow(); row.Funnel.V != tailnetFunnelUnknown {
		t.Fatalf("resolved did not defer to tsnet's value: %#v", row)
	}
}

// R2: a probe failure reports funnel "unknown", never ok, and leaves the
// transport serving and healthy (R5).
func TestDNSProbe_UnknownKeepsTransportHealthy(t *testing.T) {
	tr := newAppWebhookTransport("tailscale", directTestPath,
		func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		directTestSecret, func(int, string, string, ...any) {}, func(tui.Event) {})
	tr.EnableDNSProbe()
	if err := tr.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tr.Stop)
	tr.SetNetworkCondition(true, "Running", tailnetFunnelOK)
	tr.SetDNSProbeState(dnsProbeUnknown)
	row := tr.HealthRow()
	if row.Funnel.V != tailnetFunnelUnknown || row.State != "listening" || !tr.IsHealthyOrStartingUp() {
		t.Fatalf("unknown row = %#v", row)
	}
}

// --- engine level ---

// startTailscaleDNSEngine is startTailscaleEngine with the probe's resolver and
// sleep, and optionally delivery replay, under the test's control.
func startTailscaleDNSEngine(t *testing.T, res funnelDNSResolver, probeWait func(context.Context, time.Duration) bool, replayAPI hookDeliveryAPI) (*Engine, func(), *syncLog) {
	t.Helper()
	isolateWarnings(t)
	installFunnelResolver(t, res)
	oldPace := replayPace
	replayPace = 0
	t.Cleanup(func() { replayPace = oldPace })
	t.Setenv(DefaultAppWebhookSecretEnv, directTestSecret)
	t.Setenv(DefaultTailscaleAuthKeyEnv, tailscaleTestKey)
	eng := testEngine(t, &mockGitHubClient{}, &mockClaudeInvoker{})
	eng.fabrikDir = t.TempDir()
	eng.cfg.EventSource = EventSourceTailscale
	eng.cfg.TailscaleHostname = "fabrik-models"
	eng.cfg.AppWebhookPath = directTestPath
	if probeWait != nil {
		eng.SetFunnelDNSWaitForTest(probeWait)
	}
	if replayAPI != nil {
		eng.cfg.ReplayFailedDeliveries = true
		eng.SetReplayAPIForTest(replayAPI)
		eng.SetReplayWaitForTest(func(ctx context.Context, d time.Duration) bool {
			if d == tailnetReplayStartGrace {
				return true
			}
			<-ctx.Done()
			return false
		})
	}
	logs := &syncLog{}
	eng.SetLogSinkForTest(func(_ int, tag, msg string) { logs.add("[%s] %s", tag, msg) })
	orig := fetchAppHookConfigFn
	fetchAppHookConfigFn = func(*Engine) (gh.AppHookConfig, error) { return gh.AppHookConfig{}, errors.New("no app") }
	t.Cleanup(func() { fetchAppHookConfigFn = orig })
	cache, _ := eng.UseBoardCacheForTest()
	return eng, eng.setUpIngestion(context.Background(), cache), logs
}

// gatedWait blocks every probe sleep until release() and counts them.
type gatedWait struct {
	ch    chan struct{}
	mu    sync.Mutex
	waits int
}

func newGatedWait() *gatedWait { return &gatedWait{ch: make(chan struct{})} }

func (g *gatedWait) wait(ctx context.Context, _ time.Duration) bool {
	g.mu.Lock()
	g.waits++
	g.mu.Unlock()
	select {
	case <-g.ch:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *gatedWait) release() { g.ch <- struct{}{} }
func (g *gatedWait) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waits
}

// AC1 end to end: no records, then records. Health shows pending-dns (and the
// aggregate mode is not healthy), then ok; the reconnect handler is called
// exactly once at the transition.
func TestTailscale_DNSProbe_PendingThenOK(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "fabrik-models.tail1234.ts.net.") })
	g := newGatedWait()
	res := &scriptedResolver{steps: []resolveStep{{}, {ok: true}}}
	eng, cleanup, logs := startTailscaleDNSEngine(t, res, g.wait, nil)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	var calls int
	var mu sync.Mutex
	tr.SetReconnectHandler(func() { mu.Lock(); calls++; mu.Unlock() })

	replayWaitFor(t, "first probe", func() bool { return g.count() >= 1 })
	row := tr.HealthRow()
	if row.Funnel.V != tailnetFunnelPendingDNS || row.State != tailnetStateNotResolvable {
		t.Fatalf("pending row = %#v", row)
	}
	if mode := eng.transports.Mode(); mode == "healthy" || mode == string(WebhookStreamHealthy) {
		t.Fatalf("webhook mode = %q while the name is unresolved", mode)
	}

	g.release()
	replayWaitFor(t, "resolution", func() bool { return !tr.dnsPendingForTest() })
	row = tr.HealthRow()
	if row.Funnel.V != tailnetFunnelOK || row.State != "listening" {
		t.Fatalf("resolved row = %#v", row)
	}
	replayWaitFor(t, "reconnect call", func() bool { mu.Lock(); defer mu.Unlock(); return calls >= 1 })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("reconnect handler calls = %d, want exactly 1", n)
	}
	if res.callCount() != 2 {
		t.Fatalf("probes = %d, want 2", res.callCount())
	}
	if got := res.names[0]; got != "fabrik-models.tail1234.ts.net" {
		t.Fatalf("probed %q: the name must be the node's reported one, without the dot", got)
	}
	if out := logs.String(); !strings.Contains(out, "requesting a catch-up reconcile") {
		t.Fatalf("no recovery log line:\n%s", out)
	}
}

// AC3 end to end: resolved at the first probe → healthy, ok, and the recovery
// path is never taken.
func TestTailscale_DNSProbe_ResolvedAtFirstProbe(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	res := &scriptedResolver{}
	eng, cleanup, logs := startTailscaleDNSEngine(t, res, nil, nil)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	replayWaitFor(t, "first probe", func() bool { return res.callCount() >= 1 && !tr.dnsPendingForTest() })
	if row := tr.HealthRow(); row.Funnel.V != tailnetFunnelOK || row.State != "listening" {
		t.Fatalf("row = %#v", row)
	}
	if out := logs.String(); strings.Contains(out, "requesting a catch-up reconcile") {
		t.Fatalf("a name that resolved at the first probe triggered the recovery:\n%s", out)
	}
}

// AC5: a failing probe never fails startup or stops the listener; the funnel
// field is "unknown" and the transport still serves.
func TestTailscale_DNSProbe_FailureNeverStopsServing(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	g := newGatedWait()
	res := &scriptedResolver{steps: []resolveStep{{err: errors.New("ns lookup: network is unreachable")}}}
	eng, cleanup, _ := startTailscaleDNSEngine(t, res, g.wait, nil)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	replayWaitFor(t, "probe failure", func() bool { return g.count() >= 1 })
	row := tr.HealthRow()
	if row.Funnel.V != tailnetFunnelUnknown || row.State != "listening" {
		t.Fatalf("row = %#v", row)
	}
	if tr.Addr() == "" || !tr.IsHealthyOrStartingUp() {
		t.Fatal("the listener stopped or the transport went unhealthy on a probe failure")
	}
	// It still handles a request.
	body := []byte(`{"zen":"x"}`)
	h := &directHarness{tr: tr, url: "http://" + tr.Addr() + directTestPath}
	resp := h.post(t, body, signDirect(body), "ping", "d-1")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

// A node that has not reported its DNS name is not probed until it does; the
// name probed is then the adopted one.
func TestTailscale_DNSProbe_NameReportedLate(t *testing.T) {
	nodes := installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "") })
	res := &scriptedResolver{}
	eng, cleanup, _ := startTailscaleDNSEngine(t, res, nil, nil)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	time.Sleep(30 * time.Millisecond)
	if res.callCount() != 0 {
		t.Fatal("probed with no name")
	}
	if row := tr.HealthRow(); row.Funnel.V != tailnetFunnelPendingDNS {
		t.Fatalf("row = %#v: an unprobed name must read pending-dns", row)
	}
	(*nodes)[0].emit(tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelOK, DNSName: "late.t.ts.net."})
	replayWaitFor(t, "probe of the adopted name", func() bool { return res.callCount() >= 1 && !tr.dnsPendingForTest() })
	if res.names[0] != "late.t.ts.net" {
		t.Fatalf("probed %q", res.names[0])
	}
}

// R6: a tsnet-reported funnel "unavailable" is shown while DNS is pending.
func TestTailscale_DNSProbe_FunnelUnavailableWins(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet {
		n := newFakeTailnet(cfg, "n.t.ts.net")
		n.upState = &tailnetState{Connected: true, BackendState: "Running", Funnel: tailnetFunnelUnavailable}
		return n
	})
	g := newGatedWait()
	eng, cleanup, _ := startTailscaleDNSEngine(t, &scriptedResolver{steps: []resolveStep{{}}}, g.wait, nil)
	defer cleanup()
	tr := tailscaleFromEngine(t, eng)
	replayWaitFor(t, "first probe", func() bool { return g.count() >= 1 })
	if row := tr.HealthRow(); row.Funnel.V != tailnetFunnelUnavailable {
		t.Fatalf("row = %#v", row)
	}
}

// --- replay interaction (R4, Task 6) ---

// While the name is pending the 90 s startup pass is held back (a redelivery
// then fails and would spend the per-delivery request budget); the resolution
// triggers the pass, which requests a delivery that failed in the window.
func TestTailscaleReplay_StartupPassHeldBackWhilePending(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	g := newGatedWait()
	res := &scriptedResolver{steps: []resolveStep{{}, {ok: true}}}
	eng, cleanup, logs := startTailscaleDNSEngine(t, res, g.wait, fake)
	defer cleanup()
	_ = tailscaleFromEngine(t, eng)
	replayWaitFor(t, "the held-back notice", func() bool {
		return strings.Contains(logs.String(), "holding the startup delivery replay pass back")
	})
	time.Sleep(50 * time.Millisecond)
	if fake.listCount() != 0 {
		t.Fatalf("a replay pass ran while the name was unresolved (%d)", fake.listCount())
	}
	g.release()
	replayWaitFor(t, "the pass the resolution triggers", func() bool { return fake.listCount() >= 1 })
}

// An unknown probe state (it could not find out) does not hold the pass back.
func TestTailscaleReplay_UnknownProbeStillGetsTheStartupPass(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	g := newGatedWait()
	res := &scriptedResolver{steps: []resolveStep{{err: errors.New("no network")}}}
	eng, cleanup, _ := startTailscaleDNSEngine(t, res, g.wait, fake)
	defer cleanup()
	_ = tailscaleFromEngine(t, eng)
	replayWaitFor(t, "startup pass", func() bool { return fake.listCount() >= 1 })
}

// A name that resolved by the grace gets the ordinary startup pass, and no
// second one from the probe.
func TestTailscaleReplay_ResolvedAtFirstProbeKeepsTheOrdinaryPass(t *testing.T) {
	installFakeTailnet(t, func(cfg tailnetNodeConfig) *fakeTailnet { return newFakeTailnet(cfg, "n.t.ts.net") })
	fake := &fakeHookAPI{}
	eng, cleanup, _ := startTailscaleDNSEngine(t, &scriptedResolver{}, nil, fake)
	defer cleanup()
	_ = tailscaleFromEngine(t, eng)
	replayWaitFor(t, "startup pass", func() bool { return fake.listCount() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := fake.listCount(); got != 1 {
		t.Fatalf("passes = %d, want only the startup pass", got)
	}
}

// dnsPendingForTest reports whether the transport's probe state is pending.
func (t *appWebhookTransport) dnsPendingForTest() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dnsProbe == dnsProbePending
}

// awaitFirstDNSProbe waits (bounded) for the tailscale transport's first probe
// to leave pending. The probe runs in the background, so a test that asserts
// the health of a node whose name resolves at once must not race it. A node
// with no reported name is never probed and times out harmlessly.
func awaitFirstDNSProbe(eng *Engine) {
	var tr *appWebhookTransport
	for _, t := range eng.transports.snapshot() {
		if a, ok := t.(*appWebhookTransport); ok && a.Name() == "tailscale" {
			tr = a
		}
	}
	if tr == nil {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for tr.dnsPendingForTest() && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}
