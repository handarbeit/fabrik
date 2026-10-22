//go:build tailscale

package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// tailscaleBuiltIn is true: this binary carries the embedded Tailscale node
// (-tags tailscale; #2143, ADR 2143). This is the only file that imports
// tailscale.com (pinned by TestOnlyTheTaggedFileImportsTailscale).
const tailscaleBuiltIn = true

// tsnetWatchRetry is how long Watch waits before re-opening a broken IPN bus
// stream.
const tsnetWatchRetry = 5 * time.Second

// tsnetStreamHealthy is how long a stream must stay up before a failure streak
// is considered over. Every stream opens with an initial burst of notifications,
// so merely receiving one proves nothing: a bus that delivers it and then drops
// again would otherwise log on every retry.
const tsnetStreamHealthy = time.Minute

// tsnetNode is the real tailnetNode over a tsnet.Server.
type tsnetNode struct {
	srv  *tsnet.Server
	logf func(format string, args ...any) // the redacted engine log; never nil

	mu      sync.Mutex
	started bool
	closed  bool
	ctx     context.Context // cancelled by Close; parents Up and Watch
	cancel  context.CancelFunc
	wg      sync.WaitGroup // in-flight Up and Watch calls
}

// newTailnetNode builds the node. It touches no network: the server starts in Up.
func newTailnetNode(cfg tailnetNodeConfig) (tailnetNode, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &tsnetNode{
		logf: logf,
		srv: &tsnet.Server{
			Hostname: cfg.Hostname,
			Dir:      cfg.StateDir,
			AuthKey:  cfg.AuthKey, // ignored by tsnet when node state exists
			// The backend's own logs stay discarded (Logf nil); the user-facing
			// lines (including any login URL, which cfg.Logf redacts) go to logf.
			UserLogf: logf,
		},
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

var errTailnetClosed = errors.New("tailnet node closed")

// begin registers an in-flight call and starts the server on first use. It
// refuses once Close has run, and holds mu so Start can never overlap Close.
func (n *tsnetNode) begin() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return errTailnetClosed
	}
	if !n.started {
		if err := n.srv.Start(); err != nil {
			return fmt.Errorf("starting the tailnet node: %w", err)
		}
		n.started = true
	}
	n.wg.Add(1)
	return nil
}

func (n *tsnetNode) Up(ctx context.Context) (tailnetState, error) {
	if err := n.begin(); err != nil {
		return tailnetState{}, err
	}
	defer n.wg.Done()
	// Close cancels n.ctx, so an Up blocked on a bad key or no network returns.
	upCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	st, err := n.srv.Up(upCtx)
	if err != nil {
		if n.ctx.Err() != nil {
			return tailnetState{}, errTailnetClosed
		}
		return tailnetState{}, err
	}
	return tsnetStateFrom(st, true), nil
}

func (n *tsnetNode) ListenFunnel(addr string) (net.Listener, error) {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil, errTailnetClosed
	}
	n.mu.Unlock()
	return n.srv.ListenFunnel("tcp", addr)
}

func (n *tsnetNode) Watch(ctx context.Context, fn func(tailnetState)) error {
	if err := n.begin(); err != nil {
		return err
	}
	defer n.wg.Done()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()

	lc, err := n.srv.LocalClient()
	if err != nil {
		return err
	}
	tr := &tsnetTracker{fn: fn}
	var pollWG sync.WaitGroup
	pollWG.Add(1)
	go func() {
		defer pollWG.Done()
		n.pollStatus(wctx, lc, tr)
	}()
	defer pollWG.Wait()
	defer cancel()
	failing := false // inside a streak of broken streams: only its first error is logged
	for wctx.Err() == nil {
		opened := time.Now()
		received, err := n.watchOnce(wctx, lc, tr)
		if wctx.Err() != nil {
			return nil
		}
		if streamEndsFailureStreak(received, time.Since(opened)) {
			failing = false
		}
		// A broken stream is re-opened and the backend state catches up on the
		// next notify. While it stays broken the status is still re-read on a
		// timer; the streak is made visible, once, in the log.
		if err != nil && !failing {
			failing = true
			n.logf("IPN bus watch failed (%v); retrying every %s — tailnet health may lag by up to %s until it recovers", err, tsnetWatchRetry, tsnetStatusPoll)
		}
		select {
		case <-wctx.Done():
		case <-time.After(tsnetWatchRetry):
		}
	}
	return nil
}

// tsnetStatusPoll is how often Watch re-reads the node status on its own. The
// IPN bus is the fast path, but a Funnel can be revoked (node attribute or HTTPS
// certificates) without a backend-state change, so the status is also re-read on
// netmap notifications and on this timer.
const tsnetStatusPoll = 30 * time.Second

// tsnetFunnelPort is the port ListenFunnel is asked for (tailnetFunnelAddr).
const tsnetFunnelPort uint16 = 443

// streamEndsFailureStreak reports whether a stream that has now ended proves
// the bus is healthy again: it delivered a notification and stayed up for
// tsnetStreamHealthy. The initial burst alone does not count.
func streamEndsFailureStreak(received bool, up time.Duration) bool {
	return received && up >= tsnetStreamHealthy
}

// watchOnce runs one IPN bus stream. received reports whether a notification
// arrived on it. A notification that carries the
// backend state or the netmap triggers a status re-read.
func (n *tsnetNode) watchOnce(ctx context.Context, lc *local.Client, tr *tsnetTracker) (received bool, err error) {
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState|ipn.NotifyInitialNetMap|ipn.NotifyRateLimit)
	if err != nil {
		return false, err
	}
	defer w.Close()
	for {
		note, err := w.Next()
		if err != nil {
			return received, err
		}
		received = true
		if note.State == nil && note.NetMap == nil {
			continue
		}
		status, serr := lc.StatusWithoutPeers(ctx)
		if serr != nil {
			status = nil
		}
		tr.observe(note.State, status)
	}
}

// pollStatus re-reads the node status every tsnetStatusPoll until ctx ends.
func (n *tsnetNode) pollStatus(ctx context.Context, lc *local.Client, tr *tsnetTracker) {
	t := time.NewTicker(tsnetStatusPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if status, err := lc.StatusWithoutPeers(ctx); err == nil {
				tr.observe(nil, status)
			}
		}
	}
}

// tsnetTracker turns backend-state notifications, netmap notifications and the
// periodic re-read into deduplicated calls of fn. A failed status read keeps the
// last known Funnel status (an unreadable status is not evidence that the Funnel
// came back), and fn runs under the lock so calls are ordered.
type tsnetTracker struct {
	mu          sync.Mutex
	fn          func(tailnetState)
	have        bool
	last        tailnetState
	backend     string // last known backend state (from a status read, else a notification)
	haveBackend bool
}

// observe merges one observation: state is the backend state a notification
// carried (nil if none) and status the node status just read (nil if the read
// failed or was not made).
func (t *tsnetTracker) observe(state *ipn.State, status *ipnstate.Status) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// A status read is the freshest source: the timer and netmap-only
	// notifications carry no state, and a notification's state may be older than
	// the status read right after it. The cached notification state is only the
	// fallback when no status is available.
	switch {
	case status != nil && status.BackendState != "":
		t.backend, t.haveBackend = status.BackendState, true
	case state != nil:
		t.backend, t.haveBackend = state.String(), true
	}
	if !t.haveBackend {
		return
	}
	backend := t.backend
	connected := backend == ipn.Running.String()
	var st tailnetState
	if status != nil {
		st = tsnetStateFrom(status, connected)
	} else {
		st = tailnetState{Connected: connected, Funnel: tailnetFunnelUnknown}
		if t.have {
			st.Funnel, st.DNSName = t.last.Funnel, t.last.DNSName
		}
	}
	st.BackendState = backend
	if t.have && st == t.last {
		return
	}
	t.have, t.last = true, st
	t.fn(st)
}

// Close cancels in-flight Up/Watch calls, waits for them and only then closes
// the server, so it never runs concurrently with Start.
func (n *tsnetNode) Close() error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	started := n.started
	n.cancel()
	n.mu.Unlock()
	n.wg.Wait()
	if !started {
		return nil
	}
	return n.srv.Close()
}

// tsnetFunnelStatus is the same check tsnet's ListenFunnel makes
// (ipn.CheckFunnelAccess): HTTPS certificates enabled for the tailnet, the
// "funnel" node attribute, and the port allowed. A node with no self status is
// unknown, not unavailable.
func tsnetFunnelStatus(self *ipnstate.PeerStatus) string {
	if self == nil {
		return tailnetFunnelUnknown
	}
	if ipn.CheckFunnelAccess(tsnetFunnelPort, self) != nil {
		return tailnetFunnelUnavailable
	}
	return tailnetFunnelOK
}

// tsnetStateFrom maps a local-client status to the seam's state.
func tsnetStateFrom(st *ipnstate.Status, connected bool) tailnetState {
	out := tailnetState{Connected: connected, BackendState: "Running", Funnel: tailnetFunnelUnknown}
	if st == nil {
		return out
	}
	if st.BackendState != "" {
		out.BackendState = st.BackendState
	}
	if self := st.Self; self != nil {
		out.DNSName = strings.TrimSuffix(self.DNSName, ".")
		out.Funnel = tsnetFunnelStatus(self)
	}
	return out
}

var _ tailnetNode = (*tsnetNode)(nil)
