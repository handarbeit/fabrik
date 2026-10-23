package engine

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

// This file is the public-DNS probe of the embedded Tailscale Funnel receiver
// (event_source: tailscale; #2425, ADR 2425). A new tailnet node's Funnel
// hostname does not resolve publicly for several minutes (about 15 in the #2143
// live check). Until it does, GitHub's deliveries fail with a 502, so the
// transport must not report itself healthy, and the deliveries that failed in
// the window have to be recovered once the name resolves.
//
// The controller here is library- and network-independent: it asks a
// funnelDNSResolver (the production one, tailnet_dns_auth.go, walks the
// delegation from the root servers and asks the zone's authoritative servers
// only), keeps the transport's probe state and the panel entry current, and
// makes the one-shot reconnect call (R4).

const (
	// funnelDNSBackoffInitial and funnelDNSBackoffMax bound the retry delay:
	// 10 s, 20 s, 40 s, then 60 s until the name resolves.
	funnelDNSBackoffInitial = 10 * time.Second
	funnelDNSBackoffMax     = 60 * time.Second
	// funnelDNSGrace is how long the name may stay unresolved before the
	// warnings panel is told (R3). A constant, not configuration. It is measured
	// from the moment probing begins.
	funnelDNSGrace = 10 * time.Minute
	// funnelDNSProbeTimeout bounds one whole probe (the delegation walk and the
	// address queries).
	funnelDNSProbeTimeout = 30 * time.Second
	// funnelDNSPanelType is the warnings-panel type; the subject is the name.
	funnelDNSPanelType = "tailscale_funnel_dns"

	tailnetFunnelPendingDNS = "pending-dns"
	// tailnetStateNotResolvable is the transport health state while the Funnel
	// name does not resolve publicly: the listener is up but nothing can reach it.
	tailnetStateNotResolvable = "listening (not publicly resolvable)"
)

// funnelDNSResolver answers whether name resolves publicly.
type funnelDNSResolver interface {
	// Resolve returns true when name has at least one A or AAAA record outside
	// the tailnet ranges in the answer of the name's authoritative servers. No
	// records (NODATA, NXDOMAIN, or only tailnet addresses) is (false, nil); a
	// failure to find out is a non-nil error.
	Resolve(ctx context.Context, name string) (bool, error)
}

// funnelDNSResolverFactory builds the production resolver; tests replace it.
var funnelDNSResolverFactory = func() funnelDNSResolver { return newAuthoritativeResolver() }

// tailnetPrefixes are the ranges a Funnel name must not be answered with: they
// are what MagicDNS gives a host on the tailnet, and say nothing about the
// public name.
var tailnetPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// publicAddr reports whether a is outside the tailnet ranges. An IPv4-mapped
// IPv6 address is judged as the IPv4 address it carries.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() {
		return false
	}
	for _, p := range tailnetPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// anyPublicAddr reports whether addrs holds at least one publicAddr.
func anyPublicAddr(addrs []netip.Addr) bool {
	for _, a := range addrs {
		if publicAddr(a) {
			return true
		}
	}
	return false
}

// funnelDNSSink is what the controller drives: the transport's probe state and
// its reconnect handler.
type funnelDNSSink interface {
	SetDNSProbeState(s dnsProbeState)
	FireReconnect()
}

// funnelDNSProbeConfig wires a funnelDNSProbe.
type funnelDNSProbeConfig struct {
	Tag      string
	Resolver funnelDNSResolver
	Sink     funnelDNSSink
	// StartReplay requests the delivery replay pass the startup grace held back
	// when the probe turns out unable to find out (nil = none).
	StartReplay func()
	// Wait sleeps d, returning false if ctx ended first.
	Wait func(ctx context.Context, d time.Duration) bool
	Now  func() time.Time
	Logf func(format string, args ...any)
	// Report and Resolve are the warnings-panel hooks.
	Report  func(c warnings.Condition)
	Resolve func(key string)
	// Ctx and WG own the probe goroutine: the transport's stop hook cancels Ctx
	// and waits on WG.
	Ctx context.Context
	WG  *sync.WaitGroup
}

// funnelDNSProbe is the probe controller. One per tailscale transport.
type funnelDNSProbe struct {
	cfg funnelDNSProbeConfig

	mu         sync.Mutex
	name       string
	gen        int // bumped by every (re)start; a stale loop writes nothing
	cancel     context.CancelFunc
	resolved   bool          // the name resolved: no more probing for the life of the node
	state      dnsProbeState // pending until the first definite answer or failure
	graceStart time.Time
	sawNot     bool // a pending or failed result was seen before resolution
	needReplay bool // the startup replay pass was held back while pending
	fired      bool // the one-shot reconnect call was made
	panelKey   string
}

func newFunnelDNSProbe(cfg funnelDNSProbeConfig) *funnelDNSProbe {
	return &funnelDNSProbe{cfg: cfg, state: dnsProbePending}
}

// Pending reports whether the name is known to be unresolved, or no answer has
// come yet. A probe that failed (unknown) is not pending.
func (p *funnelDNSProbe) Pending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == dnsProbePending
}

// SkipStartupReplayIfPending is called when the startup replay pass is due. If
// the name is pending it records that the pass was held back (so the
// resolution then requests one, whether or not an earlier probe was seen to
// fail) and returns true; the check and the record are one step, so a probe
// that resolves in between cannot miss it.
func (p *funnelDNSProbe) SkipStartupReplayIfPending() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state != dnsProbePending {
		return false
	}
	p.needReplay = true
	return true
}

// SetName starts probing name (the node's reported DNS name, never the
// configured hostname: tsnet suffixes a taken one). The same name again is a
// no-op; a different one before resolution restarts the loop and keeps the
// once-flags; after resolution it is ignored (probing stops for the life of the
// node).
func (p *funnelDNSProbe) SetName(name string) {
	if name == "" {
		return
	}
	p.mu.Lock()
	if p.resolved || name == p.name {
		p.mu.Unlock()
		return
	}
	oldKey := p.panelKey
	p.panelKey = ""
	if p.cancel != nil {
		p.cancel()
	}
	if p.name != "" {
		p.cfg.Logf("funnel name changed from %s to %s — probing the new name\n", p.name, name)
	}
	p.name = name
	p.gen++
	gen := p.gen
	if p.graceStart.IsZero() {
		p.graceStart = p.cfg.Now()
	}
	ctx, cancel := context.WithCancel(p.cfg.Ctx)
	p.cancel = cancel
	p.mu.Unlock()
	if oldKey != "" {
		p.cfg.Resolve(oldKey)
	}
	p.cfg.WG.Add(1)
	go func() {
		defer p.cfg.WG.Done()
		p.run(ctx, name, gen)
	}()
}

// current reports whether gen is still the live loop.
func (p *funnelDNSProbe) current(gen int) bool {
	return p.gen == gen && !p.resolved
}

func (p *funnelDNSProbe) run(ctx context.Context, name string, gen int) {
	delay := funnelDNSBackoffInitial
	level := 0
	loggedPending, loggedErr := -1, -1
	for {
		rctx, cancel := context.WithTimeout(ctx, funnelDNSProbeTimeout)
		ok, err := p.cfg.Resolver.Resolve(rctx, name)
		cancel()
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			if !p.record(gen, dnsProbeUnknown, name, err) {
				return
			}
			// R5: a failed probe is logged at most once per backoff level.
			if level != loggedErr {
				loggedErr = level
				p.cfg.Logf("could not check whether %s resolves publicly (retrying in %s): %v\n", name, delay, err)
			}
		case ok:
			p.resolve(gen, name)
			return
		default:
			if !p.record(gen, dnsProbePending, name, nil) {
				return
			}
			if level != loggedPending {
				loggedPending = level
				p.cfg.Logf("%s does not resolve publicly yet (the authoritative servers have no usable address); webhook deliveries cannot reach this node — checking again in %s\n", name, delay)
			}
		}
		if !p.cfg.Wait(ctx, delay) {
			return
		}
		if delay *= 2; delay > funnelDNSBackoffMax {
			delay = funnelDNSBackoffMax
		}
		level++
	}
}

// record applies an unresolved result (pending or failed) for the loop gen and
// reports the grace-period panel entry. It returns false when the loop is stale.
func (p *funnelDNSProbe) record(gen int, s dnsProbeState, name string, err error) bool {
	p.mu.Lock()
	if !p.current(gen) {
		p.mu.Unlock()
		return false
	}
	p.sawNot = true
	p.state = s
	// A held-back startup pass must not wait for a resolution a broken probe may
	// never report: once the probe is unknown it is no longer pending, so the
	// pass the grace skipped runs now (the resolution, if it comes, adds its own).
	replayNow := s == dnsProbeUnknown && p.needReplay
	if replayNow {
		p.needReplay = false
	}
	pastGrace := p.cfg.Now().Sub(p.graceStart) >= funnelDNSGrace
	var key string
	if pastGrace {
		key = warnings.Condition{Type: funnelDNSPanelType, Subject: name}.Key()
		p.panelKey = key
	}
	p.mu.Unlock()
	p.cfg.Sink.SetDNSProbeState(s)
	if replayNow && p.cfg.StartReplay != nil {
		p.cfg.Logf("the probe cannot tell whether %s resolves — running the held-back startup delivery replay pass\n", name)
		p.cfg.StartReplay()
	}
	if pastGrace {
		p.cfg.Report(funnelDNSCondition(name, err))
	}
	return true
}

// resolve handles the first successful resolution: the state goes to resolved,
// the panel entry clears, and the reconnect handler is called once — but only
// if the name was seen unresolved first (or the startup replay was held back
// for it), so a name that resolved at the first probe never triggers it.
func (p *funnelDNSProbe) resolve(gen int, name string) {
	p.mu.Lock()
	if !p.current(gen) {
		p.mu.Unlock()
		return
	}
	p.resolved = true
	p.state = dnsProbeResolved
	key := p.panelKey
	p.panelKey = ""
	fire := (p.sawNot || p.needReplay) && !p.fired
	if fire {
		p.fired = true
	}
	p.mu.Unlock()
	p.cfg.Sink.SetDNSProbeState(dnsProbeResolved)
	if key != "" {
		p.cfg.Resolve(key)
	}
	if fire {
		p.cfg.Logf("%s now resolves publicly — requesting a catch-up reconcile and a delivery replay pass for what failed meanwhile\n", name)
		p.cfg.Sink.FireReconnect()
	} else {
		p.cfg.Logf("%s resolves publicly\n", name)
	}
}

// funnelDNSCondition is the R3 panel entry. lastErr is the last probe failure,
// if the probe could not even find out.
func funnelDNSCondition(name string, lastErr error) warnings.Condition {
	detail := fmt.Sprintf("The Funnel hostname %s has not resolved publicly after %s, so GitHub cannot deliver webhooks to this daemon. "+
		"Polling and the periodic reconcile keep the board current, at poll latency. Likely causes: DNS propagation of a new node name is still in progress "+
		"(it can take about 15 minutes); Funnel is not granted to the node's tag in the tailnet policy (the \"funnel\" node attribute); "+
		"HTTPS certificates are not enabled for the tailnet. The entry clears, and missed deliveries are replayed, once the name resolves.",
		name, funnelDNSGrace)
	if lastErr != nil {
		detail += fmt.Sprintf("\n\nThe last check could not complete: %v", lastErr)
	}
	return warnings.Condition{
		Type:        funnelDNSPanelType,
		Subject:     name,
		Title:       fmt.Sprintf("Funnel hostname %s does not resolve publicly yet", name),
		Detail:      detail,
		Fingerprint: funnelDNSPanelType + ":" + name,
		LogLine:     fmt.Sprintf("Funnel hostname %s has not resolved publicly after %s — webhook deliveries cannot reach this node", name, funnelDNSGrace),
	}
}

// startTailnetReplay runs the delivery replayer r in the background and
// requests the startup pass after the fixed grace (tailnetReplayStartGrace) —
// unless the Funnel name has not resolved by then. A redelivery requested while
// the name is unresolved fails, and each failure spends the per-delivery
// request budget the pass has, so the pass that matters is the one the
// resolution triggers (R4; the probe records that it was held back). An unknown
// probe state (it could not find out) is not pending, so a broken DNS path can
// never disable the startup pass. probe nil behaves as resolved.
func (e *Engine) startTailnetReplay(ctx context.Context, r *deliveryReplayer, probe *funnelDNSProbe) {
	if r == nil {
		return
	}
	go r.Run(ctx)
	go func() {
		if !r.wait(ctx, tailnetReplayStartGrace) {
			return
		}
		if probe != nil && probe.SkipStartupReplayIfPending() {
			e.logf(0, "tailscale", "holding the startup delivery replay pass back until the Funnel name resolves publicly\n")
			return
		}
		r.Trigger()
	}()
}
