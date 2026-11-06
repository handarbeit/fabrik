package pruefer

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	gh "github.com/handarbeit/fabrik/github"
	"github.com/handarbeit/fabrik/internal/appwebhook"
	"github.com/handarbeit/fabrik/internal/appwebhook/tsnetnode"
	"github.com/handarbeit/fabrik/internal/events"
	"github.com/handarbeit/fabrik/internal/localapi"
	ptui "github.com/handarbeit/fabrik/pruefer/tui"
	"github.com/handarbeit/fabrik/warnings"
)

// appWebhookSource is Pruefer's events.EventSource for event_source: direct and
// event_source: tailscale (#2476, ADR 2476). It wraps the shared receiver
// (internal/appwebhook), which owns the HTTP handler, the listener sources, the
// replayer, route liveness and the webhook-configuration check; this type adds
// only what needs Pruefer: the delivery sink (appsource_sink.go), the footer and
// log reporting, and the wiring order. Being an events.EventSource, it runs
// under Daemon.runEventDriven unchanged — the startup reconciliation poll and
// the low-frequency fallback poll stay exactly as they are (FR-008).
//
// The wiring mirrors engine/tailnet.go and engine/intake_wire.go. Pruefer cannot
// import the engine (ADR 1113), so the order is restated here: build the
// receiver, replayer and liveness controller; install the composite reconnect
// handler; start; then (tailscale) watch the node and check the App's hook
// configuration; then start replay and liveness.

// appHookAPI is the slice of *githubauth.Reconciler this source needs: the App's
// delivery log (replay and route liveness) and its hook configuration (the
// tailscale webhook-configuration check). All of it is authenticated with the
// App's own JWT, minted inside the Reconciler.
type appHookAPI interface {
	appwebhook.HookDeliveryAPI
	FetchAppHookConfig(ctx context.Context) (gh.AppHookConfig, error)
}

// appNodeFactory builds the embedded Tailscale node. Production is tsnetnode.New;
// tests replace it with a fake so no tailnet is needed.
var appNodeFactory appwebhook.NodeFactory = tsnetnode.New

// DefaultTransportSampleInterval is how often the receiver's health row is
// sampled for the footer. The receiver's health callback only fires on derived
// state changes and deliveries; the tailnet, Funnel, route and last-delivery
// fields change without one, so the row is polled as well.
const DefaultTransportSampleInterval = 5 * time.Second

type appWebhookSource struct {
	daemon *Daemon
	cfg    Config
	// secret is the App's webhook secret; authKey is read inside the Funnel from
	// the variable named by cfg.TailscaleAuthKeyEnv, so only the secret is held.
	secret string
	api    appHookAPI
	dedupe *events.Dedupe

	// Test seams. Zero values are production.
	sampleEvery  time.Duration
	replayWait   func(ctx context.Context, d time.Duration) bool
	livenessWait func(ctx context.Context, d time.Duration) bool
}

// newAppWebhookSource builds the source for cfg.EventSource (direct or
// tailscale). secret is the already-resolved webhook secret (see
// RefuseAppSourceMisconfigured).
func newAppWebhookSource(d *Daemon, cfg Config, secret string) *appWebhookSource {
	s := &appWebhookSource{
		daemon: d,
		cfg:    cfg,
		secret: secret,
		dedupe: events.NewDedupe(0),
	}
	// A nil *Reconciler must not become a non-nil interface holding nil.
	if d.Reconciler != nil {
		s.api = d.Reconciler
	}
	return s
}

func (s *appWebhookSource) tag() string { return s.cfg.EventSource }

// Run implements events.EventSource. It blocks until ctx is cancelled. A start
// failure (port in use, tailnet unreachable, Funnel not permitted) is reported
// and leaves the daemon on its reconciliation fallback poll: Run keeps blocking
// rather than returning, so runEventDriven does not log a misleading "event
// source exited unexpectedly" (FR-010).
func (s *appWebhookSource) Run(ctx context.Context, sink events.EventSink) error {
	tag := s.tag()
	d := s.daemon
	panel := newLogPanel(d, tag)
	asink := newAppSink(ctx, d, sink, s.dedupe)
	path := s.cfg.AppWebhookPath

	var (
		listen func() (net.Listener, error)
		funnel *appwebhook.Funnel
		warmup time.Duration
		ownURL func() string
	)
	subject := s.cfg.DirectListen
	if s.cfg.EventSource == EventSourceTailscale {
		funnel = appwebhook.NewFunnel(ctx, appwebhook.FunnelConfig{
			Tag:         tag,
			Hostname:    s.cfg.TailscaleHostname,
			AuthKeyEnv:  s.cfg.TailscaleAuthKeyEnv,
			StateDir:    s.cfg.TailscaleStateDir,
			Path:        path,
			NodeFactory: func(c appwebhook.NodeConfig) (appwebhook.Node, error) { return appNodeFactory(c) },
			Logf:        logf,
		})
		listen = funnel.Listen
		warmup = appwebhook.ReplayStartGrace
		ownURL = funnel.URL
		subject = s.cfg.TailscaleHostname
	} else {
		listen = appwebhook.DirectListener(s.cfg.DirectListen)
	}

	kick := make(chan struct{}, 1)
	var lastState string
	var stateMu sync.Mutex
	rcv := appwebhook.New(appwebhook.Config{
		Name:   tag,
		Path:   path,
		Listen: listen,
		Secret: s.secret,
		Logf:   logf,
		Health: func(state string, _ map[string]int, _ string) {
			stateMu.Lock()
			changed := state != lastState
			lastState = state
			stateMu.Unlock()
			if changed {
				logf(0, tag, "event source %s\n", state)
			}
			select {
			case kick <- struct{}{}:
			default:
			}
		},
	})
	rcv.SetSink(asink.Deliver)
	rcv.SetDriftHandler(s.driftHandler(panel))

	// Delivery replay (#2144 in the engine): asks GitHub to redeliver deliveries
	// that failed while the daemon was down. Its "seen" check is the read-only
	// ledger, so it never records and never dispatches (FR-006).
	var replayer *appwebhook.Replayer
	if s.cfg.ReplayFailedDeliveries && s.api != nil {
		replayer = appwebhook.NewReplayer(s.api, ledgerView{s.dedupe}, rcv,
			func(format string, args ...any) { logf(0, tag, format, args...) }, nil)
		if s.replayWait != nil {
			replayer.SetWait(s.replayWait)
		}
	}
	// Route liveness (#2452 in the engine): whether GitHub can deliver to the
	// webhook, judged from GitHub's own delivery log.
	var liveness *appwebhook.Liveness
	if s.api != nil {
		wait := appwebhook.RealWait
		if s.livenessWait != nil {
			wait = s.livenessWait
		}
		liveness = appwebhook.NewLiveness(appwebhook.LivenessConfig{
			Tag:    tag,
			API:    s.api,
			Ledger: ledgerView{s.dedupe},
			Sink:   rcv,
			HookURL: func(c context.Context) (string, error) {
				cfg, err := s.api.FetchAppHookConfig(c)
				if err != nil {
					return "", err
				}
				return cfg.URL, nil
			},
			OwnURL:  ownURL,
			Logf:    func(format string, args ...any) { logf(0, tag, "route: "+format, args...) },
			Wait:    wait,
			Warmup:  warmup,
			Report:  panel.Report,
			Resolve: panel.Resolve,
		})
		rcv.SetArrivalObserver(liveness.Arrival)
	}
	// A reconnect (a tailnet or Funnel recovery; the direct receiver never fires
	// it) catches up: a reconciliation poll, a replay pass and a liveness check.
	// Replayer.Trigger and Liveness.Trigger are nil-safe and coalescing.
	rcv.SetReconnectHandler(func() {
		d.triggerReconciliationPoll(ctx)
		replayer.Trigger()
		liveness.Trigger()
	})

	var hookCheck appwebhook.HookChecker
	checkHook := func(url string) {
		if url == "" || s.api == nil {
			return
		}
		hookCheck.Check(appwebhook.HookCheckInput{
			Tag:     tag,
			WantURL: url,
			Fetch:   s.api.FetchAppHookConfig,
			Panel:   panel,
			Logf:    logf,
		})
	}

	var stopLiveness func()
	rcv.SetStopHook(func() {
		if funnel != nil {
			funnel.StopWatch()
		}
		if stopLiveness != nil {
			stopLiveness()
		}
		if funnel != nil {
			funnel.CloseNode()
		}
	})

	listenKey := warnings.Condition{Type: tag + "_listen", Subject: subject}.Key()
	if err := rcv.Start(ctx); err != nil {
		if funnel != nil {
			funnel.CancelWatch()
		}
		s.reportListenFailure(panel, subject, err)
	} else {
		panel.Resolve(listenKey)
		s.logStarted(rcv, funnel)
		if funnel != nil {
			funnel.Watch(rcv, panel, checkHook)
			checkHook(funnel.URL())
		}
		// Replay and liveness run in the background, after the listener is up and
		// (tailscale) a fixed grace for Funnel ingress to reach a new node, so
		// neither delays startup.
		replayer.StartAfter(ctx, warmup)
		stopLiveness = liveness.Start(ctx)
	}

	s.sampleLoop(ctx, rcv, kick)

	// ctx is cancelled: stop the listener, the node and the controllers, and
	// wait (bounded) for the replayer's goroutines.
	rcv.Stop()
	if replayer != nil && !replayer.WaitStopped(5*time.Second) {
		logf(0, tag, "replayer did not stop within 5s of shutdown\n")
	}
	return nil
}

// driftHandler reports signature drift: the shared handler's log line and
// footer banner (Daemon.SignatureDriftHandler), plus the secret-specific
// guidance for these sources. The drift condition itself is level-triggered by
// the receiver, so this only needs to relay each transition.
func (s *appWebhookSource) driftHandler(panel *logPanel) func(bool) {
	base := s.daemon.SignatureDriftHandler()
	return func(active bool) {
		if active {
			logf(0, s.tag(), "signature verification is failing on every delivery — make the App's webhook secret equal the value of $%s, and set the App's webhook content type to application/json\n",
				s.cfg.AppWebhookSecretEnv)
		}
		base(active)
	}
}

// reportListenFailure reports that the receiver could not start. The daemon
// stays up on its fallback poll.
func (s *appWebhookSource) reportListenFailure(panel *logPanel, subject string, err error) {
	tag := s.tag()
	var title, fix string
	if s.cfg.EventSource == EventSourceTailscale {
		title = fmt.Sprintf("Tailscale Funnel receiver %q could not start", subject)
		fix = fmt.Sprintf("Fix the cause (auth key in $%s, tailnet policy and HTTPS certificates, network), then restart.", s.cfg.TailscaleAuthKeyEnv)
	} else {
		title = fmt.Sprintf("Direct App webhook listener could not start on %s", subject)
		fix = "Free the port or correct direct_listen (--direct-listen / PRUEFER_DIRECT_LISTEN), then restart."
	}
	panel.Report(warnings.Condition{
		Type:        tag + "_listen",
		Subject:     subject,
		Title:       title,
		Detail:      fmt.Sprintf("event_source: %s could not start: %v\n\nThe daemon is running on its reconciliation fallback poll only. %s", tag, err, fix),
		Fingerprint: tag + "_listen:" + subject,
		LogLine:     fmt.Sprintf("%s: %v — running on the reconciliation fallback poll only", title, err),
	})
}

// logStarted prints the one-line startup summary. It names variables, never
// their values.
func (s *appWebhookSource) logStarted(rcv *appwebhook.Receiver, funnel *appwebhook.Funnel) {
	tag := s.tag()
	if funnel == nil {
		logf(0, tag, "event_source: direct — listening on %s%s (plain HTTP; TLS is the fronting proxy's job), secret from $%s\n",
			rcv.Addr(), s.cfg.AppWebhookPath, s.cfg.AppWebhookSecretEnv)
		return
	}
	if u := funnel.URL(); u != "" {
		logf(0, tag, "event_source: tailscale — node %q is up; App webhook URL: %s (secret from $%s)\n",
			s.cfg.TailscaleHostname, u, s.cfg.AppWebhookSecretEnv)
		return
	}
	logf(0, tag, "event_source: tailscale — node %q is up but has not reported its tailnet DNS name yet; the App webhook URL is logged, and checked against the App, once the name arrives\n",
		s.cfg.TailscaleHostname)
}

// sampleLoop publishes the receiver's health row to the footer: on every
// interval, and at once when the receiver's own health callback fires. It sends
// an event only when the row changed, and returns when ctx is cancelled.
func (s *appWebhookSource) sampleLoop(ctx context.Context, rcv *appwebhook.Receiver, kick <-chan struct{}) {
	every := s.sampleEvery
	if every <= 0 {
		every = DefaultTransportSampleInterval
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	var last ptui.TransportEvent
	var sent bool
	publish := func() {
		ev := transportEventFromRow(rcv.HealthRow())
		ev.At = time.Time{}
		if sent && ev == last {
			return
		}
		last, sent = ev, true
		ev.At = time.Now()
		s.daemon.emit(ev)
	}
	publish()
	for {
		select {
		case <-ctx.Done():
			return
		case <-kick:
			publish()
		case <-ticker.C:
			publish()
		}
	}
}

// transportEventFromRow flattens a receiver health row into the footer's plain
// event. Unknown values become "" (or the zero time).
func transportEventFromRow(r localapi.TransportHealth) ptui.TransportEvent {
	ev := ptui.TransportEvent{Name: r.Name, State: r.State}
	if r.Tailnet.Valid {
		ev.Tailnet = r.Tailnet.V
	}
	if r.Funnel.Valid {
		ev.Funnel = r.Funnel.V
	}
	if r.LastDeliveryAt.Valid {
		ev.LastDelivery = r.LastDeliveryAt.V
	}
	if r.Route != nil {
		ev.Route = r.Route.Liveness
	}
	return ev
}

var _ events.EventSource = (*appWebhookSource)(nil)
