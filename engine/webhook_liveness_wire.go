package engine

import (
	"context"
	"time"

	"github.com/handarbeit/fabrik/warnings"
)

// newRouteLiveness builds the route-liveness controller for transport tr (log
// and panel tag tag: "direct" or "tailscale"), or nil when it cannot work (no
// App auth, no intake): nothing is reported on the health row then. ownURL is
// the URL the receiver is served at ("" when not known); warmup is the span after
// start in which failed attempts do not count (#2452, ADR 2452).
func (e *Engine) newRouteLiveness(tr *appWebhookTransport, tag string, ownURL func() string, warmup time.Duration) *routeLiveness {
	var api livenessAPI
	switch {
	case e.replayAPIOverride != nil:
		api = e.replayAPIOverride
	case e.ghAppAuth != nil:
		api = e.ghAppAuth
	}
	if api == nil || e.intake == nil {
		return nil
	}
	wait := realReplayWait
	if e.livenessWaitOverride != nil {
		wait = e.livenessWaitOverride
	}
	rl := newRouteLiveness(routeLivenessConfig{
		Tag:    tag,
		API:    api,
		Ledger: e.intake,
		Sink:   tr,
		HookURL: func() (string, error) {
			cfg, err := fetchAppHookConfigFn(e)
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
		Report:  func(c warnings.Condition) { e.reportWarning(tag, c) },
		Resolve: func(key string) { e.resolveWarning(tag, key) },
	})
	tr.SetArrivalObserver(rl.Arrival)
	return rl
}

// startRouteLiveness runs rl in the background until ctx ends or the returned
// stop function is called; stop waits for the goroutine to exit, so a transport
// shutting down never leaves it reading engine state. nil rl is a no-op (stop is
// still callable). A stale panel entry from a previous run is cleared first: the
// in-memory verdict starts unknown, and unknown raises nothing.
func (e *Engine) startRouteLiveness(ctx context.Context, rl *routeLiveness) (stop func()) {
	if rl == nil {
		return func() {}
	}
	e.resolveWarning(rl.cfg.Tag, warnings.Condition{Type: livenessPanelType, Subject: rl.cfg.Tag}.Key())
	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rl.Run(rctx)
	}()
	return func() {
		cancel()
		<-done
	}
}
