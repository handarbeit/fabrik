package engine

import (
	"context"
	"time"

	"github.com/handarbeit/fabrik/internal/appwebhook"
)

// Route liveness itself lives in internal/appwebhook (#2475, ADR 2475; design
// #2452, ADR 2452). This file is the engine's factory for it.

// newRouteLiveness builds the route-liveness controller for transport tr (log
// and panel tag tag: "direct" or "tailscale"), or nil when it cannot work (no
// App auth, no intake): nothing is reported on the health row then. ownURL is
// the URL the receiver is served at ("" when not known); warmup is the span after
// start in which failed attempts do not count (#2452, ADR 2452).
func (e *Engine) newRouteLiveness(tr *appWebhookTransport, tag string, ownURL func() string, warmup time.Duration) *appwebhook.Liveness {
	var api appwebhook.LivenessAPI
	switch {
	case e.replayAPIOverride != nil:
		api = e.replayAPIOverride
	case e.ghAppAuth != nil:
		api = e.ghAppAuth
	}
	if api == nil || e.intake == nil {
		return nil
	}
	wait := appwebhook.RealWait
	if e.livenessWaitOverride != nil {
		wait = e.livenessWaitOverride
	}
	panel := enginePanel{e: e, tag: tag}
	rl := appwebhook.NewLiveness(appwebhook.LivenessConfig{
		Tag:    tag,
		API:    api,
		Ledger: ledgerView{e.intake},
		Sink:   tr,
		HookURL: func(ctx context.Context) (string, error) {
			cfg, err := fetchAppHookConfigFn(ctx, e)
			if err != nil {
				return "", err
			}
			return cfg.URL, nil
		},
		OwnURL:  ownURL,
		Logf:    func(format string, args ...any) { e.logf(0, tag, "route: "+format, args...) },
		Now:     e.now,
		Wait:    wait,
		Warmup:  warmup,
		Report:  panel.Report,
		Resolve: panel.Resolve,
	})
	tr.SetArrivalObserver(rl.Arrival)
	return rl
}

// SetLivenessWaitForTest replaces the sleep the webhook route-liveness
// controller (#2452) uses between wake-ups and while waiting for a probe's
// result, so a test controls it without real time. wait returns false if ctx
// ended first and must block (not return at once) when the test wants the
// controller idle. Call before setUpIngestion; production code never sets it.
func (e *Engine) SetLivenessWaitForTest(wait func(ctx context.Context, d time.Duration) bool) {
	e.livenessWaitOverride = wait
}
