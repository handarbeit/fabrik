package appwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

// This file is the library-independent half of the embedded Tailscale Funnel
// receiver (event_source: tailscale; #2143, ADR 2143). The Tailscale library
// (tsnet) is touched only by tsnetnode/node.go; everything
// here talks to it through the Node seam, so the listener acquisition,
// health, drop/recovery, URL resolution and shutdown are all tested without a
// tailnet. All request handling is the shared appWebhookTransport's.

const (
	// UpTimeout bounds how long startup waits for the node to join the
	// tailnet. Start is synchronous before the first poll, so an unreachable
	// control plane or a bad key must not hang the daemon.
	UpTimeout = 90 * time.Second
	// ReplayStartGrace holds the startup delivery-replay pass back after
	// the node and its Funnel listener are up. Up returning does not mean the
	// public Funnel relay routes to the node yet (live: about two minutes), and a
	// redelivery requested before then fails. It is a fixed grace; nothing else
	// holds the pass back (ADR 2452 removed the public-DNS probe that once did).
	ReplayStartGrace = 90 * time.Second
	// FunnelAddr is the only Funnel port the daemon uses.
	FunnelAddr = ":443"
	// NodeStateFile is tsnet's default state file (a FileStore at
	// Dir/tailscaled.state); a logged-in profile in it is what makes the auth key
	// optional (StateExists).
	NodeStateFile = "tailscaled.state"

	FunnelOK          = "ok"
	FunnelUnavailable = "unavailable"
	FunnelUnknown     = "unknown"
)

// NodeState is a snapshot of the embedded node.
type NodeState struct {
	Connected    bool   // the node is up on the tailnet (backend state Running)
	BackendState string // e.g. "Running", "NeedsLogin"; "" if unknown
	DNSName      string // the node's tailnet DNS name, without a trailing dot
	Funnel       string // FunnelOK, FunnelUnavailable or FunnelUnknown
}

// Node is the seam over tsnet.Server.
type Node interface {
	// Up joins the tailnet (blocking until running or ctx ends) and reports the
	// node's state, including its reported DNS name.
	Up(ctx context.Context) (NodeState, error)
	// ListenFunnel returns a TLS-terminated public Funnel listener.
	ListenFunnel(addr string) (net.Listener, error)
	// Watch calls fn on every change of the connection state until ctx ends.
	Watch(ctx context.Context, fn func(NodeState)) error
	// Close shuts the node down. It is safe before, during and after Up.
	Close() error
}

// NodeConfig is what the factory needs to build a node.
type NodeConfig struct {
	Hostname string
	// AuthKey may be empty (a node with state needs none). Never logged.
	AuthKey  string
	StateDir string
	// Logf receives the node's user-facing log lines (already redacted).
	Logf func(format string, args ...any)
}

// StateExists reports whether stateDir holds the state of a node that has
// logged in, which is what makes the auth key optional. A non-empty state file
// is not enough: tsnet writes the machine key at Start, before login, so a first
// start that failed on a bad or expired key leaves a non-empty file for a node
// that never joined. A completed login stores a profile (prefs JSON) whose
// "Config" (the persisted node identity) carries a NodeID, and tsnet does not
// persist a profile without one, so that is the signal. The file is a JSON object
// of base64 values; anything unreadable or without such a profile counts as no
// state, so the refusal asks for the key rather than hanging in Up.
func StateExists(stateDir string) bool {
	bs, err := os.ReadFile(filepath.Join(stateDir, NodeStateFile))
	if err != nil || len(bs) == 0 {
		return false
	}
	var entries map[string][]byte
	if json.Unmarshal(bs, &entries) != nil {
		return false
	}
	for _, v := range entries {
		var prefs struct {
			Config *struct{ NodeID string } `json:"Config"`
		}
		if json.Unmarshal(v, &prefs) == nil && prefs.Config != nil && prefs.Config.NodeID != "" {
			return true
		}
	}
	return false
}

// FunnelWebhookURL builds the App webhook URL from the name the node reports
// (not the configured hostname: tsnet suffixes a taken one).
func FunnelWebhookURL(dnsName, path string) string {
	dnsName = strings.TrimSuffix(strings.TrimSpace(dnsName), ".")
	if dnsName == "" {
		return ""
	}
	return "https://" + dnsName + path
}

// loginURL deliberately matches anywhere in a line (it is a redaction,
// not a validation): over-matching only withholds more. Scheme and host are
// case-insensitive and http is covered, so no spelling of the link slips through.
var loginURL = regexp.MustCompile(`(?i)https?://login\.tailscale\.com\S*`)

// RedactLog withholds interactive-login links from the node's log: we
// never do an interactive login, and a one-time auth link is credential-like.
func RedactLog(s string) string {
	return loginURL.ReplaceAllString(s, "[login URL withheld]")
}

// WithLineEnd ends s with a newline when it lacks one. tsnet's log lines carry
// none, and the engine log adds none, so without this several node lines run
// together on one line.
func WithLineEnd(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// NodeFactory builds the embedded node (tsnetnode.New in production; tests inject
// a fake).
type NodeFactory func(NodeConfig) (Node, error)

// FunnelConfig configures a Funnel.
type FunnelConfig struct {
	// Tag is the log tag ("tailscale").
	Tag      string
	Hostname string
	// AuthKeyEnv is the NAME of the variable holding the auth key; the value is
	// read when the node is built, goes only to the node and is never logged.
	AuthKeyEnv string
	StateDir   string
	// Path is the webhook path appended to the node's DNS name.
	Path        string
	NodeFactory NodeFactory
	Logf        LogFunc
}

// Funnel owns the embedded node's lifecycle for one receiver: the listener
// factory (Listen), the URL the node is served at, the health tracker that
// follows the tailnet connection and the Funnel status (Watch) and shutdown.
type Funnel struct {
	cfg FunnelConfig

	nodeMu sync.Mutex
	node   Node
	webURL string
	upSt   NodeState // what Up reported; seeds the health baseline

	startCtx    context.Context
	watchCtx    context.Context
	cancelWatch context.CancelFunc
	watchWG     sync.WaitGroup
}

// NewFunnel builds a Funnel whose node start and watch are bounded by ctx.
func NewFunnel(ctx context.Context, cfg FunnelConfig) *Funnel {
	f := &Funnel{cfg: cfg}
	f.watchCtx, f.cancelWatch = context.WithCancel(ctx)
	f.startCtx = ctx
	return f
}

// Listen is the receiver's listener factory: it builds the node, joins the
// tailnet (bounded by UpTimeout) and opens the public Funnel listener.
func (f *Funnel) Listen() (net.Listener, error) {
	cfg := f.cfg
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating tailscale state dir: %w", err)
	}
	// The key is read here whenever one is configured and goes solely to the
	// node. It is never logged. tsnet ignores it for a node that is logged in
	// from stored state, so passing it is harmless; passing it is also what
	// lets a node whose registration expired or was revoked (NeedsLogin, state
	// file still present) re-authenticate with a fresh key on restart.
	authKey := os.Getenv(cfg.AuthKeyEnv)
	n, err := cfg.NodeFactory(NodeConfig{
		Hostname: cfg.Hostname,
		AuthKey:  authKey,
		StateDir: cfg.StateDir,
		Logf: func(format string, args ...any) {
			cfg.Logf(0, cfg.Tag, "node: %s", WithLineEnd(RedactLog(fmt.Sprintf(format, args...))))
		},
	})
	if err != nil {
		return nil, err
	}
	upCtx, cancel := context.WithTimeout(f.startCtx, UpTimeout)
	defer cancel()
	st, err := n.Up(upCtx)
	if err != nil {
		_ = n.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("the node did not join the tailnet within %s: %w", UpTimeout, err)
		}
		return nil, fmt.Errorf("joining the tailnet as %q: %w", cfg.Hostname, err)
	}
	ln, err := n.ListenFunnel(FunnelAddr)
	if err != nil {
		_ = n.Close()
		return nil, fmt.Errorf("opening the Funnel listener (check that the tailnet policy grants the \"funnel\" node attribute to this node's tag, and that HTTPS certificates are enabled): %w", err)
	}
	f.nodeMu.Lock()
	f.node, f.webURL, f.upSt = n, FunnelWebhookURL(st.DNSName, cfg.Path), st
	f.nodeMu.Unlock()
	return ln, nil
}

// URL is the App webhook URL the node is served at ("" while the node has not
// reported its DNS name).
func (f *Funnel) URL() string {
	f.nodeMu.Lock()
	defer f.nodeMu.Unlock()
	return f.webURL
}

// CancelWatch stops the watch goroutine's context (also called when the
// receiver failed to start).
func (f *Funnel) CancelWatch() { f.cancelWatch() }

// StopWatch cancels the watch and waits for it and for any hook check it
// started.
func (f *Funnel) StopWatch() {
	f.cancelWatch()
	f.watchWG.Wait()
}

// CloseNode closes the node, if one was built.
func (f *Funnel) CloseNode() {
	f.nodeMu.Lock()
	n := f.node
	f.nodeMu.Unlock()
	if n != nil {
		if err := n.Close(); err != nil {
			f.cfg.Logf(0, f.cfg.Tag, "closing the tailnet node: %v\n", err)
		}
	}
}

// Watch starts following the started node: it applies the baseline Up and the
// Funnel listen reported (never a reconnect), then every change the node
// reports, to the receiver's network condition and the warnings panel. check is
// called with the webhook URL whenever the node first reports (or changes) its
// DNS name, to verify it against the App; it runs on a goroutine joined by
// StopWatch.
func (f *Funnel) Watch(tr *Receiver, panel Panel, check func(url string)) {
	cfg := f.cfg
	tag, hostname := cfg.Tag, cfg.Hostname
	f.nodeMu.Lock()
	n, up := f.node, f.upSt
	f.nodeMu.Unlock()

	connKey := warnings.Condition{Type: "tailscale_connection", Subject: hostname}.Key()
	funnelKey := warnings.Condition{Type: "tailscale_funnel", Subject: hostname}.Key()
	// adoptName takes the node's reported DNS name from a watch state: the first
	// time it is known (or when it changes) the webhook URL is updated, logged and
	// checked against the App. Called from the watch callback, so watchWG (held by
	// the Watch goroutine) is non-zero when the check is added to it.
	adoptName := func(dns string) {
		nu := FunnelWebhookURL(dns, cfg.Path)
		if nu == "" {
			return
		}
		f.nodeMu.Lock()
		changed := nu != f.webURL
		f.webURL = nu
		f.nodeMu.Unlock()
		if !changed {
			return
		}
		cfg.Logf(0, tag, "node %q reports its tailnet DNS name; App webhook URL: %s\n", hostname, nu)
		f.watchWG.Add(1)
		go func() {
			defer f.watchWG.Done()
			check(nu)
		}()
	}
	apply := func(st NodeState) {
		adoptName(st.DNSName)
		tr.SetNetworkCondition(st.Connected, st.BackendState, st.Funnel)
		if st.Connected {
			panel.Resolve(connKey)
			if st.Funnel == FunnelUnavailable {
				panel.Report(warnings.Condition{
					Type:    "tailscale_funnel",
					Subject: hostname,
					Title:   fmt.Sprintf("Tailscale node %q is connected but its Funnel is unavailable", hostname),
					Detail: "The node is on the tailnet but Funnel is not available for it, so GitHub deliveries cannot reach the daemon. " +
						"Polling and the periodic reconcile keep the board current. Check that the tailnet policy still grants the \"funnel\" " +
						"node attribute to this node's tag and that HTTPS certificates are enabled.",
					Fingerprint: "tailscale_funnel:" + hostname,
					LogLine:     fmt.Sprintf("Tailscale node %q is connected but its Funnel is unavailable", hostname),
				})
			} else {
				panel.Resolve(funnelKey)
			}
			return
		}
		panel.Resolve(funnelKey)
		panel.Report(warnings.Condition{
			Type:    "tailscale_connection",
			Subject: hostname,
			Title:   fmt.Sprintf("Tailscale node %q lost its tailnet connection", hostname),
			Detail: fmt.Sprintf("The embedded node's backend state is %q, so GitHub deliveries cannot reach the daemon. "+
				"Polling and the periodic reconcile keep the board current. The connection is retried automatically; "+
				"if the state is NeedsLogin, the node's key expired or was revoked — provide a new auth key in $%s and restart.",
				st.BackendState, cfg.AuthKeyEnv),
			Fingerprint: "tailscale_connection:" + hostname,
			LogLine:     fmt.Sprintf("Tailscale node %q lost its tailnet connection (state %q)", hostname, st.BackendState),
		})
	}
	// First connect: records the baseline, never calls the reconnect handler.
	// The baseline is what Up and the Funnel listen actually reported, so an
	// unavailable Funnel shows from the start and does not wait for the first
	// IPN bus notification. Up returned, so the node is connected; an empty
	// backend state or Funnel status falls back to what that success implies.
	baseState, baseFunnel := up.BackendState, up.Funnel
	if baseState == "" {
		baseState = "Running"
	}
	if baseFunnel == "" {
		baseFunnel = FunnelOK
	}
	apply(NodeState{Connected: true, BackendState: baseState, Funnel: baseFunnel})
	f.watchWG.Add(1)
	go func() {
		defer f.watchWG.Done()
		if err := n.Watch(f.watchCtx, apply); err != nil && f.watchCtx.Err() == nil {
			cfg.Logf(0, tag, "tailnet watch ended: %v\n", err)
		}
	}()
}
