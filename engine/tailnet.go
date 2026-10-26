package engine

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
// (tsnet) is touched only by tailnet_tsnet.go; everything
// here talks to it through the tailnetNode seam, so the listener acquisition,
// health, drop/recovery, URL resolution and shutdown are all tested without a
// tailnet. All request handling is the shared appWebhookTransport's.

const (
	// tailnetUpTimeout bounds how long startup waits for the node to join the
	// tailnet. Start is synchronous before the first poll, so an unreachable
	// control plane or a bad key must not hang the daemon.
	tailnetUpTimeout = 90 * time.Second
	// tailnetReplayStartGrace holds the startup delivery-replay pass back after
	// the node and its Funnel listener are up. Up returning does not mean the
	// public Funnel relay routes to the node yet (live: about two minutes), and a
	// redelivery requested before then fails. It is a fixed grace; nothing else
	// holds the pass back (ADR 2452 removed the public-DNS probe that once did).
	tailnetReplayStartGrace = 90 * time.Second
	// tailnetFunnelAddr is the only Funnel port the daemon uses.
	tailnetFunnelAddr = ":443"
	// tailnetNodeStateFile is tsnet's default state file (a FileStore at
	// Dir/tailscaled.state); a logged-in profile in it is what makes the auth key
	// optional (tailnetStateExists).
	tailnetNodeStateFile = "tailscaled.state"

	tailnetFunnelOK          = "ok"
	tailnetFunnelUnavailable = "unavailable"
	tailnetFunnelUnknown     = "unknown"
)

// tailnetState is a snapshot of the embedded node.
type tailnetState struct {
	Connected    bool   // the node is up on the tailnet (backend state Running)
	BackendState string // e.g. "Running", "NeedsLogin"; "" if unknown
	DNSName      string // the node's tailnet DNS name, without a trailing dot
	Funnel       string // tailnetFunnelOK, tailnetFunnelUnavailable or tailnetFunnelUnknown
}

// tailnetNode is the seam over tsnet.Server.
type tailnetNode interface {
	// Up joins the tailnet (blocking until running or ctx ends) and reports the
	// node's state, including its reported DNS name.
	Up(ctx context.Context) (tailnetState, error)
	// ListenFunnel returns a TLS-terminated public Funnel listener.
	ListenFunnel(addr string) (net.Listener, error)
	// Watch calls fn on every change of the connection state until ctx ends.
	Watch(ctx context.Context, fn func(tailnetState)) error
	// Close shuts the node down. It is safe before, during and after Up.
	Close() error
}

// tailnetNodeConfig is what the factory needs to build a node.
type tailnetNodeConfig struct {
	Hostname string
	// AuthKey may be empty (a node with state needs none). Never logged.
	AuthKey  string
	StateDir string
	// Logf receives the node's user-facing log lines (already redacted).
	Logf func(format string, args ...any)
}

// tailnetNodeFactory builds the node; tests replace it.
var tailnetNodeFactory = newTailnetNode

// tailnetStateExists reports whether stateDir holds the state of a node that has
// logged in, which is what makes the auth key optional. A non-empty state file
// is not enough: tsnet writes the machine key at Start, before login, so a first
// start that failed on a bad or expired key leaves a non-empty file for a node
// that never joined. A completed login stores a profile (prefs JSON) whose
// "Config" (the persisted node identity) carries a NodeID, and tsnet does not
// persist a profile without one, so that is the signal. The file is a JSON object
// of base64 values; anything unreadable or without such a profile counts as no
// state, so the refusal asks for the key rather than hanging in Up.
func tailnetStateExists(stateDir string) bool {
	bs, err := os.ReadFile(filepath.Join(stateDir, tailnetNodeStateFile))
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

// funnelWebhookURL builds the App webhook URL from the name the node reports
// (not the configured hostname: tsnet suffixes a taken one).
func funnelWebhookURL(dnsName, path string) string {
	dnsName = strings.TrimSuffix(strings.TrimSpace(dnsName), ".")
	if dnsName == "" {
		return ""
	}
	return "https://" + dnsName + path
}

// tailnetLoginURL deliberately matches anywhere in a line (it is a redaction,
// not a validation): over-matching only withholds more. Scheme and host are
// case-insensitive and http is covered, so no spelling of the link slips through.
var tailnetLoginURL = regexp.MustCompile(`(?i)https?://login\.tailscale\.com\S*`)

// redactTailnetLog withholds interactive-login links from the node's log: we
// never do an interactive login, and a one-time auth link is credential-like.
func redactTailnetLog(s string) string {
	return tailnetLoginURL.ReplaceAllString(s, "[login URL withheld]")
}

// withLineEnd ends s with a newline when it lacks one. tsnet's log lines carry
// none, and the engine log adds none, so without this several node lines run
// together on one line.
func withLineEnd(s string) string {
	if strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// startTailscaleTransport starts the embedded Funnel receiver (event_source:
// tailscale, App auth; #2143). Configuration mistakes were already refused in
// New(); an Up or Funnel failure here leaves the daemon poll-only and is
// reported on the warnings panel.
func (e *Engine) startTailscaleTransport(ctx context.Context, set *transportSet, sink func(delivery), initialRepos map[string]bool, bootstrap func()) {
	const tag = "tailscale"
	secretEnvName := appWebhookSecretEnvName(e.cfg)
	authKeyEnvName := tailscaleAuthKeyEnvName(e.cfg)
	path := appWebhookPath(e.cfg)
	stateDir := tailscaleStateDir(e.cfg, e.fabrikDir)
	hostname := e.cfg.TailscaleHostname

	var (
		nodeMu sync.Mutex
		node   tailnetNode
		webURL string
		upSt   tailnetState // what Up reported; seeds the health baseline
	)
	listenFn := func() (net.Listener, error) {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("creating tailscale state dir: %w", err)
		}
		// The key is read here whenever one is configured and goes solely to the
		// node. It is never logged. tsnet ignores it for a node that is logged in
		// from stored state, so passing it is harmless; passing it is also what
		// lets a node whose registration expired or was revoked (NeedsLogin, state
		// file still present) re-authenticate with a fresh key on restart.
		authKey := os.Getenv(authKeyEnvName)
		n, err := tailnetNodeFactory(tailnetNodeConfig{
			Hostname: hostname,
			AuthKey:  authKey,
			StateDir: stateDir,
			Logf: func(format string, args ...any) {
				e.logf(0, tag, "node: %s", withLineEnd(redactTailnetLog(fmt.Sprintf(format, args...))))
			},
		})
		if err != nil {
			return nil, err
		}
		upCtx, cancel := context.WithTimeout(ctx, tailnetUpTimeout)
		defer cancel()
		st, err := n.Up(upCtx)
		if err != nil {
			_ = n.Close()
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("the node did not join the tailnet within %s: %w", tailnetUpTimeout, err)
			}
			return nil, fmt.Errorf("joining the tailnet as %q: %w", hostname, err)
		}
		ln, err := n.ListenFunnel(tailnetFunnelAddr)
		if err != nil {
			_ = n.Close()
			return nil, fmt.Errorf("opening the Funnel listener (check that the tailnet policy grants the \"funnel\" node attribute to this node's tag, and that HTTPS certificates are enabled): %w", err)
		}
		nodeMu.Lock()
		node, webURL, upSt = n, funnelWebhookURL(st.DNSName, path), st
		nodeMu.Unlock()
		return ln, nil
	}

	tr := newAppWebhookTransport(tag, path, listenFn, os.Getenv(secretEnvName), e.logf, e.emit)
	tr.UpdateRepos(initialRepos)
	e.wireAppWebhookWarnings(tr, tag, "tailscale App webhook", secretEnvName)

	// Delivery replay (#2144) applies to this transport as to direct. Built
	// before attach, so the composite reconnect handler (reconcile + replay
	// pass) that attachTransportErr installs reaches it: a tailnet recovery
	// (SetNetworkCondition) then triggers a pass.
	e.replayer = e.newDeliveryReplayer(tr, tag)
	// Webhook route liveness (#2452): built before attach so the composite
	// reconnect handler reaches it. Failed attempts inside the Funnel ingress
	// warm-up do not count against the route.
	e.liveness = e.newRouteLiveness(tr, tag, func() string {
		nodeMu.Lock()
		defer nodeMu.Unlock()
		return webURL
	}, tailnetReplayStartGrace)
	tr.SetCoverageCheck(func() {
		e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: tag, PanelType: "tailscale_coverage", Ref: "#2143"})
		nodeMu.Lock()
		u := webURL
		nodeMu.Unlock()
		e.checkAppWebhookConfig(tag, u)
	})

	watchCtx, cancelWatch := context.WithCancel(ctx)
	var watchWG sync.WaitGroup
	var stopLiveness func() // set once the route-liveness controller is started
	tr.SetStopHook(func() {
		cancelWatch()
		watchWG.Wait()
		if stopLiveness != nil {
			stopLiveness()
		}
		nodeMu.Lock()
		n := node
		nodeMu.Unlock()
		if n != nil {
			if err := n.Close(); err != nil {
				e.logf(0, tag, "closing the tailnet node: %v\n", err)
			}
		}
	})

	listenKey := warnings.Condition{Type: "tailscale_listen", Subject: hostname}.Key()
	if err := e.attachTransportErr(ctx, set, tr, sink, bootstrap); err != nil {
		cancelWatch()
		set.AddFailed(tr)
		e.reportWarning(tag, warnings.Condition{
			Type:    "tailscale_listen",
			Subject: hostname,
			Title:   fmt.Sprintf("Tailscale Funnel receiver %q could not start", hostname),
			Detail: fmt.Sprintf("event_source: tailscale could not start the embedded node: %v\n\nThe daemon is running poll-only. "+
				"Fix the cause (auth key in $%s, tailnet policy and HTTPS certificates, network), then restart.", err, authKeyEnvName),
			Fingerprint: "tailscale_listen:" + hostname,
			LogLine:     fmt.Sprintf("Tailscale Funnel receiver %q could not start: %v — running poll-only", hostname, err),
		})
		return
	}
	e.resolveWarning(tag, listenKey)
	e.resolveWarning(tag, warnings.Condition{Type: "tailscale_signature_drift", Subject: tr.Name()}.Key())

	nodeMu.Lock()
	n, u, up := node, webURL, upSt
	nodeMu.Unlock()
	if u != "" {
		e.logf(0, tag, "event_source: tailscale — node %q is up; App webhook URL: %s (secret from $%s)\n", hostname, u, secretEnvName)
	} else {
		// Up returned before the node reported its DNS name. The URL (and with it
		// the R7 webhook-config check) is filled in from the first watch state that
		// carries the name; say so rather than print an empty URL.
		e.logf(0, tag, "event_source: tailscale — node %q is up but has not reported its tailnet DNS name yet; the App webhook URL is not known (it is logged, and checked against the App, once the name arrives)\n", hostname)
	}

	connKey := warnings.Condition{Type: "tailscale_connection", Subject: hostname}.Key()
	funnelKey := warnings.Condition{Type: "tailscale_funnel", Subject: hostname}.Key()
	// adoptName takes the node's reported DNS name from a watch state: the first
	// time it is known (or when it changes) the webhook URL is updated, logged and
	// checked against the App. Called from the watch callback, so watchWG (held by
	// the Watch goroutine) is non-zero when the check is added to it.
	adoptName := func(dns string) {
		nu := funnelWebhookURL(dns, path)
		if nu == "" {
			return
		}
		nodeMu.Lock()
		changed := nu != webURL
		webURL = nu
		nodeMu.Unlock()
		if !changed {
			return
		}
		e.logf(0, tag, "node %q reports its tailnet DNS name; App webhook URL: %s\n", hostname, nu)
		watchWG.Add(1)
		go func() {
			defer watchWG.Done()
			e.checkAppWebhookConfig(tag, nu)
		}()
	}
	apply := func(st tailnetState) {
		adoptName(st.DNSName)
		tr.SetNetworkCondition(st.Connected, st.BackendState, st.Funnel)
		if st.Connected {
			e.resolveWarning(tag, connKey)
			if st.Funnel == tailnetFunnelUnavailable {
				e.reportWarning(tag, warnings.Condition{
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
				e.resolveWarning(tag, funnelKey)
			}
			return
		}
		e.resolveWarning(tag, funnelKey)
		e.reportWarning(tag, warnings.Condition{
			Type:    "tailscale_connection",
			Subject: hostname,
			Title:   fmt.Sprintf("Tailscale node %q lost its tailnet connection", hostname),
			Detail: fmt.Sprintf("The embedded node's backend state is %q, so GitHub deliveries cannot reach the daemon. "+
				"Polling and the periodic reconcile keep the board current. The connection is retried automatically; "+
				"if the state is NeedsLogin, the node's key expired or was revoked — provide a new auth key in $%s and restart.",
				st.BackendState, authKeyEnvName),
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
		baseFunnel = tailnetFunnelOK
	}
	apply(tailnetState{Connected: true, BackendState: baseState, Funnel: baseFunnel})
	watchWG.Add(1)
	go func() {
		defer watchWG.Done()
		if err := n.Watch(watchCtx, apply); err != nil && watchCtx.Err() == nil {
			e.logf(0, tag, "tailnet watch ended: %v\n", err)
		}
	}()
	e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: tag, PanelType: "tailscale_coverage", Ref: "#2143"})
	e.checkAppWebhookConfig(tag, u)
	// Replay failed deliveries from while the daemon was not receiving (#2144).
	// Background, after the node and its Funnel listener are up and a fixed grace
	// (tailnetReplayStartGrace) for Funnel ingress to reach the node: it never delays
	// startup. A failed start (above) never reaches here, so no pass runs then.
	e.startDeliveryReplayAfter(ctx, e.replayer, tailnetReplayStartGrace)
	// Webhook route liveness from GitHub's delivery log (#2452).
	stopLiveness = e.startRouteLiveness(ctx, e.liveness)
}

// reportWarning records an operator warning, logging (not failing) on error.
func (e *Engine) reportWarning(tag string, c warnings.Condition) {
	if err := warnings.Report(e.warnWriter(tag), c); err != nil {
		e.logf(0, tag, "recording %s warning: %v\n", c.Type, err)
	}
}

// resolveWarning clears a warning by key, logging (not failing) on error.
func (e *Engine) resolveWarning(tag, key string) {
	if err := warnings.Resolve(key); err != nil {
		e.logf(0, tag, "clearing warning %s: %v\n", key, err)
	}
}
