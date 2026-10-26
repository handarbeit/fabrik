package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

func selfWithCaps(caps ...tailcfg.NodeCapability) *ipnstate.PeerStatus {
	m := tailcfg.NodeCapMap{}
	for _, c := range caps {
		m[c] = nil
	}
	return &ipnstate.PeerStatus{CapMap: m}
}

func funnelPortCap() tailcfg.NodeCapability {
	return tailcfg.NodeCapability(string(tailcfg.CapabilityFunnelPorts) + "?ports=443,8443,10000")
}

// The Funnel status is the check ListenFunnel itself makes: HTTPS certificates,
// the funnel node attribute and the port. Either missing means unavailable.
func TestTsnetFunnelStatus(t *testing.T) {
	cases := []struct {
		name string
		self *ipnstate.PeerStatus
		want string
	}{
		{"no self status", nil, tailnetFunnelUnknown},
		{"https and funnel", selfWithCaps(tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel, funnelPortCap()), tailnetFunnelOK},
		{"funnel attribute missing", selfWithCaps(tailcfg.CapabilityHTTPS, funnelPortCap()), tailnetFunnelUnavailable},
		{"https missing", selfWithCaps(tailcfg.NodeAttrFunnel, funnelPortCap()), tailnetFunnelUnavailable},
		{"neither", selfWithCaps(), tailnetFunnelUnavailable},
	}
	for _, c := range cases {
		if got := tsnetFunnelStatus(c.self); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func okStatus() *ipnstate.Status {
	return &ipnstate.Status{
		BackendState: "Running",
		Self: func() *ipnstate.PeerStatus {
			s := selfWithCaps(tailcfg.CapabilityHTTPS, tailcfg.NodeAttrFunnel, funnelPortCap())
			s.DNSName = "n.t.ts.net."
			return s
		}(),
	}
}

func revokedStatus() *ipnstate.Status {
	st := okStatus()
	st.Self = selfWithCaps(tailcfg.CapabilityHTTPS)
	st.Self.DNSName = "n.t.ts.net."
	return st
}

func newTestTracker() (*tsnetTracker, *[]tailnetState) {
	var got []tailnetState
	return &tsnetTracker{fn: func(s tailnetState) { got = append(got, s) }}, &got
}

// A Funnel revoked while the node stays Running changes no backend state: it
// reaches the transport from a status re-read (self-node change notification or timer).
func TestTsnetTracker_FunnelRevokedWithoutStateChange(t *testing.T) {
	tr, got := newTestTracker()
	running := ipn.Running
	tr.observe(&running, okStatus())
	tr.observe(nil, okStatus()) // unchanged: deduplicated
	tr.observe(nil, revokedStatus())
	if len(*got) != 2 {
		t.Fatalf("emitted %d states, want 2: %#v", len(*got), *got)
	}
	if st := (*got)[1]; !st.Connected || st.Funnel != tailnetFunnelUnavailable || st.BackendState != "Running" {
		t.Fatalf("second state = %#v", st)
	}
}

// A failed status read is not evidence that the Funnel came back: the last
// known Funnel status is kept, so no clear and no spurious recovery follow.
func TestTsnetTracker_FailedReadKeepsLastFunnel(t *testing.T) {
	tr, got := newTestTracker()
	running := ipn.Running
	tr.observe(&running, revokedStatus())
	tr.observe(&running, nil) // Running notification, status read failed
	if len(*got) != 1 {
		t.Fatalf("emitted %d states, want 1 (unchanged): %#v", len(*got), *got)
	}
	if (*got)[0].Funnel != tailnetFunnelUnavailable {
		t.Fatalf("state = %#v", (*got)[0])
	}
	// With nothing known yet, a failed read reports the Funnel as unknown.
	tr2, got2 := newTestTracker()
	tr2.observe(&running, nil)
	if len(*got2) != 1 || (*got2)[0].Funnel != tailnetFunnelUnknown || !(*got2)[0].Connected {
		t.Fatalf("first-read failure = %#v", *got2)
	}
}

// A successful read with no Self entry (a transient gap while the netmap
// refreshes) is not evidence that the Funnel came back either: the last known
// Funnel status and name are kept, so an active outage is not cleared and no
// spurious recovery follows.
func TestTsnetTracker_SelflessReadKeepsLastFunnel(t *testing.T) {
	tr, got := newTestTracker()
	running := ipn.Running
	revoked := revokedStatus()
	revoked.Self.DNSName = "n.t.ts.net."
	tr.observe(&running, revoked)
	tr.observe(nil, &ipnstate.Status{BackendState: "Running"}) // read succeeded, Self nil
	if len(*got) != 1 {
		t.Fatalf("emitted %d states, want 1 (unchanged): %#v", len(*got), *got)
	}
	if st := (*got)[0]; st.Funnel != tailnetFunnelUnavailable || st.DNSName != "n.t.ts.net" {
		t.Fatalf("state = %#v", st)
	}
	// With nothing known yet, a Self-less read still reports unknown.
	tr2, got2 := newTestTracker()
	tr2.observe(nil, &ipnstate.Status{BackendState: "Running"})
	if len(*got2) != 1 || (*got2)[0].Funnel != tailnetFunnelUnknown {
		t.Fatalf("first Self-less read = %#v", *got2)
	}
}

func TestTsnetTracker_DisconnectAndNoBackendYet(t *testing.T) {
	tr, got := newTestTracker()
	tr.observe(nil, nil) // nothing known: nothing emitted
	if len(*got) != 0 {
		t.Fatalf("emitted %#v", *got)
	}
	stopped := ipn.Stopped
	st := okStatus()
	st.BackendState = "Stopped"
	tr.observe(&stopped, st)
	if len(*got) != 1 || (*got)[0].Connected || (*got)[0].BackendState != "Stopped" {
		t.Fatalf("got %#v", *got)
	}
}

// A stream that delivers its initial burst and then drops must not end a
// failure streak, or the failure line would repeat on every retry.
func TestStreamEndsFailureStreak(t *testing.T) {
	cases := []struct {
		name     string
		received bool
		up       time.Duration
		want     bool
	}{
		{"initial burst then drop", true, 200 * time.Millisecond, false},
		{"nothing received", false, 10 * time.Minute, false},
		{"stayed up", true, tsnetStreamHealthy, true},
	}
	for _, c := range cases {
		if got := streamEndsFailureStreak(c.received, c.up); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The status poll is the fresher source: after the bus stream breaks, a node
// that drops out of Running (key expired) is reported from the status read, not
// masked by the last state a notification carried.
func TestTsnetTracker_StatusReadBeatsStaleNotificationState(t *testing.T) {
	tr, got := newTestTracker()
	running := ipn.Running
	tr.observe(&running, okStatus())
	needsLogin := okStatus()
	needsLogin.BackendState = "NeedsLogin"
	tr.observe(nil, needsLogin) // timer read; the bus delivered nothing
	if len(*got) != 2 {
		t.Fatalf("emitted %d states, want 2: %#v", len(*got), *got)
	}
	if st := (*got)[1]; st.Connected || st.BackendState != "NeedsLogin" {
		t.Fatalf("state = %#v, want disconnected / NeedsLogin", st)
	}
	// A self-change-only notification before its state notification, and a failed
	// read afterwards, fall back to the last known state.
	tr.observe(&running, nil)
	if st := (*got)[len(*got)-1]; !st.Connected {
		t.Fatalf("notification fallback = %#v", st)
	}
}

// The bus watcher and the timer both refresh the tracker. A read and the observe
// applying it are one step, so a slow older read can never be applied after a
// newer one: reads never overlap, and each is applied before the next starts.
func TestTsnetTracker_RefreshSerialisesReadAndApply(t *testing.T) {
	tr, got := newTestTracker()
	var inFlight, maxInFlight atomic.Int32
	var seq atomic.Int32
	read := func(context.Context) (*ipnstate.Status, error) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		time.Sleep(2 * time.Millisecond)
		st := okStatus()
		// Alternate Running / Stopped so a misordered apply would be visible.
		if seq.Add(1)%2 == 0 {
			st.BackendState = "Stopped"
		}
		return st, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.refresh(context.Background(), read, nil)
		}()
	}
	wg.Wait()
	if m := maxInFlight.Load(); m != 1 {
		t.Fatalf("status reads overlapped (max in flight %d, want 1)", m)
	}
	// The last state applied is the last read taken: reads 1..8, so read 8 (Stopped).
	if st := (*got)[len(*got)-1]; st.Connected {
		t.Fatalf("last applied state = %#v, want the last read (Stopped)", st)
	}
}

// A failed read is skipped when no notification carried state, and applied as
// "no status" when one did.
func TestTsnetTracker_RefreshFailedRead(t *testing.T) {
	tr, got := newTestTracker()
	fail := func(context.Context) (*ipnstate.Status, error) { return nil, errors.New("boom") }
	tr.refresh(context.Background(), fail, nil)
	if len(*got) != 0 {
		t.Fatalf("a failed timer read emitted %#v", *got)
	}
	running := ipn.Running
	tr.refresh(context.Background(), fail, &running)
	if len(*got) != 1 || !(*got)[0].Connected {
		t.Fatalf("notification with failed read = %#v", *got)
	}
}
