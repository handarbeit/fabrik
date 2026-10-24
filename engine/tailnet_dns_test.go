package engine

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

// No engine test may reach the network: unless a test installs its own
// resolver (installFunnelResolver), every tailscale transport sees a name that
// resolves at the first probe.
func init() {
	funnelDNSResolverFactory = func() funnelDNSResolver { return &scriptedResolver{} }
}

// scriptedResolver returns its steps in order, then repeats the last one. An
// empty script resolves at once.
type scriptedResolver struct {
	mu    sync.Mutex
	steps []resolveStep
	calls int
	names []string
}

type resolveStep struct {
	ok  bool
	err error
}

func (r *scriptedResolver) Resolve(_ context.Context, name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
	i := r.calls
	r.calls++
	if len(r.steps) == 0 {
		return true, nil
	}
	if i >= len(r.steps) {
		i = len(r.steps) - 1
	}
	return r.steps[i].ok, r.steps[i].err
}

func (r *scriptedResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// installFunnelResolver makes new tailscale transports probe through r.
func installFunnelResolver(t *testing.T, r funnelDNSResolver) {
	t.Helper()
	orig := funnelDNSResolverFactory
	funnelDNSResolverFactory = func() funnelDNSResolver { return r }
	t.Cleanup(func() { funnelDNSResolverFactory = orig })
}

// fakeSink records what the controller drives.
type fakeSink struct {
	mu         sync.Mutex
	states     []dnsProbeState
	reconnects int
}

func (s *fakeSink) SetDNSProbeState(st dnsProbeState) {
	s.mu.Lock()
	s.states = append(s.states, st)
	s.mu.Unlock()
}

func (s *fakeSink) FireReconnect() {
	s.mu.Lock()
	s.reconnects++
	s.mu.Unlock()
}

func (s *fakeSink) snapshot() ([]dnsProbeState, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dnsProbeState(nil), s.states...), s.reconnects
}

// probeHarness drives a funnelDNSProbe on a fake clock. Each wait advances the
// clock by the requested delay and hands control back to the test through
// waits, so the loop only proceeds when the test says so.
type probeHarness struct {
	t       *testing.T
	probe   *funnelDNSProbe
	sink    *fakeSink
	res     *scriptedResolver
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	mu      sync.Mutex
	clock   time.Time
	waits   chan time.Duration
	proceed chan struct{}
	logs    []string
	reports []warnings.Condition
	cleared []string
}

func newProbeHarness(t *testing.T, steps ...resolveStep) *probeHarness {
	t.Helper()
	h := &probeHarness{
		t:       t,
		sink:    &fakeSink{},
		res:     &scriptedResolver{steps: steps},
		clock:   time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC),
		waits:   make(chan time.Duration, 64),
		proceed: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.probe = newFunnelDNSProbe(funnelDNSProbeConfig{
		Tag:      "tailscale",
		Resolver: h.res,
		Sink:     h.sink,
		Now:      func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.clock },
		Wait: func(ctx context.Context, d time.Duration) bool {
			h.waits <- d
			select {
			case <-h.proceed:
				h.mu.Lock()
				h.clock = h.clock.Add(d)
				h.mu.Unlock()
				return true
			case <-ctx.Done():
				return false
			}
		},
		Logf: func(format string, args ...any) {
			h.mu.Lock()
			h.logs = append(h.logs, strings.TrimSpace(fmt.Sprintf(format, args...)))
			h.mu.Unlock()
		},
		Report:  func(c warnings.Condition) { h.mu.Lock(); h.reports = append(h.reports, c); h.mu.Unlock() },
		Resolve: func(key string) { h.mu.Lock(); h.cleared = append(h.cleared, key); h.mu.Unlock() },
		Ctx:     ctx,
		WG:      &h.wg,
	})
	t.Cleanup(h.stop)
	return h
}

func (h *probeHarness) stop() { h.cancel(); h.wg.Wait() }

// nextWait returns the delay of the next backoff sleep.
func (h *probeHarness) nextWait() time.Duration {
	h.t.Helper()
	select {
	case d := <-h.waits:
		return d
	case <-time.After(5 * time.Second):
		h.t.Fatal("the probe did not reach its next wait")
		return 0
	}
}

// release lets the wait the probe is blocked in return.
func (h *probeHarness) release() {
	h.t.Helper()
	select {
	case h.proceed <- struct{}{}:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the probe is not waiting")
	}
}

func (h *probeHarness) waitDone() {
	h.t.Helper()
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the probe loop did not end")
	}
}

func (h *probeHarness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.logs, "\n")
}

const probeName = "fabrik-models.tail1234.ts.net"

// AC1: no records, then records: pending, then resolved; the reconnect handler
// is called exactly once, at the transition, and probing stops.
func TestFunnelDNSProbe_PendingThenResolvedFiresOnce(t *testing.T) {
	h := newProbeHarness(t, resolveStep{}, resolveStep{}, resolveStep{ok: true})
	h.probe.SetName(probeName)
	if d := h.nextWait(); d != 10*time.Second {
		t.Fatalf("first backoff = %s, want 10s", d)
	}
	if _, n := h.sink.snapshot(); n != 0 {
		t.Fatal("reconnect fired while pending")
	}
	h.release()
	if d := h.nextWait(); d != 20*time.Second {
		t.Fatalf("second backoff = %s, want 20s", d)
	}
	h.release()
	h.waitDone()
	states, n := h.sink.snapshot()
	want := []dnsProbeState{dnsProbePending, dnsProbePending, dnsProbeResolved}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}
	if n != 1 {
		t.Fatalf("reconnect calls = %d, want exactly 1", n)
	}
	if h.res.callCount() != 3 {
		t.Fatalf("resolver called %d times, want 3 (probing stops at resolution)", h.res.callCount())
	}
	if h.probe.Pending() {
		t.Fatal("still pending after resolution")
	}
	// The node keeps running: nothing probes or fires again, and a late name
	// change is ignored.
	h.probe.SetName("other.tail1234.ts.net")
	time.Sleep(20 * time.Millisecond)
	if _, n := h.sink.snapshot(); n != 1 || h.res.callCount() != 3 {
		t.Fatalf("after resolution: reconnects=%d probes=%d", n, h.res.callCount())
	}
}

// AC3: a name that resolves at the first probe never calls the reconnect handler.
func TestFunnelDNSProbe_ResolvedAtFirstProbeNeverFires(t *testing.T) {
	h := newProbeHarness(t, resolveStep{ok: true})
	h.probe.SetName(probeName)
	h.waitDone()
	states, n := h.sink.snapshot()
	if len(states) != 1 || states[0] != dnsProbeResolved {
		t.Fatalf("states = %v", states)
	}
	if n != 0 {
		t.Fatalf("reconnect calls = %d, want 0", n)
	}
}

// R4: a failed probe followed by a resolution counts as the transition (a
// spurious catch-up is harmless, a missed one loses deliveries) — once.
func TestFunnelDNSProbe_ErrorThenResolvedFiresOnce(t *testing.T) {
	h := newProbeHarness(t, resolveStep{err: errors.New("ns lookup failed")}, resolveStep{ok: true})
	h.probe.SetName(probeName)
	h.nextWait()
	h.release()
	h.waitDone()
	states, n := h.sink.snapshot()
	if len(states) != 2 || states[0] != dnsProbeUnknown || states[1] != dnsProbeResolved {
		t.Fatalf("states = %v", states)
	}
	if n != 1 {
		t.Fatalf("reconnect calls = %d, want 1", n)
	}
}

// R1: the backoff is 10s, 20s, 40s, then 60s.
func TestFunnelDNSProbe_BackoffGrowsToTheCeiling(t *testing.T) {
	h := newProbeHarness(t, resolveStep{})
	h.probe.SetName(probeName)
	var got []time.Duration
	for i := 0; i < 6; i++ {
		got = append(got, h.nextWait())
		h.release()
	}
	want := []time.Duration{10, 20, 40, 60, 60, 60}
	for i, w := range want {
		if got[i] != w*time.Second {
			t.Fatalf("backoff = %v, want %v seconds", got, want)
		}
	}
}

// R5: a failure is logged at most once per backoff level. A level is one delay
// value, so the 60 s ceiling is a single level however many probes run at it.
func TestFunnelDNSProbe_LogsAtMostOncePerLevel(t *testing.T) {
	h := newProbeHarness(t, resolveStep{err: errors.New("boom")})
	h.probe.SetName(probeName)
	for i := 0; i < 5; i++ {
		h.nextWait()
		h.release()
	}
	h.nextWait()
	text := h.logText()
	// 6 probes ran at delays 10s, 20s, 40s, 60s, 60s, 60s: four levels.
	if got := strings.Count(text, "could not check whether"); got != 4 {
		t.Fatalf("failure lines = %d, want one per level (4):\n%s", got, text)
	}
	if strings.Contains(text, "does not resolve publicly yet") {
		t.Fatalf("a failure was logged as pending:\n%s", text)
	}
}

// R3 / AC4: past the grace the panel entry appears (naming the hostname and the
// likely causes); before it, none; it clears on resolution.
func TestFunnelDNSProbe_GraceEntryAppearsAndClears(t *testing.T) {
	h := newProbeHarness(t, resolveStep{})
	h.probe.SetName(probeName)
	// 10s + 20s + 40s + 60s... until the fake clock passes ten minutes.
	var elapsed time.Duration
	for elapsed < funnelDNSGrace {
		h.mu.Lock()
		n := len(h.reports)
		h.mu.Unlock()
		if n != 0 {
			t.Fatalf("panel entry reported after only %s", elapsed)
		}
		elapsed += h.nextWait()
		h.release()
	}
	// One more probe runs past the grace.
	h.nextWait()
	h.mu.Lock()
	reports := append([]warnings.Condition(nil), h.reports...)
	h.mu.Unlock()
	if len(reports) == 0 {
		t.Fatal("no panel entry after the grace period")
	}
	c := reports[0]
	if c.Type != "tailscale_funnel_dns" || c.Subject != probeName {
		t.Fatalf("entry = %s/%s", c.Type, c.Subject)
	}
	for _, want := range []string{probeName, "propagation", "Funnel is not granted", "HTTPS certificates"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, c.Detail)
		}
	}
}

func TestFunnelDNSProbe_GraceEntryClearsOnResolution(t *testing.T) {
	// Pending for 12 minutes of fake time, then resolved.
	steps := make([]resolveStep, 0, 30)
	for i := 0; i < 14; i++ {
		steps = append(steps, resolveStep{})
	}
	steps = append(steps, resolveStep{ok: true})
	h := newProbeHarness(t, steps...)
	h.probe.SetName(probeName)
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
loop:
	for {
		select {
		case <-h.waits:
			h.release()
		case <-done:
			break loop
		case <-time.After(5 * time.Second):
			t.Fatal("never resolved")
		}
	}
	h.waitDone()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.reports) == 0 {
		t.Fatal("entry was never reported before resolution")
	}
	if len(h.cleared) != 1 || h.cleared[0] != (warnings.Condition{Type: "tailscale_funnel_dns", Subject: probeName}).Key() {
		t.Fatalf("cleared = %v", h.cleared)
	}
}

// A name that is not known yet is not probed; a changed name before
// resolution restarts the loop on the new name, drops the old panel subject,
// and keeps the once-flags.
func TestFunnelDNSProbe_NameChangeRestartsAndKeepsOnceFlags(t *testing.T) {
	h := newProbeHarness(t, resolveStep{}, resolveStep{ok: true})
	h.probe.SetName("")
	if h.res.callCount() != 0 {
		t.Fatal("probed without a name")
	}
	h.probe.SetName("a.tail1.ts.net")
	h.nextWait() // first name: pending
	h.probe.SetName("b.tail1.ts.net")
	// The first loop ends; the new loop's first probe (script step 2) resolves.
	h.waitDone()
	_, n := h.sink.snapshot()
	if n != 1 {
		t.Fatalf("reconnect calls = %d, want 1 (the first name was seen pending)", n)
	}
	h.res.mu.Lock()
	names := append([]string(nil), h.res.names...)
	h.res.mu.Unlock()
	if len(names) != 2 || names[0] != "a.tail1.ts.net" || names[1] != "b.tail1.ts.net" {
		t.Fatalf("probed names = %v", names)
	}
}

// Shutdown joins the loop.
func TestFunnelDNSProbe_ShutdownJoinsTheLoop(t *testing.T) {
	h := newProbeHarness(t, resolveStep{})
	h.probe.SetName(probeName)
	h.nextWait()
	h.stop()
	h.waitDone()
}

// The startup replay pass is held back while pending, recorded, and then
// requested by the resolution even though no earlier probe failed.
func TestFunnelDNSProbe_HeldBackStartupReplayFiresOnResolve(t *testing.T) {
	h := newProbeHarness(t, resolveStep{ok: true})
	if !h.probe.SkipStartupReplayIfPending() {
		t.Fatal("pending probe did not hold the pass back")
	}
	h.probe.SetName(probeName)
	h.waitDone()
	if _, n := h.sink.snapshot(); n != 1 {
		t.Fatalf("reconnect calls = %d, want 1", n)
	}
	if h.probe.SkipStartupReplayIfPending() {
		t.Fatal("a resolved probe held the pass back")
	}
}

// An unknown probe (it could not find out) never disables the startup pass.
func TestFunnelDNSProbe_UnknownDoesNotHoldBackReplay(t *testing.T) {
	h := newProbeHarness(t, resolveStep{err: errors.New("no network")})
	h.probe.SetName(probeName)
	h.nextWait()
	if h.probe.SkipStartupReplayIfPending() {
		t.Fatal("an unknown probe held the pass back")
	}
}

// AC2: tailnet-only addresses are not public.
func TestPublicAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"203.0.113.9", true},
		{"2606:4700::1111", true},
		{"100.64.0.1", false},
		{"100.127.255.254", false},
		{"100.128.0.1", true},
		{"100.63.255.255", true},
		{"fd7a:115c:a1e0::1", false},
		{"fd7a:115c:a1e0:ab12::9", false},
		{"fd7a:115c:a1e1::1", true},
		{"::ffff:100.100.100.100", false}, // v4-mapped CGNAT
		{"::ffff:203.0.113.9", true},
	}
	for _, c := range cases {
		if got := publicAddr(netip.MustParseAddr(c.addr)); got != c.want {
			t.Errorf("publicAddr(%s) = %v, want %v", c.addr, got, c.want)
		}
	}
	if anyPublicAddr([]netip.Addr{netip.MustParseAddr("100.64.1.1"), netip.MustParseAddr("fd7a:115c:a1e0::2")}) {
		t.Error("tailnet-only addresses counted as public")
	}
	if !anyPublicAddr([]netip.Addr{netip.MustParseAddr("100.64.1.1"), netip.MustParseAddr("198.51.100.4")}) {
		t.Error("a public address among tailnet ones was not found")
	}
}

// A pass the grace held back runs once the probe turns out unable to find out,
// rather than waiting for a resolution that may never be reported.
func TestFunnelDNSProbe_UnknownRunsTheHeldBackReplay(t *testing.T) {
	h := newProbeHarness(t, resolveStep{err: errors.New("no network")})
	var mu sync.Mutex
	runs := 0
	h.probe.cfg.StartReplay = func() { mu.Lock(); runs++; mu.Unlock() }
	if !h.probe.SkipStartupReplayIfPending() {
		t.Fatal("pending probe did not hold the pass back")
	}
	h.probe.SetName(probeName)
	h.nextWait()
	mu.Lock()
	defer mu.Unlock()
	if runs != 1 {
		t.Fatalf("held-back pass ran %d times, want 1", runs)
	}
}

// namedResolver resolves only the names in ok; every other name is unresolved.
type namedResolver map[string]bool

func (r namedResolver) Resolve(_ context.Context, name string) (bool, error) { return r[name], nil }

// gatedSink blocks its first pending write until release is closed, recording
// writes in completion order.
type gatedSink struct {
	fakeSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedSink) SetDNSProbeState(st dnsProbeState) {
	if st == dnsProbePending {
		s.once.Do(func() {
			close(s.entered)
			<-s.release
		})
	}
	s.fakeSink.SetDNSProbeState(st)
}

// A loop SetName has replaced must not write its late result over the new
// loop's: here the old loop is inside its pending sink write while the new name
// resolves, and the transport must end up resolved, not pending for good.
func TestFunnelDNSProbe_ReplacedLoopCannotOverwriteResolution(t *testing.T) {
	sink := &gatedSink{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	p := newFunnelDNSProbe(funnelDNSProbeConfig{
		Tag:      "tailscale",
		Resolver: namedResolver{"new.t.ts.net": true},
		Sink:     sink,
		Wait:     func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); return false },
		Now:      time.Now,
		Logf:     func(string, ...any) {},
		Report:   func(warnings.Condition) {},
		Resolve:  func(string) {},
		Ctx:      ctx,
		WG:       &wg,
	})
	t.Cleanup(func() { cancel(); wg.Wait() })

	p.SetName("old.t.ts.net")
	<-sink.entered // the old loop is inside its pending write
	p.SetName("new.t.ts.net")
	time.Sleep(50 * time.Millisecond) // let the new loop reach its resolution
	close(sink.release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		states, _ := sink.snapshot()
		if n := len(states); n >= 2 {
			if states[n-1] != dnsProbeResolved {
				t.Fatalf("sink writes = %v, want the last to be resolved", states)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("sink writes = %v, want pending then resolved", states)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A loop SetName has replaced must not re-add the panel entry SetName cleared
// for the old name: here the old loop is inside its grace-period Report while
// the name changes, and the clear must land after the report, not before it.
func TestFunnelDNSProbe_ReplacedLoopCannotReAddClearedPanelEntry(t *testing.T) {
	var (
		mu       sync.Mutex
		events   []string
		entered  = make(chan struct{})
		release  = make(chan struct{})
		once     sync.Once
		nowCalls int
	)
	t0 := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	p := newFunnelDNSProbe(funnelDNSProbeConfig{
		Tag:      "tailscale",
		Resolver: namedResolver{},
		Sink:     &fakeSink{},
		Wait:     func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); return false },
		// The first read starts the grace; every later one is past it.
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			nowCalls++
			if nowCalls == 1 {
				return t0
			}
			return t0.Add(funnelDNSGrace + time.Minute)
		},
		Logf: func(string, ...any) {},
		Report: func(c warnings.Condition) {
			once.Do(func() {
				close(entered)
				<-release
			})
			mu.Lock()
			events = append(events, "report:"+c.Subject)
			mu.Unlock()
		},
		Resolve: func(key string) { mu.Lock(); events = append(events, "clear"); mu.Unlock() },
		Ctx:     ctx,
		WG:      &wg,
	})
	t.Cleanup(func() { cancel(); wg.Wait() })

	p.SetName("old.t.ts.net")
	<-entered // the old loop is inside its Report
	done := make(chan struct{})
	go func() { p.SetName("new.t.ts.net"); close(done) }()
	time.Sleep(50 * time.Millisecond) // let SetName reach its clear
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(events) < 2 || events[0] != "report:old.t.ts.net" || events[1] != "clear" {
		t.Fatalf("panel events = %v, want the old report first, then its clear", events)
	}
}

// A probe that cannot find out must not tell the operator the name does not
// resolve: the panel entry says the check could not be made, and keeps the
// likely causes that apply to a probe failure.
func TestFunnelDNSCondition_WordingFollowsWhetherTheProbeCouldTell(t *testing.T) {
	pending := funnelDNSCondition(probeName, nil)
	if !strings.Contains(pending.Title, "does not resolve publicly yet") ||
		!strings.Contains(pending.LogLine, "deliveries cannot reach this node") {
		t.Fatalf("pending entry = %q / %q", pending.Title, pending.LogLine)
	}
	unknown := funnelDNSCondition(probeName, errors.New("read udp: i/o timeout"))
	if !strings.Contains(unknown.Title, "Cannot check whether") ||
		strings.Contains(unknown.Title, "does not resolve") ||
		strings.Contains(unknown.LogLine, "cannot reach this node") {
		t.Fatalf("unknown entry claims a definite failure: %q / %q", unknown.Title, unknown.LogLine)
	}
	for _, want := range []string{probeName, "port 53", "i/o timeout"} {
		if !strings.Contains(unknown.Detail, want) {
			t.Errorf("unknown detail lacks %q:\n%s", want, unknown.Detail)
		}
	}
	if pending.Key() != unknown.Key() {
		t.Errorf("the two entries must share a key (so one clears the other): %q vs %q", pending.Key(), unknown.Key())
	}
}
