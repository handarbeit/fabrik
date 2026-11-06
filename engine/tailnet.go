package engine

import (
	"context"
	"fmt"
	"os"

	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/appwebhook/tsnetnode"
	"github.com/handarbeit/fabrik/warnings"
)

// The embedded Tailscale Funnel receiver (event_source: tailscale; #2143,
// ADR 2143) lives in internal/appwebhook (the node seam, the listener factory
// and the health tracker; #2475, ADR 2475) and internal/appwebhook/tsnetnode
// (the only importer of the Tailscale library). This file is the engine's
// wiring: configuration, the panel, replay and route liveness.

// tailnetNodeFactory builds the embedded node; tests replace it.
var tailnetNodeFactory appwebhook.NodeFactory = tsnetnode.New

// startTailscaleTransport starts the embedded Funnel receiver (event_source:
// tailscale, App auth; #2143). Configuration mistakes were already refused in
// New(); an Up or Funnel failure here leaves the daemon poll-only and is
// reported on the warnings panel.
func (e *Engine) startTailscaleTransport(ctx context.Context, set *transportSet, sink func(delivery), initialRepos map[string]bool, bootstrap func()) {
	const tag = "tailscale"
	secretEnvName := appWebhookSecretEnvName(e.cfg)
	authKeyEnvName := tailscaleAuthKeyEnvName(e.cfg)
	path := appWebhookPath(e.cfg)
	hostname := e.cfg.TailscaleHostname
	panel := enginePanel{e: e, tag: tag}

	funnel := appwebhook.NewFunnel(ctx, appwebhook.FunnelConfig{
		Tag:         tag,
		Hostname:    hostname,
		AuthKeyEnv:  authKeyEnvName,
		StateDir:    tailscaleStateDir(e.cfg, e.fabrikDir),
		Path:        path,
		NodeFactory: func(c appwebhook.NodeConfig) (appwebhook.Node, error) { return tailnetNodeFactory(c) },
		Logf:        e.logf,
	})

	tr := newAppWebhookTransport(tag, path, funnel.Listen, os.Getenv(secretEnvName), e.logf, e.emit)
	tr.UpdateRepos(initialRepos)
	e.wireAppWebhookWarnings(tr, tag, "tailscale App webhook", secretEnvName)

	// Delivery replay (#2144) applies to this transport as to direct. Built
	// before attach, so the composite reconnect handler (reconcile + replay
	// pass) that attachTransportErr installs reaches it: a tailnet recovery
	// (SetNetworkCondition) then triggers a pass.
	e.replayer = e.newDeliveryReplayer(tr.Receiver, tag)
	// Webhook route liveness (#2452): built before attach so the composite
	// reconnect handler reaches it. Failed attempts inside the Funnel ingress
	// warm-up do not count against the route.
	e.liveness = e.newRouteLiveness(tr, tag, funnel.URL, appwebhook.ReplayStartGrace)
	tr.SetCoverageCheck(func() {
		e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: tag, PanelType: "tailscale_coverage", Ref: "#2143"})
		e.checkAppWebhookConfig(tag, funnel.URL())
	})

	var stopLiveness func() // set once the route-liveness controller is started
	tr.SetStopHook(func() {
		funnel.StopWatch()
		if stopLiveness != nil {
			stopLiveness()
		}
		funnel.CloseNode()
	})

	listenKey := warnings.Condition{Type: "tailscale_listen", Subject: hostname}.Key()
	if err := e.attachTransportErr(ctx, set, tr, sink, bootstrap); err != nil {
		funnel.CancelWatch()
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

	if u := funnel.URL(); u != "" {
		e.logf(0, tag, "event_source: tailscale — node %q is up; App webhook URL: %s (secret from $%s)\n", hostname, u, secretEnvName)
	} else {
		// Up returned before the node reported its DNS name. The URL (and with it
		// the R7 webhook-config check) is filled in from the first watch state that
		// carries the name; say so rather than print an empty URL.
		e.logf(0, tag, "event_source: tailscale — node %q is up but has not reported its tailnet DNS name yet; the App webhook URL is not known (it is logged, and checked against the App, once the name arrives)\n", hostname)
	}

	funnel.Watch(tr.Receiver, panel, func(u string) { e.checkAppWebhookConfig(tag, u) })
	e.checkAppInstallationCoverage(tr, appCoverageSpec{Tag: tag, PanelType: "tailscale_coverage", Ref: "#2143"})
	e.checkAppWebhookConfig(tag, funnel.URL())
	// Replay failed deliveries from while the daemon was not receiving (#2144).
	// Background, after the node and its Funnel listener are up and a fixed grace
	// (ReplayStartGrace) for Funnel ingress to reach the node: it never delays
	// startup. A failed start (above) never reaches here, so no pass runs then.
	e.replayer.StartAfter(ctx, appwebhook.ReplayStartGrace)
	// Webhook route liveness from GitHub's delivery log (#2452).
	stopLiveness = e.liveness.Start(ctx)
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
