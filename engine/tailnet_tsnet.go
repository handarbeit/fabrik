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
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
)

// tailscaleBuiltIn is true: this binary carries the embedded Tailscale node
// (-tags tailscale; #2143, ADR 2143). This is the only file that imports
// tailscale.com (pinned by TestOnlyTheTaggedFileImportsTailscale).
const tailscaleBuiltIn = true

// tsnetWatchRetry is how long Watch waits before re-opening a broken IPN bus
// stream.
const tsnetWatchRetry = 5 * time.Second

// tsnetNode is the real tailnetNode over a tsnet.Server.
type tsnetNode struct {
	srv *tsnet.Server

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
	for wctx.Err() == nil {
		err := n.watchOnce(wctx, lc, fn)
		if wctx.Err() != nil {
			return nil
		}
		_ = err // a broken stream is re-opened; the backend state catches up on the next notify
		select {
		case <-wctx.Done():
		case <-time.After(tsnetWatchRetry):
		}
	}
	return nil
}

func (n *tsnetNode) watchOnce(ctx context.Context, lc *local.Client, fn func(tailnetState)) error {
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer w.Close()
	for {
		note, err := w.Next()
		if err != nil {
			return err
		}
		if note.State == nil {
			continue
		}
		st := tailnetState{
			Connected:    *note.State == ipn.Running,
			BackendState: note.State.String(),
			Funnel:       tailnetFunnelUnknown,
		}
		if status, serr := lc.StatusWithoutPeers(ctx); serr == nil {
			st = tsnetStateFrom(status, st.Connected)
			st.BackendState = note.State.String()
		}
		fn(st)
	}
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
		if self.HasCap(tailcfg.NodeAttrFunnel) {
			out.Funnel = tailnetFunnelOK
		} else {
			out.Funnel = tailnetFunnelUnavailable
		}
	}
	return out
}

var _ tailnetNode = (*tsnetNode)(nil)
